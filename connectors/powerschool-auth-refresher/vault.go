package main

import (
	"context"

	"github.com/leftathome/glovebox/internal/loginvault"
)

// The Vault side lives in internal/loginvault, which the login broker uses
// for the same two paths: the session, and the login state beside it. The
// names below are this binary's own for it.

// storedSession is the session currently in Vault.
type storedSession = loginvault.StoredSession

// loginState is the record this binary keeps next to the session, at a
// sibling KV path the connector never reads. It is what stops a run from
// submitting credentials the portal has already refused. The login broker
// reads and writes the same record: a latch set by either stops both.
type loginState = loginvault.State

// newVaultStore authenticates to Vault with the pod's projected
// ServiceAccount token (no static token) and returns the store.
func newVaultStore(ctx context.Context, cfg config) (*loginvault.Store, error) {
	return loginvault.Connect(ctx, loginvault.Config{
		Addr:         cfg.VaultAddr,
		CACert:       cfg.VaultCACert,
		K8sRole:      cfg.VaultK8sRole,
		KVMount:      cfg.VaultKVMount,
		SessionPath:  cfg.VaultSessionPath,
		StatePath:    cfg.VaultStatePath,
		K8sTokenPath: cfg.VaultK8sTokenPath,
	})
}

// isCASMismatch reports whether a write was refused because the path was
// no longer at the expected version.
func isCASMismatch(err error) bool { return loginvault.IsCASMismatch(err) }

// isClientError reports whether a Vault error is a 4xx (the operator must
// fix a policy or a path) as opposed to a 5xx or a network failure.
func isClientError(err error) bool { return loginvault.IsClientError(err) }
