package schoology

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	schoologylib "github.com/leftathome/schoology-go"
)

// newTestConnector builds a SchoologyConnector with a minimal valid
// config and the supplied fakeClient. Wire is NOT called -- tests that
// need a wired connector call newWiredConnector instead.
func newTestConnector(t *testing.T, client SchoologyClient) *SchoologyConnector {
	t.Helper()
	cfg := Config{
		Kids: []Kid{{Name: "k1", SchoologyUID: 1001}},
		PollSchedule: PollSchedule{
			Windows: []PollWindow{{Start: "08:00", End: "09:00"}},
		},
		Trigger: TriggerConfig{DebounceSeconds: 60, ListenPort: 8081},
		Attachments: AttachmentsConfig{
			MaxSizeMB: 25,
		},
		ParseFailureThreshold: 3,
	}
	c, err := NewConnector(client, cfg, "v0.1.0")
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	return c
}

// newWiredConnector returns a SchoologyConnector with framework
// resources wired in: real Metrics + Telemetry, a real StagingWriter and
// RuleMatcher rooted at t.TempDir().
//
// On test completion the framework Metrics registry is shut down via
// t.Cleanup so successive tests don't fail with prometheus collector
// re-registration errors.
func newWiredConnector(t *testing.T, client SchoologyClient) *SchoologyConnector {
	t.Helper()
	c := newTestConnector(t, client)

	writer, _ := newTestWriter(t)
	matcher := connector.NewRuleMatcher([]connector.Rule{
		{Match: "schoology:k1:assignment", Destination: "school"},
		{Match: "schoology:k1:feed", Destination: "school"},
		{Match: "schoology:message", Destination: "school"},
		{Match: "schoology-parse-failure:*", Destination: "school"},
	})
	metrics, err := connector.NewMetrics("schoology-connector-test-" + t.Name())
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = metrics.Shutdown() })

	if err := c.Wire(connector.ConnectorContext{
		Writer:  writer,
		Matcher: matcher,
		Metrics: metrics,
	}); err != nil {
		t.Fatalf("Wire: %v", err)
	}
	return c
}

func TestConnector_NewConnector_LoadsTimezone(t *testing.T) {
	c := newTestConnector(t, &fakeClient{})
	if c.tz == nil {
		t.Fatal("tz is nil")
	}
	if c.pollSignal == nil {
		t.Fatal("pollSignal is nil")
	}
	if c.trigger == nil {
		t.Fatal("trigger is nil")
	}
	if c.driftCounter == nil {
		t.Fatal("driftCounter is nil")
	}
}

func TestConnector_NewConnector_BadTimezone(t *testing.T) {
	t.Setenv("SCHOOLOGY_TIMEZONE", "Not/A/Zone")
	_, err := NewConnector(&fakeClient{}, Config{
		Kids: []Kid{{Name: "k1", SchoologyUID: 1}},
		PollSchedule: PollSchedule{
			Windows: []PollWindow{{Start: "08:00", End: "09:00"}},
		},
	}, "v0.1.0")
	if err == nil {
		t.Fatal("NewConnector with bad timezone returned nil error")
	}
}

func TestConnector_Wire_RejectsNilDeps(t *testing.T) {
	cases := []struct {
		name string
		cc   connector.ConnectorContext
		want string
	}{
		{
			name: "nil writer",
			cc:   connector.ConnectorContext{Writer: nil, Matcher: &connector.RuleMatcher{}, Metrics: mustMetrics(t)},
			want: "Writer",
		},
		{
			name: "nil matcher",
			cc:   connector.ConnectorContext{Writer: &connector.StagingWriter{}, Matcher: nil, Metrics: mustMetrics(t)},
			want: "Matcher",
		},
		{
			name: "nil metrics",
			cc:   connector.ConnectorContext{Writer: &connector.StagingWriter{}, Matcher: &connector.RuleMatcher{}, Metrics: nil},
			want: "Metrics",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestConnector(t, &fakeClient{})
			err := c.Wire(tc.cc)
			if err == nil {
				t.Fatalf("Wire returned nil error, want one mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Wire error = %q, want to mention %q", err.Error(), tc.want)
			}
		})
	}
}

// mustMetrics builds a real *connector.Metrics for tests that need a
// non-nil placeholder. The registry is shut down via t.Cleanup.
func mustMetrics(t *testing.T) *connector.Metrics {
	t.Helper()
	m, err := connector.NewMetrics("schoology-test-" + t.Name())
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown() })
	return m
}

func TestConnector_Wire_Success(t *testing.T) {
	c := newWiredConnector(t, &fakeClient{})
	c.wireMu.Lock()
	defer c.wireMu.Unlock()
	if c.writer == nil {
		t.Error("writer not wired")
	}
	if c.matcher == nil {
		t.Error("matcher not wired")
	}
	if c.tel == nil {
		t.Error("tel not wired")
	}
	if c.trigger.Tel == nil {
		t.Error("trigger.Tel not wired -- handler will miss schoology_trigger_requests_total")
	}
}

func TestConnector_Poll_NotWired(t *testing.T) {
	c := newTestConnector(t, &fakeClient{})
	cp := newTestCheckpoint(t)
	err := c.Poll(context.Background(), cp)
	if err == nil {
		t.Fatal("Poll before Wire returned nil error")
	}
	if !strings.Contains(err.Error(), "not wired") {
		t.Errorf("Poll error = %q, want to mention 'not wired'", err.Error())
	}
}

func TestConnector_Poll_CallsPollNow(t *testing.T) {
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
	}
	c := newWiredConnector(t, client)
	cp := newTestCheckpoint(t)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// A clean empty-no-errors poll must NOT bump the drift counter.
	if got := c.driftCounter.Count(); got != 0 {
		t.Errorf("driftCounter.Count() = %d, want 0 after a clean poll", got)
	}
}

func TestConnector_PollNow_ConvergesAllSources(t *testing.T) {
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
	}
	c := newWiredConnector(t, client)
	cp := newTestCheckpoint(t)

	for _, source := range []string{"catch_up", "scheduled", "triggered"} {
		t.Run(source, func(t *testing.T) {
			if err := c.pollNow(context.Background(), cp, source); err != nil {
				t.Fatalf("pollNow(%q): %v", source, err)
			}
		})
	}
}

func TestConnector_DriftCounter_Escalates(t *testing.T) {
	c := newTestConnector(t, &fakeClient{})

	// Threshold is 3 (set in newTestConnector). Force three
	// empty-with-errors polls and confirm checkDrift escalates.
	for i := 0; i < 3; i++ {
		c.driftCounter.RecordPoll(0, true)
	}
	err := c.checkDrift()
	if err == nil {
		t.Fatal("checkDrift after threshold polls returned nil")
	}
	if !connector.IsPermanent(err) {
		t.Errorf("checkDrift error is not permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "exceeded threshold") {
		t.Errorf("checkDrift error = %q, want to mention 'exceeded threshold'", err.Error())
	}
}

func TestConnector_DriftCounter_NoEscalateBelowThreshold(t *testing.T) {
	c := newTestConnector(t, &fakeClient{})
	c.driftCounter.RecordPoll(0, true)
	c.driftCounter.RecordPoll(0, true)
	if err := c.checkDrift(); err != nil {
		t.Errorf("checkDrift below threshold returned %v, want nil", err)
	}
}

func TestConnector_DetectBadCredentials(t *testing.T) {
	// fakeClient returns ErrSessionExpired (an IsAuthError sentinel)
	// from GetOverdueSubmissions; the assignments processor classifies
	// it as "auth" and increments dedup; pollNow's bad-creds check at
	// the end of the poll converts that into a PermanentError.
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Assignment, schoologylib.ParseErrors, error) {
			return nil, nil, schoologylib.ErrSessionExpired
		},
		FeedFunc: func(ctx context.Context, childUID int64) ([]*schoologylib.Post, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
		InboxFunc: func(ctx context.Context) ([]*schoologylib.MessageThread, schoologylib.ParseErrors, error) {
			return nil, nil, nil
		},
	}
	c := newWiredConnector(t, client)
	cp := newTestCheckpoint(t)

	err := c.pollNow(context.Background(), cp, "scheduled")
	if err == nil {
		t.Fatal("pollNow with bad credentials returned nil error")
	}
	if !connector.IsPermanent(err) {
		t.Errorf("pollNow bad-credentials error is not permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "session expired") {
		t.Errorf("pollNow error = %q, want to mention 'session expired'", err.Error())
	}
}

func TestConnector_Handler_WiresTrigger(t *testing.T) {
	c := newTestConnector(t, &fakeClient{})
	if c.Handler() == nil {
		t.Fatal("Handler() returned nil")
	}
}

func TestConnector_NextPoll_PopulatesWindow(t *testing.T) {
	c := newTestConnector(t, &fakeClient{})
	// Force a known time so the test isn't wall-clock sensitive. Use
	// the connector's configured tz so we land cleanly inside the
	// 08:00-09:00 window of the same day.
	now := time.Date(2026, 5, 21, 6, 0, 0, 0, c.tz)
	next, splay, window := c.nextPoll(now)
	if window != "08:00-09:00" {
		t.Errorf("window = %q, want %q", window, "08:00-09:00")
	}
	if splay < 0 || splay >= 3600 {
		t.Errorf("splay = %d, want 0..3599", splay)
	}
	if !next.After(now) {
		t.Errorf("next %v is not after now %v", next, now)
	}
}
