package main

import (
	"context"

	"github.com/leftathome/glovebox/internal/loginvault"
)

// The Vault side lives in internal/loginvault, shared with the PowerSchool
// login workloads. This binary uses two paths of its own account:
//
//   - the session path, which it may write but not read (Put, no
//     check-and-set): the connector receives it through ESO;
//   - the state path beside it, which it reads and writes: the login latch
//     and the count of unconfirmed logins. The connector never reads it.

// loginState is the record this binary keeps next to the session. It is
// what stops a run from submitting credentials whose last login was not
// confirmed.
type loginState = loginvault.State

// newVaultStore authenticates to Vault with the pod's projected
// ServiceAccount token (no static token) and returns the store.
func newVaultStore(ctx context.Context, cfg config) (sessionStore, error) {
	return loginvault.Connect(ctx, loginvault.Config{
		Addr:        cfg.VaultAddr,
		CACert:      cfg.VaultCACert,
		K8sRole:     cfg.VaultK8sRole,
		KVMount:     cfg.VaultKVMount,
		SessionPath: cfg.VaultSessionPath,
		StatePath:   cfg.VaultStatePath,
	})
}

// isCASMismatch reports whether a write was refused because the path was
// no longer at the expected version.
func isCASMismatch(err error) bool { return loginvault.IsCASMismatch(err) }

// isClientError reports whether a Vault error is a 4xx (the operator must
// fix a policy or a path) as opposed to a 5xx or a network failure.
func isClientError(err error) bool { return loginvault.IsClientError(err) }
