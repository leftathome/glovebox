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

// newTestConnector creates a MetaConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, pageID string, apiBase string, rules []connector.Rule) (*MetaConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "meta")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &MetaConnector{
		config: Config{
			PageID:     pageID,
			FetchPosts: true,
		},
		writer:      writer,
		matcher:     matcher,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		tokenSource: connector.NewStaticTokenSource("test-token"),
		apiBase:     apiBase,
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

// makeFeedResponse builds a JSON Graph API feed response with posts having the given IDs.
func makeFeedResponse(ids ...string) []byte {
	type post struct {
		ID          string `json:"id"`
		Message     string `json:"message"`
		CreatedTime string `json:"created_time"`
	}
	posts := make([]post, 0, len(ids))
	for _, id := range ids {
		posts = append(posts, post{
			ID:          id,
			Message:     "Test post " + id,
			CreatedTime: "2026-03-29T12:00:00+0000",
		})
	}
	resp := struct {
		Data []post `json:"data"`
	}{Data: posts}
	data, _ := json.Marshal(resp)
	return data
}

func TestPollFetchesPostsAndStages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify Bearer token is in Authorization header.
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-token" {
			t.Errorf("expected Authorization: Bearer test-token, got %q", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeFeedResponse("page_100", "page_101", "page_102"))
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "platform:facebook", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, "123456", srv.URL, rules)
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
			// First call: return posts page_100, page_101 (newest first)
			w.Write(makeFeedResponse("page_101", "page_100"))
		} else {
			// Second call: same posts plus one new one
			w.Write(makeFeedResponse("page_102", "page_101", "page_100"))
		}
		callCount++
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "platform:facebook", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, "123456", srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// First poll: should stage 2 posts.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", got)
	}

	// Second poll: should stage only 1 new post.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 3 {
		t.Errorf("expected 3 items total after second poll, got %d", got)
	}
}

func TestWebhookVerificationChallenge(t *testing.T) {
	rules := []connector.Rule{
		{Match: "event:feed", Destination: "test-agent"},
	}

	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "meta")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	matcher := connector.NewRuleMatcher(rules)

	c := &MetaConnector{
		config:      Config{PageID: "123456"},
		writer:      writer,
		matcher:     matcher,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		tokenSource: connector.NewStaticTokenSource("test-token"),
		apiBase:     "http://unused",
		verifyToken: "my-verify-token",
	}

	handler := c.Handler()

	req := httptest.NewRequest(http.MethodGet,
		"/webhook?hub.mode=subscribe&hub.verify_token=my-verify-token&hub.challenge=challenge_abc123",
		nil)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != "challenge_abc123" {
		t.Errorf("expected challenge 'challenge_abc123', got %q", rr.Body.String())
	}
}

func TestWebhookValidSignatureStaged(t *testing.T) {
	secret := "app-secret-456"

	rules := []connector.Rule{
		{Match: "event:feed", Destination: "test-agent"},
	}

	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "meta")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	matcher := connector.NewRuleMatcher(rules)

	c := &MetaConnector{
		config:      Config{PageID: "123456"},
		writer:      writer,
		matcher:     matcher,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		tokenSource: connector.NewStaticTokenSource("test-token"),
		apiBase:     "http://unused",
		appSecret:   []byte(secret),
	}

	handler := c.Handler()

	payload := []byte(`{"object":"feed","entry":[{"id":"123","changes":[]}]}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(payload)))
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

func TestWebhookInvalidSignatureRejected(t *testing.T) {
	secret := "app-secret-456"

	rules := []connector.Rule{
		{Match: "event:feed", Destination: "test-agent"},
	}

	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "meta")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	matcher := connector.NewRuleMatcher(rules)

	c := &MetaConnector{
		config:    Config{PageID: "123456"},
		writer:    writer,
		matcher:   matcher,
		appSecret: []byte(secret),
	}

	handler := c.Handler()

	payload := []byte(`{"object":"feed","entry":[]}`)

	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(payload)))
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

func TestIdentityInMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(makeFeedResponse("page_200"))
	}))
	defer srv.Close()

	rules := []connector.Rule{
		{Match: "platform:facebook", Destination: "test-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, "123456", srv.URL, rules)
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
		if identity["provider"] != "meta" {
			t.Errorf("expected identity provider 'meta', got %v", identity["provider"])
		}
		if identity["auth_method"] != "oauth" {
			t.Errorf("expected identity auth_method 'oauth', got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
