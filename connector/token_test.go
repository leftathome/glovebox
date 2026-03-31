package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenSource_StaticReturnsConfiguredToken(t *testing.T) {
	src := NewStaticTokenSource("ghp_abc123")
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ghp_abc123" {
		t.Errorf("got %q, want %q", got, "ghp_abc123")
	}
}

func TestTokenSource_StaticMultipleCallsReturnSameToken(t *testing.T) {
	src := NewStaticTokenSource("tok_xyz")
	for i := 0; i < 5; i++ {
		got, err := src.Token(context.Background())
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if got != "tok_xyz" {
			t.Errorf("call %d: got %q, want %q", i, got, "tok_xyz")
		}
	}
}

func TestTokenSource_StaticEmptyTokenReturnsEmptyString(t *testing.T) {
	src := NewStaticTokenSource("")
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestTokenSource_StaticIgnoresCancelledContext(t *testing.T) {
	src := NewStaticTokenSource("fast_token")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	got, err := src.Token(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "fast_token" {
		t.Errorf("got %q, want %q", got, "fast_token")
	}
}

// -- helpers for RefreshableTokenSource tests --

// writeTokenFile writes a tokenFile to the given directory as token.json.
func writeTokenFile(t *testing.T, dir string, tf tokenFile) {
	t.Helper()
	data, err := json.Marshal(tf)
	if err != nil {
		t.Fatalf("marshal token file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token.json"), data, 0644); err != nil {
		t.Fatalf("write token file: %v", err)
	}
}

// readTokenFile reads the token.json from a directory and returns its contents.
func readTokenFile(t *testing.T, dir string) tokenFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err != nil {
		t.Fatalf("unmarshal token file: %v", err)
	}
	return tf
}

func TestRefreshableTokenSource_ValidTokenReturnedWithoutHTTP(t *testing.T) {
	dir := t.TempDir()

	// Token that expires well in the future (no refresh needed).
	writeTokenFile(t, dir, tokenFile{
		AccessToken:  "valid_access_token",
		RefreshToken: "refresh_tok",
		TokenType:    "bearer",
		Expiry:       time.Now().Add(1 * time.Hour),
	})

	// Server that should never be called.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("HTTP server was called but should not have been")
	}))
	defer srv.Close()

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		TokenURL:     srv.URL,
	}

	src, err := NewRefreshableTokenSource(cfg, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "valid_access_token" {
		t.Errorf("got %q, want %q", got, "valid_access_token")
	}
}

func TestRefreshableTokenSource_ExpiredTokenTriggersRefresh(t *testing.T) {
	dir := t.TempDir()

	// Token that is already expired.
	writeTokenFile(t, dir, tokenFile{
		AccessToken:  "old_access",
		RefreshToken: "refresh_tok",
		TokenType:    "bearer",
		Expiry:       time.Now().Add(-1 * time.Hour),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("grant_type = %q, want refresh_token", r.FormValue("grant_type"))
		}
		if r.FormValue("refresh_token") != "refresh_tok" {
			t.Errorf("refresh_token = %q, want refresh_tok", r.FormValue("refresh_token"))
		}
		if r.FormValue("client_id") != "cid" {
			t.Errorf("client_id = %q, want cid", r.FormValue("client_id"))
		}
		if r.FormValue("client_secret") != "csec" {
			t.Errorf("client_secret = %q, want csec", r.FormValue("client_secret"))
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"new_access","refresh_token":"new_refresh","token_type":"bearer","expires_in":3600}`)
	}))
	defer srv.Close()

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		TokenURL:     srv.URL,
	}

	src, err := NewRefreshableTokenSource(cfg, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "new_access" {
		t.Errorf("got %q, want %q", got, "new_access")
	}
}

func TestRefreshableTokenSource_RefreshPersistsTokenToFile(t *testing.T) {
	dir := t.TempDir()

	writeTokenFile(t, dir, tokenFile{
		AccessToken:  "old_access",
		RefreshToken: "refresh_tok",
		TokenType:    "bearer",
		Expiry:       time.Now().Add(-1 * time.Hour),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"persisted_access","refresh_token":"persisted_refresh","token_type":"bearer","expires_in":7200}`)
	}))
	defer srv.Close()

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		TokenURL:     srv.URL,
	}

	src, err := NewRefreshableTokenSource(cfg, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Read the persisted file and verify contents.
	tf := readTokenFile(t, dir)
	if tf.AccessToken != "persisted_access" {
		t.Errorf("persisted access_token = %q, want %q", tf.AccessToken, "persisted_access")
	}
	if tf.RefreshToken != "persisted_refresh" {
		t.Errorf("persisted refresh_token = %q, want %q", tf.RefreshToken, "persisted_refresh")
	}
	if tf.TokenType != "bearer" {
		t.Errorf("persisted token_type = %q, want %q", tf.TokenType, "bearer")
	}
	if tf.Expiry.IsZero() {
		t.Error("persisted expiry should not be zero")
	}
}

func TestRefreshableTokenSource_MissingTokenFileReturnsPermanentError(t *testing.T) {
	dir := t.TempDir()
	// Do not create token.json.

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		TokenURL:     "http://unused",
	}

	_, err := NewRefreshableTokenSource(cfg, dir)
	if err == nil {
		t.Fatal("expected error for missing token file, got nil")
	}
	if !IsPermanent(err) {
		t.Errorf("expected PermanentError, got: %v", err)
	}
	wantMsg := fmt.Sprintf("token file not found at %s; run 'glovebox-auth setup' to authenticate",
		filepath.Join(dir, "token.json"))
	if err.Error() != wantMsg {
		t.Errorf("error message = %q, want %q", err.Error(), wantMsg)
	}
}

func TestRefreshableTokenSource_Refresh401ReturnsPermanentError(t *testing.T) {
	dir := t.TempDir()

	writeTokenFile(t, dir, tokenFile{
		AccessToken:  "old_access",
		RefreshToken: "refresh_tok",
		TokenType:    "bearer",
		Expiry:       time.Now().Add(-1 * time.Hour),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"unauthorized"}`)
	}))
	defer srv.Close()

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		TokenURL:     srv.URL,
	}

	src, err := NewRefreshableTokenSource(cfg, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = src.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	if !IsPermanent(err) {
		t.Errorf("expected PermanentError, got: %v", err)
	}
}

func TestRefreshableTokenSource_InvalidGrantReturnsPermanentError(t *testing.T) {
	dir := t.TempDir()

	writeTokenFile(t, dir, tokenFile{
		AccessToken:  "old_access",
		RefreshToken: "refresh_tok",
		TokenType:    "bearer",
		Expiry:       time.Now().Add(-1 * time.Hour),
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":"invalid_grant","error_description":"refresh token expired"}`)
	}))
	defer srv.Close()

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		TokenURL:     srv.URL,
	}

	src, err := NewRefreshableTokenSource(cfg, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = src.Token(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid_grant, got nil")
	}
	if !IsPermanent(err) {
		t.Errorf("expected PermanentError, got: %v", err)
	}
}

func TestRefreshableTokenSource_ConcurrentCallsDontDoubleRefresh(t *testing.T) {
	dir := t.TempDir()

	writeTokenFile(t, dir, tokenFile{
		AccessToken:  "old_access",
		RefreshToken: "refresh_tok",
		TokenType:    "bearer",
		Expiry:       time.Now().Add(-1 * time.Hour),
	})

	var callCount atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		// Small delay to increase chance of concurrent arrivals.
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"refreshed","refresh_token":"new_refresh","token_type":"bearer","expires_in":3600}`)
	}))
	defer srv.Close()

	cfg := OAuthConfig{
		ClientID:     "cid",
		ClientSecret: "csec",
		TokenURL:     srv.URL,
	}

	src, err := NewRefreshableTokenSource(cfg, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)
	tokens := make([]string, goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			tok, err := src.Token(context.Background())
			tokens[idx] = tok
			errs[idx] = err
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, err)
		}
	}
	for i, tok := range tokens {
		if tok != "refreshed" {
			t.Errorf("goroutine %d: got %q, want %q", i, tok, "refreshed")
		}
	}

	if got := callCount.Load(); got != 1 {
		t.Errorf("expected 1 HTTP call, got %d", got)
	}
}
