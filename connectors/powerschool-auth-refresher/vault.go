package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	vaultk8s "github.com/hashicorp/vault/api/auth/kubernetes"
)

// vaultStore is the production sessionStore: one KV v2 path on a client
// authenticated through the Kubernetes auth method.
type vaultStore struct {
	client *vaultapi.Client
	mount  string
	path   string
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
	return &vaultStore{client: client, mount: cfg.VaultKVMount, path: cfg.VaultSessionPath}, nil
}

// Read implements sessionStore. A path that does not exist, a deleted
// latest version and a secret without a session field all read as "none".
func (v *vaultStore) Read(ctx context.Context) (*storedSession, error) {
	secret, err := v.client.KVv2(v.mount).Get(ctx, v.path)
	if err != nil {
		if errors.Is(err, vaultapi.ErrSecretNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if secret == nil || secret.Data == nil {
		return nil, nil
	}
	doc, _ := secret.Data["session"].(string)
	if strings.TrimSpace(doc) == "" {
		return nil, nil
	}
	stored := &storedSession{Document: doc}
	if raw, ok := secret.Data["logged_in_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			stored.LoggedInAt = t
		}
	}
	if stored.LoggedInAt.IsZero() && secret.VersionMetadata != nil {
		stored.LoggedInAt = secret.VersionMetadata.CreatedTime
	}
	return stored, nil
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
// segment itself.
func (v *vaultStore) Write(ctx context.Context, data map[string]any) (int, error) {
	secret, err := v.client.KVv2(v.mount).Put(ctx, v.path, data)
	if err != nil {
		return 0, err
	}
	if secret != nil && secret.VersionMetadata != nil {
		return secret.VersionMetadata.Version, nil
	}
	return 0, nil
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
