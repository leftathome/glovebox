package loginvault_test

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/loginvault"
	"github.com/leftathome/glovebox/internal/loginvault/vaulttest"
)

// The package's behaviour against Vault's HTTP API -- the Kubernetes login,
// check-and-set, certificate verification, the token's revocation -- is
// pinned by connectors/powerschool-auth-refresher/vault_test.go, which
// drives this code through that binary's configuration. What is added here
// is what the login broker needs on top.

const (
	sessionPath = "glovebox/powerschool/default/session"
	statePath   = "glovebox/powerschool/default/login-state"
)

func connect(t *testing.T, v *vaulttest.Vault) *loginvault.Store {
	t.Helper()
	store, err := loginvault.Connect(context.Background(), v.Config(t, "glovebox-login-broker-powerschool", sessionPath, statePath))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return store
}

// Every field of the record survives a round trip, the broker's attempt
// marker included, and a writer that changes one field leaves the others
// as it read them.
func TestStateRoundTripKeepsEveryField(t *testing.T) {
	vaulttest.CleanEnv(t)
	v := vaulttest.New(t)
	store := connect(t, v)
	ctx := context.Background()

	empty, err := store.ReadState(ctx)
	if err != nil || *empty != (loginvault.State{}) {
		t.Fatalf("state of an empty path = %+v, %v", empty, err)
	}

	at := time.Date(2026, 10, 12, 13, 5, 9, 0, time.UTC)
	want := loginvault.State{
		Salt: "0123abcd", Blocked: true, BlockedReason: "consecutive_failures", BlockedAt: at,
		CredentialFingerprint: "ff00", UnconfirmedCount: 2,
		ServedRequest: "2026-10-12T13:05:09Z 5f2a9c0e11d24b7a", RequestLoginAt: at.Add(-time.Hour),
		AttemptStartedAt: at.Add(time.Minute),
	}
	if err := store.WriteState(ctx, &want); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadState(ctx)
	if err != nil || *got != want {
		t.Fatalf("round trip:\n got %+v\nwant %+v (%v)", got, want, err)
	}

	// Every value is a string, so the record reads the same through
	// `vault kv get`; an operator clearing the marker sees "".
	got.AttemptStartedAt = time.Time{}
	if err := store.WriteState(ctx, got); err != nil {
		t.Fatal(err)
	}
	raw, _ := v.Secret(statePath)
	if raw["attempt_started_at"] != "" || raw["login_blocked"] != "true" || raw["unconfirmed_count"] != "2" ||
		raw["served_request"] != want.ServedRequest {
		t.Errorf("stored record = %v", raw)
	}
}

// ReadFields returns whatever fields a site's driver stored, not only a
// "session" document: a second site's session is several fields.
func TestReadFieldsReturnsEveryFieldAndTheLoginTime(t *testing.T) {
	vaulttest.CleanEnv(t)
	v := vaulttest.New(t)
	store := connect(t, v)
	ctx := context.Background()

	fields, at, version, err := store.ReadFields(ctx)
	if err != nil || fields != nil || !at.IsZero() || version != 0 {
		t.Fatalf("empty path: %v %v %d %v", fields, at, version, err)
	}

	if _, err := store.Write(ctx, map[string]any{"sess_id": "a", "csrf_token": "b", "logged_in_at": "2026-10-12T13:00:00Z"}, 0); err != nil {
		t.Fatal(err)
	}
	fields, at, version, err = store.ReadFields(ctx)
	if err != nil || fields["sess_id"] != "a" || fields["csrf_token"] != "b" || version != 1 ||
		!at.Equal(time.Date(2026, 10, 12, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("fields %v at %v version %d err %v", fields, at, version, err)
	}
	// Such a secret has no "session" document: Read says "none" and the
	// scheduled PowerSchool job would log in rather than misread it.
	if stored, _, err := store.Read(ctx); err != nil || stored != nil {
		t.Errorf("Read of a secret without a session field = %v, %v", stored, err)
	}

	// Without logged_in_at, the version's creation time stands in.
	v.Put(sessionPath, map[string]any{"session": "{}"})
	_, at, version, err = store.ReadFields(ctx)
	if err != nil || version != 2 || time.Since(at) > time.Minute || at.IsZero() {
		t.Errorf("at %v version %d err %v", at, version, err)
	}

	store.Close()
	if store.Token() != "" {
		t.Error("the token was kept after Close")
	}
	if _, revokes, roles := v.Counts(); revokes != 1 || len(roles) != 1 || roles[0] != "glovebox-login-broker-powerschool" {
		t.Errorf("revokes %d, roles %v", revokes, roles)
	}
}

func TestCredentialFingerprint(t *testing.T) {
	a := loginvault.CredentialFingerprint("salt", "ab", "c")
	if a == loginvault.CredentialFingerprint("salt", "a", "bc") {
		t.Error(`("ab","c") and ("a","bc") have the same fingerprint`)
	}
	if a == loginvault.CredentialFingerprint("other", "ab", "c") || len(a) != 64 {
		t.Errorf("fingerprint %q", a)
	}
}
