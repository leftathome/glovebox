package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// GDriveConnector polls the Google Drive Changes API for file activity.
type GDriveConnector struct {
	config       Config
	writer       connector.StagingBackend
	matcher      *connector.RuleMatcher
	fetchCounter *connector.FetchCounter
	httpClient   *http.Client
	tokenSource  connector.TokenSource
	apiBase      string // e.g. "https://www.googleapis.com" or test server URL
}

// driveChange represents a single change entry from the Drive Changes API.
type driveChange struct {
	FileID  string    `json:"fileId"`
	Removed bool      `json:"removed"`
	File    *fileInfo `json:"file,omitempty"`
}

// fileInfo holds the file metadata fields we care about.
type fileInfo struct {
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	ModifiedTime string `json:"modifiedTime"`
}

// changesResponse is the top-level response from the Drive Changes API.
type changesResponse struct {
	Changes           []driveChange `json:"changes"`
	NewStartPageToken string        `json:"newStartPageToken"`
}

// startPageTokenResponse is the response from the Drive startPageToken endpoint.
type startPageTokenResponse struct {
	StartPageToken string `json:"startPageToken"`
}

// changeContent is the JSON payload staged for each change event.
type changeContent struct {
	FileID       string `json:"fileId"`
	FileName     string `json:"fileName"`
	MimeType     string `json:"mimeType"`
	ModifiedTime string `json:"modifiedTime"`
	Removed      bool   `json:"removed"`
}

const cpKey = "drive:changes"

func (c *GDriveConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	pageToken, hasCheckpoint := checkpoint.Load(cpKey)
	if !hasCheckpoint {
		// First run: fetch the initial start page token.
		token, err := c.fetchStartPageToken(ctx)
		if err != nil {
			return fmt.Errorf("fetch start page token: %w", err)
		}
		pageToken = token
		logger.Info("fetched initial start page token", "token", pageToken)
	}

	changes, newToken, err := c.fetchChanges(ctx, pageToken)
	if err != nil {
		return fmt.Errorf("fetch changes: %w", err)
	}

	result, ok := c.matcher.Match(cpKey)
	if !ok {
		logger.Warn("no rule for drive:changes, skipping")
		return nil
	}

	for _, change := range changes {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if status := c.fetchCounter.TryFetch("drive"); !status.Allowed() {
			logger.Info("fetch limit reached, stopping", "status", status)
			break
		}

		content := changeContent{
			FileID:  change.FileID,
			Removed: change.Removed,
		}
		subject := "file change: " + change.FileID
		if change.File != nil {
			content.FileName = change.File.Name
			content.MimeType = change.File.MimeType
			content.ModifiedTime = change.File.ModifiedTime
			subject = "file change: " + change.File.Name
		}

		contentBytes, err := json.Marshal(content)
		if err != nil {
			return fmt.Errorf("marshal change content: %w", err)
		}

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "gdrive",
			Sender:           "drive",
			Subject:          subject,
			Timestamp:        time.Now().UTC(),
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "google", AuthMethod: "oauth"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(contentBytes); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}
	}

	if err := checkpoint.Save(cpKey, newToken); err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}

	return nil
}

func (c *GDriveConnector) fetchStartPageToken(ctx context.Context) (string, error) {
	url := c.apiBase + "/drive/v3/changes/startPageToken"
	body, err := c.fetchAPI(ctx, url)
	if err != nil {
		return "", err
	}

	var resp startPageTokenResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse startPageToken response: %w", err)
	}
	return resp.StartPageToken, nil
}

func (c *GDriveConnector) fetchChanges(ctx context.Context, pageToken string) ([]driveChange, string, error) {
	url := c.apiBase + "/drive/v3/changes?pageToken=" + pageToken +
		"&fields=changes(fileId,file(name,mimeType,modifiedTime),removed),newStartPageToken"

	body, err := c.fetchAPI(ctx, url)
	if err != nil {
		return nil, "", err
	}

	var resp changesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", fmt.Errorf("parse changes response: %w", err)
	}

	return resp.Changes, resp.NewStartPageToken, nil
}

func (c *GDriveConnector) fetchAPI(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("get token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	const maxBody = 10 << 20 // 10 MB
	limited := io.LimitReader(resp.Body, maxBody)
	return io.ReadAll(limited)
}
