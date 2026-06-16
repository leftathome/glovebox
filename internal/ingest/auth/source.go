package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// TokenSource abstracts where ingest bearer tokens come from. It is the
// provider-agnostic seam consumed by TokenStore.Reload: List enumerates
// the provisioned source-ids and Read returns one source-id's raw
// (unvalidated) hex token. All validation, duplicate detection, and the
// atomic store swap live in Reload, so a source only has to fetch.
//
// The production source is VaultTokenSource. Dev/single-node
// deployments use EnvTokenSource or FileTokenSource so the auth +
// archive-delivery path is exercisable without a Vault-backed cluster
// (see glovebox-4ypk). Those sources are opt-in via ingest.auth.source
// and must never be the default.
type TokenSource interface {
	// List returns the set of provisioned source-ids. A non-nil error
	// aborts the whole reload and leaves the previous token set intact
	// (matching spec 10 section 4.1); an empty slice with nil error
	// means "no tokens provisioned".
	List(ctx context.Context) ([]string, error)
	// Read returns the raw, unvalidated hex token string for one
	// source-id. A non-nil error drops only that entry (logged and
	// counted via ReloadConfig.OnError); it does not fail the reload.
	Read(ctx context.Context, sourceID string) (string, error)
}

// VaultTokenSource is the production TokenSource. It adapts the
// low-level VaultClient (Vault KV v2 list/read) to the provider-agnostic
// TokenSource contract, keeping the KV-v2 specifics (mount, base path,
// and the data["token"] field) out of TokenStore.Reload.
type VaultTokenSource struct {
	client   VaultClient
	kvMount  string
	basePath string
}

// NewVaultTokenSource wraps an authenticated VaultClient. kvMount is the
// KV v2 mount (e.g. "secret") and basePath is the tokens prefix (e.g.
// "glovebox/ingest-tokens").
func NewVaultTokenSource(client VaultClient, kvMount, basePath string) *VaultTokenSource {
	return &VaultTokenSource{client: client, kvMount: kvMount, basePath: basePath}
}

func (v *VaultTokenSource) List(ctx context.Context) ([]string, error) {
	return v.client.ListPath(ctx, v.kvMount, v.basePath)
}

func (v *VaultTokenSource) Read(ctx context.Context, sourceID string) (string, error) {
	data, err := v.client.ReadKV(ctx, v.kvMount, v.basePath+"/"+sourceID)
	if err != nil {
		return "", err
	}
	tok, _ := data["token"].(string)
	return tok, nil
}

// EnvTokenSource serves a single source-id/token pair held in memory,
// typically populated from environment variables. DEV/SINGLE-NODE ONLY:
// it carries exactly one static token and never reloads. Use Vault in
// production.
type EnvTokenSource struct {
	sourceID string
	token    string
}

// NewEnvTokenSource returns an EnvTokenSource for the given pair. Either
// value being empty yields a source with no entries (List returns an
// empty slice), so an unconfigured env source is a clean no-op rather
// than an error.
func NewEnvTokenSource(sourceID, token string) *EnvTokenSource {
	return &EnvTokenSource{sourceID: sourceID, token: token}
}

func (e *EnvTokenSource) List(context.Context) ([]string, error) {
	if e.sourceID == "" || e.token == "" {
		return nil, nil
	}
	return []string{e.sourceID}, nil
}

func (e *EnvTokenSource) Read(_ context.Context, sourceID string) (string, error) {
	if sourceID != e.sourceID || e.sourceID == "" {
		return "", fmt.Errorf("env token source: unknown source-id %q", sourceID)
	}
	return e.token, nil
}

// FileTokenSource serves source-id -> token pairs from a JSON object on
// disk (e.g. {"recognizer":"<64-hex>"}). The file is re-read on every
// List/Read call so the reload loop picks up edits without a restart,
// giving the same hot-reload behavior as the Vault source. DEV/
// SINGLE-NODE ONLY: the file holds plaintext tokens. Use Vault in
// production.
type FileTokenSource struct {
	path string
}

// NewFileTokenSource returns a FileTokenSource backed by the JSON file
// at path.
func NewFileTokenSource(path string) *FileTokenSource {
	return &FileTokenSource{path: path}
}

// load reads and parses the JSON token map. A read or parse failure is
// returned to the caller; List propagates it (aborting the reload),
// which is the correct fail-safe for a misconfigured dev source.
func (f *FileTokenSource) load() (map[string]string, error) {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return nil, fmt.Errorf("file token source: read %s: %w", f.path, err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("file token source: parse %s: %w", f.path, err)
	}
	return m, nil
}

func (f *FileTokenSource) List(context.Context) ([]string, error) {
	m, err := f.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (f *FileTokenSource) Read(_ context.Context, sourceID string) (string, error) {
	m, err := f.load()
	if err != nil {
		return "", err
	}
	tok, ok := m[sourceID]
	if !ok {
		return "", fmt.Errorf("file token source: unknown source-id %q", sourceID)
	}
	return tok, nil
}
