package schoology

import (
	"os"
	"strings"
	"testing"
)

func TestRejectedSession_RoundTrip(t *testing.T) {
	r := NewRejectedSession(t.TempDir())
	old := SessionFingerprint("sess-old")
	fresh := SessionFingerprint("sess-new")

	if r.IsRejected(old) {
		t.Fatal("nothing marked yet, but IsRejected = true")
	}
	if err := r.Mark(old); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if !r.IsRejected(old) {
		t.Fatal("marked session not reported as rejected")
	}
	// A refreshed session must be let through.
	if r.IsRejected(fresh) {
		t.Fatal("a different session is reported as rejected")
	}
}

func TestRejectedSession_MarkerHoldsNoSessionID(t *testing.T) {
	r := NewRejectedSession(t.TempDir())
	const sess = "super-secret-session-id"
	if err := r.Mark(SessionFingerprint(sess)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), sess) {
		t.Fatal("marker file contains the raw session id")
	}
	info, err := os.Stat(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("marker mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestRejectedSession_DisabledAndFailOpen(t *testing.T) {
	var disabled RejectedSession
	fp := SessionFingerprint("x")
	if err := disabled.Mark(fp); err != nil {
		t.Fatalf("disabled Mark: %v", err)
	}
	if disabled.IsRejected(fp) {
		t.Fatal("disabled marker reported a rejection")
	}
	if NewRejectedSession("").Path != "" {
		t.Fatal("empty state dir should disable the marker")
	}
	// Unreadable marker (missing file) fails open.
	if NewRejectedSession(t.TempDir()).IsRejected(fp) {
		t.Fatal("missing marker reported a rejection")
	}
	if err := NewRejectedSession(t.TempDir()).Mark(""); err == nil {
		t.Fatal("Mark accepted an empty fingerprint")
	}
}
