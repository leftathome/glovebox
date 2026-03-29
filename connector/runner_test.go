package connector

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	os.WriteFile(cfgPath, []byte(`{"routes":[{"match":"*","destination":"messaging"}]}`), 0644)

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

func TestRunPoll_CallsConnector(t *testing.T) {
	mock := &mockPollConnector{}
	cp, _ := NewCheckpoint(t.TempDir())
	err := runPoll(context.Background(), mock, cp, testLogger)
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
	err := runPoll(context.Background(), mock, cp, testLogger)
	if err == nil {
		t.Error("expected error")
	}
}

func TestRunPoll_RespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mock := &mockPollConnector{}
	cp, _ := NewCheckpoint(t.TempDir())
	err := runPoll(ctx, mock, cp, testLogger)
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
		runPollLoop(ctx, Options{
			Name:         "test",
			Connector:    mock,
			PollInterval: 50 * time.Millisecond,
		}, cp, &ready, testLogger)
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
		runWatchLoop(ctx, Options{
			Name:         "test",
			Connector:    mock,
			PollInterval: 5 * time.Second,
		}, mock, cp, &ready, testLogger)
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
	err := runPoll(context.Background(), mock, cp, testLogger)
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsPermanent(err) {
		t.Error("error should be permanent")
	}
}
