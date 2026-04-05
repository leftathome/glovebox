package ingest_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/config"
	"github.com/leftathome/glovebox/internal/ingest"
	"github.com/leftathome/glovebox/internal/staging"
)

// newTestHandler creates a handler with sensible defaults for integration tests.
// The caller must provide a staging dir. The allowlist permits any destination
// agent listed.
func newTestHandler(t *testing.T, stagingDir string, allowlist []string, backpressure int) *ingest.Handler {
	t.Helper()
	if backpressure <= 0 {
		backpressure = 100
	}
	cfg := config.IngestConfig{
		Enabled:               true,
		Port:                  0,
		MaxBodyBytes:          64 * 1024 * 1024,
		MaxMetadataBytes:      256 * 1024,
		BackpressureThreshold: backpressure,
		RequestTimeoutSeconds: 60,
	}
	h := ingest.NewHandler(stagingDir, cfg, allowlist)
	if err := h.InitQueueDepth(); err != nil {
		t.Fatalf("InitQueueDepth: %v", err)
	}
	h.SetReady()
	return h
}

// newTestServer wraps the handler in an httptest server whose URL includes the
// /v1/ingest path.
func newTestServer(t *testing.T, h *ingest.Handler) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", h)
	return httptest.NewServer(mux)
}

// defaultItemOpts returns ItemOptions suitable for most integration tests.
func defaultItemOpts() connector.ItemOptions {
	return connector.ItemOptions{
		Source:           "integration-test",
		Sender:           "test-sender",
		Subject:          "test subject",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "test-agent",
		ContentType:      "text/plain",
	}
}

// stagingDirs returns all non-hidden directories inside dir.
func stagingDirs(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	var dirs []os.DirEntry
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			dirs = append(dirs, e)
		}
	}
	return dirs
}

// readStagedMetadata reads and parses metadata.json from the first non-hidden
// directory in stagingDir.
func readStagedMetadata(t *testing.T, itemDir string) staging.ItemMetadata {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(itemDir, "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var meta staging.ItemMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	return meta
}

func TestIntegrationEndToEndHTTPIngest(t *testing.T) {
	stagingDir := t.TempDir()
	h := newTestHandler(t, stagingDir, []string{"test-agent"}, 0)
	ts := newTestServer(t, h)
	defer ts.Close()

	backend := connector.NewHTTPStagingBackend(
		ts.URL+"/v1/ingest",
		"test-connector",
		ts.Client(),
	)

	opts := defaultItemOpts()
	item, err := backend.NewItem(opts)
	if err != nil {
		t.Fatalf("NewItem: %v", err)
	}

	content := []byte("hello from the integration test")
	if err := item.WriteContent(content); err != nil {
		t.Fatalf("WriteContent: %v", err)
	}

	if err := item.Commit(); err != nil {
		t.Fatalf("Commit returned error: %v", err)
	}

	// Verify a directory appeared in staging with content.raw and metadata.json.
	dirs := stagingDirs(t, stagingDir)
	if len(dirs) != 1 {
		t.Fatalf("expected 1 staging dir, got %d", len(dirs))
	}

	itemDir := filepath.Join(stagingDir, dirs[0].Name())

	// Check content.raw
	gotContent, err := os.ReadFile(filepath.Join(itemDir, "content.raw"))
	if err != nil {
		t.Fatalf("read content.raw: %v", err)
	}
	if string(gotContent) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", gotContent, content)
	}

	// Check metadata.json
	meta := readStagedMetadata(t, itemDir)
	if meta.Source != "integration-test" {
		t.Errorf("source: got %q, want %q", meta.Source, "integration-test")
	}
	if meta.Sender != "test-sender" {
		t.Errorf("sender: got %q, want %q", meta.Sender, "test-sender")
	}
	if meta.DestinationAgent != "test-agent" {
		t.Errorf("destination_agent: got %q, want %q", meta.DestinationAgent, "test-agent")
	}
}

func TestIntegrationEndToEndWithIdentity(t *testing.T) {
	stagingDir := t.TempDir()
	h := newTestHandler(t, stagingDir, []string{"test-agent"}, 0)
	ts := newTestServer(t, h)
	defer ts.Close()

	backend := connector.NewHTTPStagingBackend(
		ts.URL+"/v1/ingest",
		"test-connector",
		ts.Client(),
	)

	// Set config-level identity.
	backend.SetConfigIdentity(&connector.ConfigIdentity{
		Provider:   "test-provider",
		AuthMethod: "oauth2",
		Tenant:     "config-tenant",
	})

	opts := defaultItemOpts()
	// Per-item identity overrides tenant and adds scopes.
	opts.Identity = &connector.Identity{
		AccountID:  "user-123",
		Provider:   "test-provider",
		AuthMethod: "oauth2",
		Scopes:     []string{"read", "write"},
		Tenant:     "item-tenant",
	}

	item, err := backend.NewItem(opts)
	if err != nil {
		t.Fatalf("NewItem: %v", err)
	}
	if err := item.WriteContent([]byte("identity test content")); err != nil {
		t.Fatalf("WriteContent: %v", err)
	}
	if err := item.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	dirs := stagingDirs(t, stagingDir)
	if len(dirs) != 1 {
		t.Fatalf("expected 1 staging dir, got %d", len(dirs))
	}

	meta := readStagedMetadata(t, filepath.Join(stagingDir, dirs[0].Name()))

	if meta.Identity == nil {
		t.Fatal("expected identity in metadata, got nil")
	}
	if meta.Identity.AccountID != "user-123" {
		t.Errorf("account_id: got %q, want %q", meta.Identity.AccountID, "user-123")
	}
	if meta.Identity.Provider != "test-provider" {
		t.Errorf("provider: got %q, want %q", meta.Identity.Provider, "test-provider")
	}
	if meta.Identity.AuthMethod != "oauth2" {
		t.Errorf("auth_method: got %q, want %q", meta.Identity.AuthMethod, "oauth2")
	}
	// Per-item tenant overrides config tenant.
	if meta.Identity.Tenant != "item-tenant" {
		t.Errorf("tenant: got %q, want %q", meta.Identity.Tenant, "item-tenant")
	}
	if len(meta.Identity.Scopes) != 2 || meta.Identity.Scopes[0] != "read" || meta.Identity.Scopes[1] != "write" {
		t.Errorf("scopes: got %v, want [read write]", meta.Identity.Scopes)
	}
}

func TestIntegrationEndToEndBackpressureAndRecovery(t *testing.T) {
	stagingDir := t.TempDir()
	h := newTestHandler(t, stagingDir, []string{"test-agent"}, 2)
	ts := newTestServer(t, h)
	defer ts.Close()

	backend := connector.NewHTTPStagingBackend(
		ts.URL+"/v1/ingest",
		"test-connector",
		ts.Client(),
	).WithRetry(5, 10*time.Millisecond)

	commitItem := func(label string) error {
		item, err := backend.NewItem(defaultItemOpts())
		if err != nil {
			t.Fatalf("NewItem(%s): %v", label, err)
		}
		if err := item.WriteContent([]byte("content-" + label)); err != nil {
			t.Fatalf("WriteContent(%s): %v", label, err)
		}
		return item.Commit()
	}

	// First two items succeed (queue depth goes to 2, threshold is 2).
	if err := commitItem("item-1"); err != nil {
		t.Fatalf("item-1: %v", err)
	}
	if err := commitItem("item-2"); err != nil {
		t.Fatalf("item-2: %v", err)
	}

	// Third item will hit backpressure. Decrement queue in a goroutine to
	// unblock it.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(50 * time.Millisecond)
		h.DecrementQueue()
	}()

	if err := commitItem("item-3"); err != nil {
		t.Fatalf("item-3 (after backpressure recovery): %v", err)
	}
	wg.Wait()

	dirs := stagingDirs(t, stagingDir)
	if len(dirs) != 3 {
		t.Fatalf("expected 3 staging dirs, got %d", len(dirs))
	}
}

func TestIntegrationEndToEndInvalidMetadata(t *testing.T) {
	stagingDir := t.TempDir()
	h := newTestHandler(t, stagingDir, []string{"test-agent"}, 0)
	ts := newTestServer(t, h)
	defer ts.Close()

	backend := connector.NewHTTPStagingBackend(
		ts.URL+"/v1/ingest",
		"test-connector",
		ts.Client(),
	)

	opts := defaultItemOpts()
	opts.DestinationAgent = "" // invalid -- required field

	item, err := backend.NewItem(opts)
	if err != nil {
		t.Fatalf("NewItem: %v", err)
	}
	if err := item.WriteContent([]byte("should not land")); err != nil {
		t.Fatalf("WriteContent: %v", err)
	}

	err = item.Commit()
	if err == nil {
		t.Fatal("expected Commit to return an error for empty DestinationAgent, got nil")
	}
	if !strings.Contains(err.Error(), "destination_agent") {
		t.Errorf("expected error about destination_agent, got: %v", err)
	}

	// Nothing should have been staged.
	dirs := stagingDirs(t, stagingDir)
	if len(dirs) != 0 {
		t.Fatalf("expected 0 staging dirs, got %d", len(dirs))
	}
}

func TestIntegrationEndToEndServerRestart(t *testing.T) {
	stagingDir := t.TempDir()
	h := newTestHandler(t, stagingDir, []string{"test-agent"}, 0)

	// Start first server.
	ts1 := newTestServer(t, h)

	backend := connector.NewHTTPStagingBackend(
		ts1.URL+"/v1/ingest",
		"test-connector",
		nil, // use default http.Client (not ts1.Client, since we restart)
	).WithRetry(5, 10*time.Millisecond)

	// First item succeeds.
	item1, err := backend.NewItem(defaultItemOpts())
	if err != nil {
		t.Fatalf("NewItem(1): %v", err)
	}
	if err := item1.WriteContent([]byte("before-restart")); err != nil {
		t.Fatalf("WriteContent(1): %v", err)
	}
	if err := item1.Commit(); err != nil {
		t.Fatalf("Commit(1): %v", err)
	}

	dirs := stagingDirs(t, stagingDir)
	if len(dirs) != 1 {
		t.Fatalf("expected 1 dir after first item, got %d", len(dirs))
	}

	// Stop the server.
	ts1.Close()

	// Start a new server on a different port with the same handler.
	// We need to update the backend URL, so create a new backend pointing
	// at the new server.
	ts2 := newTestServer(t, h)
	defer ts2.Close()

	backend2 := connector.NewHTTPStagingBackend(
		ts2.URL+"/v1/ingest",
		"test-connector",
		nil,
	).WithRetry(5, 10*time.Millisecond)

	// Second item succeeds against the restarted server.
	item2, err := backend2.NewItem(defaultItemOpts())
	if err != nil {
		t.Fatalf("NewItem(2): %v", err)
	}
	if err := item2.WriteContent([]byte("after-restart")); err != nil {
		t.Fatalf("WriteContent(2): %v", err)
	}
	if err := item2.Commit(); err != nil {
		t.Fatalf("Commit(2) after restart: %v", err)
	}

	dirs = stagingDirs(t, stagingDir)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs after restart, got %d", len(dirs))
	}
}
