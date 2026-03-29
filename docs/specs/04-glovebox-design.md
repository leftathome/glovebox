# Glovebox Service — Design Specification

**Version 1.1 — March 2026**

*Distilled from the OpenClaw Home Agent product, feature, and architecture specs. This document covers only the glovebox scanning service — the scope of this repository.*

*Note: The parent architecture spec (03) lists Node.js or Python as candidate languages. Go was selected during implementation planning for reasons documented in Section 3. This decision should be reflected back into the parent spec.*

---

## 1. Purpose

The glovebox is a deterministic content scanning service that sits between external data connectors and the OpenClaw agent workspaces. It inspects incoming content for prompt injection attacks and other adversarial patterns, then routes each item to one of three outcomes: PASS (deliver to agent), QUARANTINE (hold for human review), or REJECT (discard structural/authentication failures).

It is **not** an OpenClaw agent. It has no LLM dependency in Phase 1, no tools, no memory, no conversation context. It reads, assesses, and routes.

### 1.1 Design Invariants

These constraints are absolute and must hold across all code paths:

- **The glovebox never modifies content.** It reads, assesses, and routes. Content arrives and leaves byte-identical.
- **The glovebox has no network egress.** It operates on the local filesystem only. Enforced at the container/network level (Cilium NetworkPolicy in Phase 1, application firewall in Phase 2).
- **The glovebox has no delete access to the audit log.** Append-only access is enforced at the filesystem permission level, not just by convention.
- **No item reaches an agent workspace without being scanned.** There is no code path that bypasses the heuristic engine.

## 2. Scope of This Repository

This repo implements:

- **Staging watcher** — monitors the staging directory for new items from connectors
- **Heuristic engine** — configurable weighted pattern matching and custom detectors
- **Content pre-processing** — Unicode normalization, HTML stripping, streaming scan
- **Verdict & routing** — moves items to agent workspaces, quarantine, or reject
- **Pending item placeholders** — scan-in-progress markers in agent inboxes for ordered delivery
- **Audit logging** — append-only JSONL logs for all processed items
- **Connector interface contract** — defines the schema and directory structure connectors must follow (connectors themselves may live in separate repos)
- **OCI image** — containerized build for Kubernetes deployment
- **Helm chart** — Kubernetes deployment manifests

This repo does **not** implement:

- OpenClaw agents or the review agent
- Connectors (only the interface they must conform to)
- The Phase 2 LLM classifier (architecture is designed to accommodate it)
- Monitoring stack (Prometheus/Grafana) — the glovebox exposes metrics; the stack is deployed separately

## 3. Implementation Language

**Go** — chosen for:

- Single static binary, minimal OCI image
- Low memory footprint (runs on Intel NUC alongside other workloads)
- Excellent filesystem watching (`fsnotify`)
- Native concurrency model (goroutine worker pool for parallel scanning)
- Strong string/byte analysis for heuristic detectors
- `lingua-go` for language detection
- Streaming I/O for bounded-memory scanning of large content
- Phase 2 LLM classifier is an HTTP call, not an ML library dependency

## 4. Data Flow

```
Connectors write to staging/
        |
        v
  +------------------+
  |  Watcher          |  (fsnotify primary, polling fallback)
  |  detects new      |  Items sorted by directory name (timestamp-prefixed)
  |  item dirs        |
  +--------+---------+
           |
           v
  +------------------+
  |  Validate item    |  content.raw + metadata.json must exist
  |  Parse metadata   |  Reject if malformed or auth failure
  +--------+---------+
           |
           v
  +------------------+
  |  Item channel     |  Feeds validated items to worker pool
  +--------+---------+
           |
     +-----+------+------+
     v     v      v      v
  +----+ +----+ +----+ +----+
  | W1 | | W2 | | W3 | | W4 |   Scan workers (configurable pool size)
  +--+-+ +--+-+ +--+-+ +--+-+   Each: pre-process -> scan -> ScanResult
     |      |      |      |      Per-item timeout: quarantine on expiry
     +------+------+------+
           |
           v
  +------------------+
  |  Router           |  Receives ScanResults
  |  (sequential)     |  Orders by item timestamp for delivery
  +--+-----+-----+--+
     |     |     |
     v     v     v
   PASS   Q    REJECT
     |     |     |
     v     v     v
  Deliver  Quarantine   Log + delete
  to inbox dir+notify
     |     |     |
     v     v     v
  +------------------+
  |  Audit Log        |  Single entry per completed item
  +------------------+
```

## 5. Staging Item Schema (Connector Interface Contract)

### 5.1 Directory Structure

Connectors write each item as a subdirectory under `staging/`. To ensure atomic handoff, connectors MUST:

1. Write the item to a temporary directory outside `staging/` (e.g., `staging-tmp/`)
2. Once both files are fully written, atomically rename the directory into `staging/`

This prevents the glovebox from reading a partially-written item.

```
staging/<timestamp>-<uuid>/
  content.raw      # Original content (email body, webhook payload, etc.)
  metadata.json    # Structured metadata
```

### 5.2 metadata.json Schema

```json
{
  "source": "email|webhook|unifi|sms",
  "sender": "string -- originator identifier (email address, webhook ID, etc.)",
  "subject": "string -- optional, human-readable summary",
  "timestamp": "RFC3339 timestamp of original content",
  "destination_agent": "messaging|media|calendar|itinerary",
  "content_type": "text/plain|text/html|application/json",
  "ordered": false
}
```

Required fields: `source`, `sender`, `timestamp`, `destination_agent`, `content_type`.
Optional fields: `subject`, `ordered` (default: `false`).

Items missing required fields are REJECTED with reason `malformed_metadata`.

### 5.3 Content Extraction Requirement

Connectors MUST decode and extract human-readable content from structured formats before writing `content.raw`. For example:
- Email connectors extract decoded body text from MIME structure (not raw MIME with base64 transport encoding)
- If a message has multiple representations (text/plain + text/html), the connector should concatenate them with a separator
- Webhook connectors extract the payload body, not the HTTP envelope

The glovebox scans decoded content, not raw transport encoding.

### 5.4 Metadata Field Constraints

All string fields in `metadata.json` are validated at ingestion:
- Maximum length: 1024 characters for `sender`, `source`, `subject`; 64 characters for `destination_agent`, `content_type`
- No control characters (U+0000-U+001F) permitted except in `subject` (where they are stripped)
- `destination_agent` MUST match a value in the configured agent allowlist (see Section 7.4)

### 5.5 Source Authentication

Connectors are responsible for authenticating their external sources (e.g., verifying IMAP credentials, validating webhook signatures). If authentication fails, the connector writes the item with a metadata field `"auth_failure": true`. The glovebox REJECTS items with `auth_failure: true` with reason `source_auth_failure`.

## 6. Heuristic Engine

### 6.1 Rule Configuration

Rules are loaded from a JSON configuration file at startup. Hot-reload on config file change is a Phase 1 stretch goal (see Section 13).

```json
{
  "rules": [ ... ],
  "quarantine_threshold": 0.8
}
```

Each rule has:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Unique identifier for the rule |
| `patterns` | []string | Strings or regex patterns to match |
| `weight` | float64 | Signal weight when triggered (0.0-1.0) |
| `match_type` | enum | `substring`, `substring_case_insensitive`, `regex`, `custom_detector` |
| `detector` | string | For custom_detector type: function name to invoke |
| `behavior` | string | Optional: `weight_booster` changes how signal is applied |
| `boost_factor` | float64 | For weight_booster behavior: multiplier for other signals |

#### Rule Configuration Validation

At load time, the following validation is enforced. The glovebox refuses to start (or refuses to reload) if validation fails:

- `weight` must be in range [0.0, 1.0]
- `boost_factor` must be in range [1.0, 3.0]
- `quarantine_threshold` must be in range (0.0, 2.0]
- `match_type` must be a known enum value
- `custom_detector` rules must have a non-empty `detector` field that maps to a registered detector
- `regex` rules must have compilable patterns (validated at load, not at match time)
- At least one rule must be configured (refuse to start with empty ruleset)

The loaded rule configuration is logged at startup for auditability.

### 6.2 Content Pre-processing

Before heuristic rules are evaluated, content is pre-processed to defeat common evasion techniques:

1. **Unicode NFKC normalization** — maps fullwidth characters, compatibility characters, and many visually-similar characters (homoglyphs) to their canonical equivalents. This prevents evasion via Cyrillic "o", fullwidth Latin, etc.
2. **Zero-width character stripping** — removes U+200B, U+200C, U+200D, U+FEFF, U+2060, U+200E, U+200F and other invisible Unicode before matching.
3. **HTML tag stripping** (for `text/html` content) — strips all HTML tags to produce plain text, decodes HTML entities. Heuristic rules are run against BOTH the raw HTML and the stripped plain text, catching payloads hidden in tag structure and payloads in attributes/comments.

Pre-processing produces a normalized content buffer used by matchers. The original content is preserved unchanged for routing (invariant: glovebox never modifies content).

### 6.3 Built-in Rules (Phase 1)

| Rule | Match Type | Weight | Notes |
|------|-----------|--------|-------|
| `instruction_override` | regex | 1.0 | `ignore\s+(\w+\s+)*previous`, `disregard\s+(\w+\s+)*your\s+instructions`, and variants with flexible word boundaries |
| `role_reassignment` | regex | 1.0 | `you\s+are\s+now`, `act\s+as`, `pretend\s+you\s+are`, and variants |
| `tool_invocation_syntax` | substring | 0.8 | `<tool>`, `<function_call>`, `exec:`, `bash:`, etc. |
| `suspicious_encoding` | custom_detector | 0.7 | Base64 blocks in plain text, zero-width chars, excessive unicode escapes |
| `prompt_template_structure` | custom_detector | 0.6 | Content resembling system prompts or instruction blocks |
| `non_english_content` | custom_detector | 0.0 (booster) | Multiplies other signal weights by 1.5x when detected |

Note: `instruction_override` and `role_reassignment` use regex with flexible word boundaries instead of exact substring matching. This catches variants like "ignore all previous", "ignore your previous", "disregard any and all your instructions" where words are inserted between key terms.

### 6.4 Signal Compounding

1. Each rule evaluates independently against the pre-processed content
2. Triggered rules contribute their weight to a running total
3. If `non_english_content` fires, all other signal weights are multiplied by `boost_factor` before summing
4. If the total meets or exceeds `quarantine_threshold`, verdict is QUARANTINE
5. Non-English alone never triggers quarantine (weight is 0.0)

### 6.5 Custom Detectors

Custom detectors are Go functions registered by name. Phase 1 detectors:

- **`encoding_anomaly`** — detects base64-encoded blocks in plain text context (regex for base64 patterns of significant length), counts zero-width unicode characters (U+200B, U+200C, U+200D, U+FEFF, etc.), flags excessive unicode escape sequences. Note: zero-width stripping in pre-processing handles evasion; this detector additionally flags their *presence* as a suspicious signal.
- **`template_structure`** — detects content resembling LLM system prompts: "You are a...", "Your instructions are...", XML-like instruction tags (`<system>`, `<instructions>`, `<prompt>`), markdown-formatted instruction blocks with role headers (`## System`, `## Instructions`), delimited instruction sections (`---BEGIN INSTRUCTIONS---`). Distinguishes prompt-like patterns from conversational use (e.g., "you are invited" does not trigger).
- **`language_detection`** — uses `lingua-go` to classify content language; fires when primary language is not English. Requires minimum content length (20+ characters) for reliable detection. Returns detected language in signal details.

Custom detector signals include a `matched` field containing a human-readable description of what was found (e.g., `"base64 block at offset 1234, length 512"`, `"detected language: French (confidence: 0.94)"`).

### 6.6 Streaming Scan

To bound memory usage regardless of content size, the heuristic engine scans content using a streaming approach:

- Content is read in chunks via a buffered reader with configurable chunk size (default: 256KB)
- Overlap between chunks (equal to the longest pattern length) ensures matches spanning chunk boundaries are not missed
- Custom detectors that need global properties (language detection, encoding anomaly) operate on a sampled prefix + suffix (first 64KB + last 64KB)
- Memory usage is bounded to approximately `num_workers * chunk_buffer_size`, not `num_workers * file_size`

### 6.7 Per-Item Processing Timeout

Each scan worker operates with a configurable per-item timeout (default: 30 seconds). If scanning does not complete within the timeout, the item is QUARANTINED with reason `scan_timeout`. This prevents pathologically large or complex content from stalling the pipeline.

## 7. Verdict & Routing

### 7.1 Path Safety

Before routing any item, the glovebox validates the destination path:

1. `destination_agent` is checked against a configured allowlist of known agent names (e.g., `["messaging", "media", "calendar", "itinerary"]`). Any value not in the allowlist results in REJECT with reason `unknown_destination`.
2. The resolved destination path is canonicalized (`filepath.Abs` + `filepath.EvalSymlinks`) and confirmed to be a subdirectory of `agents_dir`. If the resolved path escapes `agents_dir`, the item is REJECTED with reason `path_traversal`.

This prevents path traversal attacks via crafted `destination_agent` values.

### 7.2 PASS

- Total signal weight below threshold
- Content moved from `staging/<item>/` to `agents/<destination_agent>/workspace/inbox/<item-id>/`
- If a `.pending.json` placeholder exists for this item, it is replaced by the delivered content directory
- Entry appended to `audit/pass.jsonl`
- Staging item directory deleted after successful move

### 7.3 QUARANTINE

- Total signal weight meets or exceeds threshold, or scan timeout exceeded
- Content moved to `quarantine/<timestamp>-<hash>/` containing:
  - `content.sanitized` — sanitized content representation (see Section 7.6)
  - `metadata.json` — enriched metadata containing: all original metadata fields (source, sender, subject, timestamp, destination_agent, content_type), plus: signals fired (names, weights, matched details), total score, quarantine threshold at time of scan, scan duration, reason
- If a `.pending.json` placeholder exists for this item, it is removed
- Notification entry appended to quarantine notification (see Section 7.7)
- Entry appended to `audit/rejected.jsonl`
- Staging item directory deleted after successful move

### 7.4 REJECT

- Reserved for structural and authentication failures in Phase 1:
  - Malformed metadata (missing required fields, invalid JSON)
  - Content file missing or unreadable
  - Item directory structure invalid
  - Source authentication failure (`auth_failure: true` in metadata)
  - Unknown destination agent (not in allowlist)
  - Path traversal detected in destination
- If a `.pending.json` placeholder exists for this item, it is removed
- Metadata appended to `audit/rejected.jsonl` with reason
- For items where metadata is unparseable, the `destination` field in the audit entry is set to `"unknown"`
- Content deleted
- No notification (structural failures are not security events)

### 7.5 Items Held for Retry

If a routing error occurs (e.g., destination directory temporarily unavailable), the item is moved to the `failed/` directory (not held in staging). On the next processing tick, items in `failed/` are **rescanned from scratch** before routing. This prevents stale scan verdicts from being applied after rules have been updated (TOCTOU mitigation).

Items in `failed/` are UNSCANNED and UNTRUSTED. Recovery from `failed/` always goes through the full scan pipeline, never directly to agent workspaces.

### 7.6 Content Sanitization for Quarantine

Quarantined content is by definition suspected malicious. The `content.sanitized` file is produced as follows:

1. Extract the first 4096 characters of content only (sufficient for review context, limits exposure)
2. Replace all non-ASCII characters with their Unicode escape representation (`\uXXXX`)
3. Wrap in a plaintext block clearly labeled as untrusted:
   ```
   --- UNTRUSTED QUARANTINED CONTENT (first 4096 chars) ---
   <escaped content>
   --- END UNTRUSTED CONTENT ---
   ```
4. The full content hash (SHA-256 of complete `content.raw`) is recorded in the quarantine `metadata.json` for forensic correlation

The Review Agent is constrained to never read this file (enforced by its system prompt and tool surface), but defense-in-depth requires the file itself to be as inert as possible.

### 7.7 Quarantine Notification Schema

Notifications are written as individual files in `shared/glovebox-notifications/` (one file per quarantined item, Maildir-style) rather than appending to a single file. This eliminates concurrent-write concerns.

Each notification file is named `<timestamp>-<hash>.json` and contains:

```json
{
  "quarantine_id": "<timestamp>-<hash>",
  "source": "email",
  "sender": "alice@example.com",
  "subject": "Re: meeting notes",
  "timestamp": "RFC3339",
  "content_length": 1234,
  "signals": ["instruction_override", "suspicious_encoding"],
  "total_score": 1.7,
  "quarantined_at": "RFC3339"
}
```

This file contains metadata only -- never raw content. The Review Agent reads these files to present quarantine items to the user.

## 8. Pending Item Placeholders

When the glovebox begins scanning an item whose metadata has `"ordered": true`, it writes a placeholder file to the destination agent's inbox before scanning starts:

```
agents/<destination_agent>/workspace/inbox/<timestamp>-<uuid>.pending.json
```

Contents:
```json
{
  "status": "scanning",
  "source": "email",
  "sender": "alice@example.com",
  "subject": "Re: meeting notes",
  "timestamp": "RFC3339",
  "received_at": "RFC3339"
}
```

**Lifecycle:**
- **PASS**: placeholder is replaced by the full content delivery directory
- **QUARANTINE or REJECT**: placeholder is removed
- **Crash recovery**: on startup, the glovebox removes all stale `.pending.json` files from agent inboxes and logs a warning for each

**Agent contract:** Directories in the inbox are ready to process. `.pending.json` files are informational only -- they indicate content is in the pipeline. Agents may use them for ordering awareness or ignore them entirely.

Connectors that set `"ordered": false` (the default) skip placeholder generation. Placeholders are primarily useful for ordered sources like email where message sequence matters.

## 9. Parallel Scan Architecture

### 9.1 Worker Pool

The glovebox runs a configurable number of scan workers (default: 4) as goroutines. The watcher feeds validated items into a channel; workers pull from the channel, scan, and produce ScanResults.

### 9.2 Ordering Guarantee

Scanning happens in parallel, but delivery ordering is preserved where needed:

- For items with `"ordered": true`: the router holds completed ScanResults and delivers them in FIFO order (sorted by item timestamp from directory name). A completed scan for item C waits if item B (same destination) is still scanning.
- For items with `"ordered": false`: delivered immediately on scan completion, no ordering wait.

### 9.3 Scan Timeout

Each worker's scan operates under a per-item context deadline (configurable, default: 30 seconds). On timeout, the worker produces a QUARANTINE result with reason `scan_timeout` and moves on.

## 10. Audit Log

### 10.1 Schema

All metadata string fields are serialized using Go's `encoding/json` (which properly escapes control characters, quotes, and backslashes). Log entries are never constructed via string concatenation.

Content hash is SHA-256 of the complete `content.raw` file, hex-encoded.

#### pass.jsonl

```json
{
  "timestamp": "RFC3339",
  "source": "string",
  "sender": "string",
  "content_hash": "sha256 hex",
  "content_length": 1234,
  "signals": [],
  "total_score": 0.0,
  "verdict": "pass",
  "destination": "messaging",
  "scan_duration_ms": 42
}
```

#### rejected.jsonl

```json
{
  "timestamp": "RFC3339",
  "source": "string",
  "sender": "string",
  "content_hash": "sha256 hex",
  "content_length": 1234,
  "signals": [
    {"name": "instruction_override", "weight": 1.0, "matched": "matched pattern: ignore\\s+previous at offset 234"}
  ],
  "total_score": 1.0,
  "verdict": "quarantine|reject",
  "reason": "threshold_exceeded|scan_timeout|malformed_metadata|content_unreadable|source_auth_failure|unknown_destination|path_traversal",
  "destination": "messaging|unknown",
  "scan_duration_ms": 42
}
```

For REJECT verdicts where metadata is unparseable, `destination` is set to `"unknown"`, and `source`/`sender` fields are set to `"unknown"` as well.

### 10.2 Audit Failure Behavior (Fail-Closed)

If an audit log write fails, the glovebox enters **degraded mode**:

- ALL subsequent items are QUARANTINED regardless of scan result, with reason `audit_failure`
- An alert metric is incremented (`glovebox_audit_failures_total`)
- A log message is written to stderr on every failed write
- The glovebox retries audit writes on each subsequent item; normal operation resumes automatically when writes succeed again

Audit logs SHOULD be on a separate volume from content directories to prevent a disk-fill attack against content from simultaneously disabling audit.

## 11. Metrics

The glovebox is instrumented with OpenTelemetry (Go SDK). Metrics are exported via the OTel Prometheus exporter, exposing a `/metrics` endpoint compatible with Prometheus scraping. This keeps instrumentation vendor-neutral while maintaining compatibility with the Phase 1 Prometheus/Grafana monitoring stack.

Metrics:

- `glovebox_items_processed_total` (counter, labels: verdict, destination, source)
- `glovebox_processing_duration_seconds` (histogram, labels: source)
- `glovebox_signals_triggered_total` (counter, labels: rule_name)
- `glovebox_staging_queue_depth` (gauge)
- `glovebox_quarantine_queue_depth` (gauge)
- `glovebox_pending_items` (gauge, labels: source, destination_agent)
- `glovebox_scan_workers_busy` (gauge)
- `glovebox_scan_timeouts_total` (counter, labels: source)
- `glovebox_audit_failures_total` (counter)
- `glovebox_failed_items` (gauge) — items in failed/ directory awaiting rescan

OTel also provides the extension point for adding distributed tracing and structured log export in Phase 2 without changing instrumentation code.

## 12. Configuration

Single config file (`config.json` or environment variable override):

```json
{
  "staging_dir": "/data/glovebox/staging",
  "quarantine_dir": "/data/glovebox/quarantine",
  "audit_dir": "/data/glovebox/audit",
  "failed_dir": "/data/glovebox/failed",
  "agents_dir": "/data/agents",
  "shared_dir": "/data/shared",
  "agent_allowlist": ["messaging", "media", "calendar", "itinerary"],
  "metrics_port": 9090,
  "watch_mode": "fsnotify",
  "poll_interval_seconds": 5,
  "rules_file": "/etc/glovebox/rules.json",
  "scan_workers": 4,
  "scan_timeout_seconds": 30,
  "scan_chunk_size_bytes": 262144
}
```

Directory paths are configurable to support both Kubernetes volume mounts (Phase 1) and native filesystem paths (Phase 2).

### 12.1 Backup-Critical Artifacts

The following artifacts produced by this service must be included in the system backup strategy (documented in the parent architecture spec):

- **Audit logs** (`audit/pass.jsonl`, `audit/rejected.jsonl`) — hourly backup
- **Rules configuration** (`rules.json`) — daily backup (also versioned in Git)
- **Quarantine directory** — daily backup (contains items awaiting review)

Transient directories (`staging/`, `failed/`) are NOT backed up.

## 13. Error Handling

- **Watcher errors** (fsnotify failure): fall back to polling mode, log warning
- **Processing errors** (file read failure mid-scan): move item to `failed/` directory, log error, continue processing queue. Items in `failed/` are rescanned on the next processing tick (see Section 7.5).
- **Routing errors** (destination directory missing): move item to `failed/` directory, log error. Do not hold items in staging.
- **Audit log write failure**: enter degraded mode -- quarantine all items until audit writes recover (see Section 10.2).
- **Stale pending files on startup**: remove all `.pending.json` files from agent inboxes, log a warning for each.

## 14. Graceful Shutdown

On receiving SIGTERM:

1. Stop accepting new items from the watcher
2. Wait for all in-flight scan workers to complete their current item (or timeout)
3. Route all completed scan results
4. Write final audit entries
5. Remove any `.pending.json` files for items that were in-flight but not delivered
6. Exit

In-progress operations complete or are cleanly rolled back. No item is left in a partial state.

## 15. Testing Strategy

### Unit Tests
- Heuristic engine: each rule tested in isolation with positive and negative cases
- Content pre-processing: Unicode normalization, HTML stripping, zero-width removal
- Custom detectors: each detector tested with crafted inputs
- Signal compounding: verify weight accumulation, boost multiplier, threshold comparison
- Metadata validation: valid/invalid/partial metadata parsing, allowlist enforcement
- Path safety: traversal attempts detected and rejected
- Verdict routing: verify correct file movements for each verdict type
- Pending placeholder lifecycle: created/replaced/removed correctly
- Streaming scan: matches found across chunk boundaries
- Audit log serialization: proper JSON escaping, no injection via metadata fields

### Integration Tests
- End-to-end: write item to staging, verify it appears in correct destination
- Quarantine flow: write adversarial content, verify quarantine + notification + audit
- Reject flow: write malformed item, verify rejection + audit + cleanup
- Parallel processing: multiple items in staging, all processed correctly
- Ordered delivery: items with `ordered: true` delivered in FIFO order despite parallel scanning
- Audit failure degraded mode: simulate write failure, verify all items quarantined
- Failed item rescan: item in failed/ is rescanned with current rules, not stale verdict
- Scan timeout: oversized item quarantined on timeout, other items unaffected

### Container Tests
- Build OCI image, run it with mounted test directories, verify processing works inside container
- Verify metrics endpoint is reachable
- Verify graceful shutdown on SIGTERM

## 16. Phase 2 Extension Points

The design accommodates Phase 2 additions without restructuring:

- **LLM classifier**: inserts after heuristic engine in the scan worker pipeline. Items that PASS heuristics get a second evaluation via HTTP call to local inference endpoint. Heuristic QUARANTINE is never downgraded.
- **Hot-reload**: config file watcher triggers rule reload using swap-on-boundary -- new rules take effect for the next item, not the current one. Rules file should have integrity checking (checksum verification).
- **Additional detectors**: new custom detectors are registered by adding a Go function and a config entry.
- **Rate limiting**: per-source rate limits on staging ingestion to prevent connector flood attacks.
