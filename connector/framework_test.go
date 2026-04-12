package connector

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newFrameworkTestOpts builds an Options suitable for NewFramework tests:
// a temp StateDir, a temp StagingDir (filesystem-backend mode), and a
// minimal on-disk config with a single wildcard rule.
func newFrameworkTestOpts(t *testing.T, name string, port int, c Connector) Options {
	t.Helper()
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	stateDir := filepath.Join(base, "state")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(base, "config.json")
	cfg := `{"rules":[{"match":"*","destination":"messaging"}],"fetch_limits":{"per_source":5,"per_poll":100}}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// Force filesystem-backend selection even if the outer env has an
	// ingest URL pointed somewhere during local dev.
	t.Setenv("GLOVEBOX_INGEST_URL", "")

	return Options{
		Name:       name,
		StagingDir: stagingDir,
		StateDir:   stateDir,
		ConfigFile: cfgPath,
		Connector:  c,
		HealthPort: port,
	}
}

// pickPort returns a free TCP port on localhost for the tests that need
// to bind the health server.
func pickPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// waitForPort blocks until the given port accepts TCP connections or
// the deadline passes.
func waitForPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("port %d did not become ready within %v", port, timeout)
}

func TestNewFramework_Bootstrap(t *testing.T) {
	port := pickPort(t)
	mock := &mockPollConnector{}
	opts := newFrameworkTestOpts(t, "fw-boot", port, mock)

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	defer fw.Shutdown()

	if fw.Name != "fw-boot" {
		t.Errorf("Name = %q, want %q", fw.Name, "fw-boot")
	}
	if fw.Matcher == nil {
		t.Error("Matcher is nil")
	}
	if fw.Backend == nil {
		t.Error("Backend is nil")
	}
	if fw.Writer == nil {
		t.Error("Writer is nil (expected filesystem mode)")
	}
	if fw.Metrics == nil {
		t.Error("Metrics is nil")
	}
	if fw.FetchCounter == nil {
		t.Error("FetchCounter is nil")
	}
	if fw.Checkpoint == nil {
		t.Error("Checkpoint is nil")
	}
	if fw.Ready == nil {
		t.Error("Ready is nil")
	}
	if fw.HealthPort != port {
		t.Errorf("HealthPort = %d, want %d", fw.HealthPort, port)
	}
	if len(fw.BaseConfig.Rules) != 1 {
		t.Errorf("BaseConfig.Rules len = %d, want 1", len(fw.BaseConfig.Rules))
	}
	if fw.BaseConfig.FetchLimits.PerSource != 5 {
		t.Errorf("FetchLimits.PerSource = %d, want 5", fw.BaseConfig.FetchLimits.PerSource)
	}
}

func TestNewFramework_HealthServerResponds(t *testing.T) {
	port := pickPort(t)
	mock := &mockPollConnector{}
	opts := newFrameworkTestOpts(t, "fw-health", port, mock)

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	defer fw.Shutdown()

	// Give the health server a moment to bind.
	waitForPort(t, port, 2*time.Second)

	// /healthz should always be 200.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}

	// /readyz starts 503 until Ready flips.
	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("/readyz before ready = %d, want 503", resp.StatusCode)
	}

	fw.Ready.Store(true)

	resp, err = http.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
	if err != nil {
		t.Fatalf("GET /readyz (ready): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/readyz after ready = %d, want 200", resp.StatusCode)
	}
}

func TestNewFramework_SetupCallbackInvoked(t *testing.T) {
	port := pickPort(t)
	mock := &mockPollConnector{}
	opts := newFrameworkTestOpts(t, "fw-setup", port, mock)

	var receivedCtx ConnectorContext
	called := false
	opts.Setup = func(cc ConnectorContext) error {
		called = true
		receivedCtx = cc
		return nil
	}

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	defer fw.Shutdown()

	if !called {
		t.Fatal("Setup callback was not invoked")
	}
	if receivedCtx.Backend == nil {
		t.Error("Setup ConnectorContext.Backend is nil")
	}
	if receivedCtx.Matcher == nil {
		t.Error("Setup ConnectorContext.Matcher is nil")
	}
	if receivedCtx.Metrics == nil {
		t.Error("Setup ConnectorContext.Metrics is nil")
	}
	if receivedCtx.FetchCounter == nil {
		t.Error("Setup ConnectorContext.FetchCounter is nil")
	}
}

func TestNewFramework_SetupErrorPropagates(t *testing.T) {
	port := pickPort(t)
	mock := &mockPollConnector{}
	opts := newFrameworkTestOpts(t, "fw-setup-err", port, mock)
	opts.Setup = func(cc ConnectorContext) error {
		return fmt.Errorf("bad setup")
	}

	fw, err := NewFramework(opts)
	if err == nil {
		t.Fatal("expected NewFramework to return an error when Setup fails")
	}
	if fw != nil {
		t.Error("expected nil Framework on Setup failure")
	}
}

func TestNewFramework_BadConfigFile(t *testing.T) {
	port := pickPort(t)
	opts := Options{
		Name:       "fw-badcfg",
		StagingDir: t.TempDir(),
		StateDir:   t.TempDir(),
		ConfigFile: "/nonexistent/path/config.json",
		Connector:  &mockPollConnector{},
		HealthPort: port,
	}
	fw, err := NewFramework(opts)
	if err == nil {
		t.Fatal("expected error for missing config file")
	}
	if fw != nil {
		t.Error("expected nil Framework on config load failure")
	}
}

func TestFramework_ShutdownIdempotent(t *testing.T) {
	port := pickPort(t)
	mock := &mockPollConnector{}
	opts := newFrameworkTestOpts(t, "fw-shutdown", port, mock)

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}

	if err := fw.Shutdown(); err != nil {
		t.Errorf("first Shutdown: %v", err)
	}
	// Second call must be a no-op, not a panic.
	if err := fw.Shutdown(); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
	// Third for good measure.
	if err := fw.Shutdown(); err != nil {
		t.Errorf("third Shutdown: %v", err)
	}
}

func TestFramework_ShutdownStopsHealthServer(t *testing.T) {
	port := pickPort(t)
	mock := &mockPollConnector{}
	opts := newFrameworkTestOpts(t, "fw-shutdown-srv", port, mock)

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	waitForPort(t, port, 2*time.Second)

	// Verify the server is reachable.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		t.Fatalf("GET /healthz before shutdown: %v", err)
	}
	resp.Body.Close()

	if err := fw.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// After shutdown, the port should no longer accept.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err != nil {
			return // good: connection refused or similar
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("health server still reachable on port %d after Shutdown", port)
}

func TestNewFramework_ListenerServerStarts(t *testing.T) {
	port := pickPort(t)
	// Use a listener connector whose handler returns 204 so we can tell
	// it apart from the health server's 200.
	handlerHit := make(chan struct{}, 1)
	mock := &mockListenerConnector{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case handlerHit <- struct{}{}:
			default:
			}
			w.WriteHeader(204)
		}),
	}
	opts := newFrameworkTestOpts(t, "fw-listener", port, mock)

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	defer fw.Shutdown()

	listenerPort := port + 1
	waitForPort(t, listenerPort, 2*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/anything", listenerPort))
	if err != nil {
		t.Fatalf("GET listener: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("listener status = %d, want 204", resp.StatusCode)
	}
	select {
	case <-handlerHit:
	case <-time.After(time.Second):
		t.Error("listener handler was not called")
	}
}
