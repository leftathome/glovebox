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
