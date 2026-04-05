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

// OneDriveConnector polls the Microsoft Graph delta API for drive file activity.
type OneDriveConnector struct {
	config       Config
	writer       *connector.StagingWriter
	matcher      *connector.RuleMatcher
	fetchCounter *connector.FetchCounter
	httpClient   *http.Client
	tokenSource  connector.TokenSource
	apiBase      string // e.g. "https://graph.microsoft.com" or test server URL
}

// deltaItem represents a single item from the Graph delta response value array.
type deltaItem struct {
	ID                   string      `json:"id"`
	Name                 string      `json:"name"`
	LastModifiedDateTime string      `json:"lastModifiedDateTime"`
	File                 interface{} `json:"file,omitempty"`
	Folder               interface{} `json:"folder,omitempty"`
}

// deltaResponse is the top-level response from the Graph delta API.
type deltaResponse struct {
	Value     []deltaItem `json:"value"`
	DeltaLink string      `json:"@odata.deltaLink"`
}

// changeContent is the JSON payload staged for each change event.
type changeContent struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	LastModifiedDateTime string `json:"lastModifiedDateTime"`
	ChangeType           string `json:"changeType"`
}

const cpKey = "drive:changes"

// deltaPath is the initial delta endpoint path appended to apiBase.
const deltaPath = "/v1.0/me/drive/root/delta"

func (c *OneDriveConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	var deltaURL string
	storedLink, hasCheckpoint := checkpoint.Load(cpKey)
	if hasCheckpoint {
		// Subsequent run: use the stored deltaLink directly.
		deltaURL = storedLink
	} else {
		// First run: fetch initial delta from the well-known endpoint.
		deltaURL = c.apiBase + deltaPath
		logger.Info("no checkpoint, fetching initial delta")
	}

	items, newDeltaLink, err := c.fetchDelta(ctx, deltaURL)
	if err != nil {
		return fmt.Errorf("fetch delta: %w", err)
	}

	result, ok := c.matcher.Match(cpKey)
	if !ok {
		logger.Warn("no rule for drive:changes, skipping")
		return nil
	}

	for _, item := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if status := c.fetchCounter.TryFetch("drive"); !status.Allowed() {
			logger.Info("fetch limit reached, stopping", "status", status)
			break
		}

		changeType := "file"
		if item.Folder != nil {
			changeType = "folder"
		}

		content := changeContent{
			ID:                   item.ID,
			Name:                 item.Name,
			LastModifiedDateTime: item.LastModifiedDateTime,
			ChangeType:           changeType,
		}

		contentBytes, err := json.Marshal(content)
		if err != nil {
			return fmt.Errorf("marshal change content: %w", err)
		}

		subject := "file change: " + item.Name

		si, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "onedrive",
			Sender:           "drive",
			Subject:          subject,
			Timestamp:        time.Now().UTC(),
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "microsoft", AuthMethod: "oauth"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := si.WriteContent(contentBytes); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := si.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}
	}

	if err := checkpoint.Save(cpKey, newDeltaLink); err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}

	return nil
}

func (c *OneDriveConnector) fetchDelta(ctx context.Context, deltaURL string) ([]deltaItem, string, error) {
	body, err := c.fetchAPI(ctx, deltaURL)
	if err != nil {
		return nil, "", err
	}

	var resp deltaResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", fmt.Errorf("parse delta response: %w", err)
	}

	return resp.Value, resp.DeltaLink, nil
}

func (c *OneDriveConnector) fetchAPI(ctx context.Context, url string) ([]byte, error) {
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
