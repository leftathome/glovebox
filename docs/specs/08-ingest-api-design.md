# HTTP Ingest API -- Design Specification

**Version 1.0 -- April 2026**

*This document specifies the HTTP-based content ingest transport between
connectors and the glovebox scanner, replacing the shared filesystem staging
pattern for Kubernetes deployments.*

---

## 1. Problem Statement

The current architecture couples connectors to the glovebox scanner via a shared
filesystem (the staging PVC). Connectors write items to a shared directory; the
scanner watches it with fsnotify. This design has three operational problems that
worsen as the number of connectors grows:

1. **ReadWriteMany requirement.** The staging PVC is mounted by the scanner pod
   and every enabled connector pod. RWO volumes cannot be shared across pods on
   different nodes. RWX volumes require a storage class that supports them (NFS,
   CephFS, etc.), which many clusters -- including single-node homelab setups on
   OpenEBS -- don't provide.

2. **Co-location constraint.** When using RWO (the most common access mode),
   all pods sharing the volume must run on the same node. This forces
   nodeSelector/affinity hacks that shouldn't be necessary for correctness.

3. **Permission coordination.** The scanner and connectors must agree on UIDs,
   GIDs, and fsGroup settings. The connector distroless images run as UID 65534;
   the scanner may run as a different user. Mismatched filesystem permissions
   cause silent failures (`mkdir staging-tmp: permission denied`).

Every production system in this space -- OpenTelemetry Collector, Fluent Bit,
Falco/Falcosidekick, Datadog Vector -- uses HTTP or gRPC push as the transport
between agents and a central processor. None use shared filesystems for cross-pod
data transfer.

## 2. Design Overview

Replace the shared staging PVC with an HTTP ingest endpoint on the scanner.
Connectors POST items to the scanner instead of writing to disk. The scanner
writes received items to its own local staging directory, where the existing
fsnotify watcher picks them up unchanged.

```
Before:                              After:

Connector --[filesystem]--> staging/ Connector --[HTTP POST]--> scanner:9091/v1/ingest
Connector --[filesystem]--> staging/     |
Connector --[filesystem]--> staging/     v
         |                           scanner writes to local staging/
         v                               |
     scanner (fsnotify)                  v
                                     scanner (fsnotify) -- unchanged
```

### 2.1 Design Invariants

All invariants from spec 04, Section 1.1 are preserved:

- **The glovebox never modifies content.** The ingest endpoint writes received
  bytes to disk without transformation.
- **No item reaches an agent workspace without being scanned.** HTTP ingest
  writes to the staging directory; the existing watcher/scan pipeline processes
  items from there.
- **The glovebox has no delete access to the audit log.** Unchanged.
- **The glovebox has no network egress.** The scanner accepts inbound
  connections from connectors on the ingest port (9091). It makes no outbound
  connections. Cilium/NetworkPolicy enforcement: ingress on port 9091 from
  connector pods only, ingress on port 9090 (metrics) from any source, no
  egress. The scanner remains a passive receiver.

### 2.2 Port Separation

The scanner exposes two ports:

| Port | Purpose | Access |
|------|---------|--------|
| 9090 | Metrics (`/metrics`) | Unrestricted ingress (Prometheus, monitoring) |
| 9091 | Ingest (`/v1/ingest`) | Restricted to connector pods via NetworkPolicy |

Separating ingest from metrics ensures that the NetworkPolicy restriction on
ingest is not bypassed by an unrestricted metrics rule on the same port. Standard
Kubernetes NetworkPolicy operates at L3/L4 and cannot distinguish paths on the
same port.

### 2.3 What Changes

| Component | Change |
|-----------|--------|
| Scanner | New HTTP listener on port 9091 with `/v1/ingest` endpoint |
| Scanner config | New `ingest` config block (port, limits, backpressure) |
| Connector library | New `StagingBackend` interface with `HTTPStagingBackend` |
| Connector `main.go` | Wire `GLOVEBOX_INGEST_URL` instead of `GLOVEBOX_STAGING_DIR` |
| Helm chart | Remove shared staging PVC from connectors; add ingest env var and Service |
| NetworkPolicy | Scanner ingress: port 9091 from connectors only, port 9090 unrestricted |

### 2.4 What Does Not Change

- The `ItemOptions` / `ItemMetadata` contract (specs 04, 05, 06)
- The `NewItem` -> `WriteContent` -> `Commit` API surface
- The scanner's internal processing pipeline (watcher, heuristic engine, routing)
- The other 5 PVCs (quarantine, audit, failed, agents, shared) -- scanner-only
- Checkpoint persistence (per-connector state directory)
- Health endpoints, metrics, and probes on connectors
- Phase 2 macOS deployment (filesystem mode, no HTTP ingest on macOS)

## 3. Scanner Ingest Endpoint

### 3.1 Endpoint

```
POST /v1/ingest
Content-Type: multipart/form-data
```

The ingest listener binds to a dedicated port (default 9091, configurable via
`ingest.port`). This is a separate `http.Server` from the metrics listener
(port 9090).

### 3.2 Request Format

Multipart form with exactly two parts:

| Part name | Content-Type | Max size | Description |
|-----------|-------------|----------|-------------|
| `metadata` | `application/json` | 256 KB | ItemMetadata JSON (same schema as metadata.json) |
| `content` | `application/octet-stream` | 64 MB | Raw content bytes (same as content.raw) |

The handler processes exactly these two parts. Requests with missing, duplicate,
or unexpected parts are rejected with 400. The `metadata` part Content-Type must
be `application/json`.

### 3.3 Connector Identification

Connectors must include a `X-Glovebox-Connector` header with their configured
connector name (e.g., `imap`, `rss`, `github`). The `HTTPStagingBackend` sets
this automatically from the connector name passed to `connector.Run`.

This header is not authentication (connectors can set any value). It enables
operational visibility: ingest metrics are labeled by connector name, and log
entries include the claimed source. In Phase 2, the bearer token carries verified
connector identity (see Section 3.10).

### 3.4 Request Size Limits

- **Metadata part:** Maximum 256 KB. Metadata is small (typically under 1 KB);
  this limit prevents memory exhaustion from oversized JSON payloads.
- **Content part:** Maximum 64 MB. This accommodates the largest expected content
  items (email attachments, long documents) with headroom.
- **Total request body:** Maximum 64 MB (inclusive of multipart framing).

Items exceeding any limit receive `413 Payload Too Large`. All limits are
configurable via the scanner config.

### 3.5 Response Codes

| Code | Meaning | Connector behavior |
|------|---------|-------------------|
| `202 Accepted` | Item written to staging successfully | Advance checkpoint |
| `400 Bad Request` | Malformed request (see below) | Log error, do not retry (permanent) |
| `413 Payload Too Large` | Content exceeds size limit | Log error, do not retry (permanent) |
| `429 Too Many Requests` | Scanner is overloaded | Retry with backoff; honor `Retry-After` header |
| `503 Service Unavailable` | Scanner shutting down or not ready | Retry with backoff |

400 reasons: missing `metadata` or `content` part, duplicate parts, unexpected
parts, invalid metadata JSON, metadata validation failure (per spec 04 Section
5.4), non-JSON Content-Type on metadata part.

The scanner returns `202 Accepted` (not `200 OK`) because the item has been
received and persisted to staging but not yet scanned. Scanning is asynchronous.

### 3.6 Response Body

All responses use `Content-Type: application/json`.

Success (`202`):
```json
{"status": "accepted", "item_id": "<timestamp>-<uuid>"}
```

The `item_id` matches the staging directory name and appears in audit log entries
for cross-referencing.

Error (`400`, `413`):
```json
{"status": "error", "message": "<human-readable reason>"}
```

Backpressure (`429`):
```json
{"status": "backpressure", "retry_after_seconds": 5}
```

Unavailable (`503`):
```json
{"status": "unavailable", "message": "scanner not ready"}
```

### 3.7 Backpressure

The scanner tracks the number of unprocessed items using an atomic counter
(not a directory listing -- directory scans are too slow and race under
concurrency). The counter is incremented when an ingest write completes and
decremented when the watcher dispatches an item to the scan worker pool.

On startup, the counter is initialized by counting existing items in the staging
directory. This ensures backpressure is accurate even after a crash that left
unprocessed items.

When the counter exceeds the high-water mark (`ingest.backpressure_threshold`,
default: 100), the endpoint returns `429 Too Many Requests` with a
`Retry-After` header. The check occurs before writing begins, preventing
concurrent requests from all bypassing the threshold.

### 3.8 Atomicity

The ingest handler follows the same atomic handoff protocol connectors previously
performed directly on the filesystem:

```
1. Check backpressure counter (reject if above threshold)
2. Read multipart request (streaming, bounded by size limits)
3. Create temp dir: <staging>/.ingest-tmp/<timestamp>-<uuid>/
4. Write content.raw to temp dir (streaming from request body)
5. Validate metadata (same validation as spec 04, Section 5.4)
6. Write metadata.json to temp dir
7. Atomic rename to <staging>/<timestamp>-<uuid>/
8. Increment backpressure counter
9. Return 202 with item_id
```

If any step fails, the temp directory is cleaned up and an error response is
returned. No partial items are visible to the watcher.

**Startup cleanup:** On startup, the scanner deletes `<staging>/.ingest-tmp/`
entirely, removing any orphaned temp directories from incomplete ingests during
a previous crash. This is separate from the connector-side orphan cleanup in
`staging-tmp/` (spec 05 Section 5.1).

### 3.9 Startup Readiness

The ingest endpoint returns `503 Service Unavailable` until the scanner has
completed initialization:
1. Config loaded and validated
2. Rules loaded and compiled
3. Staging directory verified writable
4. Watcher started

This prevents connectors from sending items before the scanner can process them.
The readiness gate aligns with the existing `/readyz` probe pattern.

### 3.10 Authentication

**Phase 1:** The ingest endpoint accepts requests from any source that can reach
port 9091. Access is restricted at the network level via Kubernetes NetworkPolicy
(only pods with `app.kubernetes.io/component: connector` can reach port 9091).

**Known limitation:** In Phase 1, a compromised connector can set arbitrary
values in metadata fields (`source`, `identity`, `destination_agent`, `tags`).
The scanner validates field formats and checks `destination_agent` against the
agent allowlist, but does not verify that metadata matches the connector's actual
identity. This is acceptable for the single-user home agent use case where all
connectors are trusted first-party code.

**Phase 2:** Add a shared bearer token for defense-in-depth. The token is
distributed to connectors via Kubernetes secrets. The ingest endpoint validates
the token and extracts connector identity from it, enabling enforcement that
`source` and `identity.provider` match the authenticated connector. This also
provides per-connector rate limiting and replay detection.

**Transport security:** Phase 1 uses plaintext HTTP within the cluster network.
If a service mesh (Istio, Linkerd) provides transparent mTLS, the endpoint
benefits automatically. Application-level TLS is not required for Phase 1.

### 3.11 Request Timeout

The ingest endpoint enforces a per-request timeout of 60 seconds (configurable
via `ingest.request_timeout_seconds`). Requests exceeding this (e.g., slow
uploads) are terminated and the temp directory is cleaned up.

### 3.12 Unified Metrics Model

HTTP ingest unifies the metrics model so that a single label -- `source` (from
the item's metadata `source` field, e.g., `imap`, `rss`, `github`) -- threads
through every stage of the pipeline. This replaces the previous split between
connector-side and scanner-side metric vocabularies.

#### 3.12.1 Connector Metrics (unchanged, fetch-side only)

Connectors retain metrics about their fetch operations. These are not duplicated
on the scanner.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `connector_polls_total` | counter | connector, status | Poll attempts and outcomes |
| `connector_poll_duration_seconds` | histogram | connector | Time spent in each Poll call |
| `connector_errors_total` | counter | connector, type | Fetch errors (transient/permanent) |
| `connector_checkpoint_age_seconds` | gauge | connector | Staleness of last checkpoint |
| `connector_items_dropped_total` | counter | connector, reason | Items skipped (no matching rule) |

**Removed:** `connector_items_produced_total`. The scanner is the authoritative
counter for items received. Connectors do not independently count items sent --
the HTTP 202 response is the confirmation, and the scanner records the receipt.

#### 3.12.2 Scanner Receive Metrics (new)

The scanner records every item received, regardless of transport mode (HTTP or
filesystem). All metrics use the `source` label from item metadata.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `glovebox_items_received_total` | counter | source, status | Items received (accepted/rejected/throttled) |
| `glovebox_receive_duration_seconds` | histogram | source | Time from request start to staging write complete |
| `glovebox_receive_bytes_total` | counter | source | Content bytes received |
| `glovebox_receive_to_scan_seconds` | histogram | source | Latency from staging write to watcher pickup |
| `glovebox_staging_queue_depth` | gauge | | Unprocessed items in staging (atomic counter) |

In filesystem mode, `glovebox_items_received_total` is incremented when the
watcher detects a new item. `glovebox_receive_duration_seconds` is not
applicable (no receive phase). `glovebox_receive_to_scan_seconds` is always
near-zero (watcher picks up immediately).

In HTTP mode, `glovebox_items_received_total{status="accepted"}` counts 202
responses. `glovebox_items_received_total{status="rejected"}` counts 400/413
responses. `glovebox_items_received_total{status="throttled"}` counts 429
responses.

`glovebox_staging_queue_depth` replaces the previous `glovebox_staging_queue_depth`
from spec 04 with a unified implementation: atomic counter in HTTP mode, directory
count in filesystem mode, same metric name either way.

#### 3.12.3 Scanner Processing Metrics (existing, unchanged)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `glovebox_items_processed_total` | counter | source, verdict, destination | Items scanned and routed |
| `glovebox_processing_duration_seconds` | histogram | source | Scan time per item |
| `glovebox_signals_triggered_total` | counter | rule_name | Heuristic rule matches |
| `glovebox_quarantine_queue_depth` | gauge | | Items pending human review |
| `glovebox_scan_workers_busy` | gauge | | Workers currently scanning |
| `glovebox_scan_timeouts_total` | counter | source | Scans that exceeded timeout |
| `glovebox_audit_failures_total` | counter | | Audit log write failures |
| `glovebox_failed_items` | gauge | | Items in failed/ awaiting rescan |

#### 3.12.4 End-to-End Traceability

The `source` label is the join key across all three metric groups. For any
connector, the full item lifecycle is observable:

```
connector_polls_total{connector=imap, status=success}    "fetched"
  → glovebox_items_received_total{source=imap, status=accepted}   "received"
    → glovebox_items_processed_total{source=imap, verdict=pass, destination=messaging}  "delivered"
    → glovebox_items_processed_total{source=imap, verdict=quarantine}   "quarantined"
    → glovebox_items_processed_total{source=imap, verdict=reject}  "rejected"
```

For individual item tracing beyond counters, the `item_id` returned in the 202
response ties the ingest event to the audit log entry. The audit log records
source, sender, content hash, verdict, reason, destination, and scan duration
per item (spec 04, Section 10).

#### 3.12.5 Recommended Alerts

| Alert | Condition | Meaning |
|-------|-----------|---------|
| StagingBackpressure | `glovebox_staging_queue_depth > threshold * 0.8` | Queue nearing backpressure limit |
| ScanLag | `rate(glovebox_items_received_total{status="accepted"}) > rate(glovebox_items_processed_total)` sustained 5m | Items arriving faster than scanning |
| ReceiveToScanLatency | `histogram_quantile(0.95, glovebox_receive_to_scan_seconds) > 5` | Scan pipeline saturated |
| IngestRejections | `rate(glovebox_items_received_total{status="rejected"}) > 0` sustained 5m | Connector sending bad requests |
| ConnectorStale | `connector_checkpoint_age_seconds > 3600` | Connector not polling (crash, auth failure) |

### 3.13 Graceful Shutdown

On SIGTERM, the scanner:
1. Stops accepting new ingest requests (returns `503`)
2. Completes all in-flight ingest writes (bounded by request timeout)
3. Proceeds with existing shutdown sequence (spec 04, Section 14)

## 4. Connector Library Changes

### 4.1 StagingBackend Interface

The connector library introduces an interface that abstracts the staging delivery
mechanism:

```go
// StagingBackend delivers completed staging items to the glovebox scanner.
type StagingBackend interface {
    // Stage delivers a completed item (metadata + content) to the scanner.
    // Returns nil on success. The caller must not advance the checkpoint
    // until Stage returns nil.
    Stage(ctx context.Context, meta ItemMetadata, content io.Reader) error

    // SetConfigIdentity sets default identity fields for all items.
    SetConfigIdentity(ci *ConfigIdentity)
}
```

Two implementations:

- **`FileStagingBackend`** -- the existing `StagingWriter` behavior (write to
  local filesystem via atomic rename). Used for Phase 2 macOS deployment and
  local development.
- **`HTTPStagingBackend`** -- POSTs to the scanner's `/v1/ingest` endpoint.
  Used in Kubernetes deployments.

### 4.2 HTTPStagingBackend

```go
type HTTPStagingBackend struct {
    ingestURL     string        // e.g., "http://release-glovebox-ingest:9091/v1/ingest"
    connectorName string        // set from connector.Options.Name
    httpClient    *http.Client
    retryMax      int           // default: 3
    retryBase     time.Duration // default: 1s (exponential backoff with jitter)
}
```

**Retry policy:**
- Retries on `429` and `5xx` responses, and on network errors (connection
  refused, timeout, etc.)
- Exponential backoff with full jitter: base * 2^attempt * rand(0.5, 1.5)
- Honors `Retry-After` header on 429 responses (uses the longer of backoff
  or Retry-After)
- Does not retry on `400` or `413` (permanent errors returned as
  `connector.PermanentError`)
- Maximum 3 retries by default (configurable)

The jitter provides natural thundering-herd mitigation when the scanner restarts
and multiple connectors retry simultaneously.

**Connector identification:** Every request includes the
`X-Glovebox-Connector: <connectorName>` header.

**Content buffering:** For items where content is written incrementally via
multiple `WriteContent` calls, the backend buffers to a temp file in
`os.TempDir()` before sending. For single-call writes under 4MB (hardcoded
threshold), the content is held in memory and sent directly. The `staging-tmp/`
directory is not created or used in HTTP mode.

**Identity merging:** `SetConfigIdentity` stores the config-level identity.
On `Stage`, the backend merges config identity with per-item identity (same
merge rules as `FileStagingBackend`) before including it in the metadata JSON.

### 4.3 StagingItem Adaptation

The existing `StagingItem` API (`NewItem` -> `WriteContent` -> `Commit`) is
preserved. `Commit()` delegates to the configured `StagingBackend`:

- **FileStagingBackend**: same as today (validate, write metadata.json, rename)
- **HTTPStagingBackend**: validate metadata, POST multipart to ingest endpoint

Connector code does not change. The backend is selected at startup.

### 4.4 Backend Selection

The runner selects the backend based on environment variables:

```go
if url := os.Getenv("GLOVEBOX_INGEST_URL"); url != "" {
    // HTTP mode: POST to scanner
    backend = connector.NewHTTPStagingBackend(url, connectorName, httpClient)
} else if dir := os.Getenv("GLOVEBOX_STAGING_DIR"); dir != "" {
    // Filesystem mode: write to staging directory
    backend = connector.NewFileStagingBackend(dir, connectorName)
} else {
    slog.Error("either GLOVEBOX_INGEST_URL or GLOVEBOX_STAGING_DIR must be set")
    os.Exit(1)
}
```

## 5. Scanner Config Changes

New `ingest` block in `config.json`:

```json
{
  "ingest": {
    "enabled": true,
    "port": 9091,
    "max_body_bytes": 67108864,
    "max_metadata_bytes": 262144,
    "backpressure_threshold": 100,
    "request_timeout_seconds": 60
  }
}
```

All fields are optional with the defaults shown above.

When `enabled` is `false`, the ingest listener is not started. The scanner
operates in filesystem-only mode (existing behavior). Requests to port 9091
will connection-refuse.

## 6. Helm Chart Changes

### 6.1 Scanner Deployment

- Staging PVC is now scanner-internal (no longer shared), access mode RWO
- New container port exposed: 9091 (ingest)
- New config fields in ConfigMap for ingest settings

### 6.2 Ingest Service

New Service for the ingest endpoint:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: {{ include "glovebox.fullname" . }}-ingest
spec:
  type: ClusterIP
  ports:
    - port: 9091
      targetPort: ingest
      protocol: TCP
      name: ingest
  selector:
    {{- include "glovebox.selectorLabels" . | nindent 4 }}
```

The existing metrics Service remains unchanged on port 9090.

### 6.3 Connector Deployments

- **Remove** the staging volume mount
- **Add** `GLOVEBOX_INGEST_URL` environment variable:
  ```yaml
  - name: GLOVEBOX_INGEST_URL
    value: "http://{{ include "glovebox.fullname" $ }}-ingest:9091/v1/ingest"
  ```
- Connectors retain their own state PVC (checkpoint persistence)

### 6.4 NetworkPolicy

Scanner NetworkPolicy:
```yaml
spec:
  podSelector:
    matchLabels:
      {{- include "glovebox.selectorLabels" . | nindent 6 }}
  policyTypes:
    - Egress
    - Ingress
  ingress:
    # Ingest: connectors only
    - from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/component: connector
      ports:
        - port: 9091
          protocol: TCP
    # Metrics: unrestricted (Prometheus, monitoring)
    - ports:
        - port: 9090
          protocol: TCP
  egress: []
```

Because ingest (9091) and metrics (9090) are on different ports, the unrestricted
metrics rule does not grant access to the ingest endpoint.

### 6.5 Backward Compatibility

For users who prefer filesystem mode (e.g., non-Kubernetes, local development):

```yaml
connectors:
  rss:
    ingestMode: http       # default; set to "filesystem" for shared PVC mode
```

When `ingestMode: filesystem`, the connector gets the staging volume mount and
`GLOVEBOX_STAGING_DIR` instead of `GLOVEBOX_INGEST_URL`. The staging PVC is
created with the configured `accessMode` (should be RWX for filesystem mode).

## 7. Failure Modes

### 7.1 Scanner Pod Restart

Connectors receive connection refused or 503. They retry with exponential
backoff and jitter. Once the scanner is back and passes its readiness gate,
connectors resume. No data loss -- connectors don't advance checkpoints until
the scanner confirms receipt with 202.

The jitter in connector backoff provides thundering-herd mitigation when all
connectors retry simultaneously after a scanner restart.

### 7.2 Network Partition

Same as pod restart from the connector's perspective. Retries with backoff.
Content that was in-flight during the partition is retried because the checkpoint
was not advanced.

### 7.3 Scanner Disk Full

The ingest handler detects write errors and returns 503. Connectors retry.
The scanner's existing degraded-mode logic (quarantine everything on audit
failure) applies independently.

### 7.4 Content in Flight During Scanner Crash

If the scanner crashes between receiving a request and returning 202, the
connector does not receive a response and does not advance its checkpoint.
On next poll, the connector re-fetches and re-sends the item. The temp directory
left by the incomplete ingest is cleaned up on scanner restart (Section 3.8,
startup cleanup).

### 7.5 Duplicate Delivery

If the scanner crashes after writing to staging but before the 202 reaches the
connector, the connector will re-send the item on its next poll. This produces a
duplicate in the staging directory. Duplicates are handled the same way they are
today: the scanner processes both copies independently, and the scan/route logic
is idempotent (same content, same signals, same destination). The audit log
records both deliveries.

This is acceptable for the home agent use case. If exactly-once semantics are
needed in the future, the scanner can deduplicate by content hash at the ingest
endpoint. Content-hash deduplication also provides replay protection against
malicious request replay (see Section 3.10 known limitation).

## 8. Amendments to Parent Specs

### 8.1 Architecture Spec (03)

**Section 5.3 (Connector Interface):** Step 3 changes from "Writes each item to
the glovebox staging directory" to "Delivers each item to the glovebox scanner
via HTTP POST (Kubernetes) or filesystem write (macOS)." The paragraph
"Connectors have write access only to the staging directory" is amended to:
"In filesystem mode, connectors have write access only to the staging directory.
In HTTP mode, connectors have no filesystem access to the scanner; they deliver
items via the ingest API."

**Section 2.4 (Network Policies):** Glovebox entry changes from "No egress.
Filesystem only." to "No egress. Accepts inbound ingest traffic from connectors
on port 9091 and metrics scrapes on port 9090."

### 8.2 Glovebox Design Spec (04)

**Section 1.1 (Design Invariants):** Amend: "The glovebox has no network
*egress*. It accepts inbound connections for content ingest (port 9091) and
metrics (port 9090)."

**Section 5.1 (Directory Structure):** Add note: "In HTTP ingest mode, the
staging directory is written to by the scanner's ingest handler, not directly
by connectors. The directory structure, readiness gate, and atomic handoff
behavior are unchanged."

**Section 11 (Metrics):** Add `glovebox_items_received_total`,
`glovebox_receive_duration_seconds`, `glovebox_receive_bytes_total`, and
`glovebox_receive_to_scan_seconds` (see Section 3.12 of this spec). The
`glovebox_staging_queue_depth` gauge implementation changes to an atomic counter
in HTTP mode but retains the same metric name and semantics.

**Section 14 (Graceful Shutdown):** Add to step 1: "Stop accepting new items
from the watcher *and* the ingest endpoint (return 503). Complete all in-flight
ingest writes before proceeding."

### 8.3 Connector Framework Spec (05)

**Section 5 (Staging Writer):** The `StagingWriter` is now one implementation of
the `StagingBackend` interface (renamed `FileStagingBackend`). The public API
(`NewItem`, `WriteContent`, `Commit`) is unchanged. Backend selection is based
on environment variables (Section 4.4 of this spec).

**Section 8.1 (Entry Point):** `connector.Run` wires the backend based on
environment variables. `StagingDir` in `Options` is optional when
`GLOVEBOX_INGEST_URL` is set.

**Section 10.2 (Metrics):** Remove `connector_items_produced_total`. The scanner
is the authoritative counter for items received (via `glovebox_items_received_total`).
Retain `connector_items_dropped_total` -- this fires when no rule matches, before
any item reaches the scanner.

## 9. Testing Strategy

### 9.1 Scanner Ingest Tests

- Valid multipart POST returns 202 and item appears in staging directory
- Missing metadata part returns 400
- Missing content part returns 400
- Duplicate parts return 400
- Unexpected extra parts return 400
- Non-JSON metadata Content-Type returns 400
- Invalid metadata JSON returns 400
- Metadata validation failure (missing required fields) returns 400
- Oversized metadata (> 256 KB) returns 413
- Oversized content (> 64 MB) returns 413
- Backpressure: fill staging above threshold, verify 429 with Retry-After
- Concurrent ingest: multiple POSTs in parallel, all succeed, no partial items
- Graceful shutdown: 503 returned after SIGTERM
- Startup readiness: 503 returned before scanner initialization completes
- Atomic write: verify no partial items visible to watcher during ingest
- Request timeout: slow upload terminated, temp dir cleaned up
- Orphan cleanup: crash recovery removes .ingest-tmp/ on startup
- Identity and tags survive ingest round-trip (spec 06 fields in metadata)

### 9.2 HTTPStagingBackend Tests

- Successful POST through NewItem/WriteContent/Commit
- 429 response triggers retry with backoff, honors Retry-After
- 503 response triggers retry with backoff
- 400 response returns PermanentError, no retry
- 413 response returns PermanentError, no retry
- Network error triggers retry
- Max retries exceeded returns error
- Content buffering: multi-call WriteContent produces correct multipart body
- Single-call WriteContent under 4MB uses memory, not temp file
- ConfigIdentity merged into metadata before POST
- X-Glovebox-Connector header set correctly

### 9.3 Integration Tests

- End-to-end: connector -> HTTP ingest -> staging -> scan -> agent inbox
- Failover: scanner restart mid-ingest, connector retries, item delivered
- Duplicate handling: same item sent twice, both processed

### 9.4 Helm Chart Tests

- `helm template` with default values: connectors get GLOVEBOX_INGEST_URL
- `helm template` with ingestMode=filesystem: connectors get staging volume mount
- NetworkPolicy: ingest port restricted to connectors, metrics unrestricted

## 10. Implementation Order

0. Scanner config: add `ingest` block to Config struct, defaults, env var overrides
1. Scanner: `/v1/ingest` endpoint (multipart handler, atomic write, backpressure,
   metrics, readiness gate, startup cleanup)
2. Scanner: ingest Service in Helm chart, port 9091 on deployment
3. Connector library: `StagingBackend` interface, `FileStagingBackend` (rename
   existing `StagingWriter` internals), `HTTPStagingBackend`
4. Connector library: wire backend selection in `connector.Run`
5. Helm chart: update connector deployments (remove staging mount, add ingest URL,
   add ingestMode toggle)
6. Helm chart: update NetworkPolicy (port 9091 restricted, port 9090 unrestricted)
7. Integration tests
8. Update parent specs (03, 04, 05) with amendments from Section 8
