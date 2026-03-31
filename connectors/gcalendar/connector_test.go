package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// newTestConnector creates a GCalendarConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, calendarIDs []string, apiBase string, rules []connector.Rule) (*GCalendarConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "gcalendar")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &GCalendarConnector{
		config: Config{
			CalendarIDs: calendarIDs,
		},
		writer:       writer,
		matcher:      matcher,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tokenSource:  connector.NewStaticTokenSource("test-token"),
		apiBase:      apiBase,
	}

	return c, stagingDir, stateDir
}

func newCheckpoint(t *testing.T, stateDir string) connector.Checkpoint {
	t.Helper()
	cp, err := connector.NewCheckpoint(stateDir)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	return cp
}

func countStagedItems(t *testing.T, stagingDir string) int {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			count++
		}
	}
	return count
}

// makeCalendarEvents builds a Google Calendar events list response.
func makeCalendarEvents(events ...calendarEvent) []byte {
	resp := eventsListResponse{Items: events}
	data, _ := json.Marshal(resp)
	return data
}

type calendarEvent struct {
	ID          string          `json:"id"`
	Summary     string          `json:"summary"`
	Description string          `json:"description,omitempty"`
	Location    string          `json:"location,omitempty"`
	Updated     string          `json:"updated"`
	Start       *eventDateTime  `json:"start,omitempty"`
	End         *eventDateTime  `json:"end,omitempty"`
	Attendees   []eventAttendee `json:"attendees,omitempty"`
}

type eventDateTime struct {
	DateTime string `json:"dateTime,omitempty"`
	Date     string `json:"date,omitempty"`
}

type eventAttendee struct {
	Email string `json:"email"`
}

type eventsListResponse struct {
	Items []calendarEvent `json:"items"`
}

func TestPollFetchesEventsAndStages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeCalendarEvents(
			calendarEvent{
				ID:      "evt1",
				Summary: "Team standup",
				Updated: "2025-01-01T10:00:00Z",
				Start:   &eventDateTime{DateTime: "2025-01-02T09:00:00Z"},
				End:     &eventDateTime{DateTime: "2025-01-02T09:30:00Z"},
			},
			calendarEvent{
				ID:      "evt2",
				Summary: "Lunch",
				Updated: "2025-01-01T11:00:00Z",
				Start:   &eventDateTime{DateTime: "2025-01-02T12:00:00Z"},
				End:     &eventDateTime{DateTime: "2025-01-02T13:00:00Z"},
			},
			calendarEvent{
				ID:          "evt3",
				Summary:     "Meeting with client",
				Description: "Discuss project scope",
				Location:    "Conference Room A",
				Updated:     "2025-01-01T12:00:00Z",
				Start:       &eventDateTime{DateTime: "2025-01-02T14:00:00Z"},
				End:         &eventDateTime{DateTime: "2025-01-02T15:00:00Z"},
				Attendees:   []eventAttendee{{Email: "client@example.com"}},
			},
		))
	}))
	defer srv.Close()

	calendarIDs := []string{"primary"}
	rules := []connector.Rule{
		{Match: "calendar:primary", Destination: "schedule-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, calendarIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 3 {
		t.Errorf("expected 3 staged items, got %d", count)
	}
}

func TestCheckpointPreventsDuplicates(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if callCount == 0 {
			// First call: return 2 events.
			w.Write(makeCalendarEvents(
				calendarEvent{
					ID:      "evt1",
					Summary: "Event one",
					Updated: "2025-01-01T10:00:00Z",
					Start:   &eventDateTime{DateTime: "2025-01-02T09:00:00Z"},
					End:     &eventDateTime{DateTime: "2025-01-02T09:30:00Z"},
				},
				calendarEvent{
					ID:      "evt2",
					Summary: "Event two",
					Updated: "2025-01-01T11:00:00Z",
					Start:   &eventDateTime{DateTime: "2025-01-02T10:00:00Z"},
					End:     &eventDateTime{DateTime: "2025-01-02T10:30:00Z"},
				},
			))
		} else {
			// Second call: same events plus one new one.
			w.Write(makeCalendarEvents(
				calendarEvent{
					ID:      "evt1",
					Summary: "Event one",
					Updated: "2025-01-01T10:00:00Z",
					Start:   &eventDateTime{DateTime: "2025-01-02T09:00:00Z"},
					End:     &eventDateTime{DateTime: "2025-01-02T09:30:00Z"},
				},
				calendarEvent{
					ID:      "evt2",
					Summary: "Event two",
					Updated: "2025-01-01T11:00:00Z",
					Start:   &eventDateTime{DateTime: "2025-01-02T10:00:00Z"},
					End:     &eventDateTime{DateTime: "2025-01-02T10:30:00Z"},
				},
				calendarEvent{
					ID:      "evt3",
					Summary: "Event three",
					Updated: "2025-01-01T12:00:00Z",
					Start:   &eventDateTime{DateTime: "2025-01-02T11:00:00Z"},
					End:     &eventDateTime{DateTime: "2025-01-02T11:30:00Z"},
				},
			))
		}
		callCount++
	}))
	defer srv.Close()

	calendarIDs := []string{"primary"}
	rules := []connector.Rule{
		{Match: "calendar:primary", Destination: "schedule-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, calendarIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// First poll: should stage 2 events.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", got)
	}

	// Second poll: should stage only 1 new event.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 3 {
		t.Errorf("expected 3 items total after second poll, got %d", got)
	}
}

func TestIdentityFieldsInMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeCalendarEvents(
			calendarEvent{
				ID:      "evt100",
				Summary: "Identity test event",
				Updated: "2025-01-01T10:00:00Z",
				Start:   &eventDateTime{DateTime: "2025-01-02T09:00:00Z"},
				End:     &eventDateTime{DateTime: "2025-01-02T09:30:00Z"},
			},
		))
	}))
	defer srv.Close()

	calendarIDs := []string{"primary"}
	rules := []connector.Rule{
		{Match: "calendar:primary", Destination: "schedule-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, calendarIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found = true
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}

		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}

		identity, ok := meta["identity"].(map[string]interface{})
		if !ok {
			t.Fatal("expected identity object in metadata")
		}
		if identity["provider"] != "google" {
			t.Errorf("expected identity provider 'google', got %v", identity["provider"])
		}
		if identity["auth_method"] != "oauth" {
			t.Errorf("expected identity auth_method 'oauth', got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestRuleTagsInMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeCalendarEvents(
			calendarEvent{
				ID:      "evt200",
				Summary: "Tags test event",
				Updated: "2025-01-01T10:00:00Z",
				Start:   &eventDateTime{DateTime: "2025-01-02T09:00:00Z"},
				End:     &eventDateTime{DateTime: "2025-01-02T09:30:00Z"},
			},
		))
	}))
	defer srv.Close()

	calendarIDs := []string{"primary"}
	rules := []connector.Rule{
		{
			Match:       "calendar:primary",
			Destination: "schedule-agent",
			Tags:        map[string]string{"source_type": "calendar", "priority": "normal"},
		},
	}
	c, stagingDir, stateDir := newTestConnector(t, calendarIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found = true
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}

		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}

		tags, ok := meta["tags"].(map[string]interface{})
		if !ok {
			t.Fatal("expected tags object in metadata")
		}
		if tags["source_type"] != "calendar" {
			t.Errorf("expected tag source_type 'calendar', got %v", tags["source_type"])
		}
		if tags["priority"] != "normal" {
			t.Errorf("expected tag priority 'normal', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
