package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
)

// fakeClock is a settable time source for ReloginVaultClient.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) now() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.t }
func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	f.t = f.t.Add(d)
	f.mu.Unlock()
}

// Review of glovebox-jzli: a 403 is also what a broken role or policy returns.
// Re-logging in on every 403 would then mint a new 24h token on every call
// (one per token entry per reload). A re-login is allowed only once the last
// successful login is at least minRelogin old.
func TestReloginVaultClientRateLimitsReloginOnRepeatedRejection(t *testing.T) {
	denied := &scriptedVault{err: vaultStatus(http.StatusForbidden, "permission denied")}
	login, logins := loginSequence(denied)
	clock := &fakeClock{t: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	c := NewReloginVaultClient(login)
	c.now = clock.now

	// Boot login, 403, but the token is brand new: no re-login.
	if _, err := c.ListPath(context.Background(), "secret", "p"); err == nil {
		t.Fatal("expected 403")
	}
	if *logins != 1 {
		t.Fatalf("logins after a 403 on a fresh token = %d, want 1 (no re-login)", *logins)
	}

	// A day later (the role's 24h TTL): the 403 now triggers one re-login.
	clock.advance(24 * time.Hour)
	if _, err := c.ListPath(context.Background(), "secret", "p"); err == nil {
		t.Fatal("expected 403 (policy still denies)")
	}
	if *logins != 2 {
		t.Fatalf("logins after a 403 on a day-old token = %d, want 2", *logins)
	}

	// Per-entry reads in the same reload must not each log in again.
	for i := 0; i < 5; i++ {
		_, _ = c.ReadKV(context.Background(), "secret", "p/entry")
	}
	if *logins != 2 {
		t.Errorf("logins after 5 more rejected reads in the same minute = %d, want 2", *logins)
	}
}

// When a freshly issued token is refused too, the problem is the role or the
// policy, not expiry; the error must say so.
func TestReloginVaultClientNamesRoleOrPolicyWhenFreshTokenRefused(t *testing.T) {
	denied := &scriptedVault{err: vaultStatus(http.StatusForbidden, "permission denied")}
	login, _ := loginSequence(denied)
	clock := &fakeClock{t: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	c := NewReloginVaultClient(login)
	c.now = clock.now

	_, _ = c.ListPath(context.Background(), "secret", "p") // boot login
	clock.advance(time.Hour)
	_, err := c.ListPath(context.Background(), "secret", "p")
	if err == nil || !strings.Contains(err.Error(), "role") || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("err = %v, want it to point at the Vault role/policy", err)
	}
	if !isTokenRejected(err) {
		t.Error("wrapped error no longer unwraps to the Vault 403")
	}
}

// Concurrent callers rejected on the same token share one re-login.
func TestReloginVaultClientConcurrentRejectionsShareOneLogin(t *testing.T) {
	gate := make(chan struct{})
	var entered sync.WaitGroup
	entered.Add(2)
	expired := &gatedVault{gate: gate, entered: &entered, err: vaultStatus(http.StatusForbidden, "permission denied")}
	fresh := &scriptedVault{keys: []string{"nagus"}}

	var mu sync.Mutex
	n := 0
	login := func(context.Context) (VaultClient, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n == 1 {
			return expired, nil
		}
		return fresh, nil
	}
	clock := &fakeClock{t: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
	c := NewReloginVaultClient(login)
	c.now = clock.now
	if _, _, err := c.client(context.Background(), noStaleGen); err != nil { // boot login
		t.Fatal(err)
	}
	clock.advance(time.Hour)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.ListPath(context.Background(), "secret", "p")
		}(i)
	}
	entered.Wait() // both callers are inside the expired client
	close(gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if n != 2 {
		t.Errorf("logins = %d, want 2 (boot + one shared re-login)", n)
	}
}

// gatedVault holds every call until gate closes, then fails with err.
type gatedVault struct {
	gate    chan struct{}
	entered *sync.WaitGroup
	err     error
}

func (g *gatedVault) ListPath(context.Context, string, string) ([]string, error) {
	g.entered.Done()
	<-g.gate
	return nil, g.err
}

func (g *gatedVault) ReadKV(context.Context, string, string) (map[string]any, error) {
	g.entered.Done()
	<-g.gate
	return nil, g.err
}

// The classification must hold for what the real SDK returns, not only for a
// hand-built ResponseError: run productionVaultClient against a Vault-shaped
// HTTP server that refuses the token.
func TestReloginVaultClientRecognisesRealSDKRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
	}))
	defer srv.Close()

	newClient := func() VaultClient {
		cfg := vaultapi.DefaultConfig()
		cfg.Address = srv.URL
		cfg.MaxRetries = 0
		vc, err := vaultapi.NewClient(cfg)
		if err != nil {
			t.Fatal(err)
		}
		vc.SetToken("expired-token")
		return NewProductionVaultClient(vc)
	}
	var logins int
	login := func(context.Context) (VaultClient, error) { logins++; return newClient(), nil }

	for _, tc := range []struct {
		name string
		call func(*ReloginVaultClient) error
	}{
		{"list", func(c *ReloginVaultClient) error {
			_, err := c.ListPath(context.Background(), "secret", "glovebox/ingest-tokens")
			return err
		}},
		{"kv get", func(c *ReloginVaultClient) error {
			_, err := c.ReadKV(context.Background(), "secret", "glovebox/ingest-tokens/nagus")
			return err
		}},
	} {
		logins = 0
		clock := &fakeClock{t: time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC)}
		c := NewReloginVaultClient(login)
		c.now = clock.now
		if _, _, err := c.client(context.Background(), noStaleGen); err != nil {
			t.Fatal(err)
		}
		clock.advance(time.Hour)
		err := tc.call(c)
		if !isTokenRejected(err) {
			t.Errorf("%s: SDK error %v not classified as a token rejection", tc.name, err)
		}
		if logins != 2 {
			t.Errorf("%s: logins = %d, want 2 (boot + re-login)", tc.name, logins)
		}
	}
}
