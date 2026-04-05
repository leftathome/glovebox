package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

// newTestConnector creates an XConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, userID string, feedTypes []string, apiBase string, rules []connector.Rule) (*XConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "x")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &XConnector{
		config: Config{
			UserID:    userID,
			FeedTypes: feedTypes,
		},
		writer:       writer,
		matcher:      matcher,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tokenSource:  connector.NewStaticTokenSource("test-bearer-token"),
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

// makeMentionsResponse builds a X API v2 mentions response.
func makeMentionsResponse(tweets ...struct{ ID, Text, AuthorID string }) []byte {
	type tweetData struct {
		ID        string `json:"id"`
		Text      string `json:"text"`
		AuthorID  string `json:"author_id"`
		CreatedAt string `json:"created_at"`
	}
	type response struct {
		Data []tweetData `json:"data"`
	}
	resp := response{}
	for _, tw := range tweets {
		resp.Data = append(resp.Data, tweetData{
			ID:        tw.ID,
			Text:      tw.Text,
			AuthorID:  tw.AuthorID,
			CreatedAt: "2026-03-29T12:00:00.000Z",
		})
	}
	data, _ := json.Marshal(resp)
	return data
}

func TestPollFetchesMentionsAndStages(t *testing.T) {
	tweets := []struct{ ID, Text, AuthorID string }{
		{ID: "1001", Text: "Hello @user", AuthorID: "999"},
		{ID: "1002", Text: "Hey @user check this", AuthorID: "888"},
		{ID: "1003", Text: "Ping @user", AuthorID: "777"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header.
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-bearer-token" {
			t.Errorf("expected Bearer test-bearer-token, got %q", auth)
		}
		// Verify path includes user ID.
		if !strings.Contains(r.URL.Path, "/2/users/12345/mentions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeMentionsResponse(tweets...))
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "feed:mentions", Destination: "social-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, "12345", []string{"mentions"}, srv.URL, rules)
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

func TestCheckpointUsesSinceID(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		sinceID := r.URL.Query().Get("since_id")
		if callCount == 0 {
			// First call: no since_id expected.
			if sinceID != "" {
				t.Errorf("first call should have no since_id, got %q", sinceID)
			}
			// X API returns newest first.
			tweets := []struct{ ID, Text, AuthorID string }{
				{ID: "2002", Text: "Second", AuthorID: "222"},
				{ID: "2001", Text: "First", AuthorID: "111"},
			}
			w.Write(makeMentionsResponse(tweets...))
		} else {
			// Second call: since_id should be the newest tweet ID.
			if sinceID != "2002" {
				t.Errorf("expected since_id=2002, got %q", sinceID)
			}
			tweets := []struct{ ID, Text, AuthorID string }{
				{ID: "2003", Text: "Third", AuthorID: "333"},
			}
			w.Write(makeMentionsResponse(tweets...))
		}
		callCount++
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "feed:mentions", Destination: "social-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, "12345", []string{"mentions"}, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// First poll: should stage 2 tweets.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", got)
	}

	// Second poll: should stage only 1 new tweet.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 3 {
		t.Errorf("expected 3 items total after second poll, got %d", got)
	}
}

func TestCRCChallengeResponse(t *testing.T) {
	webhookSecret := "my-webhook-secret"

	c := &XConnector{
		webhookSecret: []byte(webhookSecret),
		matcher:       connector.NewRuleMatcher(nil),
	}

	handler := c.Handler()

	crcToken := "test-crc-token-value"
	req := httptest.NewRequest(http.MethodGet, "/webhook?crc_token="+crcToken, nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}

	// Compute expected HMAC.
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(crcToken))
	expected := "sha256=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if resp["response_token"] != expected {
		t.Errorf("expected response_token %q, got %q", expected, resp["response_token"])
	}
}

func TestWebhookValidSignatureStaged(t *testing.T) {
	webhookSecret := "webhook-secret-x"

	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "x")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	rules := []connector.Rule{
		{Match: "event:tweet_create_events", Destination: "social-agent"},
	}
	matcher := connector.NewRuleMatcher(rules)

	c := &XConnector{
		config: Config{
			UserID:    "12345",
			FeedTypes: []string{"mentions"},
		},
		writer:        writer,
		matcher:       matcher,
		fetchCounter:  connector.NewFetchCounter(connector.FetchLimits{}),
		webhookSecret: []byte(webhookSecret),
	}

	handler := c.Handler()

	payload := []byte(`{"tweet_create_events":[{"id_str":"9001","text":"hello"}]}`)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write(payload)
	sig := "sha256=" + base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(payload)))
	req.Header.Set("X-Twitter-Webhooks-Signature", sig)
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

func TestWebhookInvalidSignatureRejected(t *testing.T) {
	webhookSecret := "webhook-secret-x"

	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "x")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	rules := []connector.Rule{
		{Match: "event:tweet_create_events", Destination: "social-agent"},
	}
	matcher := connector.NewRuleMatcher(rules)

	c := &XConnector{
		config: Config{
			UserID: "12345",
		},
		writer:        writer,
		matcher:       matcher,
		fetchCounter:  connector.NewFetchCounter(connector.FetchLimits{}),
		webhookSecret: []byte(webhookSecret),
	}

	handler := c.Handler()

	payload := []byte(`{"tweet_create_events":[{"id_str":"9001","text":"hello"}]}`)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(payload)))
	req.Header.Set("X-Twitter-Webhooks-Signature", "sha256=aW52YWxpZHNpZ25hdHVyZQ==")
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

func TestIdentityInMetadata(t *testing.T) {
	tweets := []struct{ ID, Text, AuthorID string }{
		{ID: "3001", Text: "Test tweet", AuthorID: "555"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeMentionsResponse(tweets...))
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "feed:mentions", Destination: "social-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, "12345", []string{"mentions"}, srv.URL, rules)
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
		if identity["provider"] != "x" {
			t.Errorf("expected identity provider 'x', got %v", identity["provider"])
		}
		if identity["auth_method"] != "oauth" {
			t.Errorf("expected identity auth_method 'oauth', got %v", identity["auth_method"])
		}
		if identity["account_id"] != "12345" {
			t.Errorf("expected identity account_id '12345', got %v", identity["account_id"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
