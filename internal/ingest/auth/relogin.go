package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

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
//
// A 403 is also what a broken role or policy returns, and then a fresh token
// is refused too. So a rejection triggers a re-login only when the last
// successful login is at least minRelogin old, which bounds it to about one
// login per reload instead of one per call, and a refused fresh token is
// reported as a role/policy problem. Callers waiting on mu do not observe
// their own ctx; they wait for at most one login.
type ReloginVaultClient struct {
	login      LoginFunc
	now        func() time.Time
	minRelogin time.Duration

	mu        sync.Mutex
	cur       VaultClient
	gen       int64 // successful logins so far; identifies cur
	lastLogin time.Time
}

// defaultMinRelogin is how old a token must be before a rejection of it
// triggers a new login.
const defaultMinRelogin = time.Minute

// NewReloginVaultClient wraps login. No login happens until the first call.
func NewReloginVaultClient(login LoginFunc) *ReloginVaultClient {
	return &ReloginVaultClient{login: login, now: time.Now, minRelogin: defaultMinRelogin}
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
	c, gen, err := r.client(ctx, noStaleGen)
	if err != nil {
		return err
	}
	err = call(c)
	if !isTokenRejected(err) {
		return err
	}
	fresh, freshGen, lerr := r.client(ctx, gen)
	if lerr != nil {
		return lerr
	}
	if freshGen == gen {
		// Too soon after the last login to try another one.
		return fmt.Errorf("vault rejected a token issued under %s ago; not logging in again yet (check the vault kubernetes auth role and its policy): %w", r.minRelogin, err)
	}
	if err := call(fresh); err != nil {
		if isTokenRejected(err) {
			return fmt.Errorf("vault rejected a freshly issued token: check the vault kubernetes auth role and its policy: %w", err)
		}
		return err
	}
	return nil
}

// noStaleGen asks client for whatever client is current.
const noStaleGen = -1

// client returns the current client and its login generation. It logs in
// when there is no client, or when the caller saw generation staleGen
// rejected, that is still the current one, and it is at least minRelogin
// old. Keying on the generation (not the client value) keeps concurrent
// callers from logging in twice for one rejected token.
func (r *ReloginVaultClient) client(ctx context.Context, staleGen int64) (VaultClient, int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil && r.gen != staleGen {
		return r.cur, r.gen, nil
	}
	if r.cur != nil && r.now().Sub(r.lastLogin) < r.minRelogin {
		return r.cur, r.gen, nil
	}
	c, err := r.login(ctx)
	if err != nil {
		r.cur = nil
		return nil, r.gen, err
	}
	r.cur = c
	r.gen++
	r.lastLogin = r.now()
	return c, r.gen, nil
}

// isTokenRejected reports whether err is Vault refusing the client token.
func isTokenRejected(err error) bool {
	var re *vaultapi.ResponseError
	if !errors.As(err, &re) {
		return false
	}
	return re.StatusCode == http.StatusForbidden || re.StatusCode == http.StatusUnauthorized
}
