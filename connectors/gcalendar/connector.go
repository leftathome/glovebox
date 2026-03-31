package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// GCalendarConnector polls Google Calendar events.
type GCalendarConnector struct {
	config       Config
	writer       *connector.StagingWriter
	matcher      *connector.RuleMatcher
	fetchCounter *connector.FetchCounter
	httpClient   *http.Client
	tokenSource  connector.TokenSource
	apiBase      string // e.g. "https://www.googleapis.com" or test server URL
}

// gcalEvent represents a single Google Calendar event from the events list.
type gcalEvent struct {
	ID          string          `json:"id"`
	Summary     string          `json:"summary"`
	Description string          `json:"description,omitempty"`
	Location    string          `json:"location,omitempty"`
	Updated     string          `json:"updated"`
	Start       json.RawMessage `json:"start,omitempty"`
	End         json.RawMessage `json:"end,omitempty"`
	Attendees   json.RawMessage `json:"attendees,omitempty"`
}

// gcalEventsResponse is the Google Calendar events list response.
type gcalEventsResponse struct {
	Items []gcalEvent `json:"items"`
}

func (c *GCalendarConnector) Poll(ctx context.Context, checkpoint connector.Checkpoint) error {
	logger := slog.Default()

	for _, calID := range c.config.CalendarIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.pollCalendar(ctx, calID, checkpoint, logger); err != nil {
			logger.Warn("calendar poll failed", "calendar_id", calID, "error", err)
		}
	}
	return nil
}

func (c *GCalendarConnector) pollCalendar(ctx context.Context, calID string, checkpoint connector.Checkpoint, logger *slog.Logger) error {
	cpKey := "calendar:" + calID

	apiURL := fmt.Sprintf("%s/calendar/v3/calendars/%s/events", c.apiBase, url.PathEscape(calID))

	params := url.Values{}
	params.Set("singleEvents", "true")
	params.Set("orderBy", "updated")
	params.Set("maxResults", "25")

	lastUpdated, hasCheckpoint := checkpoint.Load(cpKey)
	if hasCheckpoint {
		params.Set("updatedMin", lastUpdated)
	}

	fullURL := apiURL + "?" + params.Encode()

	body, err := c.fetchAPI(ctx, fullURL)
	if err != nil {
		return fmt.Errorf("fetch events for calendar %s: %w", calID, err)
	}

	var resp gcalEventsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parse events for calendar %s: %w", calID, err)
	}

	if len(resp.Items) == 0 {
		return nil
	}

	ruleKey := "calendar:" + calID
	result, ok := c.matcher.Match(ruleKey)
	if !ok {
		logger.Warn("no rule for calendar, skipping", "calendar_id", calID)
		return nil
	}

	latestUpdated := lastUpdated

	for _, ev := range resp.Items {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Dedup: skip events whose updated timestamp is not strictly newer
		// than the checkpoint. Google Calendar API returns events with
		// updatedMin >= checkpoint, so the checkpoint event itself may
		// reappear.
		if hasCheckpoint && ev.Updated <= lastUpdated {
			continue
		}

		if status := c.fetchCounter.TryFetch(calID); !status.Allowed() {
			logger.Info("fetch limit reached, stopping", "calendar_id", calID, "status", status)
			break
		}

		content, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("marshal event content: %w", err)
		}

		item, err := c.writer.NewItem(connector.ItemOptions{
			Source:           "gcalendar",
			Sender:           calID,
			Subject:          ev.Summary,
			Timestamp:        time.Now().UTC(),
			DestinationAgent: result.Destination,
			ContentType:      "application/json",
			RuleTags:         result.Tags,
			Identity:         &connector.Identity{Provider: "google", AuthMethod: "oauth"},
		})
		if err != nil {
			return fmt.Errorf("new staging item: %w", err)
		}

		if err := item.WriteContent(content); err != nil {
			return fmt.Errorf("write content: %w", err)
		}

		if err := item.Commit(); err != nil {
			return fmt.Errorf("commit item: %w", err)
		}

		if ev.Updated > latestUpdated {
			latestUpdated = ev.Updated
		}
	}

	if latestUpdated != lastUpdated && latestUpdated != "" {
		if err := checkpoint.Save(cpKey, latestUpdated); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
	}

	return nil
}

func (c *GCalendarConnector) fetchAPI(ctx context.Context, url string) ([]byte, error) {
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
