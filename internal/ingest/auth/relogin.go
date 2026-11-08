package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"

	vaultapi "github.com/hashicorp/vault/api"
)

// LoginFunc returns a freshly authenticated Vault client.
type LoginFunc func(ctx context.Context) (VaultClient, error)

// ReloginVaultClient is a VaultClient that owns its Vault login. The token
// from a single boot-time login eventually expires, after which every reload
// fails with 403 "invalid token" until the pod restarts (glovebox-jzli). This
// client logs in lazily, and when Vault refuses the current token (401/403)
// it logs in again and retries the call once. A failed login is not sticky:
// the next call tries again, so the periodic token reload doubles as the
// retry cadence.
type ReloginVaultClient struct {
	login LoginFunc

	mu  sync.Mutex
	cur VaultClient
}

// NewReloginVaultClient wraps login. No login happens until the first call.
func NewReloginVaultClient(login LoginFunc) *ReloginVaultClient {
	return &ReloginVaultClient{login: login}
}

// ListPath implements VaultClient.
func (r *ReloginVaultClient) ListPath(ctx context.Context, kvMount, path string) ([]string, error) {
	var keys []string
	err := r.do(ctx, func(c VaultClient) error {
		var err error
		keys, err = c.ListPath(ctx, kvMount, path)
		return err
	})
	return keys, err
}

// ReadKV implements VaultClient.
func (r *ReloginVaultClient) ReadKV(ctx context.Context, kvMount, path string) (map[string]any, error) {
	var data map[string]any
	err := r.do(ctx, func(c VaultClient) error {
		var err error
		data, err = c.ReadKV(ctx, kvMount, path)
		return err
	})
	return data, err
}

// do runs call against the current client, logging in first if there is
// none, and once more if Vault rejects the token.
func (r *ReloginVaultClient) do(ctx context.Context, call func(VaultClient) error) error {
	c, err := r.client(ctx, nil)
	if err != nil {
		return err
	}
	err = call(c)
	if !isTokenRejected(err) {
		return err
	}
	c, lerr := r.client(ctx, c)
	if lerr != nil {
		return lerr
	}
	return call(c)
}

// client returns the current client, logging in when there is none or when
// the current one is stale (the client the caller saw rejected). Comparing
// against stale keeps concurrent callers from logging in twice for the same
// rejected token.
func (r *ReloginVaultClient) client(ctx context.Context, stale VaultClient) (VaultClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil && r.cur != stale {
		return r.cur, nil
	}
	c, err := r.login(ctx)
	if err != nil {
		r.cur = nil
		return nil, err
	}
	r.cur = c
	return c, nil
}

// isTokenRejected reports whether err is Vault refusing the client token.
func isTokenRejected(err error) bool {
	var re *vaultapi.ResponseError
	if !errors.As(err, &re) {
		return false
	}
	return re.StatusCode == http.StatusForbidden || re.StatusCode == http.StatusUnauthorized
}
