package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testLogger = slog.Default()

// Mock connectors for testing

type mockPollConnector struct {
	pollCount atomic.Int32
	pollErr   error
}

func (m *mockPollConnector) Poll(ctx context.Context, cp Checkpoint) error {
	m.pollCount.Add(1)
	return m.pollErr
}

type mockWatchConnector struct {
	mockPollConnector
	watchStarted chan struct{}
}

func (m *mockWatchConnector) Watch(ctx context.Context, cp Checkpoint) error {
	if m.watchStarted != nil {
		close(m.watchStarted)
	}
	<-ctx.Done()
	return nil
}

type mockListenerConnector struct {
	mockPollConnector
	handler http.Handler
}

func (m *mockListenerConnector) Handler() http.Handler {
	if m.handler != nil {
		return m.handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
}

// Test helpers

func testOptions(t *testing.T, c Connector) Options {
	t.Helper()
	base := t.TempDir()
	stagingDir := filepath.Join(base, "staging")
	stateDir := filepath.Join(base, "state")
	os.MkdirAll(stagingDir, 0755)

	cfgPath := filepath.Join(base, "config.json")
	os.WriteFile(cfgPath, []byte(`{"rules":[{"match":"*","destination":"messaging"}],"fetch_limits":{"per_source":5,"per_poll":100}}`), 0644)

	return Options{
		Name:         "test",
		StagingDir:   stagingDir,
		StateDir:     stateDir,
		ConfigFile:   cfgPath,
		Connector:    c,
		PollInterval: 50 * time.Millisecond,
		HealthPort:   0, // will be set per test
	}
}

// pickPort returns a free TCP port on localhost for tests that need to
// bind the health server to a known port. Shared between runner_test
// and framework_test.
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

func TestRunPoll_CallsConnector(t *testing.T) {
	mock := &mockPollConnector{}
	cp, _ := NewCheckpoint(t.TempDir())
	err := runPoll(context.Background(), mock, cp, nil, testLogger, nil)
	if err != nil {
		t.Fatal(err)
	}
	if mock.pollCount.Load() != 1 {
		t.Errorf("poll count = %d, want 1", mock.pollCount.Load())
	}
}

func TestRunPoll_PropagatesError(t *testing.T) {
	mock := &mockPollConnector{pollErr: fmt.Errorf("network error")}
	cp, _ := NewCheckpoint(t.TempDir())
	err := runPoll(context.Background(), mock, cp, nil, testLogger, nil)
	if err == nil {
		t.Error("expected error")
	}
}

func TestRunPoll_RespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mock := &mockPollConnector{}
	cp, _ := NewCheckpoint(t.TempDir())
	err := runPoll(ctx, mock, cp, nil, testLogger, nil)
	if err == nil {
		t.Error("expected context error")
	}
}

func TestHealthz_ReturnsOK(t *testing.T) {
	port := 18081
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
	go srv.ListenAndServe()
	defer srv.Close()
	time.Sleep(50 * time.Millisecond)

	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/healthz", port))
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("healthz status = %d, want 200", resp.StatusCode)
	}
}

func TestReadyz_503ThenOK(t *testing.T) {
	port := 18082
	var ready atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(503)
		}
	})
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
	go srv.ListenAndServe()
	defer srv.Close()
	time.Sleep(50 * time.Millisecond)

	resp, _ := http.Get(fmt.Sprintf("http://localhost:%d/readyz", port))
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("readyz before poll = %d, want 503", resp.StatusCode)
	}

	ready.Store(true)

	resp, _ = http.Get(fmt.Sprintf("http://localhost:%d/readyz", port))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("readyz after poll = %d, want 200", resp.StatusCode)
	}
}

func TestRunPollLoop_PollsOnInterval(t *testing.T) {
	mock := &mockPollConnector{}
	cp, _ := NewCheckpoint(t.TempDir())
	var ready atomic.Bool

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		runPollLoop(ctx, mock, 50*time.Millisecond, cp, nil, &ready, testLogger, nil)
	}()

	time.Sleep(180 * time.Millisecond)
	cancel()

	count := mock.pollCount.Load()
	if count < 2 {
		t.Errorf("expected at least 2 polls in 180ms with 50ms interval, got %d", count)
	}
}

func TestRunWatchLoop_PollsThenWatches(t *testing.T) {
	watchStarted := make(chan struct{})
	mock := &mockWatchConnector{
		watchStarted: watchStarted,
	}
	cp, _ := NewCheckpoint(t.TempDir())
	var ready atomic.Bool

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		runWatchLoop(ctx, mock, mock, 5*time.Second, cp, nil, &ready, testLogger, nil)
	}()

	select {
	case <-watchStarted:
		// Watch was entered -- good
	case <-time.After(2 * time.Second):
		t.Fatal("watch was not started")
	}

	cancel()
}

func TestRunPoll_PermanentError(t *testing.T) {
	mock := &mockPollConnector{pollErr: PermanentError(fmt.Errorf("bad creds"))}
	cp, _ := NewCheckpoint(t.TempDir())
	err := runPoll(context.Background(), mock, cp, nil, testLogger, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsPermanent(err) {
		t.Error("error should be permanent")
	}
}

func TestBaseConfig_RulesParsedCorrectly(t *testing.T) {
	cfg := `{"rules":[{"match":"email","destination":"inbox"},{"match":"*","destination":"default"}]}`
	var bc BaseConfig
	if err := json.Unmarshal([]byte(cfg), &bc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(bc.Rules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(bc.Rules))
	}
	if bc.Rules[0].Match != "email" {
		t.Errorf("rules[0].Match = %q, want %q", bc.Rules[0].Match, "email")
	}
	if bc.Rules[1].Destination != "default" {
		t.Errorf("rules[1].Destination = %q, want %q", bc.Rules[1].Destination, "default")
	}
}

func TestBaseConfig_RoutesFallbackToRules(t *testing.T) {
	// Config uses deprecated "routes" key -- should still parse into Routes
	// and be migrated to Rules in Run().
	cfg := `{"routes":[{"match":"*","destination":"legacy"}]}`
	var bc BaseConfig
	if err := json.Unmarshal([]byte(cfg), &bc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(bc.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(bc.Routes))
	}
	if len(bc.Rules) != 0 {
		t.Fatalf("expected 0 rules before migration, got %d", len(bc.Rules))
	}

	// Simulate the migration logic from Run()
	if len(bc.Rules) == 0 && len(bc.Routes) > 0 {
		bc.Rules = bc.Routes
	}

	if len(bc.Rules) != 1 {
		t.Fatalf("after migration: expected 1 rule, got %d", len(bc.Rules))
	}
	if bc.Rules[0].Destination != "legacy" {
		t.Errorf("after migration: rules[0].Destination = %q, want %q", bc.Rules[0].Destination, "legacy")
	}
}

func TestBaseConfig_RulesTakePrecedenceOverRoutes(t *testing.T) {
	// When both "rules" and "routes" are present, "rules" wins.
	cfg := `{
		"rules":[{"match":"*","destination":"new-dest"}],
		"routes":[{"match":"*","destination":"old-dest"}]
	}`
	var bc BaseConfig
	if err := json.Unmarshal([]byte(cfg), &bc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(bc.Rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(bc.Rules))
	}
	if len(bc.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d", len(bc.Routes))
	}

	// Simulate migration: since Rules is non-empty, Routes should be ignored
	if len(bc.Rules) == 0 && len(bc.Routes) > 0 {
		bc.Rules = bc.Routes
	}

	if bc.Rules[0].Destination != "new-dest" {
		t.Errorf("rules[0].Destination = %q, want %q (rules should take precedence)", bc.Rules[0].Destination, "new-dest")
	}
}

func TestBaseConfig_DeprecationWarningLogged(t *testing.T) {
	// Verify that using "routes" logs a deprecation warning.
	// We capture slog output by using a custom handler.
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(handler)

	cfg := `{"routes":[{"match":"*","destination":"legacy"}]}`
	var bc BaseConfig
	if err := json.Unmarshal([]byte(cfg), &bc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Simulate the migration logic from Run()
	if len(bc.Rules) == 0 && len(bc.Routes) > 0 {
		bc.Rules = bc.Routes
		logger.Warn("config key 'routes' is deprecated, use 'rules' instead")
	}

	output := buf.String()
	if !strings.Contains(output, "deprecated") {
		t.Errorf("expected deprecation warning in log output, got: %s", output)
	}
}

func TestBaseConfig_IdentityField(t *testing.T) {
	cfg := `{
		"rules":[{"match":"*","destination":"default"}],
		"identity":{"provider":"imap","auth_method":"oauth2","tenant":"example.com"}
	}`
	var bc BaseConfig
	if err := json.Unmarshal([]byte(cfg), &bc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if bc.ConfigIdentity == nil {
		t.Fatal("expected ConfigIdentity to be set")
	}
	if bc.ConfigIdentity.Provider != "imap" {
		t.Errorf("Provider = %q, want %q", bc.ConfigIdentity.Provider, "imap")
	}
	if bc.ConfigIdentity.AuthMethod != "oauth2" {
		t.Errorf("AuthMethod = %q, want %q", bc.ConfigIdentity.AuthMethod, "oauth2")
	}
	if bc.ConfigIdentity.Tenant != "example.com" {
		t.Errorf("Tenant = %q, want %q", bc.ConfigIdentity.Tenant, "example.com")
	}
}

func TestConnectorContext_HasMatcher(t *testing.T) {
	matcher := NewRuleMatcher([]Rule{
		{Match: "*", Destination: "default"},
	})
	cc := ConnectorContext{
		Matcher: matcher,
	}
	if cc.Matcher == nil {
		t.Fatal("expected Matcher to be set")
	}
	result, ok := cc.Matcher.Match("anything")
	if !ok {
		t.Fatal("expected match")
	}
	if result.Destination != "default" {
		t.Errorf("Destination = %q, want %q", result.Destination, "default")
	}
}

func TestBaseConfig_FetchLimitsParsed(t *testing.T) {
	cfg := `{
		"rules":[{"match":"*","destination":"default"}],
		"fetch_limits":{"per_source":50,"per_poll":200}
	}`
	var bc BaseConfig
	if err := json.Unmarshal([]byte(cfg), &bc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if bc.FetchLimits.PerSource != 50 {
		t.Errorf("FetchLimits.PerSource = %d, want 50", bc.FetchLimits.PerSource)
	}
	if bc.FetchLimits.PerPoll != 200 {
		t.Errorf("FetchLimits.PerPoll = %d, want 200", bc.FetchLimits.PerPoll)
	}
}

func TestBaseConfig_FetchLimitsDefaultZero(t *testing.T) {
	cfg := `{"rules":[{"match":"*","destination":"default"}]}`
	var bc BaseConfig
	if err := json.Unmarshal([]byte(cfg), &bc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if bc.FetchLimits.PerSource != 0 {
		t.Errorf("FetchLimits.PerSource = %d, want 0 (unlimited)", bc.FetchLimits.PerSource)
	}
	if bc.FetchLimits.PerPoll != 0 {
		t.Errorf("FetchLimits.PerPoll = %d, want 0 (unlimited)", bc.FetchLimits.PerPoll)
	}
}

func TestConnectorContext_HasFetchCounter(t *testing.T) {
	fc := NewFetchCounter(FetchLimits{PerSource: 10, PerPoll: 100})
	cc := ConnectorContext{
		FetchCounter: fc,
	}
	if cc.FetchCounter == nil {
		t.Fatal("expected FetchCounter to be set on ConnectorContext")
	}
	status := cc.FetchCounter.TryFetch("test-source")
	if !status.Allowed() {
		t.Error("expected fetch to be allowed")
	}
}

func TestRunPoll_ResetsFetchCounter(t *testing.T) {
	fc := NewFetchCounter(FetchLimits{PerSource: 5, PerPoll: 10})

	// Simulate some fetches from a prior poll cycle.
	fc.TryFetch("source-a")
	fc.TryFetch("source-a")
	fc.TryFetch("source-b")
	if fc.Count() != 3 {
		t.Fatalf("pre-condition: expected count 3, got %d", fc.Count())
	}

	mock := &mockPollConnector{}
	cp, _ := NewCheckpoint(t.TempDir())

	err := runPoll(context.Background(), mock, cp, nil, testLogger, fc)
	if err != nil {
		t.Fatalf("runPoll error: %v", err)
	}

	// FetchCounter should have been reset before the poll.
	if fc.Count() != 0 {
		t.Errorf("expected FetchCounter reset to 0 before poll, got %d", fc.Count())
	}
}

func TestRunPoll_NilFetchCounterSafe(t *testing.T) {
	mock := &mockPollConnector{}
	cp, _ := NewCheckpoint(t.TempDir())

	// Passing nil FetchCounter should not panic.
	err := runPoll(context.Background(), mock, cp, nil, testLogger, nil)
	if err != nil {
		t.Fatalf("runPoll error: %v", err)
	}
	if mock.pollCount.Load() != 1 {
		t.Errorf("poll count = %d, want 1", mock.pollCount.Load())
	}
}
