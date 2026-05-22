package auth

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
)

// ---- source-id format ----

func TestSourceIDFormat_AcceptsValid(t *testing.T) {
	cases := []string{
		"recognizer",
		"workstation-mbox-importer",
		"friend-alice",
		"a1",
		"abc-def-123",
		"a",                                                                  // single letter
		strings.Repeat("a", 64),                                              // max length
		"abc-def-ghi-jkl-mno-pqr-stu-vwx-yz0-123",                            // multiple single dashes
	}
	for _, s := range cases {
		if !validSourceID(s) {
			t.Errorf("validSourceID(%q) = false, want true", s)
		}
	}
}

func TestSourceIDFormat_RejectsInvalid(t *testing.T) {
	cases := []string{
		"",                  // empty
		"Recognizer",        // uppercase
		"-recognizer",       // leading dash
		"recognizer-",       // trailing dash
		"foo--bar",          // double dash
		"xn--abc",           // IDN prefix (also caught by double-dash rule)
		"abc def",           // space
		"abc/def",           // slash
		"1abc",              // starts with digit
		strings.Repeat("a", 65), // too long (65 chars)
		"foo.bar",           // dot
		"foo_bar",           // underscore
	}
	for _, s := range cases {
		if validSourceID(s) {
			t.Errorf("validSourceID(%q) = true, want false", s)
		}
	}
}

// ---- token format ----

func TestTokenFormat_AcceptsValid(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, ok := decodeToken(valid); !ok {
		t.Errorf("decodeToken(valid) returned !ok")
	}
}

func TestTokenFormat_RejectsInvalid(t *testing.T) {
	cases := []string{
		"",         // empty
		"tooshort", // < 64 chars
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0", // too long (65)
		"0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef",  // uppercase
		"0123456789xyabcd0123456789abcdef0123456789abcdef0123456789abcdef",  // non-hex (x, y)
		"0123456789abcdef 123456789abcdef0123456789abcdef0123456789abcdef",  // space inside
		strings.Repeat("g", 64),                                              // non-hex letter
	}
	for _, s := range cases {
		if _, ok := decodeToken(s); ok {
			t.Errorf("decodeToken(%q) returned ok", s)
		}
	}
}

// ---- Lookup ----

func TestTokenStore_LookupReturnsSourceID(t *testing.T) {
	s := NewTokenStore()
	tok1 := mustDecodeTokenForTest(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	tok2 := mustDecodeTokenForTest(t, "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
	s.swap([]tokenEntry{
		{token: tok1, sourceID: "recognizer"},
		{token: tok2, sourceID: "friend-alice"},
	})

	if id, ok := s.Lookup(tok1); !ok || id != "recognizer" {
		t.Errorf("Lookup(tok1) = (%q, %v), want (recognizer, true)", id, ok)
	}
	if id, ok := s.Lookup(tok2); !ok || id != "friend-alice" {
		t.Errorf("Lookup(tok2) = (%q, %v), want (friend-alice, true)", id, ok)
	}
	var unknown [32]byte
	if _, ok := s.Lookup(unknown); ok {
		t.Error("Lookup(unknown) returned ok=true")
	}
}

func TestTokenStore_LookupEmptyStoreReturnsFalse(t *testing.T) {
	s := NewTokenStore()
	var any32 [32]byte
	if id, ok := s.Lookup(any32); ok || id != "" {
		t.Errorf("Lookup on empty store = (%q, %v), want (\"\", false)", id, ok)
	}
}

// mustNewTokenStore exercise (verifies the helper itself).
func TestMustNewTokenStoreHelper(t *testing.T) {
	hex1 := "11111111111111111111111111111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	s := mustNewTokenStore(t, map[string]string{"recognizer": hex1})
	tok := mustDecodeTokenForTest(t, hex1)
	if id, ok := s.Lookup(tok); !ok || id != "recognizer" {
		t.Errorf("Lookup after mustNewTokenStore = (%q, %v), want (recognizer, true)", id, ok)
	}
}

// ---- Reload ----

const (
	hexA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hexB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	hexC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestReload_HappyPath_TwoSources(t *testing.T) {
	fv := &fakeVault{
		Data: map[string]map[string]any{
			"recognizer":   {"token": hexA},
			"friend-alice": {"token": hexB},
		},
	}
	s := NewTokenStore()
	var onErrors []string
	cfg := ReloadConfig{
		Client:   fv,
		KVMount:  "secret",
		BasePath: "glovebox/ingest-tokens",
		OnError:  func(sid string) { onErrors = append(onErrors, sid) },
	}
	if err := s.Reload(context.Background(), cfg); err != nil {
		t.Fatalf("Reload returned err: %v", err)
	}
	if len(onErrors) != 0 {
		t.Errorf("OnError fired for healthy load: %v", onErrors)
	}
	tokA := mustDecodeTokenForTest(t, hexA)
	tokB := mustDecodeTokenForTest(t, hexB)
	if id, ok := s.Lookup(tokA); !ok || id != "recognizer" {
		t.Errorf("Lookup(tokA) = (%q, %v), want (recognizer, true)", id, ok)
	}
	if id, ok := s.Lookup(tokB); !ok || id != "friend-alice" {
		t.Errorf("Lookup(tokB) = (%q, %v), want (friend-alice, true)", id, ok)
	}
}

func TestReload_MalformedSourceID_Skipped(t *testing.T) {
	fv := &fakeVault{
		Data: map[string]map[string]any{
			"recognizer": {"token": hexA},
		},
		ExtraList: []string{"BAD--ID"}, // upper + double dash + uppercase
	}
	s := NewTokenStore()
	var onErrors []string
	cfg := ReloadConfig{
		Client:   fv,
		KVMount:  "secret",
		BasePath: "glovebox/ingest-tokens",
		OnError:  func(sid string) { onErrors = append(onErrors, sid) },
	}
	if err := s.Reload(context.Background(), cfg); err != nil {
		t.Fatalf("Reload returned err: %v", err)
	}
	if len(onErrors) != 1 || onErrors[0] != "BAD--ID" {
		t.Errorf("OnError = %v, want [BAD--ID]", onErrors)
	}
	tokA := mustDecodeTokenForTest(t, hexA)
	if id, ok := s.Lookup(tokA); !ok || id != "recognizer" {
		t.Errorf("Lookup(tokA) = (%q, %v), want (recognizer, true)", id, ok)
	}
}

func TestReload_MalformedToken_Skipped(t *testing.T) {
	fv := &fakeVault{
		Data: map[string]map[string]any{
			"recognizer":     {"token": hexA},
			"bad-token-host": {"token": "not-hex"},
		},
	}
	s := NewTokenStore()
	var onErrors []string
	cfg := ReloadConfig{
		Client:   fv,
		KVMount:  "secret",
		BasePath: "glovebox/ingest-tokens",
		OnError:  func(sid string) { onErrors = append(onErrors, sid) },
	}
	if err := s.Reload(context.Background(), cfg); err != nil {
		t.Fatalf("Reload returned err: %v", err)
	}
	if len(onErrors) != 1 || onErrors[0] != "bad-token-host" {
		t.Errorf("OnError = %v, want [bad-token-host]", onErrors)
	}
	tokA := mustDecodeTokenForTest(t, hexA)
	if id, ok := s.Lookup(tokA); !ok || id != "recognizer" {
		t.Errorf("Lookup(tokA) = (%q, %v), want (recognizer, true)", id, ok)
	}
}

func TestReload_VaultReadError_PerEntrySkipped(t *testing.T) {
	fv := &fakeVault{
		Data: map[string]map[string]any{
			"recognizer":   {"token": hexA},
			"vault-broken": {"token": hexB},
		},
		ReadErrs: map[string]error{
			"vault-broken": errors.New("transient vault read failure"),
		},
	}
	s := NewTokenStore()
	var onErrors []string
	cfg := ReloadConfig{
		Client:   fv,
		KVMount:  "secret",
		BasePath: "glovebox/ingest-tokens",
		OnError:  func(sid string) { onErrors = append(onErrors, sid) },
	}
	if err := s.Reload(context.Background(), cfg); err != nil {
		t.Fatalf("Reload returned err: %v", err)
	}
	if len(onErrors) != 1 || onErrors[0] != "vault-broken" {
		t.Errorf("OnError = %v, want [vault-broken]", onErrors)
	}
	tokA := mustDecodeTokenForTest(t, hexA)
	if id, ok := s.Lookup(tokA); !ok || id != "recognizer" {
		t.Errorf("Lookup(tokA) = (%q, %v), want (recognizer, true)", id, ok)
	}
}

func TestReload_DuplicateToken_DropsBoth(t *testing.T) {
	// Two source-ids share the same token bytes. BOTH must be dropped.
	fv := &fakeVault{
		Data: map[string]map[string]any{
			"recognizer":   {"token": hexA},
			"friend-alice": {"token": hexA}, // collision
			"third-clean":  {"token": hexB}, // unrelated valid entry
		},
	}
	s := NewTokenStore()
	var onErrors []string
	cfg := ReloadConfig{
		Client:   fv,
		KVMount:  "secret",
		BasePath: "glovebox/ingest-tokens",
		OnError:  func(sid string) { onErrors = append(onErrors, sid) },
	}
	if err := s.Reload(context.Background(), cfg); err != nil {
		t.Fatalf("Reload returned err: %v", err)
	}
	sort.Strings(onErrors)
	want := []string{"friend-alice", "recognizer"}
	if len(onErrors) != 2 || onErrors[0] != want[0] || onErrors[1] != want[1] {
		t.Errorf("OnError = %v, want %v", onErrors, want)
	}
	// Colliding tokens are dropped; the unrelated valid entry survives.
	tokA := mustDecodeTokenForTest(t, hexA)
	tokB := mustDecodeTokenForTest(t, hexB)
	if _, ok := s.Lookup(tokA); ok {
		t.Error("Lookup(colliding tokA) = ok=true, want false (both source-ids dropped)")
	}
	if id, ok := s.Lookup(tokB); !ok || id != "third-clean" {
		t.Errorf("Lookup(tokB) = (%q, %v), want (third-clean, true)", id, ok)
	}
}

// CRITICAL test from the plan: a triple-collision drops ALL THREE
// source-ids, not just two. A single-pass "delete the second
// occurrence" bug would let one entry leak through; the two-pass
// algorithm in Reload prevents this.
func TestReload_TripleDuplicateToken_DropsAllThree(t *testing.T) {
	fv := &fakeVault{
		Data: map[string]map[string]any{
			"first":       {"token": hexA},
			"second":      {"token": hexA},
			"third":       {"token": hexA}, // triple collision
			"clean-entry": {"token": hexB},
		},
	}
	s := NewTokenStore()
	var onErrors []string
	cfg := ReloadConfig{
		Client:   fv,
		KVMount:  "secret",
		BasePath: "glovebox/ingest-tokens",
		OnError:  func(sid string) { onErrors = append(onErrors, sid) },
	}
	if err := s.Reload(context.Background(), cfg); err != nil {
		t.Fatalf("Reload returned err: %v", err)
	}
	sort.Strings(onErrors)
	want := []string{"first", "second", "third"}
	if len(onErrors) != 3 {
		t.Fatalf("OnError = %v, want exactly 3 entries: %v", onErrors, want)
	}
	for i := range want {
		if onErrors[i] != want[i] {
			t.Errorf("OnError[%d] = %q, want %q (full: %v)", i, onErrors[i], want[i], onErrors)
		}
	}
	tokA := mustDecodeTokenForTest(t, hexA)
	tokB := mustDecodeTokenForTest(t, hexB)
	if _, ok := s.Lookup(tokA); ok {
		t.Error("Lookup(colliding tokA) = ok=true, want false (all three source-ids must be dropped)")
	}
	if id, ok := s.Lookup(tokB); !ok || id != "clean-entry" {
		t.Errorf("Lookup(tokB) = (%q, %v), want (clean-entry, true)", id, ok)
	}
}

func TestReload_TopLevelListError_PreservesPriorStore(t *testing.T) {
	// Prime the store with a valid entry.
	s := NewTokenStore()
	tokSeed := mustDecodeTokenForTest(t, hexA)
	s.swap([]tokenEntry{{token: tokSeed, sourceID: "recognizer"}})

	fv := &fakeVault{
		ListErr: errors.New("vault unreachable"),
	}
	cfg := ReloadConfig{
		Client:   fv,
		KVMount:  "secret",
		BasePath: "glovebox/ingest-tokens",
	}
	if err := s.Reload(context.Background(), cfg); err == nil {
		t.Fatal("Reload returned nil err on top-level list failure; want non-nil")
	}
	// Prior store must be intact.
	if id, ok := s.Lookup(tokSeed); !ok || id != "recognizer" {
		t.Errorf("Lookup after failed reload = (%q, %v), want (recognizer, true)", id, ok)
	}
}

func TestReload_MissingTokenField_Skipped(t *testing.T) {
	fv := &fakeVault{
		Data: map[string]map[string]any{
			"recognizer":     {"token": hexA},
			"no-token-field": {"notes": "operator forgot the token field"},
		},
	}
	s := NewTokenStore()
	var onErrors []string
	cfg := ReloadConfig{
		Client:   fv,
		KVMount:  "secret",
		BasePath: "glovebox/ingest-tokens",
		OnError:  func(sid string) { onErrors = append(onErrors, sid) },
	}
	if err := s.Reload(context.Background(), cfg); err != nil {
		t.Fatalf("Reload returned err: %v", err)
	}
	if len(onErrors) != 1 || onErrors[0] != "no-token-field" {
		t.Errorf("OnError = %v, want [no-token-field]", onErrors)
	}
}

// ---- LoadErr accessor ----

func TestLoadErr_HealthyAfterSuccess(t *testing.T) {
	s := NewTokenStore()
	if err := s.LoadErr(); err != nil {
		t.Errorf("LoadErr on fresh store = %v, want nil", err)
	}
	fv := &fakeVault{
		Data: map[string]map[string]any{
			"recognizer": {"token": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		},
	}
	if err := s.Reload(context.Background(), ReloadConfig{Client: fv, BasePath: "glovebox/ingest-tokens"}); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if err := s.LoadErr(); err != nil {
		t.Errorf("LoadErr after success = %v, want nil", err)
	}
}

func TestLoadErr_ReportsListFailure(t *testing.T) {
	s := NewTokenStore()
	listErr := errors.New("vault is sealed")
	fv := &fakeVault{ListErr: listErr}
	if err := s.Reload(context.Background(), ReloadConfig{Client: fv, BasePath: "glovebox/ingest-tokens"}); err == nil {
		t.Fatal("Reload returned nil; expected list error")
	}
	got := s.LoadErr()
	if got == nil || !errors.Is(got, listErr) {
		t.Errorf("LoadErr = %v, want wrapped vault-is-sealed error", got)
	}
}

// ---- sanitizeForLog ----

func TestSanitizeForLog_StripsControlsAndCaps(t *testing.T) {
	in := "abc\x00def\nghi\x7f"
	got := sanitizeForLog(in)
	want := "abc?def?ghi?"
	if got != want {
		t.Errorf("sanitizeForLog(%q) = %q, want %q", in, got, want)
	}
	long := strings.Repeat("x", 200)
	got = sanitizeForLog(long)
	if len(got) != 64 {
		t.Errorf("sanitizeForLog cap len = %d, want 64", len(got))
	}
}
