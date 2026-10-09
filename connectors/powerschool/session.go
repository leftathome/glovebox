package powerschool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// libraryModule is the module path of powerschool-go.
const libraryModule = "github.com/leftathome/powerschool-go"

// LibraryVersion returns the version of powerschool-go linked into this
// binary, from its build info (the replacement's version when go.mod
// replaces the module), or "unknown". It is logged with every escalation.
func LibraryVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return libraryVersionFrom(bi.Deps)
}

// libraryVersionFrom picks powerschool-go's version out of a dependency
// list.
func libraryVersionFrom(deps []*debug.Module) string {
	for _, dep := range deps {
		if dep == nil || dep.Path != libraryModule {
			continue
		}
		if dep.Replace != nil && dep.Replace.Version != "" {
			return dep.Replace.Version
		}
		if dep.Version != "" {
			return dep.Version
		}
	}
	return "unknown"
}

// LoadSession reads the saved PowerSchool session the connector runs on.
//
// The connector never holds the account's username or password and never
// logs in: login needs a browser and a POST, neither of which the read
// path is allowed (see Transport). A separate login workload produces the
// session with powerschool-go's Client.ExportSession and stores its JSON
// form ({"cookies":[...],"expires_at":"..."}); this function reads that
// JSON back.
//
// Two delivery forms are accepted, file first:
//
//	POWERSCHOOL_SESSION_FILE  path to the JSON document
//	POWERSCHOOL_SESSION       the JSON document itself (envFrom a Secret)
//
// getenv is injected so tests need not mutate the process environment.
func LoadSession(getenv func(string) string, now time.Time) (*pslib.SessionExport, error) {
	var raw []byte
	switch {
	case getenv("POWERSCHOOL_SESSION_FILE") != "":
		data, err := os.ReadFile(getenv("POWERSCHOOL_SESSION_FILE"))
		if err != nil {
			return nil, fmt.Errorf("read session file: %w", err)
		}
		raw = data
	case getenv("POWERSCHOOL_SESSION") != "":
		raw = []byte(getenv("POWERSCHOOL_SESSION"))
	default:
		return nil, fmt.Errorf("neither POWERSCHOOL_SESSION_FILE nor POWERSCHOOL_SESSION is set")
	}
	return ParseSession(raw, now)
}

// SessionFingerprint returns a stable, non-reversible identifier for a
// session: a SHA-256 over its cookies in name order. Two documents with the
// same cookies have the same fingerprint whatever their order or expiry;
// any changed, added or removed cookie changes it.
func SessionFingerprint(s *pslib.SessionExport) string {
	if s == nil {
		return ""
	}
	pairs := make([]string, 0, len(s.Cookies))
	for _, c := range s.Cookies {
		if c != nil {
			pairs = append(pairs, c.Name+"="+c.Value)
		}
	}
	sort.Strings(pairs)
	return connector.SessionFingerprint(strings.Join(pairs, "\x1f"))
}

// WaitForFreshSession returns the session to run on, having first waited
// out a session the portal has already refused.
//
// A refused session is routine, not exceptional: the account is shared with
// its human owner and the portal appears to keep one live session per
// account, so the owner signing in ends the connector's session. The poll
// that discovers it returns a permanent error, the process exits, and the
// kubelet starts it again with the same session file. If the loaded session
// is the one recorded in rejected, this function blocks -- re-reading the
// session every interval and contacting nothing upstream -- until load
// returns a different one, or ctx ends. With the session mounted as a file,
// an in-place update of the Secret ends the wait without a pod restart.
//
// onWait, if not nil, is called once, the first time the function is about
// to wait. The binary uses it to bring up WaitHealth: connector.Run has not
// started, so without it nothing answers the kubelet's probes and the pod
// is restarted instead of sitting Running and NotReady.
//
// The first load must succeed (a missing or malformed session is a startup
// error). Later failures are tolerated: the file may be mid-rewrite.
func WaitForFreshSession(ctx context.Context, rejected connector.RejectedSession,
	load func() (*pslib.SessionExport, error), interval time.Duration, onWait func()) (*pslib.SessionExport, error) {
	session, err := load()
	if err != nil {
		return nil, err
	}
	waiting := false
	for rejected.IsRejected(SessionFingerprint(session)) {
		if !waiting {
			waiting = true
			slog.Warn("powerschool: this session was already rejected upstream; " +
				"waiting for a new one, not contacting the portal")
			if onWait != nil {
				onWait()
			}
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
		if fresh, err := load(); err == nil {
			session = fresh
		}
	}
	if waiting {
		slog.Info("powerschool: a new session is in place; starting")
	}
	return session, nil
}

// ParseSession decodes and validates a session document. Errors never
// include the document: it is a live credential.
func ParseSession(raw []byte, now time.Time) (*pslib.SessionExport, error) {
	var s pslib.SessionExport
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &s); err != nil {
		return nil, fmt.Errorf("session is not valid JSON")
	}
	usable := s.Cookies[:0]
	for _, c := range s.Cookies {
		if c != nil && c.Name != "" && c.Value != "" {
			usable = append(usable, c)
		}
	}
	s.Cookies = usable
	if len(s.Cookies) == 0 {
		return nil, fmt.Errorf("session has no cookies")
	}
	if s.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("session has no expires_at")
	}
	if !s.ExpiresAt.After(now) {
		return nil, fmt.Errorf("session expired at %s", s.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return &s, nil
}
