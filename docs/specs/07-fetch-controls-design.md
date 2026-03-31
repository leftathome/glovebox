# Connector Fetch Controls -- Design Specification

**Version 1.1 -- March 2026**

*This document specifies fetch limits, User-Agent compliance, and rate limit
handling for glovebox connectors.*

---

## 1. Purpose

Connectors fetch content from external sources on each poll cycle. Without
controls, a connector could:

1. **Exhaust downstream token budgets** by fetching thousands of items from a
   large backlog, each of which gets scanned and eventually processed by an LLM
2. **Get blocked by APIs** that enforce rate limits (GitHub, GitLab, X, LinkedIn)
3. **Violate web etiquette** by not identifying as a bot or ignoring robots.txt

This spec adds three controls to the connector library:

- **Fetch limits**: cap items per poll to control throughput cost
- **User-Agent**: proper bot identification on all HTTP requests
- **Rate limiter**: respect API rate limit headers and back off on 429s

## 2. Fetch Limits

### 2.1 Configuration

New fields in `BaseConfig`:

```json
{
    "rules": [...],
    "fetch_limits": {
        "per_source": 50,
        "per_poll": 200
    }
}
```

```go
type FetchLimits struct {
    PerSource int `json:"per_source"` // max items per source per poll (0 = unlimited)
    PerPoll   int `json:"per_poll"`   // max items total across all sources (0 = unlimited)
}
```

Adding `FetchLimits` to `BaseConfig` is non-breaking: the zero value means
unlimited, so existing configs without this field behave as before.

### 2.2 FetchCounter

The library provides a thread-safe `FetchCounter` that tracks and enforces
limits:

```go
type FetchCounter struct {
    limits       FetchLimits
    mu           sync.Mutex
    totalCount   int
    sourceCounts map[string]int
}

func NewFetchCounter(limits FetchLimits) *FetchCounter

// TryFetch checks if fetching one more item from the given source is
// allowed. Returns a FetchStatus indicating whether the fetch is allowed
// and why it was denied if not.
func (fc *FetchCounter) TryFetch(source string) FetchStatus

// Count returns the total items fetched so far in this poll cycle.
func (fc *FetchCounter) Count() int

// Reset clears all counts for a new poll cycle.
func (fc *FetchCounter) Reset()
```

```go
type FetchStatus int

const (
    FetchAllowed       FetchStatus = iota
    FetchSourceLimit                       // per-source limit reached
    FetchPollLimit                         // aggregate poll limit reached
)

func (s FetchStatus) Allowed() bool { return s == FetchAllowed }
```

Thread-safety: `FetchCounter` is protected by `sync.Mutex` so it is safe for
connectors that poll multiple sources concurrently.

### 2.3 Connector Usage Pattern

```go
func (c *MyConnector) Poll(ctx context.Context, cp connector.Checkpoint) error {
    for _, source := range c.config.Sources {
        for _, item := range fetchItems(source) {
            status := c.fetchCounter.TryFetch(source.Name)
            if status == connector.FetchPollLimit {
                return nil // global limit -- stop entire poll
            }
            if status == connector.FetchSourceLimit {
                break // source limit -- move to next source
            }
            // stage item...
        }
    }
    return nil
}
```

For pagination-aware connectors, check `TryFetch` **before requesting the next
page**, not just before processing each item:

```go
for page := 1; ; page++ {
    if !c.fetchCounter.TryFetch(source).Allowed() {
        break
    }
    items := fetchPage(source, page)
    // process items...
}
```

### 2.4 Runner Integration

The runner creates a `FetchCounter` from `BaseConfig.FetchLimits` and stores it
on the `StagingWriter` (or a shared struct accessible via `ConnectorContext`).
Before each poll cycle, the runner calls `Reset()` to clear counts.

Since `ConnectorContext` is set up once (via the `Setup` callback), the counter
is a pointer that persists across polls -- only its internal state is reset.
The counter is added to `ConnectorContext`:

```go
type ConnectorContext struct {
    Writer       *StagingWriter
    Matcher      *RuleMatcher
    Metrics      *Metrics
    FetchCounter *FetchCounter  // new
}
```

The runner calls `FetchCounter.Reset()` before each `runPoll` invocation.

### 2.5 Framework-Level Enforcement

As a safety net, the `StagingWriter` also checks the `FetchCounter` in
`Commit()`. If the counter indicates the poll limit is exceeded, `Commit()`
returns an error. This catches connectors that forget to call `TryFetch`,
preventing silent limit violations.

## 3. User-Agent

### 3.1 Standard User-Agent String

The library provides a configurable User-Agent string with a sensible default:

```go
var DefaultUserAgent = "GloveboxBot/0.2.0 (+https://github.com/leftathome/glovebox)"
```

Format follows RFC 7231 and bot conventions:
- `Bot` in the product name signals automated access
- URL where operators can learn about the bot
- No version-specific information beyond the major.minor

The URL is configurable via `HTTPClientOptions.UserAgent` for operators who
prefer not to expose their infrastructure details.

### 3.2 HTTP Client Helper

The library provides an `HTTPClient` that wraps `http.Client` with the standard
User-Agent and optional rate limiting:

```go
func NewHTTPClient(opts HTTPClientOptions) *http.Client

type HTTPClientOptions struct {
    Timeout     time.Duration // default 30s
    UserAgent   string        // default: DefaultUserAgent
    RateLimiter *RateLimiter  // optional, nil = no rate limiting
}
```

The returned `http.Client` uses a custom `RoundTripper` that:
1. **Always** sets the User-Agent header (overwriting any connector-set value)
2. Checks the rate limiter before sending (if configured)
3. Handles 429 responses with backoff (if rate limiter configured)

The User-Agent is always overwritten (not "unless already set") to prevent
connectors from accidentally using ad-hoc strings after migration.

### 3.3 robots.txt

For connectors that fetch web pages (RSS link fetching), the library provides
an optional robots.txt checker:

```go
type RobotsChecker struct {
    client *http.Client
    cache  map[string]*robotsData // origin -> parsed rules
    mu     sync.Mutex
}

func NewRobotsChecker(client *http.Client) *RobotsChecker

// Allowed checks if the GloveboxBot user-agent may fetch the given URL.
func (rc *RobotsChecker) Allowed(ctx context.Context, targetURL string) bool

// Reset clears the cache (call between poll cycles).
func (rc *RobotsChecker) Reset()
```

**Cache bounds:** Max 100 origins cached per poll cycle. LRU eviction when full.

**Failure policy:**
- robots.txt returns 4xx (not found, forbidden): **allow** (standard convention)
- robots.txt returns 5xx (server error): **deny** (standard convention, retry next poll)
- robots.txt fetch times out or DNS fails: **deny** (conservative)
- robots.txt redirect: follow up to 3 redirects, only to the same origin (no cross-origin SSRF)

API connectors (GitHub, Jira, etc.) do not need robots.txt -- they are using
authenticated APIs, not scraping web pages.

### 3.4 ETag / If-Modified-Since (Future Enhancement)

Respecting `ETag`/`If-None-Match` and `Last-Modified`/`If-Modified-Since`
headers would reduce bandwidth and server load for feed polling. This is not
in scope for v0.2.0 but is planned for a future release. The `HTTPClient`
architecture (custom RoundTripper) makes this straightforward to add later.

## 4. Rate Limiter

### 4.1 RateLimiter

```go
type RateLimiter struct {
    remaining int
    resetAt   time.Time
    mu        sync.Mutex
}

func NewRateLimiter() *RateLimiter

// Update reads rate limit headers from an HTTP response and updates
// internal state. Call after every API request.
func (rl *RateLimiter) Update(resp *http.Response)

// Wait blocks until the rate limit allows another request. Returns
// immediately if not rate-limited. Respects context cancellation.
func (rl *RateLimiter) Wait(ctx context.Context) error
```

### 4.2 Header Parsing

Supports standard headers used by major APIs:

| Header | Used by | Format |
|--------|---------|--------|
| `X-RateLimit-Remaining` | GitHub, GitLab, X | Integer (requests left) |
| `X-RateLimit-Reset` | GitHub, GitLab, X | Unix timestamp (seconds) |
| `Retry-After` | Any (429 response) | Seconds to wait |
| `RateLimit-Remaining` | LinkedIn, IETF draft | Integer (requests left) |
| `RateLimit-Reset` | LinkedIn, IETF draft | Seconds until reset |

**Disambiguation:** `X-RateLimit-Reset` is always a Unix timestamp.
`RateLimit-Reset` (IETF draft) is always seconds-until-reset. `Update` checks
for the `X-` prefix to determine parsing mode. If both are present,
`X-RateLimit-*` takes precedence (more established convention).

**Sanity bounds:** Computed wait times are capped at `maxWait` (default 5
minutes). Any header-derived wait exceeding this cap is truncated and a warning
is logged.

### 4.3 Behavior

1. After each response, `Update(resp)` reads headers and updates state
2. Before each request, `Wait(ctx)` checks:
   - If `remaining > 0` or no rate limit info: return immediately
   - If `remaining == 0`: sleep until `resetAt` (capped at 5 minutes), then return
   - If context is cancelled during sleep: return `ctx.Err()`
3. On 429 response: read `Retry-After` header, sleep that duration (capped
   at 5 minutes)
4. Pre-emptive slowdown: when `remaining < 10`, add a small delay (100ms)
   between requests to avoid hitting the wall

### 4.4 Integration with HTTPClient

When `RateLimiter` is provided to `NewHTTPClient`, the custom `RoundTripper`
calls `Wait()` before each request and `Update()` after each response. This
is transparent to connector code.

## 5. Crawl-Delay

The `crawl-delay` directive in robots.txt specifies the minimum time between
requests to the same origin. This is distinct from the poll interval (time
between poll cycles) -- a connector with `fetch_links=true` could fire many
requests to the same origin within a single poll.

For v0.2.0, the `RobotsChecker` reads and respects `crawl-delay` if present,
enforcing a minimum inter-request delay per origin. Default behavior when no
`crawl-delay` is specified: no delay (the rate limiter and fetch limits provide
sufficient control).

## 6. Migration

### 6.1 Existing Connectors

All 10 connectors need to be updated:
- Replace hand-rolled `http.Client{Timeout: 30s}` with `connector.NewHTTPClient`
- Remove ad-hoc User-Agent strings (library handles it)
- Add `FetchCounter` usage in Poll loops
- API connectors (GitHub, GitLab, Jira, Trello, LinkedIn, Meta, X): wire
  `RateLimiter` into HTTPClient
- RSS: use `RobotsChecker` for link fetching
- Bluesky: wire `RateLimiter` for XRPC API

### 6.2 Config

New `fetch_limits` field in connector config. Optional, defaults to unlimited.

## 7. Out of Scope

- OAuth rate limit negotiation (e.g., requesting higher limits)
- Per-endpoint rate limits (one limiter per connector, not per API path)
- ETag/If-Modified-Since conditional requests (planned for future release)
