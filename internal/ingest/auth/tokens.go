// Package auth provides bearer-token authentication for the ingest
// archive-delivery endpoint per spec 10. The TokenStore type holds an
// in-memory set of source-id -> token mappings loaded from Vault KV v2
// and serves constant-time-friendly lookups per spec 10 section 4.3.
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"sync/atomic"

	vaultapi "github.com/hashicorp/vault/api"
	vaultk8s "github.com/hashicorp/vault/api/auth/kubernetes"
)

// tokenEntry holds one source-id's token bytes. Bytes are stored as a
// fixed-size array (not a slice) so a constant-time compare against a
// fixed-size request token has no allocation surprises.
type tokenEntry struct {
	token    [32]byte
	sourceID string
}

// TokenStore holds the validated set of tokens. Lookups are
// constant-time linear scans per spec 10 section 4.3. The atomic
// pointer is the sole synchronization primitive; lookups read without
// locking and Reload swaps the pointer atomically with a freshly
// allocated slice. No mutex is needed.
type TokenStore struct {
	entries atomic.Pointer[[]tokenEntry] // atomic swap on reload
	loadErr atomic.Pointer[error]        // most recent reload error (nil = healthy)
}

// NewTokenStore returns an empty store. Call Reload before serving.
func NewTokenStore() *TokenStore {
	s := &TokenStore{}
	empty := []tokenEntry{}
	s.entries.Store(&empty)
	return s
}

// LoadErr returns the most recent Reload error (nil = healthy). C3's
// startup path uses this to detect "first load failed, no cache"
// without needing to capture the Reload error at boot. Safe to call
// concurrently with Reload.
func (s *TokenStore) LoadErr() error {
	p := s.loadErr.Load()
	if p == nil {
		return nil
	}
	return *p
}

// sourceIDRe matches the spec 10 section 3.2 format:
//   - lowercase letter start
//   - lowercase alphanumeric middle
//   - single dashes only (no leading/trailing/consecutive)
//   - max 64 chars
var sourceIDRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// validSourceID reports whether s satisfies spec 10 section 3.2.
func validSourceID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	return sourceIDRe.MatchString(s)
}

// decodeToken validates a 64-char lowercase hex string and returns its
// 32 bytes. Returns ok=false on any format violation; the caller
// surfaces 401 without distinguishing the failure mode. The character
// validation short-circuits on length and case (format checks, not
// timing-sensitive against token bytes); see spec 10 section 4.4.
func decodeToken(s string) ([32]byte, bool) {
	var out [32]byte
	if len(s) != 64 {
		return out, false
	}
	// Reject any uppercase or non-hex character before decoding;
	// hex.Decode would otherwise accept mixed case.
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return out, false
		}
	}
	if _, err := hex.Decode(out[:], []byte(s)); err != nil {
		return out, false
	}
	return out, true
}

// Lookup returns the source-id for a token. Returns "", false if no
// match. The iteration is constant-time-friendly: every entry is
// compared, no early exit. The branch on the source-id copy fires at
// most once per request because tokens are required globally unique by
// the Reload duplicate-detection pass (spec 10 section 4.1 step 7).
func (s *TokenStore) Lookup(req [32]byte) (string, bool) {
	entries := *s.entries.Load()
	var (
		matched  uint32
		sourceID string
	)
	for _, e := range entries {
		eq := uint32(subtle.ConstantTimeCompare(e.token[:], req[:]))
		prev := matched
		matched |= eq
		// Copy source-id iff this entry matched AND no earlier entry
		// did. Tokens are globally unique (load-time check), so the
		// residual branch fires at most once per request.
		if eq == 1 && prev == 0 {
			sourceID = e.sourceID
		}
	}
	return sourceID, matched == 1
}

// swap atomically replaces the entries slice. Called by Reload after
// validating and assembling the new entry set.
func (s *TokenStore) swap(entries []tokenEntry) {
	s.entries.Store(&entries)
}

// VaultClient is the test seam for the parts of vaultapi.Client that
// Reload uses. Production wires this to a wrapped *vaultapi.Client via
// NewProductionVaultClient; tests pass an in-memory fake.
type VaultClient interface {
	// ListPath returns the keys under kvMount/metadata/path. Empty
	// list with nil error means no entries; errors are propagated
	// unchanged.
	ListPath(ctx context.Context, kvMount, path string) ([]string, error)
	// ReadKV returns the raw KV v2 data map for kvMount/data/path.
	ReadKV(ctx context.Context, kvMount, path string) (map[string]any, error)
}

// ReloadConfig controls a single Reload invocation.
type ReloadConfig struct {
	// Source supplies the current token set. The production source is
	// VaultTokenSource; dev/single-node deployments use EnvTokenSource
	// or FileTokenSource (see glovebox-4ypk). Vault KV-v2 specifics
	// (mount, base path, the data["token"] field) live inside the
	// source, not here.
	Source TokenSource
	Logger *slog.Logger
	// OnError, if non-nil, is invoked once per per-entry error
	// (malformed source-id, vault read failure, malformed token,
	// duplicate-token collision). The caller wires this to a metric
	// counter such as glovebox_ingest_token_load_errors_total.
	OnError func(sourceID string)
}

// parsedEntry is the intermediate shape produced by Reload's pass 1
// and consumed by pass 2. Per-entry validation errors short-circuit
// pass 1 (the entry is dropped, logged, and OnError fires). Pass 2
// drops any token-bytes value that appears more than once in pass 1.
type parsedEntry struct {
	sourceID string
	token    [32]byte
}

// Reload pulls the current set of ingest tokens from Vault, validates
// each, applies the two-pass duplicate-detection mandate from spec 10
// section 4.1 step 7, and atomically swaps the result into the store.
// Per-entry errors are logged and reported via OnError but do not fail
// the reload as a whole. Returns an error only when the top-level
// Vault list call fails (in which case the previous store stays
// intact, matching spec 10 section 4.1).
//
// The TWO-PASS algorithm is required to handle triple/quadruple
// collisions: if three source-ids share the same token bytes, ALL
// three must be dropped. A single-pass "delete the second occurrence"
// would leak one entry through. See spec 10 section 4.1 step 7.
func (s *TokenStore) Reload(ctx context.Context, cfg ReloadConfig) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	sourceIDs, err := cfg.Source.List(ctx)
	if err != nil {
		s.loadErr.Store(&err)
		return fmt.Errorf("list tokens: %w", err)
	}

	// PASS 1: validate per-entry; build counts and parsed entries.
	counts := make(map[[32]byte]int, len(sourceIDs))
	parsed := make([]parsedEntry, 0, len(sourceIDs))
	for _, sid := range sourceIDs {
		if !validSourceID(sid) {
			cfg.Logger.Error("glovebox ingest source_id malformed",
				"source_id_raw", sanitizeForLog(sid))
			if cfg.OnError != nil {
				cfg.OnError(sid)
			}
			continue
		}
		tokStr, readErr := cfg.Source.Read(ctx, sid)
		if readErr != nil {
			cfg.Logger.Error("glovebox ingest token read failed",
				"source_id", sid, "error", readErr.Error())
			if cfg.OnError != nil {
				cfg.OnError(sid)
			}
			continue
		}
		tok, ok := decodeToken(tokStr)
		if !ok {
			cfg.Logger.Error("glovebox ingest token malformed",
				"source_id", sid)
			if cfg.OnError != nil {
				cfg.OnError(sid)
			}
			continue
		}
		counts[tok]++
		parsed = append(parsed, parsedEntry{sourceID: sid, token: tok})
	}

	// PASS 2: emit only the entries whose token bytes appear exactly
	// once. Any token-bytes value seen 2+ times drops ALL of its
	// source-ids (so a 3-way collision drops three entries, not two).
	// OnError fires once per dropped source-id so the
	// token_load_errors_total metric reflects every dropped record.
	next := make([]tokenEntry, 0, len(parsed))
	for _, e := range parsed {
		if counts[e.token] > 1 {
			cfg.Logger.Error("glovebox ingest token duplicate",
				"source_id", e.sourceID,
				"collision_count", counts[e.token])
			if cfg.OnError != nil {
				cfg.OnError(e.sourceID)
			}
			continue
		}
		next = append(next, tokenEntry{token: e.token, sourceID: e.sourceID})
	}

	s.swap(next)
	var nilErr error
	s.loadErr.Store(&nilErr)
	return nil
}

// sanitizeForLog returns a printable variant of s with NUL, control
// chars, and newlines replaced. Caps at 64 bytes. Used only when
// logging VALUES the operator might want to see but which originated
// from untrusted input (Vault list-response keys).
func sanitizeForLog(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			c = '?'
		}
		out = append(out, c)
	}
	return string(out)
}

// productionVaultClient adapts *vaultapi.Client to the VaultClient
// interface. The K8s auth login is the caller's responsibility (use
// LoginK8s). Pattern mirrors connectors/schoology-auth-refresher/main.go's
// newVaultClient.
type productionVaultClient struct {
	client *vaultapi.Client
}

// NewProductionVaultClient wraps a *vaultapi.Client to satisfy
// VaultClient. The Vault client must already be authenticated (see
// LoginK8s).
func NewProductionVaultClient(client *vaultapi.Client) VaultClient {
	return &productionVaultClient{client: client}
}

// ListPath enumerates KV v2 keys under kvMount/metadata/path.
func (p *productionVaultClient) ListPath(ctx context.Context, kvMount, path string) ([]string, error) {
	secret, err := p.client.Logical().ListWithContext(ctx, kvMount+"/metadata/"+path)
	if err != nil {
		return nil, err
	}
	if secret == nil || secret.Data == nil {
		return nil, nil
	}
	keysRaw, _ := secret.Data["keys"].([]any)
	out := make([]string, 0, len(keysRaw))
	for _, k := range keysRaw {
		if s, ok := k.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// ReadKV reads a KV v2 secret at kvMount/data/path and returns the
// inner data map.
func (p *productionVaultClient) ReadKV(ctx context.Context, kvMount, path string) (map[string]any, error) {
	secret, err := p.client.KVv2(kvMount).Get(ctx, path)
	if err != nil {
		return nil, err
	}
	if secret == nil {
		return nil, fmt.Errorf("secret not found: %s/%s", kvMount, path)
	}
	return secret.Data, nil
}

// LoginK8s drives the Vault K8s auth method using the pod's projected
// ServiceAccount token at the default path. Mirrors the
// connectors/schoology-auth-refresher/main.go newVaultClient pattern.
// The returned client is wired through NewProductionVaultClient by the
// caller.
func LoginK8s(ctx context.Context, addr, role string) (*vaultapi.Client, error) {
	cfg := vaultapi.DefaultConfig()
	cfg.Address = addr
	client, err := vaultapi.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("build vault client: %w", err)
	}
	k8sAuth, err := vaultk8s.NewKubernetesAuth(role)
	if err != nil {
		return nil, fmt.Errorf("build k8s auth: %w", err)
	}
	if _, err := client.Auth().Login(ctx, k8sAuth); err != nil {
		return nil, fmt.Errorf("vault k8s login: %w", err)
	}
	return client, nil
}
