# Glovebox Connector Framework — Design Specification

**Version 1.0 — March 2026**

*This document specifies the connector library, scaffold tooling, and first-party connector implementations for the glovebox content scanning service.*

---

## 1. Purpose

Connectors bridge external data sources to the glovebox scanning pipeline. Each connector authenticates to a remote service, fetches new content since its last checkpoint, and writes items to the glovebox staging directory using the atomic handoff protocol defined in the glovebox design spec (04, Section 5).

The connector framework provides a public Go library (`github.com/leftathome/glovebox/connector`) that handles common plumbing — staging writes, checkpoint persistence, config-based routing, execution lifecycle, health checks, and metrics — so that connector authors only need to implement the fetch logic specific to their data source.

## 2. Scope

### In scope:
- **Connector library** (`connector/`) — public, importable by external repos
- **Content helpers** (`connector/content/`) — optional MIME decoder and HTML-to-text extractor
- **Scaffold generator** (`generator/`) — creates new connector directories from templates
- **First-party connectors** — IMAP/POP3 and RSS (Round 1)

### Out of scope:
- Content inspection or scanning (that's the glovebox)
- Agent routing decisions based on content analysis (downstream concern)
- OAuth token refresh flows (handled by the deployment layer or connector-specific code)
- Round 2 and 3 connectors (designed against this framework later)

## 3. Package Structure

```
connector/                          # Public library (importable)
  connector.go                      # Core interfaces: Connector, Watcher, Listener
  runner.go                         # Execution engine: poll loop, HTTP listener, signals
  checkpoint.go                     # State persistence (Save/Load backed by JSON file)
  staging.go                        # Staging writer (atomic rename, metadata.json)
  route.go                          # Config-based routing rules
  content/                          # Optional content helpers
    mime.go                         # MIME multipart decoder
    html.go                         # HTML-to-text extractor

connectors/                         # First-party implementations
  imap/
    main.go
    connector.go
    config.go
    config.json
    Dockerfile
    README.md
  rss/
    main.go
    connector.go
    config.go
    config.json
    Dockerfile
    README.md

generator/                          # Scaffold templates
  generate.go                       # Entry point: go run ./generator new-connector <name>
  templates/
    connector.go.tmpl
    config.go.tmpl
    main.go.tmpl
    config.json.tmpl
    Dockerfile.tmpl
    README.md.tmpl
```

## 4. Core Interfaces

### 4.1 Connector (required)

Every connector must implement `Connector`:

```go
type Connector interface {
    // Poll fetches new content since the last checkpoint.
    // Called once for poll-once mode, periodically for long-running mode.
    Poll(ctx context.Context, checkpoint Checkpoint) error
}
```

### 4.2 Watcher (optional)

Connectors that maintain a persistent connection (e.g., IMAP IDLE) implement `Watcher`:

```go
type Watcher interface {
    // Watch blocks and processes events as they arrive.
    // Called after Poll completes. Blocks until ctx is cancelled.
    Watch(ctx context.Context, checkpoint Checkpoint) error
}
```

### 4.3 Listener (optional)

Connectors that receive webhook events implement `Listener`:

```go
type Listener interface {
    // Handler returns an http.Handler for receiving webhook events.
    Handler() http.Handler
}
```

### 4.4 Execution Modes

A connector implements `Connector` (required) plus optionally `Watcher` and/or `Listener`. The runner orchestrates based on which interfaces are implemented:

| Interfaces | Behavior | Deployment |
|-----------|----------|------------|
| `Connector` only | Poll once, exit | CronJob |
| `Connector` + re-poll | Poll on interval, run indefinitely | Deployment |
| `Connector` + `Watcher` | Poll to catch up, then Watch (blocks) | Deployment |
| `Connector` + `Listener` | Poll to catch up, then serve HTTP | Deployment |
| All three | Poll, then Watch + Listener in parallel | Deployment |

## 5. Staging Writer

The library provides `StagingWriter` for the atomic handoff protocol:

```go
writer := connector.NewStagingWriter(stagingDir, connectorName)

item := writer.NewItem(connector.ItemOptions{
    Source:           "imap",
    Sender:           "alice@example.com",
    Subject:          "Re: meeting notes",
    Timestamp:        msg.Date,
    DestinationAgent: router.Match("folder:INBOX"),
    ContentType:      "text/html",
    Ordered:          true,
})

item.WriteContent(bodyBytes)
item.Commit()
```

### 5.1 Atomic Handoff

1. `NewItem` creates a temp directory under `<stagingDir>-tmp/<connectorName>/<timestamp>-<uuid>/`
2. `WriteContent` writes `content.raw` to the temp directory. May be called once with all bytes or multiple times (appending). Alternatively, `ContentWriter() io.Writer` returns a writer for streaming content.
3. `Commit` validates metadata fields (see 5.3), writes `metadata.json`, and atomically renames the directory into `<stagingDir>/`. Returns an error if validation or rename fails. Connectors MUST NOT advance the checkpoint until `Commit` returns successfully.
4. If the connector crashes between `NewItem` and `Commit`, the temp directory is orphaned (invisible to glovebox) and cleaned up on next startup. Cleanup is scoped to the connector's own subdirectory under `staging-tmp/` -- it never touches other connectors' temp directories.

### 5.2 Metadata Construction

`Commit` constructs `metadata.json` from `ItemOptions`:

```go
type ItemOptions struct {
    Source           string
    Sender           string
    Subject          string
    Timestamp        time.Time
    DestinationAgent string
    ContentType      string
    Ordered          bool
    AuthFailure      bool    // set true if source authentication failed for this item
}
```

```json
{
    "source": "imap",
    "sender": "alice@example.com",
    "subject": "Re: meeting notes",
    "timestamp": "2026-03-28T12:00:00Z",
    "destination_agent": "messaging",
    "content_type": "text/html",
    "ordered": true,
    "auth_failure": false
}
```

The connector author never constructs metadata.json manually.

### 5.3 Metadata Validation

The staging writer validates all metadata fields against the constraints from the glovebox design spec (04, Section 5.4) before writing `metadata.json`. `Commit` returns an error if validation fails:

- `source`, `sender`, `subject`: max 1024 characters
- `destination_agent`, `content_type`: max 64 characters
- No control characters (U+0000-U+001F) permitted except in `subject` (where they are stripped)
- `destination_agent` must be a non-empty string (glovebox validates against its own allowlist, but the connector should not produce empty destinations)

This catches problems at the source rather than having the glovebox reject items downstream.

### 5.3 Content Extraction Requirement

Per the glovebox design spec (Section 5.3), connectors MUST decode and extract human-readable content from structured formats before writing `content.raw`. The content helpers in `connector/content/` provide optional utilities for this:

- `content.DecodeMIME(raw []byte) ([]Part, error)` — parse multipart MIME into parts
- `content.HTMLToText(html []byte) []byte` — strip HTML tags, decode entities

These are optional. An RSS connector writes the entry body directly. An IMAP connector uses the MIME decoder. A webhook connector passes the payload through.

## 6. Checkpoint State

```go
type Checkpoint interface {
    Load(key string) (string, bool)
    Save(key string, value string)
    Delete(key string)
}
```

### 6.1 Implementation

Backed by a JSON file at `<stateDir>/state.json`. Each connector gets its own state directory and file — no collision between connectors.

- `Save` persists immediately (write-to-temp + atomic rename)
- `Load` reads from the in-memory cache (populated from file on startup)
- Complex state (e.g., list of seen feed URLs) can be JSON-encoded into the value string

### 6.2 Usage Pattern

```go
func (c *IMAPConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
    for _, folder := range c.config.Folders {
        lastUID, _ := cp.Load("uid:" + folder.Name)
        messages := c.fetchSince(folder, lastUID)
        for _, msg := range messages {
            // write to staging...
            cp.Save("uid:" + folder.Name, msg.UID)
        }
    }
    return nil
}
```

Checkpoint is saved per-item, not per-poll. If the connector crashes mid-poll, it resumes from the last successfully checkpointed item.

## 7. Config-Based Routing

### 7.1 Route Configuration

```json
{
    "routes": [
        {"match": "folder:INBOX",     "destination": "messaging"},
        {"match": "folder:Calendar",  "destination": "calendar"},
        {"match": "folder:Contacts",  "destination": "crm"},
        {"match": "*",                "destination": "messaging"}
    ]
}
```

### 7.2 Router

```go
router := connector.NewRouter(config.Routes)

dest := router.Match("folder:INBOX")     // -> "messaging"
dest := router.Match("folder:Calendar")  // -> "calendar"
dest := router.Match("anything-else")    // -> "messaging" (wildcard)
```

- Match keys are connector-defined strings — the library imposes no vocabulary
- IMAP uses `folder:X`, RSS uses `feed:X`, GitHub uses `repo:X` or `event:X`
- Rules are evaluated in order, first match wins
- `*` matches anything (wildcard / catch-all)
- If no route matches and no wildcard exists, the item is skipped: a warning is logged with the unmatched key, the `connector_items_dropped_total` metric is incremented, and the checkpoint is NOT advanced (so the item will be retried if routes are fixed). Config validation at startup warns if no wildcard route is defined

### 7.3 Simple Configurations

A connector that routes everything to one agent:

```json
{
    "routes": [
        {"match": "*", "destination": "messaging"}
    ]
}
```

## 8. Runner / Execution Engine

### 8.1 Entry Point

```go
func main() {
    connector.Run(connector.Options{
        Name:         "imap",
        StagingDir:   os.Getenv("GLOVEBOX_STAGING_DIR"),
        StateDir:     os.Getenv("GLOVEBOX_STATE_DIR"),
        ConfigFile:   os.Getenv("GLOVEBOX_CONNECTOR_CONFIG"),
        Connector:    &IMAPConnector{},
        PollInterval: 5 * time.Minute,
    })
}
```

### 8.2 Lifecycle

1. Load config from `ConfigFile`
2. Initialize checkpoint from state file
3. Initialize staging writer, clean up orphaned items in `staging-tmp/`
4. Initialize router from config routes
5. Execute based on interfaces implemented (see Section 4.4)
6. For long-running mode: re-run `Poll` on `PollInterval` between `Watch` events
7. Signal handling: SIGTERM cancels context, in-flight work completes, checkpoint saved

### 8.3 Health Endpoints

The runner exposes health endpoints on a configurable port (default 8080):

- **`/healthz`** (liveness) — true as soon as the HTTP server starts. If this fails, K8s kills and restarts the pod.
- **`/readyz`** (readiness) — true after the connector has loaded config, connected to the remote service, authenticated, AND completed at least one successful poll. Until true, K8s doesn't route traffic and the operator knows the connector is not yet producing data.

Readiness progression:
1. Process starts -> `/healthz` true, `/readyz` false
2. Config loaded, connected, authenticated -> `/readyz` still false
3. First `Poll` completes without error -> `/readyz` true

If authentication fails with a permanent error, the process exits (CrashLoopBackOff). If transient (network blip), `/readyz` remains false until a poll succeeds.

## 9. Error Handling

### 9.1 Error Classification

```go
// Transient (default) — logged, retry on next cycle
return fmt.Errorf("IMAP connection failed: %w", err)

// Permanent — connector exits with non-zero status
return connector.PermanentError(fmt.Errorf("invalid credentials: %w", err))
```

- **Transient errors**: logged, Poll/Watch continues on next cycle. Checkpoint is NOT advanced past the failed item so it's retried next time.
- **Permanent errors**: logged, connector exits. K8s restarts it; operator sees CrashLoopBackOff and investigates.

Unwrapped errors are treated as transient by default. Connector authors explicitly mark permanent failures.

### 9.2 Watch Errors

If `Watch` returns a transient error, the runner waits `PollInterval`, re-runs `Poll` to catch up on any missed items, then re-enters `Watch`. If `Watch` returns a permanent error, the connector exits.

### 9.3 Poll and Watch Concurrency

`Poll` and `Watch` share the same checkpoint and are mutually exclusive -- `Watch` is paused while `Poll` runs. The runner does not call `Poll` and `Watch` concurrently. In long-running mode with `PollInterval`, the sequence is: `Poll` -> `Watch` (until next poll interval) -> pause `Watch` -> `Poll` -> resume `Watch`.

### 9.4 Partial Poll Failures

If a poll encounters an error mid-way through (e.g., fetched 50 of 100 messages, then connection drops), the 50 successfully checkpointed items are preserved. The next poll resumes from the last checkpoint.

## 10. Observability

### 10.1 Logging

Standard `log/slog` structured logging. The runner sets up the logger with connector name and run ID. Connectors use `slog.Info`/`slog.Error` directly.

### 10.2 Metrics

OTel metrics with Prometheus exporter, same pattern as glovebox. Connector name is a **label**, not a namespace — one dashboard works for all connectors.

Metrics provided by the runner (automatic):

| Metric | Type | Labels |
|--------|------|--------|
| `connector_polls_total` | counter | connector, status |
| `connector_items_produced_total` | counter | connector, destination |
| `connector_poll_duration_seconds` | histogram | connector |
| `connector_errors_total` | counter | connector, type |
| `connector_checkpoint_age_seconds` | gauge | connector |
| `connector_items_dropped_total` | counter | connector, reason |

Connectors can register additional connector-specific metrics if needed.

## 11. Credentials and Authentication

**Note:** This section is superseded by the detailed design in
`docs/specs/06-connector-auth-and-provenance-design.md` which covers OAuth token
lifecycle, identity propagation, and the unified rules config.

### 11.1 Static Credentials

Secrets (API keys, PATs, passwords) are injected as environment variables by the
deployment layer (K8s secrets, 1Password Connect, `op run`). Connectors read
`os.Getenv()` directly or use `StaticTokenSource`.

### 11.2 OAuth Token Lifecycle

For connectors that use OAuth 2.0, the library provides `TokenSource` interface
with `RefreshableTokenSource` for automatic token refresh and atomic
persistence. See spec 06 for details.

### 11.3 Identity and Provenance

Each item carries an optional `identity` object in metadata.json and optional
`tags` resolved from the unified rules config. See spec 06 for the full schema.

## 12. Scaffold Generator

### 12.1 Usage

```bash
go run ./generator new-connector rss
```

### 12.2 Output

Creates `connectors/rss/` with:

| File | Contents |
|------|----------|
| `main.go` | Wires `connector.Run` with the connector implementation |
| `connector.go` | Stub implementing `connector.Connector` with empty `Poll` method |
| `config.go` | Config struct with routes + connector-specific fields |
| `config.json` | Example config with wildcard route |
| `Dockerfile` | Multi-stage build (golang:1.26 -> distroless), same pattern as glovebox |
| `README.md` | Usage, config reference, env var placeholders |

### 12.3 Generated Skeleton

```go
type RSSConnector struct {
    config  Config
    writer  *connector.StagingWriter
    router  *connector.Router
}

func (c *RSSConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
    // TODO: implement fetch logic
    return nil
}
```

The connector author fills in `Poll`, adds connector-specific config fields, and they're done.

## 13. First-Party Connectors

### 13.1 IMAP Connector

**Implements:** `Connector` + `Watcher` (IMAP IDLE for live events after catch-up)

*Note: POP3 support is deferred. POP3 has fundamentally different semantics (no folders, no persistent UIDs, no IDLE). If needed, it would be a separate connector.*

**Data fetched:**
- Sent and received emails (per configured folders)
- Folder structure
- Address book / contacts (if supported by server)
- Notes (if supported by server)

**Routing keys:** `folder:<name>` (e.g., `folder:INBOX`, `folder:Sent`, `folder:Contacts`)

**Checkpoint:** Last-seen UID per folder (`uid:<folder-name>`)

**Content extraction:** Uses `content.DecodeMIME` to extract text/plain and text/html parts from MIME messages. Multi-part messages concatenate representations with a separator per the glovebox spec.

**Content types produced:** `text/plain` for plain text messages, `text/html` for HTML messages, `text/plain` + `text/html` concatenated for multipart messages.

**Env vars:** `IMAP_HOST`, `IMAP_PORT`, `IMAP_USERNAME`, `IMAP_PASSWORD`, `IMAP_TLS`

### 13.2 RSS Feed Connector

**Implements:** `Connector` only (poll-based)

**Data fetched:**
- Feed metadata (title, description, link)
- Individual entries (title, content, author, published date)
- Follow and fetch text from linked URLs (configurable, off by default)

**Routing keys:** `feed:<feed-name>` (derived from config)

**Checkpoint:** Last-fetched entry ID or published timestamp per feed

**Content extraction:** Entry body written directly. If `fetch_links` is enabled, linked page content is fetched and HTML-stripped using `content.HTMLToText`.

**Link fetching security:** When `fetch_links` is enabled, a configurable link policy controls which URLs may be fetched. The default policy (`"safe"`) denies private/internal IP ranges (RFC 1918, link-local, loopback), denies non-HTTPS schemes, and enforces response size limit (default 1MB) and request timeout (default 10s). Rules override the default for specific domains, networks, or schemes:

```json
{
    "fetch_links": true,
    "link_policy": {
        "default": "safe",
        "rules": [
            {"match": "domain:wiki.home.lan", "allow": true},
            {"match": "network:10.0.0.0/8",   "allow": true},
            {"match": "scheme:ftp",            "allow": false}
        ]
    }
}
```

Setting `"default": "unrestricted"` disables all checks for operators who know their network topology. Rules use the same first-match-wins pattern as route configuration.

**Content types produced:** `text/html` for feed entry bodies, `text/plain` when `fetch_links` extracts linked page text.

**Env vars:** None required (RSS is unauthenticated). Optional: `HTTP_PROXY` for network access.

## 14. Testing Strategy

### 14.1 Library Tests (`connector/`)

- Staging writer: atomic rename, metadata.json schema, orphan cleanup
- Checkpoint: save/load/delete, atomic persistence, concurrent access
- Router: exact match, wildcard, first-match-wins, no-match behavior
- Runner: lifecycle (poll-only, poll+watch, poll+listener), signal handling, health endpoints
- Content helpers: MIME decoding, HTML stripping

### 14.2 Connector Tests (`connectors/<name>/`)

- IMAP: mock IMAP server, fetch since UID, IDLE notification, folder routing
- RSS: mock HTTP server serving Atom/RSS feeds, entry deduplication, link fetching

### 14.3 Integration Tests

- Write items via connector library, verify they appear in glovebox staging directory with correct schema
- Verify checkpoint persistence across restarts
- Verify health endpoint transitions

## 15. Phase 2 Considerations

The connector framework is designed to accommodate future connectors without structural changes:

- **OAuth connectors** (GitHub, LinkedIn, etc.): token refresh logic lives in connector-specific code, not the library. The library doesn't need to know about OAuth.
- **Rate-limited APIs**: connectors implement their own rate limiting / backoff. The library's transient error handling + checkpoint resumption supports this naturally.
- **Webhook connectors**: implement `Listener`, the runner handles HTTP server lifecycle.
- **Bidirectional connectors** (e.g., send emails via IMAP): out of scope for Phase 1. The connector interface is read-only by design. Write capabilities would be a separate interface added later.
