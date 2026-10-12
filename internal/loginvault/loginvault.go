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
	// AttemptStartedAt is set by every login workload immediately before a
	// login and cleared when the login's outcome has been recorded. While it
	// is fresh (AttemptInProgress) another workload must not log in: a login
	// is in flight. Found stale, it means a process died in the middle of a
	// login whose outcome nobody knows.
	AttemptStartedAt time.Time
	// CooldownUntil: no login before this time. Set after a login that
	// failed before the credentials were submitted.
	CooldownUntil time.Time
	// LoginDay ("2006-01-02", on the account's local calendar) and
	// LoginDayCount: how many logins that may have submitted the
	// credentials were made that day, by any workload sharing this record.
	LoginDay      string
	LoginDayCount int

	// Version is the KV version this State was read at (0: the path did
	// not exist). It is not stored. WriteState is check-and-set against it
	// and advances it, so two workloads can never both write on top of the
	// same read: the second is refused and must read again.
	Version int
}

// SameRecord reports whether s and o would be stored as the same record:
// every stored field equal, times compared to the second (the precision
// they are stored at), Version aside.
//
// It is how a writer recognises its own write. Vault can apply a write and
// then fail to say so (a 5xx, a dropped connection). The retry is then
// refused as a check-and-set mismatch -- against the writer's own earlier
// write. Read back, a record that is exactly what the writer was trying to
// store was stored by it, once; treating that as somebody else's change
// would apply an outcome twice, or leave a marker nobody clears.
func (s *State) SameRecord(o *State) bool {
	if s == nil || o == nil {
		return false
	}
	same := func(a, b time.Time) bool { return a.Unix() == b.Unix() || (a.IsZero() && b.IsZero()) }
	return s.Salt == o.Salt &&
		s.Blocked == o.Blocked && s.BlockedReason == o.BlockedReason && same(s.BlockedAt, o.BlockedAt) &&
		s.CredentialFingerprint == o.CredentialFingerprint &&
		s.UnconfirmedCount == o.UnconfirmedCount &&
		s.ServedRequest == o.ServedRequest && same(s.RequestLoginAt, o.RequestLoginAt) &&
		same(s.AttemptStartedAt, o.AttemptStartedAt) && same(s.CooldownUntil, o.CooldownUntil) &&
		s.LoginDay == o.LoginDay && s.LoginDayCount == o.LoginDayCount
}

// StateStore is the part of a store WriteStateOnce needs.
type StateStore interface {
	ReadState(ctx context.Context) (*State, error)
	WriteState(ctx context.Context, state *State) error
}

// WriteStateOnce stores state so that it is applied exactly once, whatever
// Vault answers. A transient failure (5xx, network) is retried up to
// attempts times, pause apart. If a retry is then refused as a
// check-and-set mismatch, the record is read back: when it is exactly what
// was being written (SameRecord), the earlier attempt was applied after
// all, and this is success -- state.Version is set to the stored version.
// Any other mismatch, and any other refusal (4xx), is returned at once:
// somebody else wrote, and the caller must decide again.
func WriteStateOnce(ctx context.Context, store StateStore, state *State, attempts int, pause func(context.Context) error) error {
	var err error
	uncertain := false // an earlier attempt failed in a way that may have applied it
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = store.WriteState(ctx, state); err == nil {
			return nil
		}
		if IsCASMismatch(err) {
			if uncertain {
				if stored, rerr := store.ReadState(ctx); rerr == nil && stored.SameRecord(state) {
					state.Version = stored.Version
					return nil
				}
			}
			return err
		}
		if IsClientError(err) {
			return err
		}
		uncertain = true
		if attempt < attempts {
			if perr := pause(ctx); perr != nil {
				break
			}
		}
	}
	return err
}

// AttemptFreshFor is how long an attempt marker means "a login is in
// flight". It is longer than any workload lets a login run (the broker
// bounds a refresh at 6 minutes, the scheduled job a run at 8).
const AttemptFreshFor = 10 * time.Minute

// AttemptInProgress reports whether the marker says a login is in flight
// at now. A marker dated further in the future than AttemptFreshFor is a
// broken clock, not a login, and is not "in progress".
func (s *State) AttemptInProgress(now time.Time) bool {
	if s.AttemptStartedAt.IsZero() {
		return false
	}
	d := now.Sub(s.AttemptStartedAt)
	return d < AttemptFreshFor && d > -AttemptFreshFor
}

// DayOf names the local calendar day of t in loc, the form of LoginDay.
func DayOf(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("2006-01-02")
}

// LoginsOn returns how many logins the record counts for day.
func (s *State) LoginsOn(day string) int {
	if s.LoginDay != day {
		return 0
	}
	return s.LoginDayCount
}

// CountLogin adds one login to day's count, starting a new count when the
// day has changed.
func (s *State) CountLogin(day string) {
	if s.LoginDay != day {
		s.LoginDay, s.LoginDayCount = day, 0
	}
	s.LoginDayCount++
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
	stateCooldown    = "cooldown_until"
	stateLoginDay    = "login_day"
	stateLoginCount  = "login_day_count"
)

// ReadState returns the state; a zero state when none is stored.
func (v *Store) ReadState(ctx context.Context) (*State, error) {
	data, meta, err := v.get(ctx, v.statePath)
	if err != nil {
		return nil, err
	}
	state := &State{}
	if meta != nil {
		state.Version = meta.Version
	}
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
	if t, err := time.Parse(time.RFC3339, str(stateCooldown)); err == nil {
		state.CooldownUntil = t
	}
	state.LoginDay = str(stateLoginDay)
	if n, err := strconv.Atoi(str(stateLoginCount)); err == nil && n > 0 {
		state.LoginDayCount = n
	}
	return state, nil
}

// WriteState stores the state, every field of it: a writer that knows
// nothing of a field another workload set still writes it back unchanged,
// because it read it first.
//
// The write is check-and-set against state.Version, the version the state
// was read at. If another workload has written since, Vault refuses it
// (IsCASMismatch) and nothing is stored: the caller must read again and
// decide again, and in particular must not log in on what it read before.
// On success state.Version is advanced, so the same State can be written
// again.
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
		stateCooldown:    stamp(state.CooldownUntil),
		stateLoginDay:    state.LoginDay,
		stateLoginCount:  strconv.Itoa(state.LoginDayCount),
	}
	secret, err := v.client.KVv2(v.mount).Put(ctx, v.statePath, data, vaultapi.WithCheckAndSet(state.Version))
	if err != nil {
		return err
	}
	if secret != nil && secret.VersionMetadata != nil {
		state.Version = secret.VersionMetadata.Version
	} else {
		state.Version++
	}
	return nil
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
