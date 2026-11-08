package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// glovebox-l9zp: a whole-reload failure (Vault refusing the token) used to
// leave no metric behind -- OnError only fires for individual bad entries.
// OnReload reports every reload's outcome.
func TestReloadReportsListFailureToOnReload(t *testing.T) {
	src := NewVaultTokenSource(&fakeVault{ListErr: errors.New("Code: 403. permission denied")}, "secret", "glovebox/ingest-tokens")
	var calls int
	var gotErr error
	gotTokens := -1
	cfg := ReloadConfig{Source: src, OnReload: func(err error, tokens int) {
		calls++
		gotErr, gotTokens = err, tokens
	}}

	if err := NewTokenStore().Reload(context.Background(), cfg); err == nil {
		t.Fatal("Reload: expected the list error")
	}
	if calls != 1 {
		t.Fatalf("OnReload calls = %d, want 1", calls)
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "403") {
		t.Errorf("OnReload err = %v, want the list error", gotErr)
	}
	if gotTokens != 0 {
		t.Errorf("OnReload tokens = %d, want 0 on failure", gotTokens)
	}
}

func TestReloadReportsSuccessAndTokenCountToOnReload(t *testing.T) {
	src := NewVaultTokenSource(&fakeVault{Data: map[string]map[string]any{
		"nagus":      {"token": strings.Repeat("a", 64)},
		"recognizer": {"token": strings.Repeat("b", 64)},
	}}, "secret", "glovebox/ingest-tokens")
	var calls int
	var gotErr error
	gotTokens := -1
	cfg := ReloadConfig{Source: src, OnReload: func(err error, tokens int) {
		calls++
		gotErr, gotTokens = err, tokens
	}}

	if err := NewTokenStore().Reload(context.Background(), cfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if calls != 1 || gotErr != nil {
		t.Fatalf("OnReload calls=%d err=%v, want 1 call with nil error", calls, gotErr)
	}
	if gotTokens != 2 {
		t.Errorf("OnReload tokens = %d, want 2", gotTokens)
	}
}
