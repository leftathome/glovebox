package connector

import (
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// frameworkTestOpts adapts the shared testOptions helper for NewFramework
// tests: it forces filesystem-backend selection, overrides the connector
// name so per-test metrics registrations don't collide, clears
// PollInterval (framework tests don't run the poll loop), and binds the
// health server to the given port.
func frameworkTestOpts(t *testing.T, name string, port int, c Connector) Options {
	t.Helper()
	t.Setenv("GLOVEBOX_INGEST_URL", "")
	opts := testOptions(t, c)
	opts.Name = name
	opts.PollInterval = 0
	opts.HealthPort = port
	return opts
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
	opts := frameworkTestOpts(t, "fw-boot", port, mock)

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
	opts := frameworkTestOpts(t, "fw-health", port, mock)

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	defer fw.Shutdown()

	waitForPort(t, port, 2*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}

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
	opts := frameworkTestOpts(t, "fw-setup", port, mock)

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
	opts := frameworkTestOpts(t, "fw-setup-err", port, mock)
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
	opts := frameworkTestOpts(t, "fw-shutdown", port, mock)

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}

	if err := fw.Shutdown(); err != nil {
		t.Errorf("first Shutdown: %v", err)
	}
	if err := fw.Shutdown(); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
	if err := fw.Shutdown(); err != nil {
		t.Errorf("third Shutdown: %v", err)
	}
}

func TestFramework_ShutdownStopsHealthServer(t *testing.T) {
	port := pickPort(t)
	mock := &mockPollConnector{}
	opts := frameworkTestOpts(t, "fw-shutdown-srv", port, mock)

	fw, err := NewFramework(opts)
	if err != nil {
		t.Fatalf("NewFramework: %v", err)
	}
	waitForPort(t, port, 2*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		t.Fatalf("GET /healthz before shutdown: %v", err)
	}
	resp.Body.Close()

	if err := fw.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
		if err != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("health server still reachable on port %d after Shutdown", port)
}

func TestNewFramework_ListenerServerStarts(t *testing.T) {
	port := pickPort(t)
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
	opts := frameworkTestOpts(t, "fw-listener", port, mock)

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
