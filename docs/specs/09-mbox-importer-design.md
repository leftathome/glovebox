# Mbox Archive Importer -- Design Specification

**Version 1.0 -- April 2026**

*This document specifies the mbox archive importer: the first member of a new `importers/` family on the glovebox side of the archive-importer pattern established in archiver spec 01. It covers the connector framework library refactor that supports both importers and existing connectors, the mbox importer binary itself (streaming parse, idempotent survey generation, rule-based pre-filter, side-car manifest, checkpoint-based resume, Message-ID dedup, bounded-concurrency ingest), configuration, and the V1 deploy model (K8s Job reading from a PVC).*

---

## 1. Problem Statement

Glovebox's existing connectors pull from **live external sources** (Gmail API, IMAP, RSS, GitHub, etc.) on a polling schedule. They are long-running, trickle-ingested, and source-specific. The content creators are potentially low-trust and constantly producing new material -- the scrutiny pipeline is sized for steady flow.

A different class of source needs to land in glovebox: **finished archives**. A Google Takeout mbox containing 220,000 emails going back twenty years. A WhatsApp chat export. A Slack workspace dump. An old Thunderbird mail profile. These share properties the existing connectors do not:

- **One-shot execution.** The archive is finite. The importer runs to completion and exits.
- **Bursty throughput.** 220k messages arrive in one batch, not 50/day.
- **Static source.** The file doesn't change under you; you can checkpoint by byte offset.
- **Pre-ingest filtering is mandatory at scale.** A 12 GB mbox with 40% Spam/Trash/Promotions is half the ingest work it should be if you don't filter before the ingest round-trip.
- **Idempotency matters operationally.** A killed-and-restarted import must not duplicate; re-running the same mbox a year later (with new messages appended) must only ingest what's new.

The mbox importer is the first concrete handler in this class. It also establishes the structural pattern and shared library factoring that subsequent format handlers (Google Chat JSON, Keep notes, NotebookLM notebooks, Gemini chat history, My Activity JSON, WhatsApp chat exports, Slack exports, etc.) will follow.

This spec builds on archiver's spec 01 (archive importer pattern), which defines the cross-repo architecture, event schema, and taxonomy of layers. Readers should be familiar with that document's taxonomy and media-type conventions.

## 2. The `importers/` Directory

Glovebox grows a new top-level directory, `importers/`, sibling to the existing `connectors/`. The two families serve different trigger patterns but share substantial framework code.

```
glovebox/
  connector/        public library -- framework primitives, shared by both
  connectors/       live-source polling connectors (existing)
    gmail/
    imap/
    rss/
    ...
  importers/        finished-archive one-shot handlers (new)
    mbox/           V1 deliverable
    (future)        google-chat, keep, gemini, notebooklm, my-activity, ...
```

### 2.1 Why a Separate Directory

The two families have meaningfully different shapes that reward being visibly distinct in the repo:

| Aspect | `connectors/` | `importers/` |
|---|---|---|
| Source | Live remote service (API, IMAP, ...) | Finished local archive file |
| Lifetime | Long-running (polling loop) | One-shot (run to completion, exit) |
| Scale pattern | Steady trickle | Burst (whole archive at once) |
| Checkpoint | Source-specific cursor (message UID, ETag, pagination token) | Byte offset in the archive file |
| Filter timing | Post-ingest (glovebox rules route content) | Pre-ingest (filter before round-trip, for scale) |
| Trigger | Scheduled poll or push notification | CLI invocation or archive event |
| Trust posture | Content scanned for prompt injection | Content scanned for prompt injection (identical) |
| Backpressure profile | Predictable | Can overwhelm the scanner pipeline |

The shared library handles what's common (staging backend, checkpoint persistence, metrics, rules engine, identity, config loading). The directory split makes the runtime shape difference visible to readers navigating the repo.

### 2.2 Connector Framework Library Refactor

The current `connector.Run(opts)` combines framework bootstrap (config load, staging backend selection per spec 08's env-driven backend selection, metrics init, rule matcher, fetch counter, health server, signal handling) and the connector runtime loop (poll/watch/listen dispatch) in a single entry point.

To support both families sharing primitives, split the package into:

- **Framework bootstrap** (shared by both families):
  - `connector.NewFramework(opts)` returns a `*Framework` with initialized backend, staging/HTTP, rule matcher, metrics, fetch counter, checkpoint, health/metrics HTTP server. Caller calls `fw.Shutdown()` when done.
  - All of `backend.go`, `http_backend.go`, `staging.go`, `checkpoint.go`, `rule.go`, `metrics.go`, `identity.go`, `ratelimit.go`, `fetchlimit.go`, `token.go`, `webhook.go`, `httpclient.go`, `robots.go` stay in place as library code, referenced by `Framework`.

- **Connector runtime** (polling/watcher/listener loop):
  - `connector.RunPollLoop(fw, c Connector)` -- the current `runPollLoop` behavior.
  - `connector.RunWatchLoop(fw, w Watcher)` -- the current `runWatchLoop` behavior.
  - Listener dispatch unchanged.
  - Each existing `connectors/*/main.go` calls `NewFramework(...)` + one of the Run* functions.

- **Importer runtime** (one-shot against a source):
  - New package `importer/` (or `connector/importer/` -- see open questions) provides `importer.RunOneShot(fw, i Importer)`.
  - `Importer` interface: `Survey(ctx, src) (*Survey, error)` and `Import(ctx, src, survey, filter) error`.
  - Handles: survey-if-missing bootstrap, filter loading, progress reporting, manifest writing, checkpoint-based resume.

Existing connectors continue to work unchanged because `connector.Run(opts)` stays as a thin wrapper around the new primitives for backwards compatibility. No `connectors/*/main.go` files need to change during the refactor; migrating them to call `NewFramework` explicitly is optional follow-up work that each connector can do when next touched.

### 2.3 Refactor Scope

The refactor is part of V1 because it is forced by the need to share the bootstrap logic between `connectors/*` and `importers/*`. It is not a speculative cleanup.

In scope for the refactor:
- Extract `connector.NewFramework`, `connector.Framework` struct, `Framework.Shutdown` from the current monolithic `Run`.
- Extract `connector.RunPollLoop`, `connector.RunWatchLoop`, listener dispatch into standalone functions that take a `*Framework`.
- Keep `connector.Run(opts)` as a backwards-compat shim that wires these together for existing callers.
- Define the `Importer` interface and `importer.RunOneShot`.

Out of scope for this spec (follow-up):
- Migrating existing `connectors/*/main.go` files to call `NewFramework` directly.
- Splitting `connector/` into subpackages (`connector/framework/`, `connector/runtime/`).

## 3. The Mbox Importer Binary

### 3.1 Runtime Shape

`importers/mbox/` produces a single Go binary. V1 invocation is via CLI, with arguments specifying the archive file, the filter config, and the ingest target:

```bash
mbox-importer \
    --source /data/takeout_gmail_2026-04-11.mbox \
    --filter /config/filter.json \
    --ingest-url http://glovebox:9091/v1/ingest \
    --source-name takeout-gmail-2026-04-11
```

The binary:

1. Checks for `<source>.survey.v1.json`. If absent or stale (source size/mtime mismatch), runs survey mode (streaming parse with no ingest, aggregate report written out). If present and fresh, uses it.
2. Loads the filter config.
3. Loads any existing `<source>.import-manifest.v1.json` and `<source>.checkpoint`. Resume behavior is governed by a single rule (see §3.1.1 below).
4. Streams the mbox, applies the filter rules, applies standard glovebox destination-agent rules (§3.5), pushes matching messages to the ingest URL with bounded concurrency, maintains Message-ID dedup, checkpoints periodically.
5. On clean completion: writes a final manifest with status `complete`, removes the checkpoint file.
6. On signal or unrecoverable error: writes the current manifest with status `interrupted` or `failed` respectively, keeps the checkpoint, exits non-zero for re-run.

A later "archive-event listener" mode (V2, see §6) can be added without rewriting the binary -- it would receive `archive/google-takeout/mail` events (once archiver's V2 recognizers emit them), extract `output_path` from the event, and invoke the same code path.

### 3.1.1 Resume Rule

A single deterministic rule governs startup:

- Manifest `status == "complete"` → exit 0 immediately; archive is already imported.
- Manifest `status == "interrupted"` and checkpoint file exists → resume from the checkpoint's byte offset; retain the manifest's `message_ids_ingested` set for dedup.
- Manifest `status == "failed"` → do not auto-resume; require explicit `--resume` flag to retry, because the previous run hit a terminal error worth investigating.
- Manifest absent, or `status == "in_progress"` (indicates previous run died without writing a clean status) → start fresh; a stale checkpoint without a matching manifest status is ignored and overwritten.
- `--resume=false` overrides any of the above and forces a fresh start (deletes existing manifest and checkpoint).
- `--resume=true` forces resume semantics even for `failed` status.

The `--resume` flag's default is "do whatever the rule above says"; explicit values override.

### 3.2 Streaming Parser

The mbox parser uses `bufio.Scanner` with a custom split function. The split pattern is `^From ` at the start of a line (mbox's message delimiter, per RFC 4155). Scanner buffer size is configurable (default 64 MB to handle pathological messages with large attachments inline).

Working set is one message at a time -- a few MB typically, tens of MB for large-attachment outliers. The 12 GB archive fits in a pod with 256 MB of memory.

Per-message processing:
- Raw bytes captured (for later ingest payload).
- Headers parsed with `net/mail.ReadMessage` on the header block.
- Filter fields extracted: `Message-ID`, `Date`, `From`, `List-Id`, `List-Post`, `X-Gmail-Labels`, `Content-Length` / byte size.
- Body retained but not parsed further; glovebox ingest receives the raw RFC 5322 message bytes.

If a message is malformed (cannot parse headers, no `Message-ID`), it goes to the error bucket in the manifest and is not ingested. The import continues.

### 3.3 Idempotent Survey Generation

On any invocation that operates on the mbox, the importer checks for `<source>.survey.v1.json` next to the source file. If absent, it generates it first; all subsequent operations (filter, ingest) use the survey as context.

The survey is produced by a streaming pass that reads every message's headers, aggregates without ingesting, and writes a versioned JSON report.

### 3.3.1 Survey Schema

`<source>.survey.v1.json`:

```json
{
  "schema_version": "1.0",
  "source_path": "/data/takeout_gmail_2026-04-11.mbox",
  "source_size": 12583279104,
  "source_mtime": "2026-04-11T12:34:56Z",
  "survey_started_at": "2026-04-12T09:00:00Z",
  "survey_completed_at": "2026-04-12T09:12:47Z",
  "total_messages": 247312,
  "total_bytes": 12583279104,
  "malformed_messages": 3,

  "labels": {
    "INBOX": 45230,
    "Sent": 18204,
    "Spam": 89102,
    "Trash": 12431,
    "Category/Promotions": 62101,
    "Category/Social": 14205,
    "Category/Updates": 29107,
    "IMPORTANT": 3217,
    "STARRED": 412,
    "...": "..."
  },

  "list_ids": [
    {"list_id": "lwn-announce@lwn.net", "count": 12453},
    {"list_id": "dev@oldjob.example.com", "count": 8291},
    {"list_id": "tigers-discuss@lists.detroit.org", "count": 4107}
  ],

  "senders": [
    {"address": "noreply@amazon.com", "count": 5102},
    {"address": "boss@oldjob.example.com", "count": 1283},
    {"address": "...", "count": 0}
  ],

  "date_histogram": {
    "pre-2005": 0,
    "2005-2010": 5102,
    "2010-2015": 42103,
    "2015-2020": 120504,
    "2020-2025": 79603
  },

  "size_histogram": {
    "lt_10kb": 212000,
    "10kb_to_1mb": 33000,
    "1mb_to_10mb": 2100,
    "gt_10mb": 203
  }
}
```

The survey is the single source of truth for filter authoring. A user writes the filter config by reading the survey and deciding what to include. The `senders` list is bounded (top N without List-Id, configurable, default 100) to keep the survey file tractable.

### 3.3.2 Survey as Prerequisite

Every operation on the mbox implicitly requires the survey. If absent, it's generated first. This is idempotent:

- `mbox-importer --source file.mbox` with no survey: generates survey, then runs import.
- `mbox-importer --source file.mbox` with stale survey (mbox mtime changed): regenerates survey.
- `mbox-importer --source file.mbox --survey-only`: generates survey, exits.
- `mbox-importer --source file.mbox` with up-to-date survey: uses it, runs import.

Staleness check: compare `source_size` and `source_mtime` in the survey against the file on disk. Mismatch -> regenerate. (This catches Takeouts re-downloaded with new data.)

### 3.4 Rule-Based Pre-Filter

The filter is a list of rules evaluated first-match-wins. Each rule has an `action` of `include` or `exclude`. The filter is conceptually similar in shape to glovebox's existing routing rules -- but the mechanics differ (match is an object of typed fields rather than a pattern string, and the outcome is pre-ingest include/exclude rather than post-ingest destination routing), so the sidecar key is named `filter_rules` to avoid confusion with BaseConfig's `rules` used for destination-agent routing (see §3.5).

### 3.4.1 Filter Schema

`<source>.filter.json` (authored by the user; optional, absent defaults to "include everything"):

```json
{
  "schema_version": "1.0",
  "filter_rules": [
    {"match": {"list_id": "dev@oldjob.example.com"}, "action": "include"},
    {"match": {"list_id": "*@lwn.net"}, "action": "exclude"},
    {"match": {"label": "Spam"}, "action": "exclude"},
    {"match": {"label": "Trash"}, "action": "exclude"},
    {"match": {"label": "Category/Promotions"}, "action": "exclude"},
    {"match": {"label": "Category/Social"}, "action": "exclude"},
    {"match": {"sender_domain": "newsletter.*"}, "action": "exclude"},
    {"match": {"max_size_bytes": 20971520}, "action": "exclude"},
    {"match": {"label": "INBOX"}, "action": "include"},
    {"match": {"label": "IMPORTANT"}, "action": "include"},
    {"match": {"label": "STARRED"}, "action": "include"}
  ]
}
```

### 3.4.2 Match Fields

Supported match fields in V1:

| Field | Semantics |
|---|---|
| `label` | Exact match against any `X-Gmail-Labels` header value on the message. |
| `list_id` | Glob match against `List-Id` or `List-Post` header. `*@lwn.net` matches any LWN list. |
| `sender` | Exact match against `From` address (just the addr-spec, not the display name). |
| `sender_domain` | Glob match against the domain portion of the `From` address. |
| `subject_contains` | Substring match against `Subject`. |
| `date_after` | RFC 3339 date string; message `Date` header must be after. |
| `date_before` | RFC 3339 date string; message `Date` header must be before. |
| `min_size_bytes` | Numeric; message size >= threshold. |
| `max_size_bytes` | Numeric; message size <= threshold. |

### 3.4.3 Action Semantics

- `include`: message passes the filter; continues to destination-agent routing and ingest.
- `exclude`: message rejected pre-ingest; counted in the manifest under the matched rule.

If no rule matches, the default action is `exclude`. Users who want opt-out semantics ("include everything except these labels") add a trailing `{"action": "include"}` rule as a wildcard terminator. The example in §3.4.1 is opt-in (explicit includes at the end).

### 3.4.4 Filter Evaluation

For each message:
1. Extract match fields from headers.
2. Walk `filter_rules` in order.
3. First matching rule's action is final.
4. If no rule matches, apply default `exclude`.
5. Increment counter for the matching rule (or "default-excluded") in the manifest.

Filter evaluation is pure and cheap -- it runs on parsed headers, no body inspection. Filtered-out messages skip the ingest call entirely, which is the whole point.

### 3.5 Destination-Agent Rules

Every item posted to glovebox ingest must carry a `destination_agent` field in its metadata; the ingest handler validates this against the scanner's configured `agent_allowlist` (see `internal/config/config.go` field `AgentAllowlist`). This is unchanged from existing connectors -- and the importer uses the same mechanism they do.

The importer's main config (Helm ConfigMap-mounted) includes a standard `rules` array with the existing `Rule` shape (`match: <pattern-string>`, `destination: <agent-name>`), identical to `connectors/imap/config.json` and others. After a message passes the pre-filter (§3.4) and is about to be ingested, the importer runs these rules through the standard glovebox `RuleMatcher` to determine its `destination_agent`, which is set on the ingest metadata.

The two rule layers in the importer:

| Layer | Config location | Shape | Timing | Purpose |
|---|---|---|---|---|
| **Filter rules** | `<source>.filter.json` (per-archive sidecar) | `{match: {...typed fields...}, action: include/exclude}` | Pre-ingest | Decide whether to ingest at all; skip the round-trip for clearly unwanted content |
| **Destination rules** | Importer main config (Helm ConfigMap, BaseConfig `rules`) | `{match: <pattern>, destination: <agent>}` | Pre-ingest, after filter passes | Decide which domain agent receives the content |

Separating them keeps per-archive decisions (filter) distinct from cluster-level routing policy (destination rules), while reusing the existing RuleMatcher infrastructure.

### 3.6 Import Session Manifest

`<source>.import-manifest.v1.json` records the state of an import run. It is read at startup (for resume semantics), updated periodically during the run, and finalized on clean completion.

The manifest extends the base `<archive>.<kind>-manifest.v1.json` shape defined in archiver spec 01 §4.3 (`kind = "import"` for this manifest). Common base fields (`schema_version`, `source_path`, `source_size`, `source_mtime`, `timestamp_start`, `timestamp_end`, `counts`, `resume_state`) are inherited; mbox-specific fields (`message_ids_ingested`, `filter_hit_counts`, `destination_rule_hit_counts`) are added on top.

```json
{
  "schema_version": "1.0",
  "kind": "import",
  "source_path": "/data/takeout_gmail_2026-04-11.mbox",
  "source_size": 12583279104,
  "source_mtime": "2026-04-11T12:34:56Z",
  "source_name": "takeout-gmail-2026-04-11",
  "status": "in_progress",
  "timestamp_start": "2026-04-12T10:00:00Z",
  "timestamp_end": null,
  "survey_ref": "takeout_gmail_2026-04-11.mbox.survey.v1.json",
  "filter_ref": "takeout_gmail_2026-04-11.mbox.filter.json",
  "filter_rules_applied": [
    {"match": {"list_id": "dev@oldjob.example.com"}, "action": "include"},
    {"match": {"label": "Spam"}, "action": "exclude"},
    "..."
  ],
  "counts": {
    "messages_seen": 124800,
    "messages_ingested": 58312,
    "messages_filtered": 64203,
    "messages_errored": 14,
    "messages_dedup_skipped": 2271,
    "bytes_processed": 6291456000
  },
  "filter_hit_counts": {
    "rule_0_dev@oldjob.example.com": 6410,
    "rule_2_label_Spam": 48231,
    "rule_3_label_Trash": 7102,
    "rule_4_label_Category/Promotions": 7890,
    "rule_5_label_Category/Social": 520,
    "rule_6_sender_domain_newsletter.*": 0,
    "rule_7_max_size_bytes": 50,
    "default_excluded": 0
  },
  "destination_rule_hit_counts": {
    "rule_0_folder_INBOX_-_messaging": 45231,
    "rule_1_wildcard_-_general": 13081
  },
  "message_ids_ingested": [
    "<abc@example.com>",
    "<def@example.com>",
    "..."
  ],
  "errors": [
    {"byte_offset": 123456789, "message_id": null, "reason": "malformed headers: missing colon on line 3"},
    {"byte_offset": 234567890, "message_id": "<abc@example.com>", "reason": "ingest call failed: 500 Internal Server Error"}
  ],
  "truncated_error_count": 0,
  "resume_state": {
    "byte_offset": 6291456000,
    "last_ingested_message_id": "<xyz@example.com>"
  }
}
```

### 3.6.1 Status Enum

The `status` field is one of:

| Value | Meaning |
|---|---|
| `in_progress` | An import is currently running, or a previous run died without writing a clean terminal status. Treated as "suspect state" -- see §3.1.1 for resume behavior. |
| `complete` | The import ran to completion; all non-filtered non-dedup-skipped messages were either ingested or recorded as errors. The checkpoint file has been removed. |
| `interrupted` | The import was cleanly interrupted by a signal (SIGTERM/SIGINT). The checkpoint is valid. Resumable. |
| `failed` | The import hit an unrecoverable error (e.g., source file truncated, ingest URL unreachable for retry limit). The checkpoint is valid but requires operator investigation before resuming. |

Status transitions: `in_progress` → (`complete` | `interrupted` | `failed`). Terminal states do not transition further without a new run.

### 3.6.2 Errors Array Cap

The `errors` array is capped at 1000 entries by default; additional errors increment `truncated_error_count` instead of being appended. The cap prevents a pathological archive (e.g., every message malformed) from ballooning the manifest.

### 3.7 Message-ID Dedup

Every successfully-ingested `Message-ID` is added to an in-memory set and persisted in the manifest's `message_ids_ingested` array (§3.6). V1 keeps this list inline in the manifest JSON. On each message, if its `Message-ID` is already in the set, skip ingest and count it under `messages_dedup_skipped`.

For a 500k-message archive, the inline list adds tens of MB to the manifest. This is acceptable for V1 -- manifests are written atomically and read once at startup, so file size dominates neither RAM nor latency. If readability or atomicity of large manifests becomes a problem in practice, migrating to a sidecar (`<source>.dedup.v1.ndjson` appended during the run, referenced by path from the manifest) is a mechanical follow-up that does not change the importer's externally-visible behavior.

Dedup is scoped to a single manifest (i.e., a single source file). Running the importer against two different mboxes independently does not cross-dedup -- each has its own manifest, each tracks its own `Message-IDs`. Cross-archive dedup is glovebox's job, if it wants to do it, post-ingest via content hash.

Rationale for Message-ID scope:
- Kill-restart safety: restarting a crashed import doesn't re-ingest what already made it through.
- Year-over-year safety: next year's Takeout mbox overlaps with this year's; running the importer against the newer file skips what was already ingested.

### 3.8 Byte-Offset Checkpointing

`<source>.checkpoint` is a small file (a few hundred bytes) containing the current byte offset in the mbox and the last-processed `Message-ID`. Written every N messages (default N=1000) and on clean shutdown.

On startup, if the checkpoint exists and the manifest says `interrupted`, resume from `byte_offset`. Message-ID dedup from the manifest's in-flight set protects against any messages partially processed across the checkpoint boundary.

Checkpoint is deleted on clean completion.

### 3.9 Bounded Concurrency Ingest

Ingest calls run in a worker pool with configurable size (default 8). Pattern:

```
main parser goroutine ----> channel of messages ----> N worker goroutines ----> ingest POST
```

Workers share a `*http.Client` (connection-pooled). On ingest error:
- 4xx (permanent): record in `errors`, do not retry, continue.
- 5xx (transient): retry with exponential backoff (3 attempts: 1s, 4s, 16s); if all fail, record in `errors` and continue.
- Context cancelled: clean shutdown; worker exits after current in-flight completes.

Concurrency cap is sized to avoid overwhelming glovebox's scanner pipeline; 8 is a conservative starting point and should be measured.

### 3.10 Ingest Envelope

Each message is POSTed to glovebox's `/v1/ingest` endpoint as a multipart request (per spec 08). The metadata is the existing `staging.ItemMetadata` shape (see `connector/staging.go` and `internal/staging/`). The importer populates the standard fields the same way a connector does, plus mbox-specific provenance:

- `source`: the `--source-name` value (e.g. `takeout-gmail-2026-04-11`). This is informational metadata, not access control; the importer sets it to record which archive a message came from.
- `destination_agent`: set from the destination-agent rules in §3.5. **Must be in the scanner's configured `agent_allowlist`** (`internal/config/config.go` `AgentAllowlist`), or the ingest handler will reject the item at validation. Operator responsibility: ensure the Helm ConfigMap's destination-routing rules only produce agents that are in the cluster's scanner allowlist.
- `sender`, `subject`, `timestamp`: populated from the message's `From`, `Subject`, `Date` headers.
- `content_type`: `message/rfc822`.
- `identity`: optional `ItemIdentity` if the importer config specifies `identity` (per spec 06's connector auth and provenance design).
- `tags`: merged from rule tags (standard RuleMatcher behavior) and optionally importer-configured fixed tags.

Content part: raw RFC 5322 message bytes from the mbox.

Provenance auditing: the importer adds a `tags` entry of the form `origin_archive:<source-name>:<byte-offset>` so that the import origin is traceable from any ingested item back to the mbox position. No schema change to `ItemMetadata` is required; tags are a free-form list already supported.

## 4. Configuration

### 4.1 Runtime Config

CLI flags:

| Flag | Required | Description |
|---|---|---|
| `--source` | yes | Path to the mbox file. |
| `--filter` | no | Path to filter JSON. Default: no filter (`include` all). |
| `--ingest-url` | yes | glovebox ingest URL. |
| `--source-name` | yes | Value for ingest metadata's `source` field. Must be in ingest's source allowlist. |
| `--concurrency` | no | Parallel ingest workers. Default 8. |
| `--survey-only` | no | Generate/update survey only; skip ingest. |
| `--regenerate-survey` | no | Force survey regeneration even if one exists. |
| `--resume` | no | Override automatic resume decision. Default is determined by the manifest/checkpoint state per §3.1.1. Set `--resume=true` to force resume (including for `failed` status); set `--resume=false` to force fresh start (deletes existing manifest and checkpoint). |

### 4.2 Sidecar Files

All artifacts co-locate with the mbox:

```
takeout_gmail_2026-04-11.mbox                        raw, read-only after initial stage
takeout_gmail_2026-04-11.mbox.survey.v1.json         generated on first run
takeout_gmail_2026-04-11.mbox.filter.json            user-authored, optional
takeout_gmail_2026-04-11.mbox.import-manifest.v1.json  running/final import record
takeout_gmail_2026-04-11.mbox.checkpoint             resume state; deleted on completion
```

## 5. V1 Deploy Model: In-Cluster K8s Job

V1 ships with a Helm chart that deploys the importer as a K8s Job reading from a PVC-backed volume. This decouples V1 from NAS availability (PVC backing is a cluster concern) and from external-ingest auth (in-cluster clients reach ingest via cluster DNS without auth, same posture as existing connectors).

### 5.1 Workflow

```bash
# 1. Create a PVC to hold the archive (size per the mbox).
helm install gb-import-takeout-gmail-pvc \
    charts/mbox-importer-pvc --set size=20Gi

# 2. Stage the file into the PVC via a throwaway utility pod.
kubectl apply -f charts/mbox-importer/stage-pod.yaml
kubectl cp ./Takeout/All\ mail\ Including\ Spam\ and\ Trash.mbox \
    glovebox/gb-import-stage:/data/takeout_gmail_2026-04-11.mbox
kubectl delete pod -n glovebox gb-import-stage

# 3. Run the import.
helm install gb-import-takeout-gmail-run \
    charts/mbox-importer --values my-import-values.yaml
```

The three-step workflow is explicit rather than hidden behind automation, because it's a one-time ceremony per archive and being able to see each step is worth more than abbreviating two commands.

### 5.2 Chart Shape

`charts/mbox-importer/` bundles:

- A `Job` that runs the importer binary against the configured PVC path.
- A `ConfigMap` holding the filter JSON.
- A `ServiceAccount` with minimal permissions.
- Values for: source PVC name, source file path, source name, ingest URL (defaults to `http://glovebox:9091/v1/ingest`), concurrency, filter (embedded in ConfigMap).

`charts/mbox-importer-pvc/` is a separate tiny chart that just creates a PVC with a configurable size and storage class. Separated from the main chart because PVCs often outlive Jobs (you want to keep the archive around after import; deleting the Job release shouldn't delete the volume).

`charts/mbox-importer/stage-pod.yaml` is a standalone manifest (not part of the chart) for the staging step, because it's short-lived and explicitly user-driven. Minimum shape: a Pod running `busybox:stable sleep infinity` (or equivalent) with:

- Volume mount: the PVC created in step 1, mounted at `/data`.
- Pod name matching the `kubectl cp` target in the workflow (`gb-import-stage`).
- Namespace matching the Helm release namespace (`glovebox`).
- No service account beyond default; no network required.

The manifest is intentionally tiny (10-15 lines) and lives next to the chart as documentation rather than as a templated chart resource, because using it correctly requires the operator to type `kubectl cp` interactively.

### 5.3 Backing Storage

The PVC's backing is whatever storage class the cluster provides:

- **Longhorn / local-path / similar** -- V1 default with NAS offline. Archive lives on local cluster storage.
- **NFS** -- once NAS is back. Same chart, same importer, just a different storage class.
- **emptyDir** -- possible for small test imports; data gone when the pod terminates.

The importer binary does not know or care about the backing.

## 6. Deferred Scope

Not in V1; named so they aren't forgotten:

- **Archive-event listener mode.** A long-running variant of the importer that subscribes to archiver notification events and processes archives as they're identified. Unblocked by archiver spec 01's V2 recognizer work (see archiver spec section 6).
- **Other format importers.** `importers/google-chat/`, `importers/keep/`, `importers/gemini/`, `importers/notebooklm/`, `importers/my-activity/`, `importers/whatsapp-chat/`, `importers/slack-channel/`. Each is its own rainy afternoon; each follows the patterns established here.
- **External invocation.** Running the importer on the user's Windows PC against a local mbox, pushing to glovebox over the public network. Blocked by external-ingest auth (spec 10).
- **Migrating existing connectors** to call `NewFramework` directly (instead of the `Run(opts)` shim). Mechanical follow-up work per connector.
- **Subpackage split of `connector/`** into `connector/framework/`, `connector/runtime/`, etc. Not required by V1; possible cleanup later.

## 7. Out of Scope

Things this spec is explicitly not handling:

- **Content scanning.** Glovebox's scrutiny pipeline processes ingested content identically whether it came from a live connector or an importer. This spec does not modify scanning.
- **Glovebox rules engine.** Post-ingest routing of content to domain agents is unchanged. The importer's pre-filter and the rules engine's post-ingest routing are complementary and operate at different stages.
- **Scanner-side backpressure against burst loads.** Burst-load ergonomics (scanner pipeline sizing, queue depth tuning, rate-limited ingest responses from glovebox) are a scanner-side concern. The importer honors existing backpressure signals (429 Retry-After from `/v1/ingest`) by sleeping and retrying.
- **Archive-event schema itself.** Defined in archiver spec 01 section 4.

## 8. Trust Posture

Mbox content is from low-trust authors (every email sender) and receives glovebox's full scrutiny. The fact that the mbox is a finished container rather than a live stream does not reduce per-item scrutiny. What it does change is **operational posture**:

- Importers can produce burst loads on the scanner pipeline that connectors cannot.
- Pre-filter at the importer saves scrutiny work on content that's obviously unwanted (Spam label, user-excluded lists). This is an efficiency choice, not a trust decision -- a message that passes the pre-filter still receives full scrutiny downstream.

Instrument thoroughly in V1 (throughput, queue depth, filter hit counts) so we learn real numbers rather than pre-solving operational concerns that may not manifest.

## 9. Open Questions

None blocking V1. Revisit before broader rollout:

- **Filter language expressiveness.** V1 supports a flat list of first-match-wins rules with a fixed set of match fields. If real use reveals a need for boolean combinators (AND of label + date) or regex matching, extend. Don't pre-extend.
- **Parallel runs against one mbox.** V1 assumes one importer at a time per mbox. If a user wants to shard an import across multiple pods, the checkpoint and manifest shapes don't support it. Address only if someone wants it.
- **Package layout for the importer runtime.** Whether `importer.RunOneShot` lives in its own top-level `importer/` package or under `connector/importer/`. Leaning top-level for symmetry with `connectors/` vs `importers/` directory layout.

## 10. Success Criteria

V1 is done when:

- A Go binary at `importers/mbox/` builds, tests pass, `go vet` is clean.
- The binary, run against Steve's actual 12 GB Takeout mbox, completes an end-to-end import: survey generates, a sensible filter authored against the survey is applied, matching messages ingest into glovebox, a final manifest records what happened.
- A Helm chart in `charts/mbox-importer/` deploys the binary as a K8s Job reading from a PVC, pushing to in-cluster glovebox ingest.
- The connector framework refactor is in place; no existing connector breaks.
- Archiver's `notification-event.v1.1` schema is merged (spec 01 deliverable).
- This spec, spec 10, and archiver spec 01 are committed and reviewed.
