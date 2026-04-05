package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// newTestConnector creates a GitHubConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, repos []RepoConfig, apiBase string, rules []connector.Rule) (*GitHubConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "github")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &GitHubConnector{
		config: Config{
			Repos: repos,
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
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			count++
		}
	}
	return count
}

// makeEvents builds a JSON array of GitHub event objects.
func makeEvents(ids ...string) []byte {
	type event struct {
		ID   string          `json:"id"`
		Type string          `json:"type"`
		Repo json.RawMessage `json:"repo"`
	}
	events := make([]event, 0, len(ids))
	for _, id := range ids {
		events = append(events, event{
			ID:   id,
			Type: "PushEvent",
			Repo: json.RawMessage(`{"id":1,"name":"owner/repo"}`),
		})
	}
	data, _ := json.Marshal(events)
	return data
}

func TestPollFetchesEventsAndStages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header is present.
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeEvents("100", "101", "102"))
	}))
	defer srv.Close()

	repos := []RepoConfig{{Owner: "owner", Repo: "repo"}}
	rules := []connector.Rule{
		{Match: "repo:owner/repo", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, repos, srv.URL, rules)
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
			// First call: return events 100, 101
			w.Write(makeEvents("101", "100"))
		} else {
			// Second call: same events plus one new one
			w.Write(makeEvents("102", "101", "100"))
		}
		callCount++
	}))
	defer srv.Close()

	repos := []RepoConfig{{Owner: "owner", Repo: "repo"}}
	rules := []connector.Rule{
		{Match: "repo:owner/repo", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, repos, srv.URL, rules)
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

func TestWebhookValidSignature(t *testing.T) {
	secret := "webhook-secret-123"

	repos := []RepoConfig{{Owner: "owner", Repo: "repo"}}
	rules := []connector.Rule{
		{Match: "event:push", Destination: "test-agent"},
	}

	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "github")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	matcher := connector.NewRuleMatcher(rules)

	c := &GitHubConnector{
		config: Config{
			Repos:         repos,
			WebhookSecret: "GITHUB_WEBHOOK_SECRET",
		},
		writer:       writer,
		matcher:      matcher,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		webhookSecret: []byte(secret),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tokenSource:  connector.NewStaticTokenSource("test-token"),
		apiBase:      "http://unused",
	}

	handler := c.Handler()

	payload := []byte(`{"action":"completed","ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(payload)))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	count := countStagedItems(t, stagingDir)
	if count != 1 {
		t.Errorf("expected 1 staged webhook item, got %d", count)
	}
}

func TestWebhookInvalidSignature(t *testing.T) {
	secret := "webhook-secret-123"

	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "github")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	rules := []connector.Rule{
		{Match: "event:push", Destination: "test-agent"},
	}
	matcher := connector.NewRuleMatcher(rules)

	c := &GitHubConnector{
		config: Config{
			WebhookSecret: "GITHUB_WEBHOOK_SECRET",
		},
		writer:       writer,
		matcher:      matcher,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		webhookSecret: []byte(secret),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tokenSource:  connector.NewStaticTokenSource("test-token"),
		apiBase:      "http://unused",
	}

	handler := c.Handler()

	payload := []byte(`{"action":"completed"}`)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(payload)))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=bad_signature_value_0000000000000000000000000000000000000000")
	req.Header.Set("Content-Type", "application/json")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid signature, got %d", rr.Code)
	}

	count := countStagedItems(t, stagingDir)
	if count != 0 {
		t.Errorf("expected 0 staged items for invalid signature, got %d", count)
	}
}

func TestIdentityFieldsInMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeEvents("200"))
	}))
	defer srv.Close()

	repos := []RepoConfig{{Owner: "owner", Repo: "repo"}}
	rules := []connector.Rule{
		{Match: "repo:owner/repo", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, repos, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
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
		if identity["provider"] != "github" {
			t.Errorf("expected identity provider 'github', got %v", identity["provider"])
		}
		if identity["auth_method"] != "pat" {
			t.Errorf("expected identity auth_method 'pat', got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestRuleTagsInMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeEvents("300"))
	}))
	defer srv.Close()

	repos := []RepoConfig{{Owner: "owner", Repo: "repo"}}
	rules := []connector.Rule{
		{
			Match:       "repo:owner/repo",
			Destination: "test-agent",
			Tags:        map[string]string{"source_type": "vcs", "priority": "normal"},
		},
	}
	c, stagingDir, stateDir := newTestConnector(t, repos, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
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
		if tags["source_type"] != "vcs" {
			t.Errorf("expected tag source_type 'vcs', got %v", tags["source_type"])
		}
		if tags["priority"] != "normal" {
			t.Errorf("expected tag priority 'normal', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
