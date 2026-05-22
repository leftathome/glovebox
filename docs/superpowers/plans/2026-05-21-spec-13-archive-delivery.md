# Spec 13 -- Archive Delivery Endpoint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the `/v1/archives*` tus.io resumable upload endpoint (spec 13) and promote spec 10 from stub to implemented bearer-token auth. The recognizer service can deliver multi-GB archive artifacts (mbox + Takeout subtrees) and glovebox stages them for media-type-specific importers.

**Architecture:** A new sibling endpoint surface in `internal/ingest/` co-located with the existing `/v1/ingest` listener (port 9091). Auth + rate-limit middleware in a new `internal/ingest/auth/` package; tus.io handler + untar dispatcher + finalize in a new `internal/ingest/archives/` package. Existing `/v1/ingest` remains untouched per spec 10 v1 scope.

**Tech Stack:** Go 1.26, `hashicorp/vault/api` (already used by the schoology refresher), `golang.org/x/time/rate` (new dep) for rate limiting, `archive/tar` (stdlib) for untar, `crypto/subtle` (stdlib) for constant-time compare. No tus.io library dependency -- protocol is rolled in-tree because tusd is a complete server, not an embeddable library, and the protocol surface is small enough to hand-roll cleanly against our atomic-finalize semantics.

---

## File Structure

```
internal/ingest/
├── server.go                            (MODIFY — mount /v1/archives* handlers)
├── handler.go                           (UNCHANGED — /v1/ingest stays as-is)
├── auth/                                (NEW package — spec 10)
│   ├── tokens.go                        Vault loader + in-memory token store + reload
│   ├── tokens_test.go
│   ├── proxy.go                         X-Forwarded-For trust resolution
│   ├── proxy_test.go
│   ├── ratelimit.go                     Per-IP + global rate limiter
│   ├── ratelimit_test.go
│   ├── middleware.go                    HTTP wrapper that ties tokens + proxy + ratelimit
│   └── middleware_test.go
├── archives/                            (NEW package — spec 13 §4)
│   ├── metadata.go                      Upload-Metadata parse + validate
│   ├── metadata_test.go
│   ├── store.go                         In-memory upload-id store, source-id binding, per-upload mutex
│   ├── store_test.go
│   ├── untar.go                         Tar safety rules + streaming extract
│   ├── untar_test.go
│   ├── finalize.go                      sha256 verify + media-type dispatch + atomic rename
│   ├── finalize_test.go
│   ├── handler.go                       tus.io HTTP handlers (OPTIONS, POST, HEAD, PATCH, DELETE, GET)
│   ├── handler_test.go
│   ├── quota.go                         Background measurement goroutine
│   ├── quota_test.go
│   ├── cleanup.go                       Orphan-tmp cleanup goroutine
│   ├── cleanup_test.go
│   └── integration_test.go              End-to-end with a real tus client
├── audit_provenance.go                  (NEW — `delivered_by` plumbing into staging metadata)
└── audit_provenance_test.go

charts/glovebox/
├── values.yaml                          (MODIFY — add ingest.auth + ingest.archives blocks)
└── templates/
    ├── archive-tokens-externalsecret.yaml      (NEW — example ExternalSecret)
    └── archive-networkpolicy.yaml              (NEW — recognizer-ns ingress)

scripts/
└── archive-smoke-test.sh                       (NEW — 12 GiB tus.io upload smoke test)

go.mod                                          (MODIFY — add golang.org/x/time/rate)
```

---

## Conventions (Read Before Starting)

- **Branch:** `chore/beads-glovebox-p1zx` (currently checked out). Do NOT push to github (broken HTTPS); `git push gitlab spec-12-schoology-connector` works for other branches but this work commits locally only. Final merge to main via local `git merge --no-ff`, mirroring the spec-12 pattern.
- **Beads tracking:** every task below is one bead. Claim via `bd update <id> --claim`, do the work, close via `bd close <id>` from the orchestrator (NOT the implementer agent). The orchestrator dispatches implementer + 2 reviewers per task; review findings bundle into one cleanup commit per wave.
- **TDD discipline:** every task starts with a failing test, runs it to confirm it fails for the right reason, writes minimal code, runs the test to confirm it passes, then commits. Don't skip the "run it failing first" step — that's where you catch test-doesn't-actually-test-what-you-think bugs.
- **Commit message format:** `<package>: <imperative summary> (glovebox-<id>)`.
- **Verification at task boundaries:** `go vet ./...` AND `go test ./internal/ingest/...` AND (for chart tasks) `helm lint charts/glovebox/`. Failures BLOCK the commit; the implementer fixes before reporting back.
- **No emojis in code** (per project CLAUDE.md).
- **No `bd edit`** (opens vim and blocks agents).
- **Test helpers:** `internal/ingest/archives/` tests share helpers via a `helpers_test.go` file added in the first task that needs them. Other tasks add to it rather than re-defining.
- **Nil-safety convention:** every type that could be nil in tests (Telemetry-style) has nil-safe Record/StartSpan/etc. helpers. Mirrors spec 12 §13's pattern.
- **Spec 12 §5 is the Vault K8s auth precedent.** Re-read `connectors/schoology-auth-refresher/main.go`'s `newVaultClient` function before writing the spec 10 token loader; the pattern is identical.

---

## Beads layout (create these BEFORE Wave A starts)

The orchestrator creates these as child beads of glovebox-p1zx, in `bd create` parallel-subagent style. Wave A's three are independent (parallel-able). Wave B's three are touch-disjoint within `archives/` (parallel-able). Wave C is sequential. Wave D is parallel.

```
Wave A (parallel, blocks Wave C):
  glovebox-A1  spec 13 impl: Vault token loader + reload (internal/ingest/auth/tokens.go)
  glovebox-A2  spec 13 impl: rate limiter + proxy trust (internal/ingest/auth/{proxy,ratelimit}.go)
  glovebox-A3  spec 13 impl: delivered_by provenance plumbing (internal/ingest/audit_provenance.go)

Wave B (parallel, blocks Wave C):
  glovebox-B1  spec 13 impl: Upload-Metadata parse + validate (internal/ingest/archives/metadata.go)
  glovebox-B2  spec 13 impl: upload-id store + per-upload mutex (internal/ingest/archives/store.go)
  glovebox-B3  spec 13 impl: tar safety + streaming extract (internal/ingest/archives/untar.go)

Wave C (sequential, depends on Wave A + Wave B):
  glovebox-C1  spec 13 impl: finalize (sha256 verify + dispatch + atomic rename)
  glovebox-C2  spec 13 impl: tus.io HTTP handler (mounts B1+B2+B3+C1)
  glovebox-C3  spec 13 impl: server wire-up + quota goroutine + cleanup goroutine
  glovebox-C4  spec 13 impl: end-to-end integration test

Wave D (parallel, depends on Wave C):
  glovebox-D1  spec 13 impl: Helm chart updates (values.yaml + ExternalSecret + NetworkPolicy)
  glovebox-D2  spec 13 impl: smoke test script (12 GiB upload against container image)
```

---

## Wave A: Foundations (parallel-dispatch)

### Task A1: Vault Token Loader + Reload

**Files:**
- Create: `internal/ingest/auth/tokens.go`
- Create: `internal/ingest/auth/tokens_test.go`
- Modify: `go.mod` (add `golang.org/x/time/rate` — actually this lands in A2; A1 only touches vault deps which are already present)

**Spec references:** spec 10 §3 (token model), §4.1 (loading), §4.2 (reload), §4.3 (validation storage shape).

- [ ] **Step 1: Add the `tokenEntry` type and the in-memory store skeleton.**

```go
// internal/ingest/auth/tokens.go
package auth

import (
    "context"
    "crypto/subtle"
    "encoding/hex"
    "fmt"
    "log/slog"
    "regexp"
    "sync"
    "sync/atomic"

    vaultapi "github.com/hashicorp/vault/api"
    vaultk8s "github.com/hashicorp/vault/api/auth/kubernetes"
)

// tokenEntry holds one source-id's token bytes. Bytes are stored as a
// fixed-size array (not a slice) so a constant-time compare against
// a fixed-size request token has no allocation surprises.
type tokenEntry struct {
    token    [32]byte
    sourceID string
}

// TokenStore holds the validated set of tokens. Lookups are
// constant-time linear scans per spec 10 §4.3.
type TokenStore struct {
    mu       sync.RWMutex
    entries  atomic.Pointer[[]tokenEntry]   // atomic swap on reload
    loadErr  atomic.Pointer[error]          // most recent reload error (nil = healthy)
}

// NewTokenStore returns an empty store. Call Reload before serving.
func NewTokenStore() *TokenStore {
    s := &TokenStore{}
    empty := []tokenEntry{}
    s.entries.Store(&empty)
    return s
}
```

- [ ] **Step 2: Write the failing test for source-id format validation.**

```go
// internal/ingest/auth/tokens_test.go
package auth

import "testing"

func TestSourceIDFormat_AcceptsValid(t *testing.T) {
    cases := []string{
        "recognizer",
        "workstation-mbox-importer",
        "friend-alice",
        "a1",
        "abc-def-123",
    }
    for _, s := range cases {
        if !validSourceID(s) {
            t.Errorf("validSourceID(%q) = false, want true", s)
        }
    }
}

func TestSourceIDFormat_RejectsInvalid(t *testing.T) {
    cases := []string{
        "",                  // empty
        "Recognizer",        // uppercase
        "-recognizer",       // leading dash
        "recognizer-",       // trailing dash
        "foo--bar",          // double dash
        "xn--abc",           // IDN prefix (also caught by double-dash rule)
        "abc def",           // space
        "abc/def",           // slash
        string(make([]byte, 65)), // too long
    }
    for _, s := range cases {
        if validSourceID(s) {
            t.Errorf("validSourceID(%q) = true, want false", s)
        }
    }
}
```

- [ ] **Step 3: Run the test to confirm it fails (function doesn't exist yet).**

```bash
go test ./internal/ingest/auth/ -run TestSourceIDFormat -v -count=1
```

Expected: `undefined: validSourceID` compile error.

- [ ] **Step 4: Implement `validSourceID` per spec 10 §3.2.**

```go
// internal/ingest/auth/tokens.go (append)

// sourceIDRe matches the spec 10 §3.2 format:
//   - lowercase letter start
//   - lowercase alnum middle
//   - single dashes only (no leading/trailing/consecutive)
//   - max 64 chars
var sourceIDRe = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

func validSourceID(s string) bool {
    if len(s) == 0 || len(s) > 64 {
        return false
    }
    return sourceIDRe.MatchString(s)
}
```

- [ ] **Step 5: Run the test to confirm it passes.**

```bash
go test ./internal/ingest/auth/ -run TestSourceIDFormat -v -count=1
```

Expected: PASS.

- [ ] **Step 6: Write the failing test for token format validation.**

```go
// internal/ingest/auth/tokens_test.go (append)

func TestTokenFormat_AcceptsValid(t *testing.T) {
    valid := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    if _, ok := decodeToken(valid); !ok {
        t.Errorf("decodeToken(valid) returned !ok")
    }
}

func TestTokenFormat_RejectsInvalid(t *testing.T) {
    cases := []string{
        "",                    // empty
        "tooshort",            // < 64 chars
        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef0", // too long
        "0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef",  // uppercase
        "0123456789xyabcd0123456789abcdef0123456789abcdef0123456789abcdef",  // non-hex
    }
    for _, s := range cases {
        if _, ok := decodeToken(s); ok {
            t.Errorf("decodeToken(%q) returned ok", s)
        }
    }
}
```

- [ ] **Step 7: Implement `decodeToken` and run the test.**

```go
// internal/ingest/auth/tokens.go (append)

// decodeToken validates a 64-char lowercase hex string and returns
// its 32 bytes. Returns ok=false on any format violation; the caller
// surfaces 401 without distinguishing the failure mode.
func decodeToken(s string) ([32]byte, bool) {
    var out [32]byte
    if len(s) != 64 {
        return out, false
    }
    // Reject any uppercase before decoding — hex.Decode accepts mixed case.
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
```

```bash
go test ./internal/ingest/auth/ -run TestTokenFormat -v -count=1
```

Expected: PASS.

- [ ] **Step 8: Add the constant-time `Lookup` method with bitwise-OR aggregation per spec 10 §4.3.**

```go
// internal/ingest/auth/tokens.go (append)

// Lookup returns the source-id for a token. Returns "", false if no
// match. The iteration is constant-time-friendly: every entry is
// compared, no early exit. ConstantTimeSelect handles the source-id
// copy branchlessly except for the residual string-copy on success.
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
        // Copy source-id iff this entry matched AND no earlier entry did.
        // Tokens are required globally unique (load-time check), so the
        // residual branch fires at most once per request.
        if eq == 1 && prev == 0 {
            sourceID = e.sourceID
        }
    }
    return sourceID, matched == 1
}
```

- [ ] **Step 9: Write the failing test for `Lookup`.**

```go
func TestTokenStore_LookupReturnsSourceID(t *testing.T) {
    s := NewTokenStore()
    tok1 := mustDecodeTokenForTest(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
    tok2 := mustDecodeTokenForTest(t, "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")
    s.swap([]tokenEntry{
        {token: tok1, sourceID: "recognizer"},
        {token: tok2, sourceID: "friend-alice"},
    })

    if id, ok := s.Lookup(tok1); !ok || id != "recognizer" {
        t.Errorf("Lookup(tok1) = (%q, %v), want (recognizer, true)", id, ok)
    }
    if id, ok := s.Lookup(tok2); !ok || id != "friend-alice" {
        t.Errorf("Lookup(tok2) = (%q, %v), want (friend-alice, true)", id, ok)
    }
    var unknown [32]byte
    if _, ok := s.Lookup(unknown); ok {
        t.Error("Lookup(unknown) returned ok=true")
    }
}

func mustDecodeTokenForTest(t *testing.T, s string) [32]byte {
    t.Helper()
    tok, ok := decodeToken(s)
    if !ok {
        t.Fatalf("decodeToken(%q): not ok", s)
    }
    return tok
}
```

- [ ] **Step 10: Add the `swap` helper and run the test.**

```go
// internal/ingest/auth/tokens.go (append)

// swap atomically replaces the entries slice. Called by Reload after
// validating + assembling the new entry set.
func (s *TokenStore) swap(entries []tokenEntry) {
    s.entries.Store(&entries)
}
```

```bash
go test ./internal/ingest/auth/ -run TestTokenStore_LookupReturnsSourceID -v -count=1
```

Expected: PASS.

- [ ] **Step 11: Implement `Reload` against a `vaultapi.Client` seam.**

```go
// internal/ingest/auth/tokens.go (append)

// VaultClient is the test seam for the parts of vaultapi.Client we use.
// Production wires this to *vaultapi.Client; tests pass an in-memory fake.
type VaultClient interface {
    // ListPath returns the keys under <kvMount>/metadata/<path>. Empty
    // list (no error) means no entries; error is propagated unchanged.
    ListPath(ctx context.Context, kvMount, path string) ([]string, error)
    // ReadKV returns the raw KV v2 data map for <kvMount>/data/<path>.
    ReadKV(ctx context.Context, kvMount, path string) (map[string]any, error)
}

// ReloadConfig controls a single Reload invocation.
type ReloadConfig struct {
    Client   VaultClient
    KVMount  string  // e.g. "secret"
    BasePath string  // e.g. "glovebox/ingest-tokens"
    Logger   *slog.Logger
    OnError  func(sourceID string)  // optional; invoked once per per-entry error
}

// Reload pulls the current set of ingest tokens from Vault, validates
// each, and atomically swaps them into the store. Per-entry errors are
// logged + reported via OnError but do not fail the reload as a whole.
// Returns an error only when the top-level Vault call fails (in which
// case the previous store stays intact, matching spec 10 §4.1).
func (s *TokenStore) Reload(ctx context.Context, cfg ReloadConfig) error {
    if cfg.Logger == nil { cfg.Logger = slog.Default() }
    if cfg.KVMount == "" { cfg.KVMount = "secret" }
    sourceIDs, err := cfg.Client.ListPath(ctx, cfg.KVMount, cfg.BasePath)
    if err != nil {
        s.loadErr.Store(&err)
        return fmt.Errorf("list %s: %w", cfg.BasePath, err)
    }
    seen := make(map[[32]byte]string, len(sourceIDs))
    next := make([]tokenEntry, 0, len(sourceIDs))
    for _, sid := range sourceIDs {
        if !validSourceID(sid) {
            cfg.Logger.Error("glovebox ingest source_id malformed", "source_id_raw", sanitizeForLog(sid))
            if cfg.OnError != nil { cfg.OnError(sid) }
            continue
        }
        data, err := cfg.Client.ReadKV(ctx, cfg.KVMount, cfg.BasePath+"/"+sid)
        if err != nil {
            cfg.Logger.Error("glovebox ingest vault read failed", "source_id", sid, "error", err.Error())
            if cfg.OnError != nil { cfg.OnError(sid) }
            continue
        }
        tokStr, _ := data["token"].(string)
        tok, ok := decodeToken(tokStr)
        if !ok {
            cfg.Logger.Error("glovebox ingest token malformed", "source_id", sid)
            if cfg.OnError != nil { cfg.OnError(sid) }
            continue
        }
        if prev, dup := seen[tok]; dup {
            cfg.Logger.Error("glovebox ingest token duplicate",
                "source_id_a", prev, "source_id_b", sid)
            // Per spec 10 §4.1 step 7: drop BOTH entries on duplicate.
            // We remove the already-added entry by index search.
            for i := range next {
                if next[i].sourceID == prev {
                    next = append(next[:i], next[i+1:]...)
                    break
                }
            }
            delete(seen, tok)
            if cfg.OnError != nil { cfg.OnError(prev); cfg.OnError(sid) }
            continue
        }
        seen[tok] = sid
        next = append(next, tokenEntry{token: tok, sourceID: sid})
    }
    s.swap(next)
    var nilErr error
    s.loadErr.Store(&nilErr)
    return nil
}

// sanitizeForLog returns a printable variant of s with NUL / control
// chars + newlines replaced. Caps at 64 bytes. Used only when logging
// VALUES the operator might want to see but which originated from
// untrusted input (Vault list response keys).
func sanitizeForLog(s string) string {
    if len(s) > 64 { s = s[:64] }
    out := make([]byte, 0, len(s))
    for i := 0; i < len(s); i++ {
        c := s[i]
        if c < 0x20 || c == 0x7f { c = '?' }
        out = append(out, c)
    }
    return string(out)
}
```

- [ ] **Step 12: Write tests for Reload using a fake VaultClient.**

```go
// Sketch — flesh out with cases for: happy path with 2 sources;
// malformed source-id skipped; malformed token skipped; duplicate
// tokens drop BOTH; top-level list error preserves prior store.
//
// Use a `fakeVault` struct implementing VaultClient with maps for
// list responses and per-path data. ~80 lines of test code.
```

(Detailed test code omitted; follow the spec 10 §4.1 acceptance criteria — these tests are mandatory before commit.)

- [ ] **Step 13: Add production Vault client wrapper.**

```go
// internal/ingest/auth/tokens.go (append)

type productionVaultClient struct {
    client *vaultapi.Client
}

// NewProductionVaultClient wraps a *vaultapi.Client to satisfy
// VaultClient. The K8s auth login is the caller's responsibility
// (mirrors connectors/schoology-auth-refresher/main.go's
// newVaultClient pattern; cite that in the doc comment).
func NewProductionVaultClient(client *vaultapi.Client) VaultClient {
    return &productionVaultClient{client: client}
}

func (p *productionVaultClient) ListPath(ctx context.Context, kvMount, path string) ([]string, error) {
    // KVv2 list uses the "metadata" subpath.
    secret, err := p.client.Logical().ListWithContext(ctx, kvMount+"/metadata/"+path)
    if err != nil {
        return nil, err
    }
    if secret == nil || secret.Data == nil { return nil, nil }
    keysRaw, _ := secret.Data["keys"].([]any)
    out := make([]string, 0, len(keysRaw))
    for _, k := range keysRaw {
        if s, ok := k.(string); ok {
            out = append(out, s)
        }
    }
    return out, nil
}

func (p *productionVaultClient) ReadKV(ctx context.Context, kvMount, path string) (map[string]any, error) {
    secret, err := p.client.KVv2(kvMount).Get(ctx, path)
    if err != nil { return nil, err }
    if secret == nil { return nil, fmt.Errorf("secret not found: %s/%s", kvMount, path) }
    return secret.Data, nil
}

// LoginK8s drives the Vault K8s auth method. Same pattern as
// connectors/schoology-auth-refresher/main.go's newVaultClient.
func LoginK8s(ctx context.Context, addr, role string) (*vaultapi.Client, error) {
    cfg := vaultapi.DefaultConfig()
    cfg.Address = addr
    client, err := vaultapi.NewClient(cfg)
    if err != nil { return nil, fmt.Errorf("vault client: %w", err) }
    k8sAuth, err := vaultk8s.NewKubernetesAuth(role)
    if err != nil { return nil, fmt.Errorf("k8s auth helper: %w", err) }
    if _, err := client.Auth().Login(ctx, k8sAuth); err != nil {
        return nil, fmt.Errorf("vault k8s login: %w", err)
    }
    return client, nil
}
```

- [ ] **Step 14: Run full package tests + vet + commit.**

```bash
go test ./internal/ingest/auth/ -count=1 -v
go vet ./internal/ingest/auth/...
git add internal/ingest/auth/tokens.go internal/ingest/auth/tokens_test.go
git commit -m "ingest/auth: Vault token loader + in-memory store (glovebox-A1)"
```

Expected: tests PASS, vet CLEAN, commit succeeds.

**Exit criteria for A1:** `TokenStore` with `Lookup` (constant-time linear scan), `Reload` (Vault → in-memory swap, with duplicate-token rejection AND per-entry-error tolerance), `LoginK8s` helper. Vault client is a seam (`VaultClient` interface) so tests don't need a real Vault.

---

### Task A2: Rate Limiter + Proxy Trust

**Files:**
- Create: `internal/ingest/auth/proxy.go`
- Create: `internal/ingest/auth/proxy_test.go`
- Create: `internal/ingest/auth/ratelimit.go`
- Create: `internal/ingest/auth/ratelimit_test.go`
- Modify: `go.mod` (add `golang.org/x/time/rate`)

**Spec references:** spec 10 §5.3 (per-IP rate limit, trusted-proxy CIDR, global backstop, LRU).

- [ ] **Step 1: `go get golang.org/x/time/rate` and commit go.mod/go.sum.**

```bash
go get golang.org/x/time/rate
go mod tidy
git add go.mod go.sum
git commit -m "deps: add golang.org/x/time/rate (glovebox-A2)"
```

- [ ] **Step 2: Write failing test for proxy.ResolveClientIP.**

```go
// internal/ingest/auth/proxy_test.go
package auth

import (
    "net"
    "testing"
)

func TestResolveClientIP_NoTrustedCIDRs_UsesRemoteAddr(t *testing.T) {
    r := &ProxyResolver{}
    got := r.ResolveClientIP("203.0.113.5:54321", "10.0.0.1")
    if got != "203.0.113.5" {
        t.Errorf("got %q, want 203.0.113.5", got)
    }
}

func TestResolveClientIP_TrustedPeer_UsesXFFRightmost(t *testing.T) {
    _, cidr, _ := net.ParseCIDR("10.244.0.0/16")
    r := &ProxyResolver{TrustedCIDRs: []*net.IPNet{cidr}}
    got := r.ResolveClientIP("10.244.0.5:54321", "198.51.100.7, 192.0.2.3")
    if got != "192.0.2.3" {
        t.Errorf("got %q, want 192.0.2.3", got)
    }
}

func TestResolveClientIP_UntrustedPeer_IgnoresXFF(t *testing.T) {
    _, cidr, _ := net.ParseCIDR("10.244.0.0/16")
    r := &ProxyResolver{TrustedCIDRs: []*net.IPNet{cidr}}
    // Untrusted peer attempting to forge XFF.
    got := r.ResolveClientIP("198.51.100.99:54321", "10.0.0.1")
    if got != "198.51.100.99" {
        t.Errorf("got %q, want 198.51.100.99 (XFF must be ignored)", got)
    }
}
```

- [ ] **Step 3: Implement `ProxyResolver`.**

```go
// internal/ingest/auth/proxy.go
package auth

import (
    "net"
    "strings"
)

// ProxyResolver derives the client IP from r.RemoteAddr + X-Forwarded-For
// honoring the spec 10 §5.3 trusted-proxy rule.
type ProxyResolver struct {
    TrustedCIDRs []*net.IPNet
}

// ResolveClientIP returns the client IP. remoteAddr is r.RemoteAddr;
// xff is r.Header.Get("X-Forwarded-For") (may be empty). The result is
// always a non-empty string; on parse failure returns remoteAddr verbatim
// minus its port (best-effort) so downstream rate-limit keying never sees
// "".
func (p *ProxyResolver) ResolveClientIP(remoteAddr, xff string) string {
    host, _, err := net.SplitHostPort(remoteAddr)
    if err != nil { host = remoteAddr }
    if xff == "" { return host }
    peerIP := net.ParseIP(host)
    if peerIP == nil { return host }
    trusted := false
    for _, c := range p.TrustedCIDRs {
        if c.Contains(peerIP) { trusted = true; break }
    }
    if !trusted { return host }
    // Use the right-most entry per RFC 7239 (the last hop the chain
    // appended, i.e. the closest trusted proxy's view of its peer).
    parts := strings.Split(xff, ",")
    last := strings.TrimSpace(parts[len(parts)-1])
    if net.ParseIP(last) == nil { return host }
    return last
}
```

```bash
go test ./internal/ingest/auth/ -run TestResolveClientIP -v -count=1
```

Expected: PASS.

- [ ] **Step 4: Add IP bucketing helper (CIDR /24 IPv4 / /64 IPv6 per spec 10 §6.4).**

Write failing test:

```go
func TestBucketIP_IPv4_24(t *testing.T) {
    if got := BucketIP("203.0.113.42"); got != "203.0.113.0/24" {
        t.Errorf("got %q, want 203.0.113.0/24", got)
    }
}
func TestBucketIP_IPv6_64(t *testing.T) {
    if got := BucketIP("2001:db8:1:2:3:4:5:6"); got != "2001:db8:1:2::/64" {
        t.Errorf("got %q, want 2001:db8:1:2::/64", got)
    }
}
```

Implement:

```go
// internal/ingest/auth/proxy.go (append)
func BucketIP(s string) string {
    ip := net.ParseIP(s)
    if ip == nil { return "unknown" }
    if ip4 := ip.To4(); ip4 != nil {
        ip4[3] = 0
        return ip4.String() + "/24"
    }
    // IPv6: zero out lower 64 bits.
    ip6 := ip.To16()
    if ip6 == nil { return "unknown" }
    for i := 8; i < 16; i++ { ip6[i] = 0 }
    return ip6.String() + "/64"
}
```

```bash
go test ./internal/ingest/auth/ -run TestBucketIP -v -count=1
```

Expected: PASS.

- [ ] **Step 5: Write failing test for per-IP rate limiter.**

```go
// internal/ingest/auth/ratelimit_test.go
package auth

import (
    "testing"
    "time"
)

func TestRateLimiter_AllowsBelowThreshold(t *testing.T) {
    rl := NewRateLimiter(RateLimitConfig{
        Window:          time.Minute,
        PerIPMaxRejected: 10,
        LRUCapacity:     100,
        GlobalMaxRejected: 1000,
    })
    for i := 0; i < 10; i++ {
        if !rl.AllowReject("198.51.100.5") {
            t.Fatalf("attempt %d unexpectedly rate-limited", i+1)
        }
    }
}

func TestRateLimiter_TripsAtThreshold(t *testing.T) {
    rl := NewRateLimiter(RateLimitConfig{
        Window:          time.Minute,
        PerIPMaxRejected: 3,
        LRUCapacity:     100,
        GlobalMaxRejected: 1000,
    })
    for i := 0; i < 3; i++ {
        if !rl.AllowReject("198.51.100.5") {
            t.Fatalf("attempt %d unexpectedly rate-limited", i+1)
        }
    }
    if rl.AllowReject("198.51.100.5") {
        t.Error("4th attempt should have been rate-limited")
    }
}

func TestRateLimiter_GlobalBackstop(t *testing.T) {
    rl := NewRateLimiter(RateLimitConfig{
        Window:          time.Minute,
        PerIPMaxRejected: 1000,  // effectively disabled per-IP
        LRUCapacity:     10000,
        GlobalMaxRejected: 3,
    })
    if !rl.AllowReject("198.51.100.1") { t.Fatal("1") }
    if !rl.AllowReject("198.51.100.2") { t.Fatal("2") }
    if !rl.AllowReject("198.51.100.3") { t.Fatal("3") }
    if rl.AllowReject("198.51.100.4") {
        t.Error("global backstop should have tripped at attempt 4")
    }
}
```

- [ ] **Step 6: Implement RateLimiter.**

```go
// internal/ingest/auth/ratelimit.go
package auth

import (
    "container/list"
    "sync"
    "time"

    "golang.org/x/time/rate"
)

// RateLimitConfig parameters; see spec 10 §5.3 + §9.
type RateLimitConfig struct {
    Window            time.Duration // sliding window length
    PerIPMaxRejected  int           // rejections allowed per IP per window
    LRUCapacity       int           // cap on tracked IPs (default 1000)
    GlobalMaxRejected int           // process-wide backstop per window
}

// RateLimiter tracks 401 attempts. Successful auths do NOT consume
// budget. Per-IP buckets + a single global bucket are checked together;
// either tripping returns false from AllowReject.
type RateLimiter struct {
    cfg RateLimitConfig

    mu       sync.Mutex
    perIP    map[string]*list.Element
    lru      *list.List
    global   *rate.Limiter
}

type bucketEntry struct {
    ip      string
    limiter *rate.Limiter
}

func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
    if cfg.LRUCapacity <= 0 { cfg.LRUCapacity = 1000 }
    // rate.Every gives a token every (Window / Max) seconds; bucket size = Max.
    // golang.org/x/time/rate is per-token, not per-window, so we approximate
    // a fixed window with a token bucket sized to N and refilled over Window.
    return &RateLimiter{
        cfg:    cfg,
        perIP:  make(map[string]*list.Element, cfg.LRUCapacity),
        lru:    list.New(),
        global: rate.NewLimiter(rate.Every(cfg.Window/time.Duration(cfg.GlobalMaxRejected)),
            cfg.GlobalMaxRejected),
    }
}

// AllowReject returns true if the rejection is permitted (under both
// per-IP and global caps). Returns false when the IP OR the global
// counter has tripped; the caller should respond 429.
func (r *RateLimiter) AllowReject(ip string) bool {
    if !r.global.Allow() { return false }
    r.mu.Lock()
    defer r.mu.Unlock()
    el, ok := r.perIP[ip]
    if !ok {
        // Evict LRU tail if at capacity.
        if r.lru.Len() >= r.cfg.LRUCapacity {
            old := r.lru.Back()
            if old != nil {
                delete(r.perIP, old.Value.(*bucketEntry).ip)
                r.lru.Remove(old)
            }
        }
        lim := rate.NewLimiter(
            rate.Every(r.cfg.Window/time.Duration(r.cfg.PerIPMaxRejected)),
            r.cfg.PerIPMaxRejected)
        el = r.lru.PushFront(&bucketEntry{ip: ip, limiter: lim})
        r.perIP[ip] = el
    } else {
        r.lru.MoveToFront(el)
    }
    return el.Value.(*bucketEntry).limiter.Allow()
}
```

- [ ] **Step 7: Run tests.**

```bash
go test ./internal/ingest/auth/ -count=1 -v
```

Expected: all PASS.

- [ ] **Step 8: Commit.**

```bash
git add internal/ingest/auth/proxy.go internal/ingest/auth/proxy_test.go \
        internal/ingest/auth/ratelimit.go internal/ingest/auth/ratelimit_test.go
git commit -m "ingest/auth: rate limiter + proxy IP trust (glovebox-A2)"
```

**Exit criteria for A2:** `ProxyResolver.ResolveClientIP` honors trusted-proxy CIDR; `BucketIP` returns CIDR strings; `RateLimiter` enforces per-IP + global caps with LRU eviction.

---

### Task A3: `delivered_by` Provenance Plumbing

**Files:**
- Create: `internal/ingest/audit_provenance.go`
- Create: `internal/ingest/audit_provenance_test.go`

**Spec references:** spec 10 §6 (provenance), spec 06 §5.2 (Identity block), §8.3 (audit log).

- [ ] **Step 1: Write failing test for the request-context helpers.**

```go
// internal/ingest/audit_provenance_test.go
package ingest

import (
    "context"
    "testing"
)

func TestWithDeliveredBy_RoundTrip(t *testing.T) {
    ctx := WithDeliveredBy(context.Background(), "recognizer")
    got, ok := DeliveredBy(ctx)
    if !ok || got != "recognizer" {
        t.Errorf("DeliveredBy = (%q, %v), want (recognizer, true)", got, ok)
    }
}

func TestDeliveredBy_AbsentReturnsFalse(t *testing.T) {
    if _, ok := DeliveredBy(context.Background()); ok {
        t.Error("DeliveredBy on bare ctx returned ok=true")
    }
}
```

- [ ] **Step 2: Implement and run.**

```go
// internal/ingest/audit_provenance.go
package ingest

import "context"

type ctxKey int

const deliveredByKey ctxKey = 1

// WithDeliveredBy returns a derived context carrying the validated
// source-id from the bearer token. Set by the auth middleware after
// successful validation per spec 10 §6.1.
func WithDeliveredBy(ctx context.Context, sourceID string) context.Context {
    return context.WithValue(ctx, deliveredByKey, sourceID)
}

// DeliveredBy returns the source-id set by WithDeliveredBy, or
// ("", false) if absent.
func DeliveredBy(ctx context.Context) (string, bool) {
    s, ok := ctx.Value(deliveredByKey).(string)
    if !ok || s == "" { return "", false }
    return s, true
}
```

```bash
go test ./internal/ingest/ -run TestWithDeliveredBy -v -count=1
go test ./internal/ingest/ -run TestDeliveredBy -v -count=1
```

Expected: both PASS.

- [ ] **Step 3: Add a small helper to write Identity into metadata.json sidecar shape.**

Write test:

```go
func TestBuildIdentity_FromContext(t *testing.T) {
    ctx := WithDeliveredBy(context.Background(), "recognizer")
    id := BuildIdentity(ctx)
    if id == nil { t.Fatal("BuildIdentity returned nil") }
    if id.Provider != "ingest" { t.Errorf("Provider=%q", id.Provider) }
    if id.AuthMethod != "bearer_token" { t.Errorf("AuthMethod=%q", id.AuthMethod) }
    if id.AccountID != "recognizer" { t.Errorf("AccountID=%q", id.AccountID) }
}

func TestBuildIdentity_AbsentReturnsNil(t *testing.T) {
    if id := BuildIdentity(context.Background()); id != nil {
        t.Errorf("BuildIdentity(bare ctx) = %+v, want nil", id)
    }
}
```

Implement:

```go
// internal/ingest/audit_provenance.go (append)

// Identity matches the spec 06 §5.2 shape that lands in metadata.json
// + the audit log. Defined here rather than importing the existing
// scanner-side type to avoid a circular dep.
type Identity struct {
    Provider   string `json:"provider"`
    AuthMethod string `json:"auth_method"`
    AccountID  string `json:"account_id"`
}

// BuildIdentity constructs the Identity block from a request context
// that has been through the auth middleware. Returns nil if the context
// has no delivered_by (caller writes a default or skips).
func BuildIdentity(ctx context.Context) *Identity {
    sid, ok := DeliveredBy(ctx)
    if !ok { return nil }
    return &Identity{
        Provider:   "ingest",
        AuthMethod: "bearer_token",
        AccountID:  sid,
    }
}
```

```bash
go test ./internal/ingest/ -run TestBuildIdentity -v -count=1
```

Expected: PASS.

- [ ] **Step 4: Commit.**

```bash
git add internal/ingest/audit_provenance.go internal/ingest/audit_provenance_test.go
git commit -m "ingest: delivered_by + Identity request-context plumbing (glovebox-A3)"
```

**Exit criteria for A3:** request context carries `delivered_by` from auth middleware to handler; `BuildIdentity` produces the spec 06 §5.2 block.

---

## Wave A → Wave A Cleanup (orchestrator)

After A1+A2+A3 land and reviewers report:

1. Bundle review findings into one cleanup commit per the spec-12 pattern.
2. Close glovebox-A1, A2, A3.
3. Verify: `go vet ./internal/ingest/auth/... ./internal/ingest/...` clean; tests pass.

---

## Wave B: Protocol Building Blocks (parallel-dispatch)

### Task B1: Upload-Metadata Parse + Validate

**Files:**
- Create: `internal/ingest/archives/metadata.go`
- Create: `internal/ingest/archives/metadata_test.go`

**Spec references:** spec 13 §4.2 (metadata schema), §3.3 (server-set fields rejected from client).

- [ ] **Step 1: Failing test for the happy path.**

```go
// internal/ingest/archives/metadata_test.go
package archives

import (
    "encoding/base64"
    "testing"
)

func TestParseUploadMetadata_HappyPath(t *testing.T) {
    enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
    header := strings.Join([]string{
        "archive_id " + enc("abc123-takeout-001"),
        "archive_filename " + enc("mail.mbox"),
        "subtree_relative_path " + enc("."),
        "media_type " + enc("archive/mbox"),
        "matcher_id " + enc("google-takeout/mail"),
        "provider " + enc("google"),
        "sha256 " + enc("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"),
        "size_bytes " + enc("1024"),
    }, ",")
    m, err := ParseUploadMetadata(header, 1024)  // declared Upload-Length=1024
    if err != nil { t.Fatalf("parse: %v", err) }
    if m.ArchiveID != "abc123-takeout-001" { t.Errorf("ArchiveID=%q", m.ArchiveID) }
    if m.MediaType != "archive/mbox" { t.Errorf("MediaType=%q", m.MediaType) }
}

// Additional tests:
// - rejects archive_id with newline/NUL/uppercase
// - rejects archive_filename containing /, .., NUL
// - rejects unknown media_type
// - rejects size_bytes != Upload-Length
// - rejects sha256 not 64 lowercase hex chars
// - rejects matcher_id with control chars
// - rejects header containing delivered_by or delivered_at keys (server-set only)
// Each is one focused test ~10 lines.
```

- [ ] **Step 2: Implement `Metadata`, `ParseUploadMetadata`, allow-listed media types.**

```go
// internal/ingest/archives/metadata.go
package archives

import (
    "encoding/base64"
    "errors"
    "fmt"
    "regexp"
    "strconv"
    "strings"
    "unicode/utf8"
)

// Metadata is the parsed + validated Upload-Metadata block per spec 13 §4.2.
type Metadata struct {
    ArchiveID           string
    ArchiveFilename     string
    SubtreeRelativePath string
    MediaType           string
    MatcherID           string
    Provider            string
    SHA256              string  // hex
    SizeBytes           int64
}

// MediaShape is "raw" or "tar"; the dispatch table in finalize.go uses
// this to choose between rename and untar.
type MediaShape string
const (
    MediaRaw MediaShape = "raw"
    MediaTar MediaShape = "tar"
)

// mediaAllowList per spec 13 §4.5. Hard-coded; new entries require a
// code review.
var mediaAllowList = map[string]MediaShape{
    "archive/mbox":                    MediaRaw,
    "archive/google-takeout-subtree":  MediaTar,
}

// Validation regexes per spec 13 §4.2.
var (
    archiveIDRe       = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)
    archiveFilenameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
    matcherIDRe       = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,256}$`)
    providerRe        = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
    sha256Re          = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

var (
    ErrMetadataMissing       = errors.New("metadata missing required key")
    ErrMetadataInvalid       = errors.New("metadata value invalid")
    ErrMetadataReservedKey   = errors.New("metadata contains server-set key")
    ErrMetadataUnknownMediaType = errors.New("metadata media_type not in allow-list")
    ErrMetadataSizeMismatch  = errors.New("metadata size_bytes != Upload-Length")
    ErrMetadataTooLong       = errors.New("upload-metadata header exceeds 4 KiB")
)

const uploadMetadataMaxBytes = 4096

// ParseUploadMetadata parses the Upload-Metadata header and validates
// every field. uploadLength is the declared Upload-Length; size_bytes
// MUST match (spec §4.2 enforced at POST per the iteration-3 fix).
func ParseUploadMetadata(header string, uploadLength int64) (*Metadata, error) {
    if len(header) > uploadMetadataMaxBytes {
        return nil, ErrMetadataTooLong
    }
    raw, err := decodeMetadataPairs(header)
    if err != nil { return nil, err }
    if _, has := raw["delivered_by"]; has {
        return nil, fmt.Errorf("%w: delivered_by", ErrMetadataReservedKey)
    }
    if _, has := raw["delivered_at"]; has {
        return nil, fmt.Errorf("%w: delivered_at", ErrMetadataReservedKey)
    }
    m := &Metadata{
        ArchiveID:           raw["archive_id"],
        ArchiveFilename:     raw["archive_filename"],
        SubtreeRelativePath: raw["subtree_relative_path"],
        MediaType:           raw["media_type"],
        MatcherID:           raw["matcher_id"],
        Provider:            raw["provider"],
        SHA256:              raw["sha256"],
    }
    sizeStr := raw["size_bytes"]
    if sizeStr == "" { return nil, fmt.Errorf("%w: size_bytes", ErrMetadataMissing) }
    sz, perr := strconv.ParseInt(sizeStr, 10, 64)
    if perr != nil || sz < 0 {
        return nil, fmt.Errorf("%w: size_bytes %q", ErrMetadataInvalid, sizeStr)
    }
    m.SizeBytes = sz
    if m.SizeBytes != uploadLength { return nil, ErrMetadataSizeMismatch }

    if !archiveIDRe.MatchString(m.ArchiveID) {
        return nil, fmt.Errorf("%w: archive_id", ErrMetadataInvalid)
    }
    if !archiveFilenameRe.MatchString(m.ArchiveFilename) ||
        strings.Contains(m.ArchiveFilename, "..") {
        return nil, fmt.Errorf("%w: archive_filename", ErrMetadataInvalid)
    }
    if !utf8.ValidString(m.SubtreeRelativePath) ||
        strings.ContainsAny(m.SubtreeRelativePath, "\x00") ||
        len(m.SubtreeRelativePath) > 1024 ||
        hasControlChar(m.SubtreeRelativePath) {
        return nil, fmt.Errorf("%w: subtree_relative_path", ErrMetadataInvalid)
    }
    if !matcherIDRe.MatchString(m.MatcherID) {
        return nil, fmt.Errorf("%w: matcher_id", ErrMetadataInvalid)
    }
    if !providerRe.MatchString(m.Provider) {
        return nil, fmt.Errorf("%w: provider", ErrMetadataInvalid)
    }
    if !sha256Re.MatchString(m.SHA256) {
        return nil, fmt.Errorf("%w: sha256", ErrMetadataInvalid)
    }
    if _, ok := mediaAllowList[m.MediaType]; !ok {
        return nil, fmt.Errorf("%w: %q", ErrMetadataUnknownMediaType, m.MediaType)
    }
    return m, nil
}

// Shape returns the media shape for the parsed metadata. Safe to call
// only after a successful ParseUploadMetadata.
func (m *Metadata) Shape() MediaShape { return mediaAllowList[m.MediaType] }

func hasControlChar(s string) bool {
    for i := 0; i < len(s); i++ {
        if s[i] < 0x20 && s[i] != '\t' { return true }
    }
    return false
}

func decodeMetadataPairs(header string) (map[string]string, error) {
    out := make(map[string]string)
    for _, kv := range strings.Split(header, ",") {
        kv = strings.TrimSpace(kv)
        if kv == "" { continue }
        sp := strings.IndexByte(kv, ' ')
        if sp < 0 {
            return nil, fmt.Errorf("%w: pair missing space", ErrMetadataInvalid)
        }
        key := kv[:sp]
        val := kv[sp+1:]
        decoded, err := base64.StdEncoding.DecodeString(val)
        if err != nil {
            return nil, fmt.Errorf("%w: %s base64", ErrMetadataInvalid, key)
        }
        out[key] = string(decoded)
    }
    return out, nil
}
```

- [ ] **Step 3: Run + commit.**

```bash
go test ./internal/ingest/archives/ -run TestParseUploadMetadata -v -count=1
go vet ./internal/ingest/archives/...
git add internal/ingest/archives/metadata.go internal/ingest/archives/metadata_test.go
git commit -m "ingest/archives: Upload-Metadata parser + validator (glovebox-B1)"
```

**Exit criteria for B1:** every spec §4.2 field validated; server-set keys (delivered_by, delivered_at) rejected at parse; unknown media_type rejected at parse; size_bytes vs Upload-Length mismatch rejected at parse.

---

### Task B2: Upload-ID Store + Per-Upload Mutex + Source-ID Binding

**Files:**
- Create: `internal/ingest/archives/store.go`
- Create: `internal/ingest/archives/store_test.go`

**Spec references:** spec 13 §4.1 (upload-id format, source-id binding), §4.4 (per-upload mutex), §5.4 (per-source + global concurrent caps).

Detailed steps mirror Task B1's TDD shape: write tests for upload-id creation + binding + lookup + concurrent-cap enforcement; implement; verify; commit.

**Required surface:**

```go
// internal/ingest/archives/store.go
package archives

import (
    "crypto/rand"
    "encoding/hex"
    "errors"
    "fmt"
    "sync"
    "time"
)

// UploadState holds the per-upload-id state. The Mu protects all
// mutable fields; HEAD/PATCH/DELETE handlers acquire it.
type UploadState struct {
    Mu sync.Mutex
    ID            string     // 128-bit hex
    SourceID      string
    ArchiveID     string
    UploadLength  int64
    Offset        int64       // cumulative bytes received
    Hasher        hash.Hash    // sha256, updated incrementally
    Meta          *Metadata
    CreatedAt     time.Time
    LastActivity  time.Time   // for §4.4 idle timeout
}

type Store struct {
    mu      sync.Mutex
    uploads map[string]*UploadState     // upload-id → state
    perSrc  map[string]int              // source-id → concurrent count
    cfg     StoreConfig
}

type StoreConfig struct {
    PerSourceMaxConcurrent int
    GlobalMaxConcurrent    int
}

var (
    ErrConcurrencyPerSource = errors.New("per-source concurrent upload cap exceeded")
    ErrConcurrencyGlobal    = errors.New("global concurrent upload cap exceeded")
    ErrUploadNotFound       = errors.New("upload id not found or not owned by caller")
    ErrUploadBusy           = errors.New("upload busy")
)

// New creates an empty store.
func NewStore(cfg StoreConfig) *Store {
    return &Store{
        uploads: make(map[string]*UploadState),
        perSrc:  make(map[string]int),
        cfg:     cfg,
    }
}

// Create allocates a new upload-id bound to sourceID. Returns
// ErrConcurrencyPerSource / ErrConcurrencyGlobal if caps tripped.
func (s *Store) Create(sourceID string, m *Metadata, uploadLength int64) (*UploadState, error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    if len(s.uploads) >= s.cfg.GlobalMaxConcurrent {
        return nil, ErrConcurrencyGlobal
    }
    if s.perSrc[sourceID] >= s.cfg.PerSourceMaxConcurrent {
        return nil, ErrConcurrencyPerSource
    }
    id, err := newUploadID()
    if err != nil { return nil, err }
    now := time.Now().UTC()
    st := &UploadState{
        ID:           id,
        SourceID:     sourceID,
        ArchiveID:    m.ArchiveID,
        UploadLength: uploadLength,
        Meta:         m,
        Hasher:       sha256.New(),
        CreatedAt:    now,
        LastActivity: now,
    }
    s.uploads[id] = st
    s.perSrc[sourceID]++
    return st, nil
}

// Get returns the upload state for id, OR ErrUploadNotFound if it
// doesn't exist or doesn't belong to sourceID. Returning the same
// error for both cases prevents cross-source existence leaks (spec
// §4.1 binding rule).
func (s *Store) Get(id, sourceID string) (*UploadState, error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    st, ok := s.uploads[id]
    if !ok || st.SourceID != sourceID { return nil, ErrUploadNotFound }
    return st, nil
}

// Remove decrements the per-source counter and removes the upload-id
// entry. Called from DELETE, finalize, and orphan cleanup.
func (s *Store) Remove(id string) {
    s.mu.Lock()
    defer s.mu.Unlock()
    if st, ok := s.uploads[id]; ok {
        s.perSrc[st.SourceID]--
        if s.perSrc[st.SourceID] <= 0 { delete(s.perSrc, st.SourceID) }
        delete(s.uploads, id)
    }
}

func newUploadID() (string, error) {
    var b [16]byte
    if _, err := rand.Read(b[:]); err != nil {
        return "", fmt.Errorf("upload-id rand: %w", err)
    }
    return hex.EncodeToString(b[:]), nil
}
```

Tests: per-source cap, global cap, source-id binding (Get with wrong source-id returns NotFound), upload-id uniqueness.

```bash
go test ./internal/ingest/archives/ -run TestStore -v -count=1
git add internal/ingest/archives/store.go internal/ingest/archives/store_test.go
git commit -m "ingest/archives: upload-id store + concurrent caps + binding (glovebox-B2)"
```

---

### Task B3: Tar Safety + Streaming Extract

**Files:**
- Create: `internal/ingest/archives/untar.go`
- Create: `internal/ingest/archives/untar_test.go`

**Spec references:** spec 13 §4.7 (tar safety rules — seven steps).

Tests (each failing first, then implementation):

- absolute path → reject
- `..` component → reject
- NUL in name → reject
- non-UTF8 name → reject
- name too long (> 4096) → reject
- component too long (> 255) → reject
- symlink typeflag → reject
- hardlink typeflag → reject
- device file → reject
- pax header with `path` key → reject
- cumulative size > 2× declared limit → reject
- entry count > 1M → reject
- happy path: 3 regular files extracted into expected paths with mode 0600
- happy path: 2 directories + nested files extracted with directory mode 0700

Implementation surface:

```go
// internal/ingest/archives/untar.go
package archives

import (
    "archive/tar"
    "errors"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "strings"
    "unicode/utf8"
)

const (
    untarPathMax       = 4096
    untarNameMax       = 255
    untarMaxEntries    = 1_000_000
    untarSizeFactor    = 2  // cumulative extracted size <= sizeFactor × declared total
)

var (
    ErrTarPaxOverride    = errors.New("pax path/linkpath override rejected")
    ErrTarTypeflag       = errors.New("disallowed typeflag")
    ErrTarNameInvalid    = errors.New("entry name invalid")
    ErrTarPathTraversal  = errors.New("entry name path traversal")
    ErrTarTooLarge       = errors.New("entry or cumulative size exceeded cap")
    ErrTarTooMany        = errors.New("entry count exceeded cap")
)

type UntarConfig struct {
    DestDir     string  // existing dir to write entries into (already mkdir'd)
    UploadLength int64   // declared total upload size; cumulative cap = 2× this
}

// Untar streams src (a tar archive body) into cfg.DestDir, applying
// every spec §4.7 safety rule. Returns the number of entries extracted
// + the cumulative bytes written. On any rule violation returns (n, w, err)
// where err is one of the Err* sentinels and the caller deletes DestDir.
func Untar(src io.Reader, cfg UntarConfig) (entries int, written int64, err error) {
    tr := tar.NewReader(src)
    for {
        if entries >= untarMaxEntries { return entries, written, ErrTarTooMany }
        h, terr := tr.Next()
        if errors.Is(terr, io.EOF) { return entries, written, nil }
        if terr != nil { return entries, written, fmt.Errorf("tar read: %w", terr) }
        // Reject pax overrides (set on the SAME header for the next entry).
        if h.PAXRecords != nil {
            if _, ok := h.PAXRecords["path"]; ok {
                return entries, written, ErrTarPaxOverride
            }
            if _, ok := h.PAXRecords["linkpath"]; ok {
                return entries, written, ErrTarPaxOverride
            }
        }
        // Allow-list typeflag.
        switch h.Typeflag {
        case tar.TypeReg, tar.TypeDir:
            // proceed
        default:
            return entries, written, fmt.Errorf("%w: %v", ErrTarTypeflag, h.Typeflag)
        }
        name := h.Name
        if !validUntarName(name) {
            return entries, written, ErrTarNameInvalid
        }
        if hasPathTraversal(name) {
            return entries, written, ErrTarPathTraversal
        }
        dst := filepath.Join(cfg.DestDir, filepath.Clean(name))
        // Defense in depth: confirm dst is still under DestDir.
        rel, rerr := filepath.Rel(cfg.DestDir, dst)
        if rerr != nil || strings.HasPrefix(rel, "..") {
            return entries, written, ErrTarPathTraversal
        }
        if h.Typeflag == tar.TypeDir {
            if err := os.MkdirAll(dst, 0700); err != nil {
                return entries, written, fmt.Errorf("mkdir: %w", err)
            }
            entries++
            continue
        }
        // Regular file. Cap cumulative write before opening dst.
        if h.Size < 0 || h.Size > cfg.UploadLength {
            return entries, written, ErrTarTooLarge
        }
        if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
            return entries, written, fmt.Errorf("mkdir parent: %w", err)
        }
        f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
        if err != nil { return entries, written, fmt.Errorf("create file: %w", err) }
        n, werr := writeCapped(f, tr, cfg.UploadLength*untarSizeFactor-written)
        f.Close()
        written += n
        if werr != nil {
            if errors.Is(werr, errExceededCap) {
                return entries, written, ErrTarTooLarge
            }
            return entries, written, fmt.Errorf("write entry: %w", werr)
        }
        entries++
    }
}

var errExceededCap = errors.New("exceeded cumulative cap")

// writeCapped writes from src to dst stopping at limit bytes. Returns
// errExceededCap if src had more bytes than limit.
func writeCapped(dst io.Writer, src io.Reader, limit int64) (int64, error) {
    n, err := io.Copy(dst, io.LimitReader(src, limit))
    if err != nil { return n, err }
    // If src still has more bytes, we've blown the cap.
    var probe [1]byte
    if m, _ := src.Read(probe[:]); m > 0 { return n + int64(m), errExceededCap }
    return n, nil
}

func validUntarName(name string) bool {
    if name == "" || len(name) > untarPathMax { return false }
    if strings.ContainsRune(name, 0) { return false }
    if !utf8.ValidString(name) { return false }
    for i := 0; i < len(name); i++ {
        c := name[i]
        if c < 0x20 || c == 0x7f { return false }
    }
    // Per-component length cap.
    for _, comp := range strings.Split(name, "/") {
        if len(comp) > untarNameMax { return false }
    }
    return true
}

func hasPathTraversal(name string) bool {
    if strings.HasPrefix(name, "/") { return true }
    if strings.Contains(name, "\\") { return true }
    // Windows drive letter check.
    if len(name) >= 2 && name[1] == ':' { return true }
    for _, comp := range strings.Split(name, "/") {
        if comp == ".." { return true }
    }
    return false
}
```

Tests build tar archives in memory using `archive/tar.NewWriter` and feed them into `Untar`. Cover every spec §4.7 rule.

```bash
go test ./internal/ingest/archives/ -run TestUntar -v -count=1
git add internal/ingest/archives/untar.go internal/ingest/archives/untar_test.go
git commit -m "ingest/archives: tar safety + streaming extract (glovebox-B3)"
```

**Exit criteria for B3:** every spec §4.7 rule has a passing rejection test; the happy paths (raw + nested) extract with the correct modes.

---

## Wave B → Wave B Cleanup (orchestrator)

Same as Wave A: bundle review findings, close glovebox-B1/B2/B3.

---

## Wave C: Handler + Finalize + Wire-Up + Integration Test

### Task C1: Finalize (sha256 verify + dispatch + atomic rename)

**Files:**
- Create: `internal/ingest/archives/finalize.go`
- Create: `internal/ingest/archives/finalize_test.go`

**Spec references:** spec 13 §4.6 (finalize procedure), §4.8 (receipt JSON), §5.2 (atomicity).

Key behavior:
1. sha256 of received bytes == metadata sha256 (or 400).
2. on-disk size == size_bytes (or 400).
3. Media dispatch: raw → rename tmp into `.finalize/raw/<archive_filename>`; tar → call `Untar` against `.finalize/tree/`.
4. Write `metadata.json` LAST inside `.finalize/`.
5. `os.Rename(".tmp-archives/<id>.finalize", "archives/<archive_id>")` is the single atomic publish step.
6. Remove tmp file + state entry on success.

Sketch:

```go
// internal/ingest/archives/finalize.go
package archives

import (
    "context"
    "crypto/subtle"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "os"
    "path/filepath"
    "time"
)

type FinalizeReceipt struct {
    ArchiveID         string     `json:"archive_id"`
    ReceivedAt        time.Time  `json:"received_at"`
    DeliveredBy       string     `json:"delivered_by"`
    MediaType         string     `json:"media_type"`
    SizeBytes         int64      `json:"size_bytes"`
    SHA256            string     `json:"sha256"`
    SHA256Verified    bool       `json:"sha256_verified"`
    StagedPath        string     `json:"staged_path"`
    EntriesExtracted  int        `json:"entries_extracted"`
    RawFilename       string     `json:"raw_filename,omitempty"`
}

type FinalizeConfig struct {
    StagingRoot string  // <staging_root>; archives/ and .tmp-archives/ live under it
}

var (
    ErrSHAMismatch  = errors.New("sha256 mismatch")
    ErrSizeMismatch = errors.New("size mismatch")
)

// Finalize takes a fully-uploaded UploadState and stages it under
// <StagingRoot>/archives/<archive_id>/. On success returns the receipt
// JSON; on failure returns an error and cleans up the tmp + finalize
// dir. The upload state's Hasher MUST already reflect all received bytes.
func Finalize(ctx context.Context, st *UploadState, cfg FinalizeConfig) (*FinalizeReceipt, error) {
    // 1. sha256 verify (constant-time compare against the claimed hex).
    computed := st.Hasher.Sum(nil)
    claimedHex := st.Meta.SHA256
    claimedBytes, _ := hex.DecodeString(claimedHex)
    if subtle.ConstantTimeCompare(computed, claimedBytes) != 1 {
        cleanupTmp(cfg.StagingRoot, st.ID)
        return nil, ErrSHAMismatch
    }
    // 2. size verify.
    tmpPath := filepath.Join(cfg.StagingRoot, ".tmp-archives", st.ID)
    fi, err := os.Stat(tmpPath)
    if err != nil { return nil, fmt.Errorf("stat tmp: %w", err) }
    if fi.Size() != st.UploadLength {
        cleanupTmp(cfg.StagingRoot, st.ID)
        return nil, ErrSizeMismatch
    }
    // 3. Build .finalize/ dir.
    finalizeDir := filepath.Join(cfg.StagingRoot, ".tmp-archives", st.ID+".finalize")
    if err := os.MkdirAll(finalizeDir, 0700); err != nil {
        return nil, fmt.Errorf("mkdir finalize: %w", err)
    }
    // 4. Dispatch by media shape.
    receipt := &FinalizeReceipt{
        ArchiveID:      st.ArchiveID,
        ReceivedAt:     time.Now().UTC(),
        DeliveredBy:    st.SourceID,
        MediaType:      st.Meta.MediaType,
        SizeBytes:      st.UploadLength,
        SHA256:         claimedHex,
        SHA256Verified: true,
        StagedPath:     filepath.Join("archives", st.ArchiveID),
    }
    switch st.Meta.Shape() {
    case MediaRaw:
        rawDir := filepath.Join(finalizeDir, "raw")
        if err := os.MkdirAll(rawDir, 0700); err != nil {
            os.RemoveAll(finalizeDir); cleanupTmp(cfg.StagingRoot, st.ID)
            return nil, fmt.Errorf("mkdir raw: %w", err)
        }
        if err := os.Rename(tmpPath, filepath.Join(rawDir, st.Meta.ArchiveFilename)); err != nil {
            os.RemoveAll(finalizeDir); cleanupTmp(cfg.StagingRoot, st.ID)
            return nil, fmt.Errorf("rename tmp -> raw: %w", err)
        }
        receipt.RawFilename = st.Meta.ArchiveFilename
    case MediaTar:
        treeDir := filepath.Join(finalizeDir, "tree")
        if err := os.MkdirAll(treeDir, 0700); err != nil {
            os.RemoveAll(finalizeDir); cleanupTmp(cfg.StagingRoot, st.ID)
            return nil, fmt.Errorf("mkdir tree: %w", err)
        }
        f, err := os.Open(tmpPath)
        if err != nil {
            os.RemoveAll(finalizeDir); cleanupTmp(cfg.StagingRoot, st.ID)
            return nil, fmt.Errorf("open tmp for untar: %w", err)
        }
        n, _, uerr := Untar(f, UntarConfig{DestDir: treeDir, UploadLength: st.UploadLength})
        f.Close()
        if uerr != nil {
            os.RemoveAll(finalizeDir); cleanupTmp(cfg.StagingRoot, st.ID)
            return nil, fmt.Errorf("untar: %w", uerr)
        }
        receipt.EntriesExtracted = n
        // Tmp file is no longer needed.
        os.Remove(tmpPath)
    }
    // 5. Write metadata.json LAST (after content is in place).
    metaPath := filepath.Join(finalizeDir, "metadata.json")
    metaBytes, _ := json.MarshalIndent(receipt, "", "  ")
    if err := atomicWrite(metaPath, metaBytes); err != nil {
        os.RemoveAll(finalizeDir)
        return nil, fmt.Errorf("write metadata: %w", err)
    }
    // 6. Atomic publish.
    finalPath := filepath.Join(cfg.StagingRoot, "archives", st.ArchiveID)
    if err := os.Rename(finalizeDir, finalPath); err != nil {
        os.RemoveAll(finalizeDir)
        return nil, fmt.Errorf("rename to final: %w", err)
    }
    return receipt, nil
}

func cleanupTmp(stagingRoot, uploadID string) {
    os.Remove(filepath.Join(stagingRoot, ".tmp-archives", uploadID))
    os.RemoveAll(filepath.Join(stagingRoot, ".tmp-archives", uploadID+".finalize"))
}

func atomicWrite(path string, data []byte) error {
    tmp := path + ".tmp"
    if err := os.WriteFile(tmp, data, 0600); err != nil { return err }
    return os.Rename(tmp, path)
}
```

Tests: happy raw, happy tar, sha256 mismatch (rejects + cleans up), size mismatch (rejects + cleans up), tar safety violation propagated (rejects + cleans up), atomic rename failure simulated by pre-existing target.

```bash
go test ./internal/ingest/archives/ -run TestFinalize -v -count=1
git add internal/ingest/archives/finalize.go internal/ingest/archives/finalize_test.go
git commit -m "ingest/archives: finalize (sha256 + dispatch + atomic rename) (glovebox-C1)"
```

---

### Task C2: tus.io HTTP Handler

**Files:**
- Create: `internal/ingest/archives/handler.go`
- Create: `internal/ingest/archives/handler_test.go`

**Spec references:** spec 13 §4.1 (method table), §4.3 (pre-flight idempotency), §4.4 (PATCH), §5.2 (st_dev check).

Mounts at `/v1/archives`. Required handlers:

- `OPTIONS /v1/archives` → 200 + `Tus-Version`, `Tus-Max-Size`, `Tus-Extension: creation,termination,checksum`
- `POST /v1/archives` → idempotency check first; on miss, store.Create + allocate `.tmp-archives/<id>` → 201
- `HEAD /v1/archives/<id>` → 200 + `Upload-Offset` + `Upload-Length` + `Tus-Expires`
- `PATCH /v1/archives/<id>` → acquire per-upload mutex (TryLock; 409 upload_busy on fail) → append to tmp file + update hasher + update offset → if offset == length, call `Finalize` → 204
- `DELETE /v1/archives/<id>` → cleanup tmp + store.Remove → 204
- `GET /v1/archives/<archive_id>` → read `archives/<id>/metadata.json` → 200 with body, OR 404

Implementation is large — split into per-method helpers. Tests use `httptest.NewRecorder` + `httptest.NewRequest` against the handler.

Per-method test list (failing-first then implementation per spec):
- OPTIONS returns correct headers.
- POST without Authorization → 401.
- POST with bad token → 401.
- POST with valid token + valid metadata → 201 + Location.
- POST with reserved metadata key → 400.
- POST with size_bytes != Upload-Length → 400.
- POST with unknown media_type → 400.
- POST with archive_id colliding (same source-id, same sha256) → 303 + Location.
- POST with archive_id colliding (same source-id, different sha256) → 409.
- POST with archive_id colliding (different source-id) → 409 (no leak).
- POST when per-source cap exceeded → 429.
- POST when global cap exceeded → 429.
- HEAD on unknown upload-id → 404.
- HEAD on upload-id from different source-id → 404 (binding check).
- HEAD happy path → 200 + Upload-Offset + Upload-Length + Tus-Expires.
- PATCH wrong Content-Type → 415.
- PATCH offset mismatch → 409 offset_mismatch.
- PATCH concurrent (mutex held) → 409 upload_busy.
- PATCH happy path appending bytes → 204 + new Upload-Offset.
- PATCH that completes the upload → triggers Finalize → 204 with Upload-Offset == Upload-Length.
- PATCH that triggers sha256 mismatch in Finalize → 400.
- DELETE happy path → 204; tmp file + state removed.
- GET on finalized archive → 200 + receipt.
- GET on non-finalized → 404.
- Hard-cap 503 when global storage_pct over threshold (use a fake quota provider).

This is the biggest single task in the plan. Estimated 600-800 LOC + ~800 LOC of tests. May be split into C2a (POST + idempotency + binding), C2b (HEAD/PATCH/DELETE), C2c (GET + OPTIONS) if the implementer requests.

```bash
go test ./internal/ingest/archives/ -count=1 -v
git add internal/ingest/archives/handler.go internal/ingest/archives/handler_test.go
git commit -m "ingest/archives: tus.io HTTP handler (glovebox-C2)"
```

**Exit criteria for C2:** every spec §4.1 method + every spec §4.4 PATCH rule + every spec §4.3 idempotency branch has a passing test. The handler depends on Wave A + B already landed.

---

### Task C3: Server Wire-Up + Quota Goroutine + Cleanup Goroutine

**Files:**
- Modify: `internal/ingest/server.go`
- Create: `internal/ingest/archives/quota.go`
- Create: `internal/ingest/archives/quota_test.go`
- Create: `internal/ingest/archives/cleanup.go`
- Create: `internal/ingest/archives/cleanup_test.go`

**Spec references:** spec 13 §5.2 (st_dev startup check), §5.4 (storage measurement), §5.5 (cleanup).

Steps:
1. Quota goroutine: walks `archives/` + `.tmp-archives/`, sums by source-id; updates gauges; computes hard-cap pct. Exposed via `func StorageStats() (totalPct float64, perSource map[string]int64)`.
2. Cleanup goroutine: scans `.tmp-archives/` on a 60-min interval (configurable); deletes orphans per §5.5 thresholds.
3. server.go: at startup, runs the §5.2 st_dev check. If pass, mounts the `/v1/archives*` handler tree (created by C2). If fail, registers a fallback handler that returns 503 for `/v1/archives*` and logs the reason at boot.
4. The auth middleware (from Wave A) wraps the archives handler.

Tests:
- StorageStats over a fixture staging directory returns correct sums.
- Cleanup deletes a tmp file older than 72h; preserves a fresh one.
- Cleanup deletes a `.finalize/` dir older than 1h; preserves a fresh one.
- st_dev startup check: same-FS passes; different-FS fails and the handler returns 503.

```bash
go test ./internal/ingest/archives/ -count=1 -v
go vet ./...
git add internal/ingest/server.go internal/ingest/archives/quota.go ... internal/ingest/archives/cleanup.go ...
git commit -m "ingest: archive listener wire-up + quota + cleanup (glovebox-C3)"
```

---

### Task C4: End-to-End Integration Test

**Files:**
- Create: `internal/ingest/archives/integration_test.go`

Drives the full handler from a real tus client (use `https://github.com/eventials/go-tus` OR a hand-rolled client in `_test.go` — implementer's choice; hand-rolled is fine because the protocol surface is small):

1. Set up a `httptest.Server` wrapping the assembled handler (auth middleware + archives handler + a real TokenStore + a real Store + a tmpfs staging dir).
2. Upload a 50 MB synthetic mbox with valid sha256 → expect 201, then PATCHes, then 204. Assert the finalized `archives/<id>/raw/<filename>` exists with correct content.
3. Upload a 5-file tarball → assert `archives/<id>/tree/` contains all 5 files with mode 0600.
4. Upload with a corrupted sha256 (claim X, send bytes that hash to Y) → assert 400 at finalize + tmp dir cleaned.
5. Upload a tarball with a `..` path → assert 400 + tmp dir cleaned + no extracted files.
6. Resume an interrupted upload: PATCH 25 MB, simulate connection drop, HEAD to read offset (`Upload-Offset = 25 MB`), PATCH the rest, expect successful finalize.
7. Two clients with valid tokens uploading concurrently with the SAME archive_id but different source-ids → both succeed (no cross-source collision since idempotency is scoped per spec §4.3).

```bash
go test ./internal/ingest/archives/ -run TestIntegration -v -count=1
git add internal/ingest/archives/integration_test.go
git commit -m "ingest/archives: end-to-end integration test (glovebox-C4)"
```

---

## Wave C → Wave C Cleanup (orchestrator)

Same pattern.

---

## Wave D: Helm + Smoke Test

### Task D1: Helm Chart Updates

**Files:**
- Modify: `charts/glovebox/values.yaml`
- Create: `charts/glovebox/templates/archive-tokens-externalsecret.yaml`
- Create: `charts/glovebox/templates/archive-networkpolicy.yaml`

Steps:
1. Add `ingest.auth:` + `ingest.archives:` blocks to `values.yaml` matching spec 13 §8.4 + spec 10 §9 defaults verbatim (lruCapacity 1000, globalRateLimit 100/60s, trustedProxyCIDRs placeholder, etc.).
2. Add example ExternalSecret template that pulls `secret/glovebox/ingest-tokens/recognizer` from Vault into a K8s Secret. Operator-facing example; gated by `.Values.ingest.auth.enabled`.
3. Add a NetworkPolicy template granting TCP/9091 ingress to the openclaw-recognizer namespace label.
4. Verify:

```bash
helm template charts/glovebox/ --set ingest.auth.enabled=true --set ingest.archives.enabled=true | grep -E "^kind:" | sort | uniq -c
helm lint charts/glovebox/ --set ingest.auth.enabled=true --set ingest.archives.enabled=true
```

Both must run clean. Resource count check confirms the new ExternalSecret + NetworkPolicy render only when enabled.

```bash
git add charts/glovebox/
git commit -m "chart: ingest auth + archives Helm wiring (glovebox-D1)"
```

---

### Task D2: Smoke Test Script

**Files:**
- Create: `scripts/archive-smoke-test.sh`

Bash script:
1. Skip if `docker` not on PATH.
2. Build the glovebox image: `docker build -t glovebox-archive:smoke -f Dockerfile .`
3. Run the container with a tmpfs staging dir, a single hard-coded test token, NO Vault (use a `FAKE_TOKEN=<value>` env var for the smoke test build OR a dev-only `--no-vault` flag).
4. Generate a 12 GiB sparse mbox file: `truncate -s 12G /tmp/smoke.mbox; sha256sum /tmp/smoke.mbox > /tmp/smoke.sha256`.
5. Drive a tus upload from the host against the container's port. POST to initiate, loop PATCH chunks of 64 MiB, final 204.
6. Assert `archives/<archive_id>/raw/smoke.mbox` exists in the container's staging volume with the expected size and sha256.
7. Cleanup: kill container, remove `/tmp/smoke.mbox`.

```bash
chmod +x scripts/archive-smoke-test.sh
git add scripts/archive-smoke-test.sh
git update-index --chmod=+x scripts/archive-smoke-test.sh
git commit -m "scripts: 12 GiB archive smoke test (glovebox-D2)"
```

This is the bead acceptance criterion check.

---

## Wave D → Wave D Cleanup (orchestrator)

Same pattern.

---

## Final Verification

After all four waves close:

```bash
go vet ./...
go test ./... -count=1
helm lint charts/glovebox/ --set ingest.archives.enabled=true --set ingest.auth.enabled=true
helm template charts/glovebox/ --set ingest.archives.enabled=true --set ingest.auth.enabled=true | grep -E "^kind:" | sort | uniq -c
# (if docker available) bash scripts/archive-smoke-test.sh
```

Then dispatch ONE final security review against the merged result before declaring the work shippable. The security reviewer's iteration-1 list (spec-level, items #1-#18) becomes the implementation-level checklist for this final pass.

Close glovebox-p1zx, the spec 10 bead (if separately tracked), and the spec 09 watcher-mode follow-up `glovebox-c9zt` remains open as a separate work item (the v1 archive delivery ships without spec 09's watcher mode; archives stage to disk but require manual processing until c9zt lands).

---

## Cross-References

- **Spec 13** (`docs/specs/13-archive-delivery-design.md`) — the contract this plan implements.
- **Spec 10** (`docs/specs/10-external-ingest-auth-design.md`) — the bearer-token auth layer Wave A implements.
- **Spec 12 §5** (`docs/specs/12-schoology-connector-design.md`) — Vault K8s auth precedent referenced by Task A1.
- **Spec 06 §5.2** + **§8.3** — Identity block + audit log shape referenced by Task A3 and spec 10 §6.
- **Bead p1zx** — the originating recognizer-team ask. Acceptance criteria: spec written ✓, endpoint accepts multi-GB bodies (verified in D2), bearer-token enforced (Wave A), idempotent on archive_id + sha256 (C2 tests), PVC ≥ 50 GiB (D1), NetworkPolicy in place (D1), Vault path documented (spec 10 §3.1), smoke test (D2).
- **Bead c9zt** — spec 09 watcher mode follow-up; NOT in scope of this plan but required before automatic mbox processing.
