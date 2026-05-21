package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"

	"github.com/leftathome/schoology-go/auth"
)

// validCreds returns a fully-populated *auth.Credentials with the
// 5 required fields set.
func validCreds() *auth.Credentials {
	return &auth.Credentials{
		Host:      "school.schoology.com",
		SessID:    "SESS-abc",
		CSRFToken: "csrf-xyz",
		CSRFKey:   "csrf-key-1",
		UID:       "12345",
	}
}

// validConfig returns a config that passes missingFields().
func validConfig() config {
	return config{
		SchoologyHost:     "school.schoology.com",
		SchoologyUsername: "parent@example.com",
		SchoologyPassword: "hunter2",
		VaultAddr:         "http://vault:8200",
		VaultK8sRole:      "glovebox-schoology-refresher",
		VaultKVMount:      "secret",
		VaultSessionPath:  "glovebox/schoology/test/session",
	}
}

// silentLogger returns an slog.Logger that drops everything, keeping
// test output clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// stubWriter records the last Put call. recordedErr is returned to
// run().
type stubWriter struct {
	calls       int
	lastPath    string
	lastData    map[string]any
	returnErr   error
	returnedVer int
}

func (s *stubWriter) Put(ctx context.Context, path string, data map[string]any) (int, error) {
	s.calls++
	s.lastPath = path
	s.lastData = data
	return s.returnedVer, s.returnErr
}

// loginFromCreds returns a loginFunc that yields creds (or err).
func loginFromCreds(creds *auth.Credentials, err error) loginFunc {
	return func(ctx context.Context, host, username, password string) (*auth.Credentials, error) {
		return creds, err
	}
}

func TestRun_Success(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	creds := validCreds()
	sw := &stubWriter{returnedVer: 7}
	got := run(context.Background(), cfg, loginFromCreds(creds, nil), sw, silentLogger())
	if got != exitSuccess {
		t.Fatalf("exit code: got %d, want %d", got, exitSuccess)
	}
	if sw.calls != 1 {
		t.Fatalf("writer call count: got %d, want 1", sw.calls)
	}
	if sw.lastPath != cfg.VaultSessionPath {
		t.Errorf("vault path: got %q, want %q", sw.lastPath, cfg.VaultSessionPath)
	}
	want := map[string]any{
		"host":       "school.schoology.com",
		"sess_id":    "SESS-abc",
		"csrf_token": "csrf-xyz",
		"csrf_key":   "csrf-key-1",
		"uid":        "12345",
	}
	if !reflect.DeepEqual(sw.lastData, want) {
		t.Errorf("vault data: got %#v, want %#v", sw.lastData, want)
	}
}

func TestRun_ConfigError_Missing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*config)
	}{
		{"missing host", func(c *config) { c.SchoologyHost = "" }},
		{"missing username", func(c *config) { c.SchoologyUsername = "" }},
		{"missing password", func(c *config) { c.SchoologyPassword = "" }},
		{"missing vault addr", func(c *config) { c.VaultAddr = "" }},
		{"missing vault role", func(c *config) { c.VaultK8sRole = "" }},
		{"missing session path", func(c *config) { c.VaultSessionPath = "" }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			tc.mutate(&cfg)
			sw := &stubWriter{}
			// loginFunc must not be invoked when config is invalid; use
			// a stub that fails loudly if called.
			noLogin := func(context.Context, string, string, string) (*auth.Credentials, error) {
				t.Fatal("login called despite config error")
				return nil, nil
			}
			got := run(context.Background(), cfg, noLogin, sw, silentLogger())
			if got != exitConfigError {
				t.Fatalf("exit code: got %d, want %d", got, exitConfigError)
			}
			if sw.calls != 0 {
				t.Errorf("writer should not be called on config error; got %d calls", sw.calls)
			}
		})
	}
}

func TestRun_BadCredentials(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	sw := &stubWriter{}
	got := run(context.Background(), cfg,
		loginFromCreds(nil, auth.ErrBadCredentials),
		sw, silentLogger())
	if got != exitBadCredentials {
		t.Fatalf("exit code: got %d, want %d", got, exitBadCredentials)
	}
	if sw.calls != 0 {
		t.Errorf("writer should not be called after bad credentials; got %d calls", sw.calls)
	}
}

func TestRun_BadCredentials_Wrapped(t *testing.T) {
	t.Parallel()
	// errors.Is should still find the sentinel through wrapping.
	wrapped := errors.Join(errors.New("login pipeline"), auth.ErrBadCredentials)
	cfg := validConfig()
	sw := &stubWriter{}
	got := run(context.Background(), cfg,
		loginFromCreds(nil, wrapped),
		sw, silentLogger())
	if got != exitBadCredentials {
		t.Fatalf("exit code: got %d, want %d", got, exitBadCredentials)
	}
	if sw.calls != 0 {
		t.Errorf("writer should not be called after bad credentials; got %d calls", sw.calls)
	}
}

func TestRun_LoginTimeout(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	sw := &stubWriter{}
	got := run(context.Background(), cfg,
		loginFromCreds(nil, auth.ErrLoginTimeout),
		sw, silentLogger())
	if got != exitTransient {
		t.Fatalf("exit code: got %d, want %d", got, exitTransient)
	}
	if sw.calls != 0 {
		t.Errorf("writer should not be called after transient login failure; got %d calls", sw.calls)
	}
}

func TestRun_LoginNetworkError(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	sw := &stubWriter{}
	got := run(context.Background(), cfg,
		loginFromCreds(nil, errors.New("network unreachable")),
		sw, silentLogger())
	if got != exitTransient {
		t.Fatalf("exit code: got %d, want %d", got, exitTransient)
	}
	if sw.calls != 0 {
		t.Errorf("writer should not be called after transient login failure; got %d calls", sw.calls)
	}
}

func TestRun_VaultWrite_Forbidden(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"permission denied", errors.New("error writing secret to secret/data/glovebox/schoology/test/session: Error making API request.\n\nCode: 403. Errors:\n\n* permission denied")},
		{"403 status only", errors.New("403 Forbidden response from vault")},
		{"400 bad request", errors.New("400 Bad Request: invalid path")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			sw := &stubWriter{returnErr: tc.err}
			got := run(context.Background(), cfg,
				loginFromCreds(validCreds(), nil),
				sw, silentLogger())
			if got != exitSecretWriteRejected {
				t.Fatalf("exit code: got %d, want %d", got, exitSecretWriteRejected)
			}
		})
	}
}

func TestRun_VaultWrite_5xx(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"500 internal", errors.New("error writing secret: Code: 500. Internal Server Error")},
		{"network reset", errors.New("connection reset by peer")},
		{"timeout", errors.New("context deadline exceeded while contacting vault")},
		// Regression: an embedded "400" / "403" / "404" substring inside
		// an otherwise-5xx message used to misroute to exitSecretWriteRejected
		// because isClientError's substring fallback matched bare digits.
		// With markers tightened to keyword-only, these now route correctly
		// to exitTransient.
		{"502 with 400 in request id", errors.New("Code: 502. request id a-400-b")},
		{"503 with 403 in timestamp", errors.New("Code: 503. timestamp 2024-04-03T12:00:00Z")},
		{"504 with 404 in trace id", errors.New("Code: 504. trace id 404aaa-bbb")},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			sw := &stubWriter{returnErr: tc.err}
			got := run(context.Background(), cfg,
				loginFromCreds(validCreds(), nil),
				sw, silentLogger())
			if got != exitTransient {
				t.Fatalf("exit code: got %d, want %d", got, exitTransient)
			}
		})
	}
}

// TestRun_VaultWrite_ResponseError_Structural exercises the primary
// isClientError path: a *vaultapi.ResponseError with a 4xx status code.
// In production this is what the Vault client actually returns; the
// substring fallback (covered elsewhere) is only for wrapped/stringified
// errors. This test locks the structural-typing branch against accidental
// removal.
func TestRun_VaultWrite_ResponseError_Structural(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		statusCode int
		wantExit   int
	}{
		{"403 forbidden", 403, exitSecretWriteRejected},
		{"400 bad request", 400, exitSecretWriteRejected},
		{"404 not found", 404, exitSecretWriteRejected},
		{"429 too many requests", 429, exitSecretWriteRejected},
		{"500 internal server", 500, exitTransient},
		{"503 service unavailable", 503, exitTransient},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			respErr := &vaultapi.ResponseError{
				HTTPMethod: "PUT",
				URL:        "https://vault.example/v1/secret/data/glovebox/schoology/test/session",
				StatusCode: tc.statusCode,
				Errors:     []string{"vault returned an error"},
			}
			sw := &stubWriter{returnErr: respErr}
			got := run(context.Background(), cfg,
				loginFromCreds(validCreds(), nil),
				sw, silentLogger())
			if got != tc.wantExit {
				t.Fatalf("exit code: got %d, want %d (status %d)", got, tc.wantExit, tc.statusCode)
			}
		})
	}
}

func TestCredentialsToMap(t *testing.T) {
	t.Parallel()
	got := credentialsToMap(validCreds())
	want := map[string]any{
		"host":       "school.schoology.com",
		"sess_id":    "SESS-abc",
		"csrf_token": "csrf-xyz",
		"csrf_key":   "csrf-key-1",
		"uid":        "12345",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("credentialsToMap: got %#v, want %#v", got, want)
	}
}
