package auth

import (
	"context"
	"errors"
	"os"
	"testing"
)

var errTestList = errors.New("list failed")

func TestEnvTokenSource_ListAndRead(t *testing.T) {
	const sid = "recognizer"
	const tok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	src := NewEnvTokenSource(sid, tok)

	ids, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: unexpected error: %v", err)
	}
	if len(ids) != 1 || ids[0] != sid {
		t.Fatalf("List = %v, want [%q]", ids, sid)
	}

	got, err := src.Read(context.Background(), sid)
	if err != nil {
		t.Fatalf("Read: unexpected error: %v", err)
	}
	if got != tok {
		t.Fatalf("Read = %q, want %q", got, tok)
	}
}

func TestEnvTokenSource_EmptyYieldsNoEntries(t *testing.T) {
	src := NewEnvTokenSource("", "")
	ids, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("List = %v, want empty", ids)
	}
}

func TestEnvTokenSource_ReadUnknownSourceIDErrors(t *testing.T) {
	src := NewEnvTokenSource("recognizer", "abc")
	if _, err := src.Read(context.Background(), "other"); err == nil {
		t.Fatal("Read(other): expected error, got nil")
	}
}

func writeTokenFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/tokens.json"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	return path
}

func TestFileTokenSource_ListSortedAndRead(t *testing.T) {
	const tokA = "0000000000000000000000000000000000000000000000000000000000000001"
	const tokB = "0000000000000000000000000000000000000000000000000000000000000002"
	path := writeTokenFile(t, `{"zebra":"`+tokB+`","alpha":"`+tokA+`"}`)
	src := NewFileTokenSource(path)

	ids, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: unexpected error: %v", err)
	}
	if len(ids) != 2 || ids[0] != "alpha" || ids[1] != "zebra" {
		t.Fatalf("List = %v, want [alpha zebra] (sorted)", ids)
	}

	got, err := src.Read(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Read: unexpected error: %v", err)
	}
	if got != tokA {
		t.Fatalf("Read(alpha) = %q, want %q", got, tokA)
	}
}

func TestFileTokenSource_MissingFileListErrors(t *testing.T) {
	src := NewFileTokenSource("/no/such/tokens.json")
	if _, err := src.List(context.Background()); err == nil {
		t.Fatal("List on missing file: expected error, got nil")
	}
}

func TestFileTokenSource_ReadUnknownSourceIDErrors(t *testing.T) {
	path := writeTokenFile(t, `{"alpha":"abc"}`)
	src := NewFileTokenSource(path)
	if _, err := src.Read(context.Background(), "missing"); err == nil {
		t.Fatal("Read(missing): expected error, got nil")
	}
}

func TestFileTokenSource_RereadsOnEachList(t *testing.T) {
	path := writeTokenFile(t, `{"alpha":"abc"}`)
	src := NewFileTokenSource(path)
	if _, err := src.List(context.Background()); err != nil {
		t.Fatalf("first List: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"alpha":"abc","beta":"def"}`), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	ids, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("second List: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("List after rewrite = %v, want 2 entries (hot reload)", ids)
	}
}

func TestVaultTokenSource_WrapsClient(t *testing.T) {
	const tok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fv := &fakeVault{Data: map[string]map[string]any{
		"recognizer": {"token": tok},
	}}
	src := NewVaultTokenSource(fv, "secret", "glovebox/ingest-tokens")

	ids, err := src.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 1 || ids[0] != "recognizer" {
		t.Fatalf("List = %v, want [recognizer]", ids)
	}

	got, err := src.Read(context.Background(), "recognizer")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != tok {
		t.Fatalf("Read = %q, want %q", got, tok)
	}
}

func TestVaultTokenSource_ListErrorPropagates(t *testing.T) {
	fv := &fakeVault{ListErr: errTestList}
	src := NewVaultTokenSource(fv, "secret", "base")
	if _, err := src.List(context.Background()); err == nil {
		t.Fatal("List: expected propagated error, got nil")
	}
}

func TestReload_FromTokenSource(t *testing.T) {
	const sid = "recognizer"
	const tok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	s := NewTokenStore()
	if err := s.Reload(context.Background(), ReloadConfig{Source: NewEnvTokenSource(sid, tok)}); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	req := mustDecodeTokenForTest(t, tok)
	got, ok := s.Lookup(req)
	if !ok || got != sid {
		t.Fatalf("Lookup = (%q,%v), want (%q,true)", got, ok, sid)
	}
}
