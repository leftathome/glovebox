# Connector Authentication and Data Provenance -- Design Specification

**Version 1.1 -- March 2026**

*This document specifies authentication patterns, identity propagation, and
data provenance for glovebox connectors. It extends specs 04 and 05.*

---

## 1. Purpose

As glovebox expands beyond unauthenticated sources (RSS) and simple credential
sources (IMAP) to OAuth-based APIs (GitHub, GitLab, LinkedIn, etc.), the
connector library needs standardized support for:

1. **Token lifecycle management** -- acquiring, refreshing, and persisting OAuth
   tokens so connector authors don't each reinvent this
2. **Identity propagation** -- capturing who authenticated to a source and
   carrying that identity through the pipeline to agent workspaces and audit logs
3. **Multi-tenant support** -- separating content by operator-defined tenants
   when multiple users share a glovebox instance
4. **Unified item rules** -- a single config block that determines both routing
   destination and metadata tags per item

## 2. Auth Pattern Matrix

| Connector | Primary Auth | Token Refresh? | Webhook Verify | Identity Source |
|-----------|-------------|----------------|----------------|-----------------|
| GitHub | PAT or GitHub App (JWT + installation token) | App: yes (1hr) | HMAC-SHA256 | Token owner / app installation |
| GitLab | PAT or OAuth 2.0 | OAuth: yes | Secret token header | Token owner |
| Jira | API token (basic) or OAuth 2.0 (3LO) | OAuth: yes | Shared secret | Atlassian account |
| Trello | API key + token | No (long-lived) | None (callback URL) | Trello member |
| LinkedIn | OAuth 2.0 (3-legged + PKCE) | Yes (60-day refresh) | N/A (poll only) | LinkedIn member |
| Meta | OAuth 2.0 (Facebook Login) | Yes (long-lived exchange) | App secret HMAC | Facebook/IG user |
| Bluesky | App password or OAuth (DPoP) | DPoP: yes | N/A (firehose) | DID (AT Protocol) |
| X | OAuth 2.0 + PKCE | Yes (2hr access tokens) | CRC challenge (HMAC-SHA256) | X user ID |

**Key observations:**
- 6 of 8 connectors need OAuth token refresh
- 3 of 8 need webhook signature verification (HMAC-SHA256 variants)
- All need identity extraction from their authentication context

## 3. TokenSource Interface

```go
type TokenSource interface {
    Token(ctx context.Context) (string, error)
}
```

Connectors call `Token()` before each API request. The interface is deliberately
simple -- it returns a bearer token string. Three implementations:

### 3.1 StaticTokenSource

Wraps a string value (from env var). For PATs, API keys, app passwords. No
refresh, no persistence. If the token is invalid, the connector gets a 401 from
the upstream API and returns a `PermanentError`.

### 3.2 RefreshableTokenSource

Wraps an OAuth2 configuration and a token file. Thread-safe: concurrent
`Token()` calls during refresh are serialized via `sync.Mutex` -- one goroutine
performs the refresh while others block and receive the new token.

**Behavior:**

1. On first call, loads token from file in state directory
2. If token file is missing, returns `PermanentError` with message:
   `"token file not found at <path>; run 'glovebox-auth setup <provider>' to authenticate"`
3. If token is valid (not expired, with 30-second buffer), returns it
4. If expired, uses the refresh token to obtain a new access token
5. Persists the new token atomically (temp file + rename, same pattern as
   checkpoint)
6. If refresh fails with 401 or `invalid_grant`, returns `PermanentError`
   (operator must re-authenticate)

**OAuth2 configuration:**

```go
type OAuthConfig struct {
    ClientID     string   // from env var, e.g. GITHUB_CLIENT_ID
    ClientSecret string   // from env var, e.g. GITHUB_CLIENT_SECRET
    TokenURL     string   // provider's token endpoint
    Scopes       []string // requested scopes (for refresh requests)
}
```

These fields come from environment variables. The connector's `main.go` reads
them and constructs the `OAuthConfig`. The library does not parse a config file
for OAuth settings -- only the token file.

**Token file format** (stored at `<stateDir>/token.json`):

```json
{
    "access_token": "gho_xxxxxxxxxxxx",
    "refresh_token": "ghr_xxxxxxxxxxxx",
    "token_type": "bearer",
    "expiry": "2026-03-30T12:00:00Z"
}
```

Each connector gets its own `stateDir` (per spec 05, Section 6.1), so token
files do not collide with each other or with the checkpoint (`state.json`).

Atomic persistence uses the same temp-file-plus-rename pattern as the checkpoint
to prevent corruption on crash.

### 3.3 GitHubAppTokenSource

Special case for GitHub Apps, which are common enough to warrant first-class
support:

1. Loads private key from env var or file
2. Generates a JWT signed with the private key (10-minute validity)
3. Exchanges the JWT for an installation access token (1-hour validity)
4. Caches and refreshes automatically
5. Thread-safe (same serialization as RefreshableTokenSource)

### 3.4 Credential Management

Secrets (client secrets, private keys, API keys) come from environment variables
or mounted files, injected by the deployment layer (K8s secrets, 1Password
Connect, `op run`). The connector library has no opinion about where secrets are
stored -- it only manages the OAuth token lifecycle after initial authentication.

### 3.5 Device Code Flow (Out of Scope)

Interactive OAuth flows (device code, authorization code with browser redirect)
are out of scope for this design. They will be addressed in a separate bead as a
CLI tool (`glovebox-auth setup <provider>`) that performs the interactive flow
and writes the initial token file that `RefreshableTokenSource` then manages.

## 4. Webhook Signature Verification

Shared helper for connectors that receive webhooks:

```go
func VerifyHMAC(payload []byte, signature string, secret []byte, algo string) bool
```

- `algo`: `"sha256"` (covers GitHub, Meta, X)
- Handles hex-encoded and base64-encoded signatures
- Constant-time comparison to prevent timing attacks

GitLab uses a simpler secret-header comparison that doesn't need this helper.

## 5. Identity and Provenance Schema

### 5.1 New Fields in metadata.json

These fields extend the schema defined in spec 04, Section 5.2:

```json
{
    "source": "github",
    "sender": "octocat",
    "subject": "Fix login bug (#42)",
    "timestamp": "2026-03-30T12:00:00Z",
    "destination_agent": "messaging",
    "content_type": "text/plain",
    "ordered": false,
    "auth_failure": false,

    "identity": {
        "account_id": "steve@github",
        "provider": "github",
        "auth_method": "oauth",
        "scopes": ["repo", "read:org"],
        "tenant": "steve"
    },

    "tags": {
        "team": "platform",
        "env": "production"
    }
}
```

### 5.2 Identity Object

| Field | Type | Required | Max Length | Description |
|-------|------|----------|-----------|-------------|
| `account_id` | string | No | 1024 | Stable identifier for who authenticated. Format is connector-defined (email, username, numeric ID). May be empty if the auth method does not expose the token owner (e.g., some PATs). |
| `provider` | string | Yes | 64 | Service name the identity came from (github, gitlab, imap, etc.) |
| `auth_method` | string | Yes | 64 | One of: `pat`, `oauth`, `api_key`, `app_password`, `github_app`, `none`. Not validated against this enum -- new methods may be added. |
| `scopes` | []string | No | 64 each, 32 max | OAuth scopes or permissions carried by the token |
| `tenant` | string | No | 256 | Operator-defined string for multi-tenant routing. Set in connector config. |

**Design decisions:**

- `identity` is an optional nested object. If omitted entirely, the item has no
  provenance (acceptable for unauthenticated sources like RSS).
- If `identity` is present, only `provider` and `auth_method` are required.
  `account_id` is optional because some auth methods (PATs without a "whoami"
  API call) cannot determine the token owner without extra work. Connectors
  SHOULD populate `account_id` when available.
- `tenant` is a configuration-level field, not derived from the token. The
  operator assigns tenants in their deployment config.
- Glovebox validates structure (field lengths, no control characters per spec 04
  Section 5.4 rules) but does not interpret identity semantics.
- Audit logs include the full `identity` block with no hashing. The audit log
  is already access-controlled.

### 5.3 Tags

`tags` is an optional `map[string]string` on each item. Tags are resolved from
the unified rules config (see Section 6) and stamped on items by the staging
writer at `Commit()` time.

**Validation constraints:**
- Tag keys: max 64 characters, alphanumeric plus `-`, `_`, `.`
- Tag values: max 1024 characters, no control characters
- Maximum 32 tags per item
- Glovebox validates these constraints and rejects items that violate them

Tags from rules are first-match-wins (same as routing). Tags do NOT accumulate
across multiple rules -- only the first matching rule's tags are applied.

Connectors may also set tags programmatically via `ItemOptions.Tags`. Per-item
tags merge with rule-matched tags, with per-item winning on key conflict.

### 5.4 Identity Merge Semantics

Identity fields come from two sources:
1. **Config-level** (`BaseConfig.Identity`): `tenant`, `provider`, `auth_method`
2. **Per-item** (`ItemOptions.Identity`): `account_id`, `scopes`, plus any
   override of config fields

The library merges them at `Commit()` time. Config-level fields provide defaults;
per-item fields override on conflict. The merged identity is what appears in
`metadata.json`.

**Valid combinations:**
- Config sets `provider`, `auth_method`, `tenant`. Connector code sets
  `account_id` and `scopes` per item. Result: full identity.
- Config sets `provider: "rss"`, `auth_method: "none"`. No per-item identity.
  Result: identity with no `account_id` (valid, since `account_id` is optional).
- No config identity, no per-item identity. Result: `identity` field omitted
  from metadata.json entirely.

## 6. Unified Rules Config

### 6.1 Motivation

The original connector config used a `routes` array for destination routing.
With the addition of tags, maintaining separate arrays for routing and tagging
using the same match keys would be redundant. A single `rules` array determines
both destination and tags per item.

### 6.2 New Format

```json
{
    "rules": [
        {
            "match": "feed:engadget",
            "destination": "media",
            "tags": {"team": "platform", "category": "news"}
        },
        {
            "match": "folder:INBOX",
            "destination": "messaging",
            "tags": {"priority": "high"}
        },
        {
            "match": "*",
            "destination": "general",
            "tags": {"env": "production"}
        }
    ],
    "identity": {
        "tenant": "steve",
        "provider": "github",
        "auth_method": "oauth"
    }
}
```

### 6.3 Semantics

- Same first-match-wins evaluation as the current router
- `destination` is required per rule (same as current `routes`)
- `tags` is optional per rule (omit for rules that only need routing)
- Only the first matching rule applies -- tags do NOT accumulate across rules
- `*` matches anything (wildcard / catch-all)
- If no rule matches and no wildcard exists, the item is skipped (same behavior
  as current router: warning logged, checkpoint not advanced)

### 6.4 Backward Compatibility

For a smooth transition from v0.1.0, the library accepts both `routes` and
`rules` in the config file:

```go
type BaseConfig struct {
    Rules    []Rule `json:"rules"`
    Routes   []Rule `json:"routes"` // deprecated, accepted as fallback
    Identity *ConfigIdentity `json:"identity,omitempty"`
}
```

If `rules` is empty and `routes` is non-empty, the library uses `routes` and
logs a deprecation warning at startup. If both are present, `rules` takes
precedence. This fallback will be removed in a future major version.

### 6.5 Library Changes

`Router` is refactored to `RuleMatcher`:

```go
type Rule struct {
    Match       string            `json:"match"`
    Destination string            `json:"destination"`
    Tags        map[string]string `json:"tags,omitempty"`
}

type MatchResult struct {
    Destination string
    Tags        map[string]string
}

type RuleMatcher struct { ... }

func NewRuleMatcher(rules []Rule) *RuleMatcher
func (rm *RuleMatcher) Match(key string) (MatchResult, bool)
```

`ConnectorContext` changes:

```go
type ConnectorContext struct {
    Writer  *StagingWriter
    Matcher *RuleMatcher   // was Router *Router
    Metrics *Metrics
}
```

### 6.6 Config-Level Identity

The `identity` block in the connector config uses a subset of the full identity
schema:

```go
type ConfigIdentity struct {
    AccountID  string `json:"account_id,omitempty"`
    Provider   string `json:"provider,omitempty"`
    AuthMethod string `json:"auth_method,omitempty"`
    Tenant     string `json:"tenant,omitempty"`
}
```

All fields are optional at config level. They provide defaults that are merged
with per-item identity at `Commit()` time (see Section 5.4).

## 7. Connector Library Plumbing

### 7.1 Updated ItemOptions

```go
type ItemOptions struct {
    Source           string
    Sender           string
    Subject          string
    Timestamp        time.Time
    DestinationAgent string
    ContentType      string
    Ordered          bool
    AuthFailure      bool
    Identity         *Identity         // new
    Tags             map[string]string // new
}
```

Tags from `ItemOptions.Tags` merge with tags from the matched rule (per-item
wins on conflict). The connector calls `Matcher.Match(key)` to get the
`MatchResult`, sets `ItemOptions.DestinationAgent = result.Destination`, and
the staging writer handles tag merging at `Commit()` time.

### 7.2 StagingWriter Tag and Identity Flow

1. Connector calls `matcher.Match(key)` to get `MatchResult`
2. Connector creates item: `writer.NewItem(ItemOptions{DestinationAgent: result.Destination, Identity: &identity, Tags: perItemTags, ...})`
3. `StagingWriter` stores the `MatchResult.Tags` -- set via a new method:
   `writer.SetRuleTags(result.Tags)` called once after match, or passed through
   `NewItem`. Decision: pass rule tags through a `RuleTags` field on
   `ItemOptions` to keep the API simple:

```go
type ItemOptions struct {
    // ... existing fields ...
    Identity *Identity
    Tags     map[string]string // per-item tags from connector code
    RuleTags map[string]string // tags from RuleMatcher.Match() result
}
```

4. At `Commit()` time, the staging writer:
   a. Merges `RuleTags` with `Tags` (per-item `Tags` win on conflict)
   b. Merges config-level `Identity` with per-item `Identity` (per-item wins)
   c. Writes the merged identity and tags into `metadata.json`

### 7.3 Runner Changes

The runner reads `BaseConfig.Identity` from the config file and passes it to
`ConnectorContext` (or stores it on `StagingWriter`). The staging writer uses it
as the base for identity merging at `Commit()` time.

## 8. Glovebox Changes

These require code changes to the glovebox internals:

### 8.1 ItemMetadata Struct

`internal/staging/types.go` -- add fields:

```go
type ItemMetadata struct {
    // ... existing fields ...
    Identity *Identity         `json:"identity,omitempty"`
    Tags     map[string]string `json:"tags,omitempty"`
}
```

### 8.2 Validation

`internal/staging/validate.go` -- add validation for:
- `identity.provider`: max 64 chars, no control chars
- `identity.auth_method`: max 64 chars, no control chars
- `identity.account_id`: max 1024 chars, no control chars (if present)
- `identity.tenant`: max 256 chars, no control chars (if present)
- `identity.scopes`: max 32 entries, each max 64 chars (if present)
- `tags` keys: max 64 chars, alphanumeric plus `-_. `
- `tags` values: max 1024 chars, no control chars
- `tags`: max 32 entries

Both `identity` and `tags` are optional -- omission is valid.

### 8.3 Audit Log

`internal/audit/logger.go` -- add to `AuditEntry`:

```go
type AuditEntry struct {
    // ... existing fields ...
    Identity *staging.Identity  `json:"identity,omitempty"`
    Tags     map[string]string  `json:"tags,omitempty"`
}
```

### 8.4 Passthrough

No changes needed to routing logic. The `routing.RoutePass` and
`routing.RouteQuarantine` functions move the entire item directory (including
metadata.json) to the destination. Identity and tags are preserved because they
are part of the metadata file.

## 9. Spec 04 and 05 Updates Required

### 9.1 Spec 04 (Glovebox Design)

- Section 5.2: add `identity` and `tags` to the metadata.json schema definition
- Section 5.4: add validation rules for identity sub-fields and tags

### 9.2 Spec 05 (Connector Framework)

- Section 2: remove "OAuth token refresh flows" from out-of-scope list
- Section 7: add note that `routes` is superseded by `rules` per this spec
- Section 12: update scaffold generator references for `rules`
- Section 15: update Phase 2 note to reflect that OAuth is now in-scope

## 10. Migration

### 10.1 Version

This is a **v0.2.0** release (breaking API change under semver 0.x). The
`routes` -> `rules` rename and `Router` -> `RuleMatcher` refactor affect the
public Go API.

### 10.2 Config Backward Compatibility

The library accepts both `routes` and `rules` in config JSON (see Section 6.4).
Existing `config.json` files continue to work without changes. A deprecation
warning is logged at startup when `routes` is used.

### 10.3 Code Migration Checklist

Files requiring changes for the `routes` -> `rules` rename:

**Connector library:**
- `connector/route.go` -> `connector/rule.go` (rename types and functions)
- `connector/route_test.go` -> `connector/rule_test.go`
- `connector/runner.go` (BaseConfig, router init)
- `connector/connector.go` (ConnectorContext)
- `connector/staging.go` (ItemOptions, Commit logic)
- `connector/integration_test.go`

**Connectors:**
- `connectors/rss/connector.go`, `main.go`, `config.go`, `config.json`, tests
- `connectors/imap/connector.go`, `main.go`, `config.go`, `config.json`, tests

**Generator:**
- `generator/templates/*.tmpl` (all templates referencing routes/Router)
- `generator/generate_test.go`

**Documentation:**
- `docs/connector-guide.md`
- `docs/deployment.md`
- `AGENTS.md`
- `README.md` (quickstart config example)

### 10.4 Existing Connector Updates

- **RSS**: migrate config to `rules`, set config-level identity with
  `auth_method: "none"`, `provider: "rss"`. No `account_id`.
- **IMAP**: migrate config to `rules`, set config-level identity with
  `auth_method: "app_password"`, `provider: "imap"`. Connector code populates
  `account_id` from `IMAP_USERNAME` per-item.

## 11. Out of Scope

- Device code flow / interactive auth setup CLI (separate bead)
- Secret store integration (deployment layer concern)
- Identity-based routing in glovebox (future feature)
- Tag-based quarantine rules (future feature)
- Hashing or pseudonymizing identity in audit logs (v1 stores plaintext)

## 12. Browser-Session Refresher (Addendum)

### 12.1 Motivation

Spec 06 §3.5 deferred interactive auth flows to "a separate bead as a
CLI tool." That CLI exists today for Schoology -- it is the manual
procedure documented in `docs/AUTH-RECOVERY.md`. The operator runs a
small Go program on a workstation that calls
`schoology-go/auth.Login`, watches a visible Chromium window, writes
the resulting 5-field JSON into a Vault KV v2 path, and waits for
External Secrets Operator to sync the new session into the cluster.

This addendum specifies an in-cluster replacement for that workflow.
Schoology's parent-account session cookies expire about every 14 days,
and the manual procedure is the single largest reliability hazard for a
long-running Schoology connector deployment: an unattended 14-day
window will, eventually, expire on a holiday and CrashLoopBackOff the
pod until somebody is around to re-paste cookies. The refresher
automates the recovery loop entirely.

This is **Schoology-specific scope.** Other connectors with similar
browser-session patterns (none today) could reuse the design but no
abstraction layer is being built here -- spec 12 is the only consumer.

### 12.2 Preconditions and Scope

This addendum applies when ALL of the following are true:

1. The connector authenticates via browser-session cookies (no OAuth /
   PAT / app password surface).
2. The IdP serves a **native username/password form** with no SSO
   redirect (Google, Microsoft Entra, Okta) and **no MFA**.
3. The IdP does not employ anti-automation defenses that fingerprint
   headless Chromium (Cloudflare Turnstile, datadome, PerimeterX, etc.).
4. A Vault KV v2 path exists holding the source credentials and a
   separate Vault KV v2 path holds the synced session JSON (both
   reached via the cluster's existing Vault + ExternalSecret pipeline).

The Schoology connector satisfies all four for the parent-account
flow as of the spec 12 implementation (see spec 12 §5).

If any precondition becomes false (e.g., the district migrates to
Google SSO, or Schoology adds MFA enforcement), the refresher cannot
operate and the manual `AUTH-RECOVERY.md` procedure remains the
fallback.

### 12.3 Components

The refresher comprises four artifacts:

1. **`connectors/schoology-auth-refresher/main.go`** -- a small Go
   binary (~330 lines, including test seams) that:
   - Reads `SCHOOLOGY_HOST`, `SCHOOLOGY_USERNAME`, `SCHOOLOGY_PASSWORD`
     from environment variables (projected by K8s from a Secret that
     ESO synced from `secret/glovebox/schoology/<household>/credentials`).
   - Reads `VAULT_ADDR`, `VAULT_K8S_ROLE`, `VAULT_KV_MOUNT` (default
     `secret`), and `VAULT_SESSION_PATH`
     (e.g. `glovebox/schoology/<household>/session`) from the
     environment.
   - Authenticates to Vault using the Kubernetes auth method via the
     pod's projected ServiceAccount token (no static Vault token in
     the environment).
   - Calls
     `schoology-go/auth.LoginWithPassword(ctx, host, user, pass)`.
   - PUTs the resulting 5-field `*auth.Credentials` JSON to the
     named Vault KV v2 path via `github.com/hashicorp/vault/api`'s
     `KVv2(mount).Put(ctx, path, ...)`.
   - Exits 0 on success, non-zero on any failure. K8s Job semantics
     handle retry + backoff.
2. **`connectors/schoology-auth-refresher/Dockerfile`** -- a multi-
   stage image. Stage 1 builds the Go binary against the project's Go
   toolchain (matches `connectors/*/Dockerfile` conventions); stage 2
   is a minimal Debian-slim runtime that includes a Chromium binary
   so `go-rod` doesn't download one at runtime in-cluster. Distroless
   is tempting but doesn't ship Chromium's runtime deps; Debian-slim
   + `chromium` adds ~150 MiB but matches what the manual flow does
   on an operator's workstation.
3. **`charts/glovebox/templates/schoology-auth-refresher-cronjob.yaml`**
   -- a Helm-templated `CronJob` running every 12 days
   (`schedule: "0 6 */12 * *"`) roughly 48 hours before the 14-day
   session expiry window. Job `backoffLimit: 3`,
   `activeDeadlineSeconds: 600` (10 minutes), `restartPolicy: Never`,
   plus a `podFailurePolicy` that fails-fast on exit codes 2/3/4 so
   the Job controller doesn't retry bad-credentials past the lockout
   threshold (see §12.5). Pod uses a dedicated ServiceAccount bound
   to a Vault K8s auth role (`glovebox-schoology-refresher`) with
   policy granting `create + update` on the session KV path.
   Pod-level resources: 256Mi RAM request / 512Mi limit; 200m CPU
   request / 1000m limit (Chromium is bursty).
4. **`charts/glovebox/templates/schoology-auth-refresher-externalsecret.yaml`**
   -- two Helm-templated `ExternalSecret` resources in one file. The
   first materialises the refresher Job's source credentials Secret
   from `secret/glovebox/schoology/<household>/credentials` (KV v2
   path holding `username` + `password` + `host`). The second
   materialises the connector's session Secret from
   `secret/glovebox/schoology/<household>/session` (the path the
   refresher writes). Both use the cluster's existing Vault
   `ClusterSecretStore`. Owning both ESes in one template keeps the
   round-trip (write at Vault → read by connector) co-located.

### 12.4 Behavior

Per-invocation flow:

1. Validate that all required environment variables are present.
   Missing or empty values exit with code 2 (`config error`).
2. Authenticate to Vault via the K8s auth method using the pod's
   projected ServiceAccount token. On failure, exit code 1
   (`transient`) -- typically a Vault outage or a misconfigured role.
3. Construct a context with a 5-minute timeout (login + write should
   complete in <2 minutes; the buffer covers Chromium cold-start
   plus DOM-change tolerance).
4. Invoke `auth.LoginWithPassword(ctx, host, user, pass)`. On
   error:
   - `auth.ErrInvalidCredentials` (if the library distinguishes it) ->
     exit code 3 (`bad credentials`). Operator must update the source
     Vault KV path. Do NOT retry.
   - any other error -> exit code 1 (`transient`). The Job's
     `backoffLimit` handles retry.
5. Marshal `*Credentials` to a flat map and write it to
   `<VAULT_KV_MOUNT>/data/<VAULT_SESSION_PATH>` via
   `client.KVv2(mount).Put(ctx, path, data)`.
   - On a 4xx response (permission denied, path malformed): exit code
     4 (`secret write rejected`). Operator intervention required
     (Vault policy, role binding).
   - On 5xx or network error: exit code 1 (`transient`). Retried by
     `backoffLimit`.
6. On success, emit a single structured log line at INFO with the
   Vault path written and the response's `version` (KV v2 returns it)
   and exit 0.

The connector pod re-rolls automatically because the existing
Deployment template carries a checksum annotation on the session
Secret (see AUTH-RECOVERY.md §6).

### 12.5 Lockout Safety

Schoology may rate-limit or lock the account after repeated failed
logins. Two layers of defense:

- **Job-level `backoffLimit: 3`** with exponential backoff
  (`backoffLimitPerIndex` is not used since the Job is not indexed) --
  three failures across a single invocation, then the Job surfaces as
  failed and an operator alert fires. No further attempts happen until
  the next CronJob fire or a manual `kubectl create job --from`.
- **Process-level credential-failure short-circuit**: bad-credentials
  exit (code 3) MUST NOT be retried; the Job's
  `restartPolicy: Never` plus the explicit non-retry classification
  prevents a wrong password from triggering Schoology's lockout
  threshold.

### 12.6 Trigger Modes

v1 supports two triggers:

- **Scheduled (CronJob)** -- every 12 days, autonomous. The
  refresher writes a Kubernetes Event (and a structured log line)
  noting the outcome.
- **Manual (`kubectl create job --from=cronjob/schoology-auth-refresher
  manual-$(date +%s)`)** -- operator runs the same CronJob's pod
  template on demand. Useful for the first deploy, after a
  precondition change (§12.2), or as a workstation-free recovery.

On-401 auto-trigger from the connector (the connector emitting a
"session expired" signal that creates a Job) is deferred. The
12-day cadence plus a 2-day buffer before the 14-day expiry should
keep the connector continuously authenticated; if a one-off Job is
ever needed, a human runs the manual command.

### 12.7 Observability

The refresher must emit, at minimum:

- A structured log line per outcome (`success | bad_credentials |
  transient_failure | secret_write_failed`).
- Stdout/stderr captured by K8s -- already searchable via the cluster's
  log aggregator.
- An exit code matching §12.4 so the cluster's Job-failure alert can
  branch on it (cluster-level alerting wiring is out of scope here).

A connector-side metric (`schoology_session_refreshed_total`) is
deferred to glovebox-f7v3 (telemetry task) -- the refresher Job emits
logs only, and the alerting fabric picks up Job-failure conditions
through existing K8s primitives.

### 12.8 Out of Scope (Addendum)

- Refresher support for non-Schoology connectors. A generic
  "browser-session-refresher" library is not justified by current
  scope (Schoology is the only consumer).
- On-401 auto-triggered refresh (see §12.6 rationale).
- SSO + MFA handling -- Schoology's current parent-account surface
  doesn't require it. Districts that adopt SSO or MFA must fall back
  to AUTH-RECOVERY.md.
- Anti-automation evasion (proxy rotation, stealth profiles,
  fingerprint randomization). Required only if the IdP starts
  blocking the Chrome devtools protocol; out of scope until evidence
  appears.
- Cross-mount Secret writes. v1 writes to one Vault KV v2 mount via
  one K8s-auth role. Multi-tenant support (one refresher per
  household across many Vault namespaces) is a future concern.

### 12.9 Implementation Sizing

This is a **one-task** addendum. The Go binary is ~150 lines; the
Dockerfile is ~20 lines; the K8s YAML totals ~80 lines across two
files. Tests stub `auth.LoginWithPassword` and the Vault HTTP API
(via `httptest.Server`, which the Vault Go client accepts) and cover:
success path, bad-creds exit, transient-error exit, secret-write
rejection.

### 12.10 Connection Model: Remote Browserless (Addendum)

Supersedes the bundled-Chromium runtime described in §12.3 (component 2)
and the Chromium cold-start / `/tmp` notes in §12.4 and the chart.

The refresher no longer ships Chromium inside its own image. Instead it
drives a **remote, cluster-local Browserless browser** over the Chrome
DevTools Protocol via `schoology-go/auth.WithBrowserURL` (requires
`schoology-go >= v0.2.0`). This shrinks the refresher image back to a
distroless `gcr.io/distroless/static-debian12:nonroot` base (the binary
is pure-Go, `CGO_ENABLED=0`) and removes the ~150 MiB Debian-slim +
`chromium` layer, while centralizing browser-runtime upkeep in the
shared Browserless deployment.

Configuration (env vars, mapped from chart values):

- `BROWSERLESS_URL` (from `schoologyAuthRefresher.browserless.url`,
  e.g. `ws://browserless.browserless.svc.cluster.local:3000`) -- the rod
  ControlURL. When set, the binary calls `auth.WithBrowserURL`.
- `BROWSERLESS_TOKEN` (from
  `schoologyAuthRefresher.browserless.tokenSecret.{name,key}` via a
  `secretKeyRef`, rendered only when `tokenSecret.name` is set) -- the
  binary appends it to the ControlURL as `?token=...` (or `&token=...`
  if the URL already carries a query string).
- `ROD_BROWSER_PATH` is retained as a **local/test-only fallback** to a
  bundled binary, used only when `BROWSERLESS_URL` is empty.

The exit-code contract (§12.4) and lockout-safety layers (§12.5) are
unchanged.
