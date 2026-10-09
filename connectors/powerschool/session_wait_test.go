package powerschool

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

func cookies(pairs ...string) []*http.Cookie {
	var out []*http.Cookie
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, &http.Cookie{Name: pairs[i], Value: pairs[i+1]})
	}
	return out
}

func TestSessionFingerprint(t *testing.T) {
	expires := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	base := &pslib.SessionExport{Cookies: cookies("JSESSIONID", "synthetic-a", "psaid", "synthetic-b"), ExpiresAt: expires}
	fp := SessionFingerprint(base)

	if len(fp) != 64 || strings.Contains(fp, "synthetic") {
		t.Fatalf("fingerprint %q is not an opaque SHA-256", fp)
	}
	reordered := &pslib.SessionExport{Cookies: cookies("psaid", "synthetic-b", "JSESSIONID", "synthetic-a"), ExpiresAt: expires.Add(time.Hour)}
	if SessionFingerprint(reordered) != fp {
		t.Error("cookie order or expiry changed the fingerprint")
	}
	for name, other := range map[string]*pslib.SessionExport{
		"changed value":  {Cookies: cookies("JSESSIONID", "synthetic-c", "psaid", "synthetic-b")},
		"dropped cookie": {Cookies: cookies("JSESSIONID", "synthetic-a")},
		"added cookie":   {Cookies: cookies("JSESSIONID", "synthetic-a", "psaid", "synthetic-b", "x", "y")},
	} {
		if SessionFingerprint(other) == fp {
			t.Errorf("%s: fingerprint unchanged", name)
		}
	}
	if SessionFingerprint(nil) != "" {
		t.Error("nil session has a fingerprint")
	}
}

// writeSession writes a session document the way the login workload does.
func writeSession(t *testing.T, path, value string) {
	t.Helper()
	doc := syntheticSession(t, time.Now().Add(time.Hour), &http.Cookie{Name: "JSESSIONID", Value: value})
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write session: %v", err)
	}
}

// TestWaitForFreshSession_RejectedSessionIsNotPresentedAgain is the
// restart-after-rejection path end to end, minus the network: a poll
// reports the session refused, the marker is written, the "restarted"
// process loads the same file and must wait -- and must come up as soon as
// the file holds a different session.
func TestWaitForFreshSession_RejectedSessionIsNotPresentedAgain(t *testing.T) {
	captureLogs(t)
	stateDir := t.TempDir()
	sessionFile := filepath.Join(t.TempDir(), "session.json")
	writeSession(t, sessionFile, "synthetic-dead")
	getenv := func(k string) string {
		if k == "POWERSCHOOL_SESSION_FILE" {
			return sessionFile
		}
		return ""
	}
	var loads atomic.Int32
	load := func() (*pslib.SessionExport, error) {
		loads.Add(1)
		return LoadSession(getenv, time.Now())
	}

	// Process 1: the session is fine as far as anyone knows.
	rejected := connector.NewRejectedSession(stateDir)
	first, err := WaitForFreshSession(context.Background(), rejected, load, time.Millisecond)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}

	// Its poll is refused upstream: the connector calls OnSessionRejected
	// (wired the way main.go wires it) and returns a PermanentError.
	client := pollFixture("111111")
	client.HomeFunc = func(ctx context.Context) (*pslib.HomePage, error) { return nil, pslib.ErrSessionExpired }
	c, cp, _ := newWiredConnector(t, client, testConfig())
	c.OnSessionRejected = func() {
		if err := rejected.Mark(SessionFingerprint(first)); err != nil {
			t.Errorf("Mark: %v", err)
		}
	}
	if err := c.Poll(context.Background(), cp); !connector.IsPermanent(err) {
		t.Fatalf("Poll = %v, want a PermanentError", err)
	}
	marker, err := os.ReadFile(rejected.Path)
	if err != nil {
		t.Fatalf("no rejected-session marker: %v", err)
	}
	if strings.Contains(string(marker), "synthetic-dead") {
		t.Fatal("marker holds the session cookie")
	}

	// Process 2: same state dir, same session file. It must block.
	restarted := connector.NewRejectedSession(stateDir)
	loads.Store(0)
	type result struct {
		s   *pslib.SessionExport
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := WaitForFreshSession(context.Background(), restarted, load, 2*time.Millisecond)
		done <- result{s, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("restart with the rejected session did not wait: (%v, %v)", r.s, r.err)
	case <-time.After(100 * time.Millisecond):
	}
	if loads.Load() < 2 {
		t.Errorf("session re-read %d times while waiting, want it polled", loads.Load())
	}
	// While it waits nothing reaches the portal: no client exists yet, and
	// the one from process 1 is not touched again.
	if client.HomeCalls != 1 || len(client.SwitchCalls) != 0 || len(client.AssignmentsCalls) != 0 {
		t.Errorf("portal contacted beyond the one refused read: %d home reads, %d switches, %d lookups",
			client.HomeCalls, len(client.SwitchCalls), len(client.AssignmentsCalls))
	}

	// The login workload writes a new session; the wait ends with it.
	writeSession(t, sessionFile, "synthetic-fresh")
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("after a fresh session: %v", r.err)
		}
		if len(r.s.Cookies) != 1 || r.s.Cookies[0].Value != "synthetic-fresh" {
			t.Errorf("came up on %+v, want the fresh session", r.s.Cookies)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting after the session file changed")
	}
}

func TestWaitForFreshSession_EdgeCases(t *testing.T) {
	captureLogs(t)
	good := &pslib.SessionExport{Cookies: cookies("JSESSIONID", "synthetic-a"), ExpiresAt: time.Now().Add(time.Hour)}

	t.Run("a first load failure is a startup error", func(t *testing.T) {
		want := errors.New("session has no cookies")
		_, err := WaitForFreshSession(context.Background(), connector.NewRejectedSession(t.TempDir()),
			func() (*pslib.SessionExport, error) { return nil, want }, time.Millisecond)
		if !errors.Is(err, want) {
			t.Errorf("err = %v, want the load error", err)
		}
	})

	t.Run("no marker: returns at once", func(t *testing.T) {
		calls := 0
		s, err := WaitForFreshSession(context.Background(), connector.NewRejectedSession(t.TempDir()),
			func() (*pslib.SessionExport, error) { calls++; return good, nil }, time.Hour)
		if err != nil || s != good || calls != 1 {
			t.Errorf("(%v, %v) after %d loads, want the session after 1", s, err, calls)
		}
	})

	t.Run("no state dir: the mechanism is off", func(t *testing.T) {
		s, err := WaitForFreshSession(context.Background(), connector.NewRejectedSession(""),
			func() (*pslib.SessionExport, error) { return good, nil }, time.Hour)
		if err != nil || s != good {
			t.Errorf("(%v, %v), want the session", s, err)
		}
	})

	t.Run("a marked session waits until the context ends, tolerating load errors", func(t *testing.T) {
		rejected := connector.NewRejectedSession(t.TempDir())
		if err := rejected.Mark(SessionFingerprint(good)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		defer cancel()
		calls := 0
		_, err := WaitForFreshSession(ctx, rejected, func() (*pslib.SessionExport, error) {
			calls++
			if calls > 1 {
				return nil, errors.New("file is mid-rewrite")
			}
			return good, nil
		}, time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want the context deadline", err)
		}
		if calls < 3 {
			t.Errorf("loaded %d times, want it to keep re-reading", calls)
		}
	})
}

func TestLibraryVersionFrom(t *testing.T) {
	other := &debug.Module{Path: "example.org/other", Version: "v9.9.9"}
	cases := []struct {
		name string
		deps []*debug.Module
		want string
	}{
		{"no deps (a test binary)", nil, "unknown"},
		{"not linked", []*debug.Module{other, nil}, "unknown"},
		{"released version", []*debug.Module{other, {Path: libraryModule, Version: "v0.2.0"}}, "v0.2.0"},
		{"replaced by a pseudo-version", []*debug.Module{{Path: libraryModule, Version: "v0.1.2",
			Replace: &debug.Module{Path: "gitlab.example/x/powerschool-go", Version: "v0.1.3-0.20260101000000-abcdefabcdef"}}},
			"v0.1.3-0.20260101000000-abcdefabcdef"},
		{"replaced by a directory", []*debug.Module{{Path: libraryModule, Version: "v0.1.2",
			Replace: &debug.Module{Path: "../powerschool-go"}}}, "v0.1.2"},
	}
	for _, tc := range cases {
		if got := libraryVersionFrom(tc.deps); got != tc.want {
			t.Errorf("%s: libraryVersionFrom = %q, want %q", tc.name, got, tc.want)
		}
	}
	if LibraryVersion() == "" {
		t.Error("LibraryVersion returned an empty string")
	}
}
