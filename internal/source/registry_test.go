package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp writes contents to a temp sources.json and returns its path.
func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sources.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return path
}

const recognizerScannerJSON = `{
  "enforce": true,
  "sources": [
    { "source_id": "recognizer-scanner", "kind": "recognizer-scanner",
      "data_subject_default": "e_111111", "audience_default": ["operator"] }
  ]
}`

func TestLoad_EmptyPath(t *testing.T) {
	reg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if reg == nil {
		t.Fatal("expected non-nil registry")
	}
	if reg.Enforce() {
		t.Error("empty registry must not enforce")
	}
	if _, ok := reg.Lookup("anything"); ok {
		t.Error("empty registry must not resolve any source")
	}
}

func TestLoad_RecognizerScanner(t *testing.T) {
	reg, err := Load(writeTemp(t, recognizerScannerJSON))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reg.Enforce() {
		t.Error("expected enforce=true")
	}
	pol, ok := reg.Lookup("recognizer-scanner")
	if !ok {
		t.Fatal("expected recognizer-scanner to resolve")
	}
	if pol.Kind() != "recognizer-scanner" {
		t.Errorf("Kind()=%q, want recognizer-scanner", pol.Kind())
	}
	if pol.DataSubjectDefault() != "e_111111" {
		t.Errorf("DataSubjectDefault()=%q, want e_111111", pol.DataSubjectDefault())
	}
	aud := pol.AudienceDefault()
	if len(aud) != 1 || aud[0] != "operator" {
		t.Errorf("AudienceDefault()=%v, want [operator]", aud)
	}
	// AudienceDefault must return a copy: mutating the result must not
	// affect a subsequent lookup.
	aud[0] = "mutated"
	if again := pol.AudienceDefault(); again[0] != "operator" {
		t.Errorf("AudienceDefault() leaked internal slice: %v", again)
	}
}

func TestLoad_UnknownSource(t *testing.T) {
	reg, err := Load(writeTemp(t, recognizerScannerJSON))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := reg.Lookup("rss"); ok {
		t.Error("unknown source must not resolve")
	}
}

func TestLoad_RejectsBadAudience(t *testing.T) {
	// audience_default ["subject"] is subject-relative and requires a
	// data_subject; with no data_subject_default it must be rejected.
	bad := `{ "enforce": true, "sources": [
		{ "source_id": "x", "kind": "k", "audience_default": ["subject"] } ] }`
	if _, err := Load(writeTemp(t, bad)); err == nil {
		t.Fatal("expected error for subject-relative audience without data_subject")
	}
}

func TestLoad_RejectsDuplicateSourceID(t *testing.T) {
	dup := `{ "sources": [
		{ "source_id": "a", "kind": "k" },
		{ "source_id": "a", "kind": "k" } ] }`
	_, err := Load(writeTemp(t, dup))
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate error, got %v", err)
	}
}

func TestLoad_RejectsEmptySourceID(t *testing.T) {
	empty := `{ "sources": [ { "source_id": "", "kind": "k" } ] }`
	_, err := Load(writeTemp(t, empty))
	if err == nil || !strings.Contains(err.Error(), "source_id") {
		t.Fatalf("expected empty source_id error, got %v", err)
	}
}

func TestLookup_NilReceiver(t *testing.T) {
	var reg *Registry
	if reg.Enforce() {
		t.Error("nil registry must not enforce")
	}
	if _, ok := reg.Lookup("x"); ok {
		t.Error("nil registry must not resolve")
	}
}
