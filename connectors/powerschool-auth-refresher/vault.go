package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	vaultk8s "github.com/hashicorp/vault/api/auth/kubernetes"
)

// vaultStore is the production sessionStore: two KV v2 paths (the session
// and the login state) on a client authenticated through the Kubernetes
// auth method.
type vaultStore struct {
	client    *vaultapi.Client
	mount     string
	path      string
	statePath string
}

// newVaultStore authenticates to Vault with the pod's projected
// ServiceAccount token (no static token) and returns the store.
//
// Server certificate verification is always on. The CA that signed Vault's
// certificate comes from cfg.VaultCACert (the chart mounts a ConfigMap and
// sets VAULT_CACERT) or, when that is empty, the system roots. Vault's own
// VAULT_SKIP_VERIFY switch is refused at config time and forced off here.
func newVaultStore(ctx context.Context, cfg config) (*vaultStore, error) {
	vc := vaultapi.DefaultConfig()
	if vc.Error != nil {
		return nil, fmt.Errorf("vault client defaults: %w", vc.Error)
	}
	vc.Address = cfg.VaultAddr
	if err := vc.ConfigureTLS(&vaultapi.TLSConfig{CACert: cfg.VaultCACert}); err != nil {
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
	if cfg.VaultK8sTokenPath != "" {
		opts = append(opts, vaultk8s.WithServiceAccountTokenPath(cfg.VaultK8sTokenPath))
	}
	k8sAuth, err := vaultk8s.NewKubernetesAuth(cfg.VaultK8sRole, opts...)
	if err != nil {
		return nil, fmt.Errorf("build k8s auth: %w", err)
	}
	if _, err := client.Auth().Login(ctx, k8sAuth); err != nil {
		return nil, fmt.Errorf("vault k8s login: %w", err)
	}
	return &vaultStore{
		client:    client,
		mount:     cfg.VaultKVMount,
		path:      cfg.VaultSessionPath,
		statePath: cfg.VaultStatePath,
	}, nil
}

// get reads one KV v2 path. A path that does not exist returns nil data
// and version 0; a deleted latest version returns nil data and its version.
func (v *vaultStore) get(ctx context.Context, path string) (map[string]any, *vaultapi.KVVersionMetadata, error) {
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

// Read implements sessionStore. A path that does not exist, a deleted
// latest version and a secret without a session field all read as "none".
func (v *vaultStore) Read(ctx context.Context) (*storedSession, int, error) {
	data, meta, err := v.get(ctx, v.path)
	if err != nil {
		return nil, 0, err
	}
	version := 0
	if meta != nil {
		version = meta.Version
	}
	doc, _ := data["session"].(string)
	if strings.TrimSpace(doc) == "" {
		return nil, version, nil
	}
	stored := &storedSession{Document: doc}
	if raw, ok := data["logged_in_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			stored.LoggedInAt = t
		}
	}
	if stored.LoggedInAt.IsZero() && meta != nil {
		stored.LoggedInAt = meta.CreatedTime
	}
	return stored, version, nil
}

// CanWrite implements sessionStore by asking Vault what this token may do
// on the session's data path. KV v2 needs create for a path's first version
// and update for every later one, so both are required.
func (v *vaultStore) CanWrite(ctx context.Context) (bool, error) {
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

// Write implements sessionStore. KVv2.Put adds the mount and the "/data/"
// segment itself. The write is check-and-set: Vault refuses it unless the
// path is still at version cas (0 = must not exist yet).
func (v *vaultStore) Write(ctx context.Context, data map[string]any, cas int) (int, error) {
	secret, err := v.client.KVv2(v.mount).Put(ctx, v.path, data, vaultapi.WithCheckAndSet(cas))
	if err != nil {
		return 0, err
	}
	if secret != nil && secret.VersionMetadata != nil {
		return secret.VersionMetadata.Version, nil
	}
	return 0, nil
}

// Field names of the login state record. All values are strings, so the
// record reads the same through `vault kv get` as through the API.
const (
	stateSalt        = "salt"
	stateBlocked     = "login_blocked"
	stateReason      = "blocked_reason"
	stateBlockedAt   = "blocked_at"
	stateFingerprint = "credential_fingerprint"
	stateUnconfirmed = "unconfirmed_count"
	stateServed      = "served_request"
	stateRequestAt   = "request_login_at"
)

// ReadState implements sessionStore.
func (v *vaultStore) ReadState(ctx context.Context) (*loginState, error) {
	data, _, err := v.get(ctx, v.statePath)
	if err != nil {
		return nil, err
	}
	state := &loginState{}
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
	if n, err := strconv.Atoi(str(stateUnconfirmed)); err == nil && n > 0 {
		state.UnconfirmedCount = n
	}
	return state, nil
}

// WriteState implements sessionStore.
func (v *vaultStore) WriteState(ctx context.Context, state *loginState) error {
	data := map[string]any{
		stateSalt:        state.Salt,
		stateBlocked:     strconv.FormatBool(state.Blocked),
		stateReason:      state.BlockedReason,
		stateBlockedAt:   "",
		stateFingerprint: state.CredentialFingerprint,
		stateUnconfirmed: strconv.Itoa(state.UnconfirmedCount),
		stateServed:      state.ServedRequest,
		stateRequestAt:   "",
	}
	if !state.RequestLoginAt.IsZero() {
		data[stateRequestAt] = state.RequestLoginAt.UTC().Format(time.RFC3339)
	}
	if !state.BlockedAt.IsZero() {
		data[stateBlockedAt] = state.BlockedAt.UTC().Format(time.RFC3339)
	}
	_, err := v.client.KVv2(v.mount).Put(ctx, v.statePath, data)
	return err
}

// Close revokes the Vault token, so that nothing which copied it during the
// run (the browser runs as the same user and could read it from memory or
// from the ServiceAccount token it was minted from) can use it afterwards.
// Best effort, on its own short deadline: the run's context may be over.
func (v *vaultStore) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = v.client.Auth().Token().RevokeSelfWithContext(ctx, "")
	v.client.ClearToken()
}

// isCASMismatch reports whether a write was refused because the path was
// no longer at the expected version.
func isCASMismatch(err error) bool {
	return err != nil && strings.Contains(err.Error(), "check-and-set")
}

// isClientError reports whether a Vault error is a 4xx (the operator must
// fix a policy or a path) as opposed to a 5xx or a network failure. Same
// rule as schoology-auth-refresher: the structured status when there is
// one, else keyword markers -- never bare numbers, which also turn up in
// request ids and timestamps inside unrelated messages.
func isClientError(err error) bool {
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
