# Connector Authentication and Data Provenance -- Design Specification

**Version 1.0 -- March 2026**

*This document specifies authentication patterns, identity propagation, and
data provenance for glovebox connectors.*

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

Wraps an OAuth2 configuration and a token file. Behavior:

1. On first call, loads token from file in state directory
2. If token is valid (not expired), returns it
3. If expired, uses the refresh token to obtain a new access token
4. Persists the new token atomically (temp file + rename, same pattern as
   checkpoint)
5. If refresh fails with 401 or `invalid_grant`, returns `PermanentError`
   (operator must re-authenticate)

**Token file format** (stored at `<stateDir>/token.json`):

```json
{
    "access_token": "gho_xxxxxxxxxxxx",
    "refresh_token": "ghr_xxxxxxxxxxxx",
    "token_type": "bearer",
    "expiry": "2026-03-30T12:00:00Z"
}
```

Atomic persistence uses the same temp-file-plus-rename pattern as the checkpoint
to prevent corruption on crash.

### 3.3 GitHubAppTokenSource

Special case for GitHub Apps, which are common enough to warrant first-class
support:

1. Loads private key from env var or file
2. Generates a JWT signed with the private key (10-minute validity)
3. Exchanges the JWT for an installation access token (1-hour validity)
4. Caches and refreshes automatically

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

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `account_id` | string | Yes (if identity present) | Stable identifier for who authenticated. Format is connector-defined (email, username, numeric ID). |
| `provider` | string | Yes | Service name the identity came from (github, gitlab, imap, etc.) |
| `auth_method` | string | Yes | One of: `pat`, `oauth`, `api_key`, `app_password`, `github_app`, `none` |
| `scopes` | []string | No | OAuth scopes or permissions carried by the token |
| `tenant` | string | No | Operator-defined string for multi-tenant routing. Set in connector config. |

**Design decisions:**

- `identity` is an optional nested object. Unauthenticated connectors (RSS)
  omit it entirely or set `auth_method: "none"`.
- `tenant` is a configuration-level field, not derived from the token. The
  operator assigns tenants in their deployment config. This keeps multi-tenancy
  simple regardless of whether the deployment uses one-connector-per-user or
  shared-connector-multi-account.
- Glovebox passes `identity` through unchanged to agent workspaces. It validates
  structure (field lengths, no control characters) but does not interpret
  identity semantics.
- Audit logs include the full `identity` block with no hashing. The audit log
  is already access-controlled, and hashing would impede forensic tracing.

### 5.3 Tags

`tags` is an optional `map[string]string` on each item. Tags are resolved from
the unified rules config (see Section 6) and stamped on items by the staging
writer at `Commit()` time.

Glovebox validates tag key and value lengths (same constraints as other string
fields: max 1024 chars, no control characters) and passes them through to agent
workspaces and audit logs unchanged.

Use cases: team, environment, cost center, project, priority, or any other
operator-defined organizational dimension.

## 6. Unified Rules Config

### 6.1 Motivation

The original connector config used a `routes` array for destination routing:

```json
{
    "routes": [
        {"match": "feed:engadget", "destination": "media"},
        {"match": "*", "destination": "messaging"}
    ]
}
```

With the addition of tags, maintaining separate `routes` and `tags` arrays using
the same match keys would be redundant and error-prone. Instead, a single
`rules` array determines both destination and tags per item.

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
        "tenant": "steve"
    }
}
```

### 6.3 Semantics

- Same first-match-wins evaluation as the current router
- `destination` is required per rule (same as current `routes`)
- `tags` is optional per rule (omit for rules that only need routing)
- `*` matches anything (wildcard / catch-all)
- If no rule matches and no wildcard exists, the item is skipped (same behavior
  as current router: warning logged, checkpoint not advanced)

### 6.4 Library Changes

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

`BaseConfig` changes from `Routes []Route` to `Rules []Rule`.

### 6.5 Identity in Config

Top-level `identity` in the connector config provides fields that apply to all
items from this connector:

```json
{
    "identity": {
        "tenant": "steve",
        "provider": "github",
        "auth_method": "oauth"
    }
}
```

The connector code sets `account_id` and `scopes` per-item from the actual auth
context. The library merges config-level identity with per-item identity
(per-item wins on conflict).

## 7. Glovebox Changes

Glovebox changes are minimal:

1. **Validation**: accept `identity` (validate field lengths/control chars) and
   `tags` (validate key/value lengths) in metadata.json. Both are optional.
2. **Passthrough**: include `identity` and `tags` in the item metadata that is
   delivered to agent workspaces (same as all other metadata fields).
3. **Audit**: include `identity` and `tags` in JSONL audit log entries.
4. **No interpretation**: glovebox does not route, filter, or make decisions
   based on identity or tags. It is a scanning service, not an identity service.

## 8. Migration

### 8.1 Config Format

Existing connectors (IMAP, RSS) migrate from `routes` to `rules`:

Before:
```json
{"routes": [{"match": "*", "destination": "messaging"}]}
```

After:
```json
{"rules": [{"match": "*", "destination": "messaging"}]}
```

This is a breaking change acceptable at v0.1.x.

### 8.2 Existing Connectors

- **RSS**: `identity` with `auth_method: "none"`. Tags from rules.
- **IMAP**: `identity` populated from `IMAP_USERNAME` env var,
  `auth_method: "app_password"`. Tags from rules.

## 9. Out of Scope

- Device code flow / interactive auth setup CLI (separate bead)
- Secret store integration (deployment layer concern)
- Identity-based routing in glovebox (future feature)
- Tag-based quarantine rules (future feature)
- Hashing or pseudonymizing identity in audit logs (v1 stores plaintext)
