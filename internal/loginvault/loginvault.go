// Package loginvault is the Vault side of a login workload: two KV v2 paths
// on a client authenticated through the Kubernetes auth method.
//
//   - the session path holds the session a login produced. It is what ESO
//     copies to the connector, so it holds nothing else;
//   - the state path, beside it, holds the workload's own record: the latch
//     that stops further logins, the count of failed ones, when a login was
//     last made. The connector never reads it.
//
// Two workloads use it for the same account and therefore share one record:
// the scheduled PowerSchool login job (connectors/powerschool-auth-refresher)
// and the login broker (internal/loginbroker). A latch set by either stops
// both.
//
// Nothing here logs, and no error it returns carries a secret's value.
package loginvault

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	vaultk8s "github.com/hashicorp/vault/api/auth/kubernetes"
)

// Config says where Vault is and which two paths to use.
type Config struct {
	// Addr must be an https URL; the callers refuse anything else at config
	// time.
	Addr string
	// CACert is a file holding the CA bundle that signed Vault's
	// certificate. Empty means the system roots.
	CACert string
	// K8sRole is the Kubernetes auth role bound to the pod's ServiceAccount.
	K8sRole string
	// KVMount is the KV v2 mount, e.g. "secret".
	KVMount string
	// SessionPath and StatePath are KV v2 paths inside the mount. They must
	// differ.
	SessionPath string
	StatePath   string
	// K8sTokenPath overrides where the ServiceAccount token is read from.
	K8sTokenPath string
}

// StoredSession is the session currently in Vault, for a session that is
// one JSON document in the field "session" (the PowerSchool form).
type StoredSession struct {
	// Document is the session JSON, exactly what the connector is given.
	Document string
	// LoggedInAt is when that session was created: the logged_in_at field
	// the login workload writes, else the creation time of the KV version.
	// Zero when neither is known.
	LoggedInAt time.Time
}

// State is the record a login workload keeps next to the session. It is
// what stops a login from submitting credentials the site has already
// refused, and it is the only place such facts survive a pod.
type State struct {
	// Salt is random, generated on first use and kept. It makes the
	// fingerprint below useless for testing guesses of the password
	// against it without also holding this record.
	Salt string
	// Blocked is the latch. While it is set and the configured credentials
	// still have the fingerprint below, nothing logs in.
	Blocked       bool
	BlockedReason string
	BlockedAt     time.Time
	// CredentialFingerprint is SHA-256 over the salt, the username and the
	// password last submitted (or about to be). Never the values.
	CredentialFingerprint string
	// UnconfirmedCount is the number of logins in a row that did not end
	// with a session the site accepts. Reset by a confirmed login.
	UnconfirmedCount int
	// ServedRequest is the last ConfigMap login request (the connector's,
	// verbatim) the scheduled login job acted on. The broker does not use
	// it.
	ServedRequest string
	// RequestLoginAt is when a login was last made because somebody asked
	// for one. The minimum interval between such logins is measured from
	// it.
	RequestLoginAt time.Time
	// AttemptStartedAt is set by the broker immediately before a login and
	// cleared when the login's outcome has been recorded. Found set at the
	// start of a later request, it means a process died in the middle of a
	// login whose outcome nobody knows.
	AttemptStartedAt time.Time
}

// Store is the two paths on an authenticated client.
type Store struct {
	client    *vaultapi.Client
	mount     string
	path      string
	statePath string
}

// Connect authenticates to Vault with the pod's projected ServiceAccount
// token (no static token) and returns the store.
//
// Server certificate verification is always on. The CA that signed Vault's
// certificate comes from cfg.CACert or, when that is empty, the system
// roots. Vault's own VAULT_SKIP_VERIFY switch is forced off here.
func Connect(ctx context.Context, cfg Config) (*Store, error) {
	vc := vaultapi.DefaultConfig()
	if vc.Error != nil {
		return nil, fmt.Errorf("vault client defaults: %w", vc.Error)
	}
	vc.Address = cfg.Addr
	if err := vc.ConfigureTLS(&vaultapi.TLSConfig{CACert: cfg.CACert}); err != nil {
		return nil, fmt.Errorf("vault CA bundle: %w", err)
	}
	transport, ok := vc.HttpClient.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		return nil, errors.New("vault client has no TLS configuration to verify with")
	}
	transport.TLSClientConfig.InsecureSkipVerify = false

	client, err := vaultapi.NewClient(vc)
	if err != nil {
		return nil, fmt.Errorf("build vault client: %w", err)
	}
	// A token inherited from the environment would bypass the role.
	client.ClearToken()

	var opts []vaultk8s.LoginOption
	if cfg.K8sTokenPath != "" {
		opts = append(opts, vaultk8s.WithServiceAccountTokenPath(cfg.K8sTokenPath))
	}
	k8sAuth, err := vaultk8s.NewKubernetesAuth(cfg.K8sRole, opts...)
	if err != nil {
		return nil, fmt.Errorf("build k8s auth: %w", err)
	}
	if _, err := client.Auth().Login(ctx, k8sAuth); err != nil {
		return nil, fmt.Errorf("vault k8s login: %w", err)
	}
	return &Store{
		client:    client,
		mount:     cfg.KVMount,
		path:      cfg.SessionPath,
		statePath: cfg.StatePath,
	}, nil
}

// Token returns the client's current Vault token ("" after Close). Tests
// use it to prove the token is gone.
func (v *Store) Token() string { return v.client.Token() }

// get reads one KV v2 path. A path that does not exist returns nil data
// and version 0; a deleted latest version returns nil data and its version.
func (v *Store) get(ctx context.Context, path string) (map[string]any, *vaultapi.KVVersionMetadata, error) {
	secret, err := v.client.KVv2(v.mount).Get(ctx, path)
	if err != nil {
		if errors.Is(err, vaultapi.ErrSecretNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if secret == nil {
		return nil, nil, nil
	}
	return secret.Data, secret.VersionMetadata, nil
}

// loggedInAtField is the field every login workload writes beside the
// session's own fields.
const loggedInAtField = "logged_in_at"

// ReadFields returns every field of the session secret (nil when the path
// does not exist or its latest version is deleted), when that session was
// created (the logged_in_at field, else the creation time of the KV
// version; zero when neither is known) and the current KV version of the
// path (0 when it does not exist).
func (v *Store) ReadFields(ctx context.Context) (fields map[string]any, loggedInAt time.Time, version int, err error) {
	data, meta, err := v.get(ctx, v.path)
	if err != nil {
		return nil, time.Time{}, 0, err
	}
	if meta != nil {
		version = meta.Version
	}
	if raw, ok := data[loggedInAtField].(string); ok {
		if t, perr := time.Parse(time.RFC3339, raw); perr == nil {
			loggedInAt = t
		}
	}
	if loggedInAt.IsZero() && meta != nil && data != nil {
		loggedInAt = meta.CreatedTime
	}
	return data, loggedInAt, version, nil
}

// Read is ReadFields for a session kept as one document in the field
// "session". A path that does not exist, a deleted latest version and a
// secret without a session field all read as "none".
func (v *Store) Read(ctx context.Context) (*StoredSession, int, error) {
	data, loggedInAt, version, err := v.ReadFields(ctx)
	if err != nil {
		return nil, 0, err
	}
	doc, _ := data["session"].(string)
	if strings.TrimSpace(doc) == "" {
		return nil, version, nil
	}
	return &StoredSession{Document: doc, LoggedInAt: loggedInAt}, version, nil
}

// CanWrite asks Vault what this token may do on the session's data path.
// KV v2 needs create for a path's first version and update for every later
// one, so both are required.
func (v *Store) CanWrite(ctx context.Context) (bool, error) {
	caps, err := v.client.Sys().CapabilitiesSelfWithContext(ctx, v.mount+"/data/"+v.path)
	if err != nil {
		return false, err
	}
	var create, update bool
	for _, c := range caps {
		switch c {
		case "root":
			return true, nil
		case "create":
			create = true
		case "update":
			update = true
		}
	}
	return create && update, nil
}

// Write stores the session. KVv2.Put adds the mount and the "/data/"
// segment itself. The write is check-and-set: Vault refuses it unless the
// path is still at version cas (0 = must not exist yet).
func (v *Store) Write(ctx context.Context, data map[string]any, cas int) (int, error) {
	secret, err := v.client.KVv2(v.mount).Put(ctx, v.path, data, vaultapi.WithCheckAndSet(cas))
	if err != nil {
		return 0, err
	}
	if secret != nil && secret.VersionMetadata != nil {
		return secret.VersionMetadata.Version, nil
	}
	return 0, nil
}

// Field names of the state record. All values are strings, so the record
// reads the same through `vault kv get` as through the API.
const (
	stateSalt        = "salt"
	stateBlocked     = "login_blocked"
	stateReason      = "blocked_reason"
	stateBlockedAt   = "blocked_at"
	stateFingerprint = "credential_fingerprint"
	stateUnconfirmed = "unconfirmed_count"
	stateServed      = "served_request"
	stateRequestAt   = "request_login_at"
	stateAttemptAt   = "attempt_started_at"
)

// ReadState returns the state; a zero state when none is stored.
func (v *Store) ReadState(ctx context.Context) (*State, error) {
	data, _, err := v.get(ctx, v.statePath)
	if err != nil {
		return nil, err
	}
	state := &State{}
	str := func(k string) string { s, _ := data[k].(string); return s }
	state.Salt = str(stateSalt)
	state.Blocked = str(stateBlocked) == "true"
	state.BlockedReason = str(stateReason)
	state.CredentialFingerprint = str(stateFingerprint)
	if t, err := time.Parse(time.RFC3339, str(stateBlockedAt)); err == nil {
		state.BlockedAt = t
	}
	state.ServedRequest = str(stateServed)
	if t, err := time.Parse(time.RFC3339, str(stateRequestAt)); err == nil {
		state.RequestLoginAt = t
	}
	if t, err := time.Parse(time.RFC3339, str(stateAttemptAt)); err == nil {
		state.AttemptStartedAt = t
	}
	if n, err := strconv.Atoi(str(stateUnconfirmed)); err == nil && n > 0 {
		state.UnconfirmedCount = n
	}
	return state, nil
}

// WriteState stores the state, every field of it: a writer that knows
// nothing of a field another workload set still writes it back unchanged,
// because it read it first.
func (v *Store) WriteState(ctx context.Context, state *State) error {
	stamp := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339)
	}
	data := map[string]any{
		stateSalt:        state.Salt,
		stateBlocked:     strconv.FormatBool(state.Blocked),
		stateReason:      state.BlockedReason,
		stateBlockedAt:   stamp(state.BlockedAt),
		stateFingerprint: state.CredentialFingerprint,
		stateUnconfirmed: strconv.Itoa(state.UnconfirmedCount),
		stateServed:      state.ServedRequest,
		stateRequestAt:   stamp(state.RequestLoginAt),
		stateAttemptAt:   stamp(state.AttemptStartedAt),
	}
	_, err := v.client.KVv2(v.mount).Put(ctx, v.statePath, data)
	return err
}

// Close revokes the Vault token, so that nothing which copied it during the
// work (a browser runs as the same user and could read it from memory or
// from the ServiceAccount token it was minted from) can use it afterwards.
// Best effort, on its own short deadline: the caller's context may be over.
func (v *Store) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = v.client.Auth().Token().RevokeSelfWithContext(ctx, "")
	v.client.ClearToken()
}

// CredentialFingerprint identifies a username/password pair without
// revealing either: SHA-256 over the salt and both values, NUL-separated so
// ("ab","c") and ("a","bc") differ. It is what State.CredentialFingerprint
// holds; every workload sharing a State must compute it this way.
func CredentialFingerprint(salt, username, password string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + username + "\x00" + password))
	return hex.EncodeToString(sum[:])
}

// IsCASMismatch reports whether a write was refused because the path was
// no longer at the expected version.
func IsCASMismatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), "check-and-set")
}

// IsClientError reports whether a Vault error is a 4xx (the operator must
// fix a policy or a path) as opposed to a 5xx or a network failure. Same
// rule as schoology-auth-refresher: the structured status when there is
// one, else keyword markers -- never bare numbers, which also turn up in
// request ids and timestamps inside unrelated messages.
func IsClientError(err error) bool {
	if err == nil {
		return false
	}
	var re *vaultapi.ResponseError
	if errors.As(err, &re) {
		return re.StatusCode >= 400 && re.StatusCode < 500
	}
	msg := err.Error()
	for _, marker := range []string{"permission denied", "Forbidden", "Bad Request", "not found"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
