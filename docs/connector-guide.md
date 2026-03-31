# Glovebox Connector Development Guide

This guide covers everything you need to build a custom connector for the
glovebox content scanning service. Connectors bridge external data sources
(email, RSS feeds, APIs, webhooks) into the glovebox scanning pipeline.

---

## Table of Contents

1. [Overview](#1-overview)
2. [Quickstart](#2-quickstart)
3. [Core Concepts](#3-core-concepts)
4. [API Reference](#4-api-reference)
5. [Content Helpers](#5-content-helpers)
6. [Testing Patterns](#6-testing-patterns)
7. [Configuration](#7-configuration)
8. [Docker](#8-docker)
9. [Examples](#9-examples)

---

## 1. Overview

A connector authenticates to a remote service, fetches new content since its
last checkpoint, and writes items to the glovebox staging directory using an
atomic handoff protocol. The connector framework library
(`github.com/leftathome/glovebox/connector`) handles common plumbing so that
connector authors only implement the fetch logic for their data source.

### The Three Interfaces

Every connector must implement `Connector`. Optionally, it can also implement
`Watcher` and/or `Listener` for additional execution modes.

```go
// Required -- every connector implements this.
type Connector interface {
    Poll(ctx context.Context, checkpoint Checkpoint) error
}

// Optional -- for persistent connections (e.g., IMAP IDLE).
type Watcher interface {
    Watch(ctx context.Context, checkpoint Checkpoint) error
}

// Optional -- for webhook receivers.
type Listener interface {
    Handler() http.Handler
}
```

### Execution Modes

| Interfaces Implemented        | Behavior                                  | Typical Deployment |
|-------------------------------|-------------------------------------------|--------------------|
| `Connector` only              | Poll once and exit                        | CronJob            |
| `Connector` + `PollInterval`  | Poll on interval, run indefinitely        | Deployment         |
| `Connector` + `Watcher`       | Poll to catch up, then Watch (blocks)     | Deployment         |
| `Connector` + `Listener`      | Poll to catch up, then serve HTTP         | Deployment         |
| All three                     | Poll, then Watch + Listener in parallel   | Deployment         |

The runner determines execution mode by checking which interfaces your
connector struct satisfies. If `PollInterval` is 0 and neither `Watcher` nor
`Listener` is implemented, the connector runs a single poll and exits. If
`PollInterval` is 0 but a `Watcher` or `Listener` is present, the runner
defaults `PollInterval` to 5 minutes.

---

## 2. Quickstart

### Step 1: Scaffold a new connector

From the repository root:

```bash
go run ./generator new-connector my-source
```

This creates `connectors/my-source/` with:

| File             | Purpose                                              |
|------------------|------------------------------------------------------|
| `connector.go`   | Stub implementing `Connector` with an empty `Poll`  |
| `config.go`      | Config struct embedding `connector.BaseConfig`       |
| `main.go`        | Entry point wiring `connector.Run`                   |
| `config.json`    | Example config with a wildcard rule                  |
| `Dockerfile`     | Multi-stage build (golang -> distroless)             |
| `README.md`      | Usage placeholder                                    |

### Step 2: Define your config

Edit `config.go` to add connector-specific fields:

```go
package mysource

import "github.com/leftathome/glovebox/connector"

type Config struct {
    connector.BaseConfig
    APIEndpoint string `json:"api_endpoint"`
    MaxItems    int    `json:"max_items"`
}
```

### Step 3: Implement Poll

Edit `connector.go`:

```go
package mysource

import (
    "context"
    "fmt"

    "github.com/leftathome/glovebox/connector"
)

type MySourceConnector struct {
    config Config
    writer *connector.StagingWriter
    matcher *connector.RuleMatcher
}

func (c *MySourceConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
    // 1. Load checkpoint to find where we left off
    lastID, _ := cp.Load("last_id")

    // 2. Fetch new items from your source
    items, err := c.fetchItemsSince(ctx, lastID)
    if err != nil {
        return fmt.Errorf("fetch items: %w", err)
    }

    for _, item := range items {
        if ctx.Err() != nil {
            return ctx.Err()
        }

        // 3. Match the item against rules to find its destination
        result, ok := c.matcher.Match("category:" + item.Category)
        if !ok {
            continue // no matching rule, skip
        }

        // 4. Write to staging
        staged, err := c.writer.NewItem(connector.ItemOptions{
            Source:           "my-source",
            Sender:           item.Author,
            Subject:          item.Title,
            Timestamp:        item.CreatedAt,
            DestinationAgent: result.Destination,
            ContentType:      "text/plain",
            RuleTags:         result.Tags,
        })
        if err != nil {
            return fmt.Errorf("new staging item: %w", err)
        }

        if err := staged.WriteContent([]byte(item.Body)); err != nil {
            return fmt.Errorf("write content: %w", err)
        }

        if err := staged.Commit(); err != nil {
            return fmt.Errorf("commit item: %w", err)
        }

        // 5. Advance checkpoint ONLY after successful commit
        if err := cp.Save("last_id", item.ID); err != nil {
            return fmt.Errorf("save checkpoint: %w", err)
        }
    }

    return nil
}
```

### Step 4: Wire up main.go

```go
package main

import (
    "encoding/json"
    "log/slog"
    "os"
    "time"

    "github.com/leftathome/glovebox/connector"
)

func main() {
    configFile := os.Getenv("GLOVEBOX_CONNECTOR_CONFIG")
    if configFile == "" {
        configFile = "/etc/connector/config.json"
    }

    var cfg Config
    data, err := os.ReadFile(configFile)
    if err != nil {
        slog.Error("read config", "error", err)
        os.Exit(1)
    }
    if err := json.Unmarshal(data, &cfg); err != nil {
        slog.Error("parse config", "error", err)
        os.Exit(1)
    }

    c := &MySourceConnector{config: cfg}

    connector.Run(connector.Options{
        Name:       "my-source",
        StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
        StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
        ConfigFile: configFile,
        Connector:  c,
        Setup: func(cc connector.ConnectorContext) error {
            c.writer = cc.Writer
            c.matcher = cc.Matcher
            return nil
        },
        PollInterval: 5 * time.Minute,
    })
}
```

### Step 5: Configure rules

Edit `config.json`:

```json
{
    "rules": [
        {"match": "category:alerts",   "destination": "notifications"},
        {"match": "category:reports",  "destination": "analytics"},
        {"match": "*",                 "destination": "default-agent"}
    ],
    "api_endpoint": "https://api.example.com/v1",
    "max_items": 100
}
```

### Step 6: Test

```bash
go test ./connectors/my-source/...
go vet ./connectors/my-source/...
```

### Step 7: Build Docker image

```bash
docker build -f connectors/my-source/Dockerfile -t glovebox-my-source:latest .
```

---

## 3. Core Concepts

### 3.1 Staging Writer

The `StagingWriter` implements the atomic handoff protocol that delivers items
to the glovebox scanning pipeline. Items are written to a temporary directory
first, then atomically renamed into the staging directory on commit.

**Lifecycle of a staged item:**

1. `NewItem(opts)` -- creates a temp directory under `<stagingDir>-tmp/<connectorName>/`
2. `WriteContent(data)` -- writes `content.raw` into the temp directory (appends on repeated calls)
3. `ContentWriter()` -- alternative: returns an `io.WriteCloser` for streaming content
4. `Commit()` -- validates metadata, writes `metadata.json`, atomically renames into `<stagingDir>/`

If the connector crashes between `NewItem` and `Commit`, the temp directory is
orphaned and cleaned up automatically on next startup via `CleanOrphans()`.

```go
item, err := c.writer.NewItem(connector.ItemOptions{
    Source:           "my-source",
    Sender:           "alice@example.com",
    Subject:          "Weekly Report",
    Timestamp:        time.Now().UTC(),
    DestinationAgent: "analytics",
    ContentType:      "text/plain",
})
if err != nil {
    return err
}

// Write content (can be called multiple times to append)
if err := item.WriteContent([]byte(body)); err != nil {
    return err
}

// Or use streaming writer for large content:
// w, err := item.ContentWriter()
// io.Copy(w, reader)
// w.Close()

if err := item.Commit(); err != nil {
    return err
}
```

**ItemOptions fields:**

| Field              | Type       | Required | Description                                      |
|--------------------|------------|----------|--------------------------------------------------|
| `Source`           | `string`   | Yes      | Connector type (e.g., "rss", "imap")             |
| `Sender`          | `string`   | Yes      | Who sent the content                              |
| `Subject`         | `string`   | Yes      | Subject line or title                             |
| `Timestamp`       | `time.Time`| Yes      | When the content was created                      |
| `DestinationAgent`| `string`   | Yes      | Target agent (from rule match)                    |
| `ContentType`     | `string`   | Yes      | MIME type of content.raw                          |
| `Ordered`         | `bool`     | No       | Whether ordering matters for this item            |
| `AuthFailure`     | `bool`     | No       | True if source auth failed for this item          |
| `Identity`        | `*Identity`| No       | Per-item identity (merged with config identity)   |
| `Tags`            | `map[string]string` | No | Per-item metadata tags                       |
| `RuleTags`        | `map[string]string` | No | Tags from the matched rule (via MatchResult) |

Metadata validation runs on `Commit()`. It enforces max lengths (1024 chars for
source/sender/subject, 64 chars for destination_agent/content_type), strips
control characters from subjects, and requires a non-empty `DestinationAgent`.

### 3.2 Checkpoint

The `Checkpoint` interface provides persistent key-value state that survives
restarts. It is backed by a JSON file at `<stateDir>/state.json` with
atomic writes.

```go
type Checkpoint interface {
    Load(key string) (string, bool)
    Save(key string, value string) error
    Delete(key string) error
}
```

**Per-item checkpointing pattern:**

The critical rule: advance the checkpoint only AFTER a successful `Commit()`.
This ensures that if the connector crashes mid-poll, it resumes from the last
successfully committed item.

```go
for _, entry := range entries {
    // ... write to staging ...

    if err := item.Commit(); err != nil {
        return err  // checkpoint NOT advanced -- entry will be retried
    }

    // Safe to advance checkpoint now
    if err := cp.Save("last:"+feedName, entry.ID); err != nil {
        return err
    }
}
```

**Key naming conventions:**

Checkpoint keys are connector-defined strings. Common patterns:

- `last:<source-name>` -- last processed item ID for a named source
- `uid:<folder-name>` -- last seen UID for an IMAP folder
- `cursor` -- API pagination cursor

For complex state, JSON-encode the value:

```go
seen := map[string]bool{"id1": true, "id2": true}
data, _ := json.Marshal(seen)
cp.Save("seen_ids", string(data))
```

### 3.3 RuleMatcher

The `RuleMatcher` maps connector-defined match keys to destination agent names
and optional tags based on configuration. Rules are evaluated in order; first
match wins.

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

matcher := connector.NewRuleMatcher(rules)
result, ok := matcher.Match("feed:techcrunch")
// result.Destination -- the agent name
// result.Tags        -- metadata tags from the matched rule
```

**Match key conventions by connector type:**

| Connector | Key format           | Examples                           |
|-----------|----------------------|------------------------------------|
| IMAP      | `folder:<name>`      | `folder:INBOX`, `folder:Sent`      |
| RSS       | `feed:<name>`        | `feed:techcrunch`, `feed:hackernews`|
| GitHub    | `repo:<name>`        | `repo:myorg/myrepo`                |
| Webhook   | `event:<type>`       | `event:push`, `event:issue`        |

**Wildcard rule:**

A `"*"` match acts as a catch-all. If no rule matches and no wildcard is
configured, the item is skipped and a warning is logged. The runner warns at
startup if no wildcard rule is defined.

**Simple single-destination config:**

```json
{
    "rules": [
        {"match": "*", "destination": "messaging"}
    ]
}
```

**Rules with tags:**

Tags defined on a rule are included in the `MatchResult` and automatically
merged into the item's metadata at commit time (via `RuleTags` on
`ItemOptions`). Per-item tags override rule tags on key conflict.

```json
{
    "rules": [
        {"match": "feed:internal", "destination": "engineering", "tags": {"priority": "high", "source_type": "internal"}},
        {"match": "*", "destination": "default-agent", "tags": {"priority": "normal"}}
    ]
}
```

### 3.4 Identity

The identity system tracks which authenticated account produced each item.
Identity is set at two levels:

1. **Config-level identity** (`ConfigIdentity`) -- defaults from the connector
   config file, applied to all items. Set via `StagingWriter.SetConfigIdentity()`.
2. **Per-item identity** (`Identity`) -- set on individual `ItemOptions`. Per-item
   fields override config-level fields when both are present.

```go
// Config-level identity (set once during setup):
writer.SetConfigIdentity(&connector.ConfigIdentity{
    Provider:   "imap",
    AuthMethod: "app-password",
    AccountID:  "user@example.com",
})

// Per-item identity (optional, overrides config for specific items):
item, err := c.writer.NewItem(connector.ItemOptions{
    Source:           "imap",
    DestinationAgent: result.Destination,
    // ...
    Identity: &connector.Identity{
        Provider:   "imap",
        AuthMethod: "oauth2",
        AccountID:  "other-user@example.com",
        Scopes:     []string{"IMAP", "SMTP"},
    },
})
```

The framework merges these at `Commit()` time using `MergeIdentity()`. Per-item
non-empty fields override config-level values. Scopes come from the per-item
identity only (config has no scopes field).

Config identity is specified in `config.json`:

```json
{
    "identity": {
        "provider": "imap",
        "auth_method": "app-password",
        "account_id": "user@example.com"
    },
    "rules": [
        {"match": "*", "destination": "messaging"}
    ]
}
```

### 3.5 Error Handling

Errors from `Poll` and `Watch` are classified as transient or permanent:

```go
// Transient (default) -- retry on next cycle
return fmt.Errorf("connection timeout: %w", err)

// Permanent -- connector exits, K8s restarts it (CrashLoopBackOff)
return connector.PermanentError(fmt.Errorf("invalid credentials: %w", err))
```

**Transient errors:**
- Logged as warnings
- Checkpoint is NOT advanced past the failed item
- Next poll retries from the last checkpoint
- `/readyz` may remain false until a poll succeeds

**Permanent errors:**
- Logged as errors
- Connector exits with non-zero status
- Operator sees CrashLoopBackOff and investigates

Use `connector.IsPermanent(err)` to check error type. Unwrapped errors are
treated as transient by default.

**Partial poll failures:** If a poll processes 50 of 100 items and then fails,
the 50 successfully committed and checkpointed items are preserved. The next
poll resumes from the last checkpoint.

### 3.6 Health Endpoints

The runner exposes HTTP health endpoints on a configurable port (default 8080):

| Endpoint    | Purpose    | Returns 200 When                                  |
|-------------|------------|----------------------------------------------------|
| `/healthz`  | Liveness   | Always (as soon as HTTP server starts)             |
| `/readyz`   | Readiness  | After first successful poll completes              |
| `/metrics`  | Prometheus | Always (OTel metrics with Prometheus exporter)     |

**Readiness progression:**

1. Process starts: `/healthz` returns 200, `/readyz` returns 503
2. Config loaded, connections established: `/readyz` still 503
3. First `Poll` completes without error: `/readyz` returns 200

If the initial poll returns a permanent error, the process exits. If it
returns a transient error, `/readyz` remains 503 until a poll succeeds.

**Listener port:** If the connector implements `Listener`, the runner starts the
Listener's HTTP handler on `HealthPort + 1` (default 8081). This is a separate
server from the health/metrics endpoints.

---

## 4. API Reference

### Package `connector`

#### Interfaces

```go
// Connector is the required interface. Poll fetches new content.
type Connector interface {
    Poll(ctx context.Context, checkpoint Checkpoint) error
}

// Watcher is optional. Watch blocks and processes events as they arrive.
type Watcher interface {
    Watch(ctx context.Context, checkpoint Checkpoint) error
}

// Listener is optional. Handler returns an http.Handler for webhooks.
type Listener interface {
    Handler() http.Handler
}

// Checkpoint provides persistent key-value state.
type Checkpoint interface {
    Load(key string) (string, bool)
    Save(key string, value string) error
    Delete(key string) error
}
```

#### Types

```go
// Options configures the connector runner.
type Options struct {
    Name         string        // connector name (used in logs, metrics, staging paths)
    StagingDir   string        // path to glovebox staging directory
    StateDir     string        // path to connector state directory
    ConfigFile   string        // path to JSON config file
    Connector    Connector     // the connector implementation
    Setup        SetupFunc     // optional setup callback (receives framework resources)
    PollInterval time.Duration // 0 = poll once and exit
    HealthPort   int           // default 8080
}

// ConnectorContext is passed to the Setup callback.
type ConnectorContext struct {
    Writer  *StagingWriter
    Matcher *RuleMatcher
    Metrics *Metrics
}

// SetupFunc is called after the runner initializes resources.
type SetupFunc func(cc ConnectorContext) error

// BaseConfig provides the rules field that all connector configs share.
// The "routes" key is accepted for backward compatibility but deprecated.
type BaseConfig struct {
    Rules          []Rule          `json:"rules"`
    Routes         []Rule          `json:"routes"`
    ConfigIdentity *ConfigIdentity `json:"identity,omitempty"`
}

// Rule maps a match key to a destination agent, with optional tags.
type Rule struct {
    Match       string            `json:"match"`
    Destination string            `json:"destination"`
    Tags        map[string]string `json:"tags,omitempty"`
}

// MatchResult holds the destination and tags produced by a successful match.
type MatchResult struct {
    Destination string
    Tags        map[string]string
}

// Identity represents the authenticated identity that produced an item.
type Identity struct {
    AccountID  string   `json:"account_id,omitempty"`
    Provider   string   `json:"provider"`
    AuthMethod string   `json:"auth_method"`
    Scopes     []string `json:"scopes,omitempty"`
    Tenant     string   `json:"tenant,omitempty"`
}

// ConfigIdentity is the identity block from connector config.
type ConfigIdentity struct {
    AccountID  string `json:"account_id,omitempty"`
    Provider   string `json:"provider,omitempty"`
    AuthMethod string `json:"auth_method,omitempty"`
    Tenant     string `json:"tenant,omitempty"`
}

// ItemOptions configures a staged item.
type ItemOptions struct {
    Source           string
    Sender           string
    Subject          string
    Timestamp        time.Time
    DestinationAgent string
    ContentType      string
    Ordered          bool
    AuthFailure      bool
    Identity         *Identity
    Tags             map[string]string
    RuleTags         map[string]string
}
```

#### Functions

```go
// Run starts the connector lifecycle. Does not return until shutdown.
func Run(opts Options)

// PermanentError wraps an error to mark it as non-retryable.
func PermanentError(err error) error

// IsPermanent returns true if the error was wrapped with PermanentError.
func IsPermanent(err error) bool

// NewCheckpoint creates a file-backed checkpoint in stateDir.
func NewCheckpoint(stateDir string) (Checkpoint, error)

// NewStagingWriter creates a staging writer for the given connector.
func NewStagingWriter(stagingDir string, connectorName string) (*StagingWriter, error)

// NewRuleMatcher creates a rule matcher from the given rules.
func NewRuleMatcher(rules []Rule) *RuleMatcher

// NewMetrics creates OTel instruments with Prometheus exporter.
// connectorName is recorded as the "connector" label on all metrics.
func NewMetrics(connectorName string) (*Metrics, error)
```

#### StagingWriter Methods

```go
// NewItem creates a new staging item in a temp directory.
func (w *StagingWriter) NewItem(opts ItemOptions) (*StagingItem, error)

// CleanOrphans removes incomplete items from previous runs.
func (w *StagingWriter) CleanOrphans()

// SetConfigIdentity sets the config-level identity used as the base for
// identity merging at Commit() time.
func (w *StagingWriter) SetConfigIdentity(ci *ConfigIdentity)
```

#### StagingItem Methods

```go
// WriteContent writes (or appends) content to content.raw.
func (si *StagingItem) WriteContent(data []byte) error

// ContentWriter returns an io.WriteCloser for streaming content.
func (si *StagingItem) ContentWriter() (io.WriteCloser, error)

// Commit validates metadata, writes metadata.json, and atomically moves
// the item into the staging directory. Returns error if validation fails.
func (si *StagingItem) Commit() error
```

#### RuleMatcher Methods

```go
// Match returns the MatchResult for the first matching rule.
// Returns (MatchResult{}, false) if no rule matches.
func (rm *RuleMatcher) Match(key string) (MatchResult, bool)
```

#### Identity Functions

```go
// MergeIdentity merges config-level identity with per-item identity.
// Per-item fields override config fields. Returns nil if both are nil.
func MergeIdentity(config *ConfigIdentity, item *Identity) *Identity
```

#### Metrics Methods

```go
// RecordPoll increments connector_polls_total. status: "success" or "error".
func (m *Metrics) RecordPoll(status string)

// RecordPollDuration records a poll duration observation.
func (m *Metrics) RecordPollDuration(d time.Duration)

// RecordItemProduced increments connector_items_produced_total.
func (m *Metrics) RecordItemProduced(destination string)

// RecordItemDropped increments connector_items_dropped_total.
func (m *Metrics) RecordItemDropped(reason string)

// RecordError increments connector_errors_total. errType: "transient" or "permanent".
func (m *Metrics) RecordError(errType string)

// SetCheckpointAge sets the connector_checkpoint_age_seconds gauge.
func (m *Metrics) SetCheckpointAge(seconds float64)

// Handler returns an http.Handler for the Prometheus /metrics endpoint.
func (m *Metrics) Handler() http.Handler

// Shutdown flushes and shuts down the meter provider.
func (m *Metrics) Shutdown() error
```

#### Built-in Metrics

| Metric                              | Type      | Labels                |
|-------------------------------------|-----------|-----------------------|
| `connector_polls_total`             | counter   | connector, status     |
| `connector_items_produced_total`    | counter   | connector, destination|
| `connector_poll_duration_seconds`   | histogram | connector             |
| `connector_errors_total`            | counter   | connector, type       |
| `connector_checkpoint_age_seconds`  | gauge     | connector             |
| `connector_items_dropped_total`     | counter   | connector, reason     |

---

## 5. Content Helpers

The `connector/content` package provides optional utilities for processing
content before staging.

### DecodeMIME

Parses a raw MIME message into individual parts. Handles multipart messages,
base64, and quoted-printable encoding.

```go
import "github.com/leftathome/glovebox/connector/content"

parts, err := content.DecodeMIME(rawEmailBytes)
if err != nil {
    return err
}
for _, part := range parts {
    // part.ContentType -- e.g., "text/plain", "text/html"
    // part.Body        -- decoded bytes
    // part.Filename    -- attachment filename (if any)
}
```

**Part type:**

```go
type Part struct {
    ContentType string
    Body        []byte
    Filename    string
}
```

### HTMLToText

Strips HTML tags and returns plain text. Useful for extracting readable content
from HTML email bodies or web pages.

```go
plain := content.HTMLToText(htmlBytes)
```

### LinkPolicy

Controls which URLs a connector is allowed to fetch. Prevents SSRF attacks by
default (blocks private IPs, non-HTTPS schemes).

```go
policy := content.NewLinkPolicy(content.LinkPolicyConfig{
    Default: "safe",  // or "unrestricted"
    Rules: []content.LinkPolicyRule{
        {Match: "domain:wiki.internal", Allow: true},
        {Match: "network:10.0.0.0/8",  Allow: true},
        {Match: "scheme:ftp",           Allow: false},
    },
})

allowed, reason := policy.Check("https://example.com/page")
```

**Rule match types:**

| Type      | Format                  | Example                         |
|-----------|-------------------------|---------------------------------|
| `domain`  | `domain:<hostname>`     | `domain:wiki.internal`          |
| `scheme`  | `scheme:<scheme>`       | `scheme:ftp`                    |
| `network` | `network:<CIDR>`        | `network:10.0.0.0/8`           |

Rules are evaluated in order (first match wins), then the default policy
applies. In `"safe"` mode (the default), only HTTPS to public IPs is allowed.

---

## 6. Testing Patterns

### Test setup helper

Create a reusable helper that wires up a connector with temp directories:

```go
func newTestConnector(t *testing.T, feeds []FeedConfig) (*MyConnector, string, string) {
    t.Helper()

    stagingDir := t.TempDir()
    stateDir := t.TempDir()

    writer, err := connector.NewStagingWriter(stagingDir, "my-source")
    if err != nil {
        t.Fatalf("NewStagingWriter: %v", err)
    }

    rules := []connector.Rule{
        {Match: "*", Destination: "test-agent"},
    }
    matcher := connector.NewRuleMatcher(rules)

    c := &MyConnector{
        config: Config{Feeds: feeds},
        writer: writer,
        matcher: matcher,
    }

    return c, stagingDir, stateDir
}
```

### Mock HTTP servers

Use `httptest.NewServer` for connectors that fetch from HTTP endpoints:

```go
func TestPoll(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        w.Write([]byte(`[{"id": "1", "title": "Test"}]`))
    }))
    defer srv.Close()

    // Point your connector at srv.URL
    c, stagingDir, stateDir := newTestConnector(t, /* config using srv.URL */)
    cp := newCheckpoint(t, stateDir)

    err := c.Poll(context.Background(), cp)
    if err != nil {
        t.Fatalf("Poll: %v", err)
    }

    // Verify staged items
    count := countStagedItems(t, stagingDir)
    if count != 1 {
        t.Errorf("expected 1 staged item, got %d", count)
    }
}
```

### Checkpoint helper

```go
func newCheckpoint(t *testing.T, stateDir string) connector.Checkpoint {
    t.Helper()
    cp, err := connector.NewCheckpoint(stateDir)
    if err != nil {
        t.Fatalf("NewCheckpoint: %v", err)
    }
    return cp
}
```

### Counting staged items

```go
func countStagedItems(t *testing.T, stagingDir string) int {
    t.Helper()
    entries, err := os.ReadDir(stagingDir)
    if err != nil {
        t.Fatalf("read staging dir: %v", err)
    }
    count := 0
    for _, e := range entries {
        if e.IsDir() {
            count++
        }
    }
    return count
}
```

### Verifying metadata

```go
func TestMetadata(t *testing.T) {
    // ... run poll ...

    entries, _ := os.ReadDir(stagingDir)
    for _, e := range entries {
        if !e.IsDir() {
            continue
        }
        metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
        data, err := os.ReadFile(metaPath)
        if err != nil {
            t.Fatalf("read metadata: %v", err)
        }

        var meta map[string]interface{}
        if err := json.Unmarshal(data, &meta); err != nil {
            t.Fatalf("parse metadata: %v", err)
        }

        if meta["source"] != "my-source" {
            t.Errorf("expected source 'my-source', got %v", meta["source"])
        }
        if meta["destination_agent"] != "test-agent" {
            t.Errorf("expected destination 'test-agent', got %v", meta["destination_agent"])
        }
    }
}
```

### Testing checkpoint deduplication

Poll twice with the same data and verify no duplicate items are produced:

```go
func TestNoDuplicates(t *testing.T) {
    // ... setup ...

    c.Poll(context.Background(), cp)
    first := countStagedItems(t, stagingDir)

    c.Poll(context.Background(), cp)
    second := countStagedItems(t, stagingDir)

    if second != first {
        t.Errorf("expected %d items (no new), got %d", first, second)
    }
}
```

### Running tests

```bash
# Run tests for a specific connector
go test ./connectors/my-source/...

# Run all connector tests
go test ./connectors/...

# Run with verbose output
go test -v ./connectors/my-source/...

# Run vet
go vet ./connectors/my-source/...
```

---

## 7. Configuration

### Config file format

Every connector config file is JSON. It must include a `rules` array (from
`connector.BaseConfig`) and can include any connector-specific fields.
The `routes` key is accepted for backward compatibility but deprecated.

```json
{
    "rules": [
        {"match": "feed:techcrunch", "destination": "news"},
        {"match": "feed:internal",   "destination": "engineering", "tags": {"priority": "high"}},
        {"match": "*",               "destination": "default-agent"}
    ],
    "identity": {
        "provider": "rss",
        "auth_method": "none"
    },
    "api_endpoint": "https://api.example.com",
    "max_items": 100
}
```

### Config struct pattern

Embed `connector.BaseConfig` to inherit the rules and identity fields:

```go
type Config struct {
    connector.BaseConfig
    APIEndpoint string `json:"api_endpoint"`
    MaxItems    int    `json:"max_items"`
}
```

The runner parses rules from the config file automatically and initializes
the rule matcher. Your connector reads its own config fields separately in
`main.go`. If a `ConfigIdentity` is present, the runner sets it on the
staging writer automatically.

### Environment variables

The framework uses these standard environment variables:

| Variable                      | Purpose                            | Default                      |
|-------------------------------|------------------------------------|------------------------------|
| `GLOVEBOX_STAGING_DIR`        | Staging directory path             | (required)                   |
| `GLOVEBOX_STATE_DIR`          | Checkpoint state directory         | (required)                   |
| `GLOVEBOX_CONNECTOR_CONFIG`   | Path to config JSON file           | `/etc/connector/config.json` |

Connector-specific credentials should be injected as environment variables by
the deployment layer (K8s secrets, 1Password Connect, etc.). Read them with
`os.Getenv()` in your `main.go`. Never commit secrets to the repository.

---

## 8. Docker

### Dockerfile pattern

Every connector uses a multi-stage build. The scaffold generator creates this
automatically:

```dockerfile
FROM docker.io/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/my-source/

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /connector /connector
COPY connectors/my-source/config.json /etc/connector/config.json
USER nonroot:nonroot
ENTRYPOINT ["/connector"]
```

### Building

Build from the repository root (not from the connector directory), because
the build context needs access to `go.mod`, `go.sum`, and the `connector/`
library:

```bash
docker build -f connectors/my-source/Dockerfile -t glovebox-my-source:latest .
```

### Running

```bash
docker run \
    -v /path/to/staging:/staging \
    -v /path/to/state:/state \
    -e GLOVEBOX_STAGING_DIR=/staging \
    -e GLOVEBOX_STATE_DIR=/state \
    -e GLOVEBOX_CONNECTOR_CONFIG=/etc/connector/config.json \
    glovebox-my-source:latest
```

Always rebuild the container to deliver code changes. Never copy files into
a running container.

---

## 9. Examples

### RSS Connector (poll-only)

**Location:** `connectors/rss/`

Implements `Connector` only. Polls RSS and Atom feeds on an interval, stages
new entries, and optionally fetches linked page content.

Key patterns to study:
- `connector.go` -- Poll implementation with per-entry checkpointing
- `config.go` -- Config struct with feeds list, link policy, XML types
- `main.go` -- Entry point with config loading and Setup callback
- `connector_test.go` -- Tests with mock HTTP servers, checkpoint verification

### IMAP Connector (poll + watch)

**Location:** `connectors/imap/`

Implements `Connector` + `Watcher` (IMAP IDLE for live events). Polls to
catch up on missed messages, then watches for new arrivals.

Key patterns to study:
- `connector.go` -- Poll and Watch implementations
- `client.go` -- IMAP client abstraction
- `config.go` -- Config with folder list, TLS settings
- `connector_test.go` -- Tests with mock IMAP interactions
