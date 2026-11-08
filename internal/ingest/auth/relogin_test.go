package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	vaultapi "github.com/hashicorp/vault/api"
)

// scriptedVault is a VaultClient whose calls fail with err (if set) and
// otherwise return fixed data.
type scriptedVault struct {
	err   error
	keys  []string
	data  map[string]any
	calls int
}

func (s *scriptedVault) ListPath(context.Context, string, string) ([]string, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.keys, nil
}

func (s *scriptedVault) ReadKV(context.Context, string, string) (map[string]any, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.data, nil
}

// vaultStatus is the error the Vault SDK returns for an HTTP error status.
func vaultStatus(code int, msg string) error {
	return &vaultapi.ResponseError{HTTPMethod: http.MethodGet, StatusCode: code, Errors: []string{msg}}
}

// loginSequence returns a LoginFunc that hands out clients (or errors) in
// order and counts how often it was called.
func loginSequence(results ...any) (LoginFunc, *int) {
	n := 0
	return func(context.Context) (VaultClient, error) {
		r := results[min(n, len(results)-1)]
		n++
		if err, ok := r.(error); ok {
			return nil, err
		}
		return r.(VaultClient), nil
	}, &n
}

// glovebox-jzli: the token from the one boot-time login expired and every
// reload afterwards failed with 403 "invalid token" forever. A 403 must
// trigger a fresh login and one retry.
func TestReloginVaultClientRenewsOnExpiredToken(t *testing.T) {
	expired := &scriptedVault{err: vaultStatus(http.StatusForbidden, "permission denied")}
	fresh := &scriptedVault{keys: []string{"nagus"}}
	login, logins := loginSequence(expired, fresh)
	c := NewReloginVaultClient(login)

	keys, err := c.ListPath(context.Background(), "secret", "glovebox/ingest-tokens")
	if err != nil {
		t.Fatalf("ListPath after token expiry: %v", err)
	}
	if len(keys) != 1 || keys[0] != "nagus" {
		t.Fatalf("keys = %v, want [nagus]", keys)
	}
	if *logins != 2 {
		t.Errorf("logins = %d, want 2 (boot + re-login)", *logins)
	}

	// Later calls reuse the fresh client without logging in again.
	if _, err := c.ReadKV(context.Background(), "secret", "glovebox/ingest-tokens/nagus"); err != nil {
		t.Fatalf("ReadKV: %v", err)
	}
	if *logins != 2 {
		t.Errorf("logins after healthy call = %d, want 2", *logins)
	}
}

// A 401 is treated like a 403: the token is not usable.
func TestReloginVaultClientRenewsOn401(t *testing.T) {
	stale := &scriptedVault{err: vaultStatus(http.StatusUnauthorized, "missing client token")}
	fresh := &scriptedVault{data: map[string]any{"token": "x"}}
	login, logins := loginSequence(stale, fresh)
	c := NewReloginVaultClient(login)

	if _, err := c.ReadKV(context.Background(), "secret", "p"); err != nil {
		t.Fatalf("ReadKV: %v", err)
	}
	if *logins != 2 {
		t.Errorf("logins = %d, want 2", *logins)
	}
}

// A failed boot-time login must not be permanent (the old failingVaultClient
// failed every call until the pod restarted): the next call logs in again.
func TestReloginVaultClientRecoversFromFailedBootLogin(t *testing.T) {
	ok := &scriptedVault{keys: []string{"a"}}
	login, logins := loginSequence(errors.New("vault k8s login: connection refused"), ok)
	c := NewReloginVaultClient(login)

	if _, err := c.ListPath(context.Background(), "secret", "p"); err == nil {
		t.Fatal("first call: expected the login error, got nil")
	}
	if _, err := c.ListPath(context.Background(), "secret", "p"); err != nil {
		t.Fatalf("second call after Vault came back: %v", err)
	}
	if *logins != 2 {
		t.Errorf("logins = %d, want 2", *logins)
	}
}

// Errors that are not about the token (Vault down, 5xx, a missing path) are
// returned as-is, without burning a login.
func TestReloginVaultClientDoesNotReloginOnOtherErrors(t *testing.T) {
	broken := &scriptedVault{err: vaultStatus(http.StatusInternalServerError, "internal error")}
	login, logins := loginSequence(broken)
	c := NewReloginVaultClient(login)

	if _, err := c.ListPath(context.Background(), "secret", "p"); err == nil {
		t.Fatal("expected the 500 to be returned")
	}
	if *logins != 1 {
		t.Errorf("logins = %d, want 1 (no re-login on 5xx)", *logins)
	}
	if broken.calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on 5xx)", broken.calls)
	}
}

// If the fresh token is refused too (role or policy broken), the call fails
// after exactly one re-login instead of looping.
func TestReloginVaultClientRetriesOnlyOnce(t *testing.T) {
	denied := &scriptedVault{err: vaultStatus(http.StatusForbidden, "permission denied")}
	login, logins := loginSequence(denied, denied, denied)
	c := NewReloginVaultClient(login)

	if _, err := c.ListPath(context.Background(), "secret", "p"); err == nil {
		t.Fatal("expected a 403 when the fresh token is refused too")
	}
	if *logins != 2 {
		t.Errorf("logins = %d, want 2", *logins)
	}
	if denied.calls != 2 {
		t.Errorf("calls = %d, want 2 (original + one retry)", denied.calls)
	}
}
