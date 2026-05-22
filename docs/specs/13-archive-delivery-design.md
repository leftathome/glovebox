# Archive Delivery API -- Design Specification

**Version 1.0 -- May 2026**

*This document specifies the archive delivery endpoint for Glovebox: a resumable HTTP upload surface sized for multi-GB archive artifacts (mbox files, recognized Takeout subtrees, future PST exports) delivered by the recognizer service or other archive-scale producers. The endpoint speaks the tus.io v1.0.0 resumable upload protocol over the existing external ingest listener, validates content integrity at finalize, performs server-side untarring for tarball media types into the existing staging PVC, and is gated by per-source bearer tokens per spec 10. Per-message / per-file work is handed off to media-type-specific importers (spec 09 mbox-importer; future Takeout subtree importer) via the existing `fsnotify`-on-staging pattern.*

---

## 1. Purpose

The recognizer service classifies incoming archive bundles (Google Takeout zips, raw mboxes, future Facebook exports, etc.) into per-media-type subtrees and needs to deliver each subtree to Glovebox for the actual per-message / per-file scanning work. Today the only ingestion surface (`/v1/ingest` per spec 08) caps content at 64 MB per request, sized for connector-scale items (one email, one RSS post). Recognizer's deliveries are archive-scale: a single Gmail Takeout mbox is currently 12 GB on the user's machine, and one classified Photos subtree from an omnibus Takeout zip can be many GB.

Decisions already settled before this spec (documented in [glovebox-p1zx](../../.beads/issues.jsonl)):

- **Per-leaf-file POST rejected.** Thousands of round-trips for one Photos year. Unworkable.
- **Recognizer-parses-and-pushes rejected.** Duplicates the work spec 09's mbox-importer (and its successors) already do.
- **Glovebox grows a new ingestion path sized for whole archive artifacts.** Recognizer pushes the archive; Glovebox stages it; importers pick it up.

The shared-PVC handoff alternative is unavailable: the cluster's RWX NAS is offline. HTTP push is the only viable transport.

## 2. Scope

### 2.1 In Scope

- A new HTTP endpoint surface `/v1/archives` co-located with the existing `/v1/ingest` listener.
- The tus.io v1.0.0 resumable upload protocol with `creation`, `termination`, and `checksum` extensions.
- A `Upload-Metadata` schema carrying recognizer-supplied provenance (archive id, source filename, media type, matcher id, provider, claimed sha256, claimed size, original subtree path).
- Pre-flight idempotency on `archive_id` + `sha256`.
- Finalize-time sha256 and size verification.
- Server-side untarring for tarball media types (`archive/google-takeout-subtree` v1, future siblings) with tar-safety rejection rules.
- Atomic staging into the existing staging PVC under `archives/<archive_id>/`.
- Per-source quota tracking and a soft-warn / hard-503 quota enforcement.
- Observability: metrics, traces, structured logs as specified in §7.
- Reference to spec 10 (promoted concurrently with this spec) for bearer-token auth.

### 2.2 Out of Scope

- Per-leaf-file ingestion (rejected; see §1).
- Server-side parsing of mbox or any other media-type-internal structure (deferred to per-importer specs: spec 09 mbox-importer; future spec for Takeout subtree).
- Adding authentication to the existing `/v1/ingest` endpoint. Spec 10's promotion applies to `/v1/archives` only in v1; future spec extends it.
- A separate "deliveries" PVC distinct from the existing staging PVC. Single staging PVC, namespaced subdirectory.
- Multi-tenant tokens or per-tenant quotas. Single-operator homelab assumption.
- Resumable rotation (token_previous overlap window). Deferred to a follow-up if rotation frequency requires it.
- A streaming-untar-during-PATCH implementation. v1 untars at finalize after the full file is on disk; the few seconds of extra I/O are acceptable.

## 3. Architecture Overview

```
   Recognizer                                 Glovebox (this spec)
   --------------                             ----------------------
   POST /v1/archives          ---------->     Auth middleware (spec 10)
   (Upload-Metadata)                          |
                                              v
                                              Pre-flight idempotency check
                                              |
   201 + Location              <----------    |- absent -> create upload state
                                                |
   PATCH .../<upload-id>       ---------->     Append + rolling sha256
   (offset + chunk)                            |
   204 No Content              <----------    Update Upload-Offset
   ...                                         |
   POST .../<upload-id>?finalize -------->     |- sha256 verify
                                              |- size verify
                                              |- media-type switch
                                              |   |- raw: rename to archives/<id>/raw/<file>
                                              |   |- tar: stream-untar to archives/<id>/tree/
                                              |- write metadata.json
                                              |- atomic rename .tmp -> archives/<id>
                                              v
   201 + receipt JSON         <----------     Notify (fsnotify trigger to importers)
                                              |
                                              (importer reads archives/<id> per spec 09)
```

### 3.1 Endpoint Surface

A single HTTP listener (the existing external listener on port 8081 per spec 08 §2.2) hosts two distinct handler trees:

- `/v1/ingest` -- unchanged. Multipart with 64 MB cap. No auth in v1 (per spec 10 §7).
- `/v1/archives*` -- new. tus.io protocol. Bearer-token auth required.

The two handlers are siblings in the same `http.ServeMux`. There is no shared protocol code between them: `/v1/ingest` is connector-scale, `/v1/archives` is archive-scale, and conflating their request paths would require runtime branching on body size that complicates both.

### 3.2 Listener Configuration

The existing listener's `http.Server.MaxBytesReader` (or equivalent body cap) applies to `/v1/ingest` only. `/v1/archives*` requests bypass the cap and instead enforce `Tus-Max-Size` (50 GiB v1, configurable) at protocol level and per-source quota at storage level (§5.4).

### 3.3 Identity Flow

After authentication (per spec 10) sets the request context's `delivered_by = <source-id>`, the source-id is the canonical client identity for the rest of the request. It is recorded in:

- Each chunk's audit log entry (per spec 06 §5.4).
- The finalized archive's `metadata.json` sidecar (§4.7).
- The staged archive's Identity block (per spec 06 §5.2) -- `auth_method: "bearer_token"`, `provider: "ingest"`, `account_id: <source-id>`.
- Every metric label that includes `source_id`.

The client's `Upload-Metadata.delivered_by` field, if present, is ignored. Server-set provenance is the only authoritative value.

## 4. Endpoint Contract (tus.io)

### 4.1 Methods

The endpoint speaks tus.io v1.0.0 with these extensions: `creation`, `termination`, `checksum`. The full method matrix:

| Method | Path | Purpose | Required headers | Response |
|---|---|---|---|---|
| `OPTIONS` | `/v1/archives` | Capability discovery | `Tus-Resumable: 1.0.0` | 200 + `Tus-Version`, `Tus-Max-Size`, `Tus-Extension`, `Tus-Resumable` |
| `POST` | `/v1/archives` | Initiate upload | `Upload-Length`, `Upload-Metadata`, `Tus-Resumable: 1.0.0`, `Authorization: Bearer <token>` | 201 + `Location: /v1/archives/<upload-id>` + `Tus-Resumable: 1.0.0`; OR (pre-flight idempotent hit) 303 + `Location: /v1/archives/<archive-id>` |
| `HEAD` | `/v1/archives/<upload-id>` | Read offset | `Tus-Resumable: 1.0.0`, `Authorization: Bearer <token>` | 200 + `Upload-Offset`, `Upload-Length`, `Tus-Resumable: 1.0.0` |
| `PATCH` | `/v1/archives/<upload-id>` | Append chunk | `Tus-Resumable: 1.0.0`, `Upload-Offset`, `Content-Type: application/offset+octet-stream`, `Content-Length`, `Authorization: Bearer <token>` | 204 + `Upload-Offset` (new total) + `Tus-Resumable: 1.0.0` |
| `DELETE` | `/v1/archives/<upload-id>` | Abort upload | `Tus-Resumable: 1.0.0`, `Authorization: Bearer <token>` | 204 |

The finalize step is NOT a separate method per tus.io: when a client's cumulative PATCH offset reaches `Upload-Length` exactly, the server transitions the upload to finalize automatically and responds with 204 + the new offset. The server's response to the final PATCH may take seconds (untar) and the client MUST tolerate that latency.

A separate `POST .../<upload-id>?finalize=1` endpoint is rejected: tus.io's "PATCH-reaches-length" semantic is sufficient and avoids inventing a custom verb.

### 4.2 `Upload-Metadata` Schema

The `Upload-Metadata` header carries `key1 value1,key2 value2,...` pairs where each value is base64-encoded per the tus.io spec. Required keys (server REJECTS the `POST` with 400 if any are missing or malformed):

| Key | Type | Description |
|---|---|---|
| `archive_id` | string | Client-supplied stable identifier. Format suggestion: `<8-hex-of-content-hash>-<original-archive-stem>-<sequence>`. MUST match `^[a-zA-Z0-9._-]{1,128}$`. |
| `archive_filename` | string | Original archive name as the user delivered it. Used only for provenance; not interpreted by the server. UTF-8, max 256 bytes. |
| `subtree_relative_path` | string | Path within the unpacked archive: `.` for raw-file deliveries (mbox), `<subdir>` for tarball subtrees. UTF-8, max 1024 bytes. |
| `media_type` | string | Stable media-type identifier. Server enforces against a static allow-list (§4.5); unknown values reject at `POST` BEFORE any bytes flow. |
| `matcher_id` | string | The recognizer matcher that claimed this subtree (e.g., `google-takeout/mail`). Recorded in metadata; not interpreted. UTF-8, max 256 bytes. |
| `provider` | string | Upstream provider (`google`, `meta`, etc.). Recorded; not interpreted. Lowercase alpha + dash, max 64 bytes. |
| `sha256` | string | Hex-encoded sha256 of the content bytes the client will send. 64 lowercase hex chars. Verified at finalize. |
| `size_bytes` | string | Decimal-encoded size in bytes. MUST equal the `Upload-Length` header. Verified at finalize. |

Server-set fields (client-supplied values are ignored):

| Key | Source | Description |
|---|---|---|
| `delivered_by` | Validated bearer token | Source-id; canonical client identity per spec 10. |
| `delivered_at` | Finalize timestamp | RFC 3339 UTC. |

Total `Upload-Metadata` header length MUST NOT exceed 4 KiB. Exceeding returns 431.

### 4.3 Pre-flight Idempotency

Before accepting a `POST /v1/archives`, the server checks for an existing finalized archive at `archives/<archive_id>/`:

- **Exists with matching sha256** -- the upload is a duplicate of a successful previous delivery. Server returns 303 See Other with `Location: /v1/archives/<archive_id>` pointing at the finalized resource. No upload state is created; no bytes flow. The client treats this as success.
- **Exists with different sha256** -- the client is reusing an archive_id for non-identical content, which is a contract violation. Server returns 409 Conflict with body `{"error":"archive_id_conflict","existing_sha256":"...","claimed_sha256":"..."}`.
- **Absent** -- normal tus.io flow proceeds. Server allocates an `upload-id`, creates `.tmp-archives/<upload-id>`, returns 201 + `Location`.

This is a deviation from vanilla tus.io's "POST always creates" semantics, but it is necessary for idempotent retry under transient network failures and is the natural fit for the recognizer's at-least-once delivery semantics.

### 4.4 PATCH Validation

For each `PATCH /v1/archives/<upload-id>`:

1. Validate `Upload-Offset` matches the server's current stored offset for this upload. Mismatch returns 409.
2. Validate `Content-Type: application/offset+octet-stream` (per tus.io). Wrong type returns 415.
3. Append the body bytes to `.tmp-archives/<upload-id>` and update a rolling sha256 (`hash/sha256.New()` writer wrapped around the bytes-to-disk).
4. Update the server's stored offset.
5. Respond 204 + new `Upload-Offset`.

If the body bytes pushed the cumulative offset beyond `Upload-Length`, the server discards the surplus, sets the offset to `Upload-Length`, and proceeds to finalize. The client SHOULD NOT send more than `Upload-Length` total bytes, but the server tolerates a small overshoot rather than failing the upload.

If a PATCH body terminates short of its declared `Content-Length` (client connection dropped), the server reads what arrived, updates the offset, persists state, and waits for the client to resume via HEAD + a subsequent PATCH.

### 4.5 Media-Type Allow-List

The server maintains a static allow-list of accepted `media_type` values, hard-coded in v1 (no operator override -- a new media type means a code change AND a corresponding importer):

| `media_type` | Storage shape | Importer (current) |
|---|---|---|
| `archive/mbox` | raw-file | spec 09 mbox-importer |
| `archive/google-takeout-subtree` | tarball | (future spec) Takeout subtree importer |

A `POST` with an unknown `media_type` returns 400 with body `{"error":"unknown_media_type","value":"..."}` BEFORE any bytes flow. This protects the storage layer from arbitrary data and forces deliberate code review when a new format lands.

### 4.6 Finalize: Validation + Untar Dispatch

When the server's stored offset equals `Upload-Length`:

1. **sha256 verification.** Compare the rolling sha256 (computed during PATCH) to `Upload-Metadata.sha256`. Mismatch → delete `.tmp-archives/<upload-id>`, free upload state, return 400 with body `{"error":"sha256_mismatch","claimed":"...","computed":"..."}`.
2. **Size verification.** Confirm the on-disk file's actual size matches `size_bytes`. Mismatch → delete + 400.
3. **Untar dispatch** by `media_type`:
   - **Raw-file** (`archive/mbox`, future `archive/pst`): the tmp file is the final artifact. Build the target directory `.tmp-archives/<upload-id>.finalize/raw/<archive_filename>` and hardlink/rename the tmp file into it.
   - **Tarball** (`archive/google-takeout-subtree`, future siblings): open the tmp file, stream through `archive/tar.NewReader`, write each entry under `.tmp-archives/<upload-id>.finalize/tree/<entry-path>` per §4.7 tar-safety rules.
4. **metadata.json sidecar.** Write `.tmp-archives/<upload-id>.finalize/metadata.json` containing the validated metadata fields (§4.2) plus server-set `delivered_by`, `delivered_at`, `sha256_verified: true`, `entries_extracted` (0 for raw-file, count for tarball).
5. **Atomic rename.** `os.Rename(".tmp-archives/<upload-id>.finalize", "archives/<archive_id>")`. This is the single atomic event that publishes the archive to the importers (§5.3).
6. **Cleanup.** Delete the tmp upload file. Free in-memory upload state.

Steps 1-5 happen synchronously inside the final PATCH's response handler. The client sees 204 (final PATCH ack) followed by a separate `HEAD` they MAY make to fetch the finalize result, OR -- preferred -- they treat 204 with `Upload-Offset == Upload-Length` as the success signal and consult the receipt via a follow-up `GET /v1/archives/<archive_id>` (specified in §4.8).

### 4.7 Tar Safety Rules

For tarball `media_type` values, the server REJECTS the entire upload (delete tmp, return 400 + identifying detail) on the first violation of any of the following:

- **Absolute paths.** An entry whose name starts with `/` or contains `:` (Windows drive prefix).
- **Path traversal.** An entry whose name contains `..` as a path component (`a/../b` is rejected even if it normalizes safely).
- **Symbolic links.** Any entry with `Typeflag = TypeSymlink` (`TypeLink` -- hard link -- also rejected; no link types).
- **Device files.** Any `TypeChar`, `TypeBlock`, `TypeFifo`.
- **Sockets.** Any `TypeFifo` / `TypeXGlobalHeader` / `TypeXHeader` -- only `TypeReg` and `TypeDir` entries are permitted.
- **Entries with non-normal modes.** Server overrides all extracted file modes to `0600` and directory modes to `0700`. Tar `mode` bits other than the permission bits are ignored.
- **Entries with non-default UID/GID.** Tar `Uid`/`Gid` are ignored; extraction runs as the connector user. Set-uid / set-gid mode bits are stripped.
- **Entry size exceeds remaining quota.** Per-source quota tracking (§5.4); reject if extracting this entry would exceed the source's soft cap.
- **Total entries exceed 1,000,000.** Belt-and-suspenders against zip-bomb-style entry-count attacks.

The rejection log line is structured (`glovebox archive upload tar safety reject`) with `archive_id`, `source_id`, `entry_path`, `entry_type`, `reason` so operators can identify adversarial inputs.

### 4.8 Finalize Receipt

After successful finalize, the resource `GET /v1/archives/<archive_id>` returns 200 + JSON:

```json
{
  "archive_id": "abcdef12-takeout-20260411t180338z-13-001",
  "received_at": "2026-05-21T19:32:14Z",
  "delivered_by": "recognizer",
  "media_type": "archive/google-takeout-subtree",
  "size_bytes": 12345678901,
  "sha256": "ab12...",
  "sha256_verified": true,
  "staged_path": "archives/abcdef12-takeout-20260411t180338z-13-001",
  "entries_extracted": 47
}
```

For raw-file media types, `entries_extracted` is 0 and a `raw_filename` field is present.

The same JSON is the body of `metadata.json` in the staged archive. A `GET` against an archive that does not exist returns 404; against one that's still uploading returns 404 (the resource is the *finalized* archive, not the in-flight upload).

## 5. Storage Layout

### 5.1 Directory Structure

All under the existing staging PVC:

```
<staging_root>/
├── archives/                          (final, post-finalize location)
│   ├── <archive_id>/
│   │   ├── metadata.json             (§4.8 receipt)
│   │   ├── raw/                       (raw-file media types only)
│   │   │   └── <archive_filename>
│   │   └── tree/                      (tarball media types only)
│   │       └── ... (entries, relative paths preserved)
│   └── ...
├── .tmp-archives/                     (in-flight uploads; not visible to importers)
│   ├── <upload-id>                    (single sparse file the PATCHes append into)
│   └── <upload-id>.finalize/          (finalize-time staging dir; renamed in one shot)
│       ├── metadata.json
│       └── raw/ OR tree/
└── ... (existing connector staging items live alongside, unchanged)
```

The exact `<staging_root>` value comes from `GLOVEBOX_STAGING_DIR` per the existing framework convention. Importers watch `<staging_root>/archives/`; they do NOT touch `<staging_root>/.tmp-archives/`.

### 5.2 Atomicity Model

The single committing event is the `os.Rename(".tmp-archives/<upload-id>.finalize", "archives/<archive_id>")` in §4.6 step 5. This rename is atomic on a single-filesystem POSIX volume (the staging PVC is exactly one mounted filesystem); either the importer sees a fully-staged `archives/<archive_id>/` with `metadata.json` and content, or it sees nothing.

If the process dies between the rolling-sha256 verification (§4.6 step 1) and the atomic rename (step 5), the `.tmp-archives/<upload-id>.finalize/` dir is left orphaned. The cleanup job (§5.5) deletes such orphans.

`metadata.json` is written to `.tmp-archives/<upload-id>.finalize/metadata.json` AFTER all raw/ or tree/ content is in place, so it never appears partially. Within the finalize dir, the metadata.json is the last file written and the rename publishes everything together.

### 5.3 Importer Pickup

Importers (spec 09 mbox-importer; future siblings) watch `<staging_root>/archives/` via `fsnotify` for `IN_CREATE` events on directory entries. The triggering event is the rename in §5.2; importers MUST NOT react to anything under `.tmp-archives/`.

The importer's contract:

1. On `IN_CREATE` for `archives/<archive_id>/`, wait up to 2 seconds for `metadata.json` to be readable (paranoia against the rename being observed before the kernel flushes the rename event's underlying directory updates -- in practice the rename is single-syscall and atomic, but the 2-second timeout costs nothing).
2. Read `metadata.json`. If `media_type` does not match the importer's allow-list, ignore the archive (a different importer will handle it).
3. Acquire an importer-specific advisory lock on `archives/<archive_id>/.importer-lock` (an existence-check + create with `O_EXCL`) so a single archive is not double-picked.
4. Process the archive (mbox → per-message items into the connector-staging surface; Takeout subtree → per-file items).
5. On success, move `archives/<archive_id>/` to `archives/.done/<archive_id>/` for retention (configurable; default 7 days then deleted).
6. On failure, leave the archive in place; the importer's failure log identifies the archive_id.

The retention dir `archives/.done/` is operator-visible for forensic audit but is NOT watched by importers (no `IN_CREATE` event for moves into it).

### 5.4 Sizing + Quota

**Default PVC size: 50 GiB.** Operator override via the Helm chart's `staging.size` value. Justification: recognizer's data PVC is 100 GiB and currently holds 4 Takeout zips + a 12 GB mbox; once those land in glovebox at archive scale the equivalent need at glovebox is roughly half (importers consume archives and shrink the on-disk footprint as they emit per-item content; the archives themselves are deleted after retention).

**Per-source soft cap.** Each `source_id` is given a default soft cap of 40% of total PVC capacity (configurable). When `archives/` usage attributable to a single `source_id` exceeds the soft cap, the server logs `glovebox archive storage near cap` at WARN with `source_id, used_bytes, soft_cap_bytes` and exposes `glovebox_archive_storage_pct{source_id}` as a Prometheus gauge for alerting. The upload continues; soft cap is informational.

**Global hard cap.** When `archives/` total usage exceeds 95% of PVC capacity, the server returns 503 Service Unavailable on new `POST /v1/archives` with `Retry-After: 600`. In-flight uploads continue (terminating them would lose work that's already on disk). The 503 lifts when usage drops below 85% (hysteresis).

**Storage measurement.** A background goroutine sums `archives/` directory sizes on a 60-second interval and publishes the totals. This is cheap on a homelab-scale tree (low hundreds of subdirectories).

### 5.5 Orphan Cleanup

On startup AND on a 60-minute interval, the server walks `.tmp-archives/` and deletes:

- Any `<upload-id>` file (no `.finalize` sibling) whose mtime is older than 24 hours. These are stale in-flight uploads where the client never resumed.
- Any `<upload-id>.finalize/` directory whose mtime is older than 1 hour. These are processes that died mid-finalize (the legitimate finalize is sub-second; an hour-old `.finalize` is wreckage).

The cleanup is logged at INFO with the count of items removed. The interval is configurable but the defaults are reasonable for homelab cadence.

## 6. Authentication

Spec 10 (External Ingest Auth, promoted from stub concurrently with this spec) defines the bearer-token model. This spec defers to spec 10 for:

- Token storage (Vault KV v2 path `secret/glovebox/ingest-tokens/<source-id>`).
- ESO projection to consumer namespaces.
- Server-side loading at startup and SIGHUP reload.
- `Authorization: Bearer <token>` header validation with `crypto/subtle.ConstantTimeCompare`.
- 401 response shape.
- Per-IP rate limiting on rejected attempts.
- Audit logging.
- Rotation semantics.

This spec adds two archive-delivery-specific requirements on top of spec 10:

1. **Auth check happens BEFORE the pre-flight idempotency check (§4.3).** A request with a bad token returns 401 even if the archive_id would have hit the idempotent fast-path; we do not leak existence information to unauthenticated callers.
2. **`source_id` is canonical provenance.** The validated token's source-id is recorded in every staged archive's metadata.json `delivered_by` field and in every metric label that includes `source_id`. The client cannot override it.

## 7. Observability

### 7.1 Metrics

All metrics use the existing framework's OTel-on-Prometheus emitter. Schoology connector (spec 12 §13) established the pattern.

| Metric | Type | Labels | Notes |
|---|---|---|---|
| `glovebox_archive_uploads_total` | counter | `source_id, media_type, status` | `status` ∈ {`created`, `completed`, `failed_sha256`, `failed_size`, `failed_untar`, `failed_auth`, `failed_quota`, `terminated`} |
| `glovebox_archive_upload_bytes_total` | counter | `source_id, media_type` | Sum of successfully received bytes |
| `glovebox_archive_upload_duration_seconds` | histogram | `source_id, media_type` | From `POST /v1/archives` to finalize completion |
| `glovebox_archive_upload_in_flight` | gauge | `source_id` | Currently active uploads per source |
| `glovebox_archive_patch_chunk_bytes` | histogram | -- | Per-PATCH body size, for client tuning |
| `glovebox_archive_extracted_entries_total` | counter | `media_type` | Total entries extracted from tarballs |
| `glovebox_archive_extracted_bytes_total` | counter | `media_type` | Total bytes written during untar |
| `glovebox_archive_storage_bytes` | gauge | -- | Current bytes under `archives/` |
| `glovebox_archive_storage_pct` | gauge | -- | `archive_storage_bytes / PVC capacity` |
| `glovebox_archive_storage_source_bytes` | gauge | `source_id` | Bytes attributable to one source |
| `glovebox_ingest_auth_total` | counter | `endpoint, status` | Spec 10 surface; `endpoint` ∈ {`/v1/ingest`, `/v1/archives`}, `status` ∈ {`accepted`, `rejected`, `rate_limited`} |
| `glovebox_ingest_auth_rejected_total` | counter | `remote_ip_bucket` | Low-cardinality bucketing of rejected source IPs |
| `glovebox_archive_tar_safety_rejections_total` | counter | `source_id, reason` | Per the §4.7 reason enum |

### 7.2 Traces

OpenTelemetry spans, exporter shared with the rest of glovebox:

- Root: `glovebox.archive.upload`
  - Attributes: `source_id`, `archive_id`, `media_type`, `size_bytes` (declared), `sha256` (declared)
  - Lifetime: from `POST /v1/archives` to either successful finalize or terminal failure / DELETE
- Child: `glovebox.archive.create` -- the `POST` handler.
- Child: `glovebox.archive.patch` -- ONE span per upload, NOT per PATCH (per-PATCH spans on a 12 GiB upload would be tens of thousands). The span starts on first PATCH and ends when the upload's offset reaches `Upload-Length` OR when a DELETE arrives. Attributes: `patches_count` (final), `bytes_received` (final).
- Child: `glovebox.archive.finalize` -- the sha256 + size verification phase.
- Child: `glovebox.archive.untar` -- one span for the entire untar, with `entries_count` attribute. NOT per-entry.

Importer-side spans (spec 09 and successors) correlate to this trace by reading `archive_id` from the staged metadata.json and using it as a tracing attribute. There is no parent/child span relationship across the staging-PVC boundary -- it is event-driven, not RPC-style.

### 7.3 Structured Logs

Library: `log/slog` per glovebox convention.

| Event | Level | Fields |
|---|---|---|
| `glovebox archive upload created` | INFO | `source_id, archive_id, media_type, declared_size_bytes, declared_sha256` |
| `glovebox archive upload completed` | INFO | `source_id, archive_id, media_type, received_bytes, duration_ms, sha256_verified, entries_extracted` |
| `glovebox archive upload aborted` | WARN | `source_id, archive_id, reason` (`sha256_mismatch` / `size_mismatch` / `tar_unsafe_entry` / `quota_exceeded` / `terminated_by_client` / `process_died` / `idempotency_conflict`) |
| `glovebox archive upload tar safety reject` | WARN | `source_id, archive_id, entry_path, entry_type, reason` |
| `glovebox archive storage near cap` | WARN | `source_id, used_bytes, soft_cap_bytes` (per-source) |
| `glovebox archive storage hard cap` | ERROR | `used_bytes, capacity_bytes, pct` (global) |
| `glovebox archive cleanup` | INFO | `removed_uploads, removed_finalize_dirs, freed_bytes` |
| `glovebox ingest authenticated` | INFO | `source_id, remote_addr, endpoint, archive_id` (if applicable) |
| `glovebox ingest auth rejected` | WARN | `remote_addr, endpoint` (NEVER token attempt) |
| `glovebox ingest auth rate limited` | WARN | `remote_addr, attempts_in_window` |

### 7.4 Recommended Alerts

Documented for operator reference; this spec does NOT auto-deploy alert rules.

- `glovebox_archive_storage_pct > 0.95` for 5m → CRITICAL: hard-cap 503s imminent.
- `glovebox_archive_storage_pct > 0.80` for 15m → WARNING: drain backlog.
- `rate(glovebox_ingest_auth_total{status="rejected"}[5m]) > 1.0` → WARNING: probable probing.
- `rate(glovebox_archive_uploads_total{status=~"failed_.*"}[10m]) > 0.1` → WARNING: integration drift.
- `absent(glovebox_archive_uploads_total{status="completed"}[24h])` AND recognizer expected-active → WARNING: recognizer silent.
- `rate(glovebox_archive_tar_safety_rejections_total[1h]) > 0` → INFO: tar-safety reject occurred; investigate.

## 8. Operational Concerns

### 8.1 NetworkPolicy

The recognizer namespace MUST be granted ingress to the glovebox external listener port. Chart change: add a `NetworkPolicy` rule (or extend the existing one) allowing TCP/8081 from `namespaceSelector: { matchLabels: { name: openclaw-recognizer } }` (the recognizer's namespace label).

Other archive-delivery clients (future workstation mbox importer, friend imports) will need their own ingress rules; not specified here.

### 8.2 PVC Sizing

Default 50 GiB per §5.4. Chart value `staging.size` controls this. PVC class follows the existing staging PVC's storage class (homelab's default).

### 8.3 Cleanup of Orphaned Uploads

Per §5.5. The cleanup goroutine starts on connector boot and runs on a 60-minute interval. No external cron required.

### 8.4 Configuration

New Helm values block:

```yaml
ingest:
  archives:
    enabled: false                  # opt-in
    maxUploadSize: 53687091200      # 50 GiB
    perSourceSoftCapPct: 40         # of total PVC
    globalHardCapPct: 95            # 503 threshold
    globalHardCapHysteresisPct: 85  # 503-lift threshold
    cleanupIntervalSeconds: 3600
    cleanupTmpAgeHours: 24
    cleanupFinalizeAgeHours: 1
    retention:
      doneArchiveDays: 7
  auth:                              # specified by spec 10
    tokensPath: secret/glovebox/ingest-tokens
    reloadIntervalSeconds: 300
    perIPRateLimit:
      window: 60s
      maxRejected: 10
```

## 9. Failure Modes

| Symptom | Cause | Server response | Recovery |
|---|---|---|---|
| 401 on POST | Bad / missing / expired bearer token | 401 + WWW-Authenticate | Operator: check token in Vault matches consumer Secret; reload glovebox if rotated |
| 409 on POST | Same archive_id, different sha256 | 409 + JSON | Client: use a fresh archive_id; OR verify the existing archive is correct and skip the re-send |
| 415 on PATCH | Wrong Content-Type | 415 | Client bug; use `application/offset+octet-stream` |
| 400 on finalize | sha256 / size mismatch | 400 + JSON detail | Client: recompute, re-send under new upload-id (since tmp is deleted) |
| 400 on finalize | Tar safety violation | 400 + JSON detail | Client: clean the offending archive entries upstream; re-send |
| 503 on POST | Global hard cap | 503 + Retry-After 600 | Operator: drain or grow PVC; clients retry after `Retry-After` |
| 429 on POST | Per-IP rate limit on 401s | 429 + Retry-After | Caller is misconfigured; fix the token, wait, retry |
| stuck upload | Client connection lost mid-PATCH | -- | Client: HEAD to read offset, resume from there. Auto-cleanup after 24h via §5.5 |
| process dies mid-finalize | Server crash | -- | On restart, `.tmp-archives/<id>.finalize/` is orphaned; cleanup removes it; client retries with the same archive_id (the original tmp file is gone, so this is a full re-send) |

## 10. Out of Scope

- Bearer-token rotation with overlap window. v1 requires a coordinated rotation; spec 10 §11 documents the future schema extension.
- Auth on `/v1/ingest`. Existing connector-scale path keeps its no-auth behavior; future spec extends spec 10 to cover it.
- Multi-tenant token scoping (per-source allowed media_types). All authenticated source-ids can upload any allowed media_type. Adding per-token scope is a spec 10 future extension.
- Streaming untar during PATCH. v1 untars at finalize after the full file is on disk. The few seconds of extra I/O is acceptable for homelab cadence.
- Server-side compression. Tarballs MUST be uncompressed tar (no .tar.gz, no .tar.zst) in v1. The recognizer's pipeline produces uncompressed tar artifacts; if compressed support is wanted later, it lands as a media-type-aware decompression in the untar dispatch.
- Per-archive_id deduplication of CONTENT across different archive_ids. If two distinct archive_ids carry the same sha256, the archive is stored twice. Content-addressed dedup is a meaningful storage optimization but adds a content-hash lookup table that v1 doesn't need.
- Glovebox-side parsing of mbox / Takeout / etc. Per-importer specs own that work.
- TLS termination at the ingest listener. Cluster ingress through Traefik handles TLS for external traffic per existing convention.
- Resumable-upload behavior across glovebox process restarts. v1 keeps upload state in memory; a process restart loses in-flight upload state (the tmp file persists on disk but the upload-id mapping is gone). Clients retry from offset 0 (or POST with the same archive_id and the pre-flight idempotency may hit, depending on whether the previous attempt actually finalized). Adding state persistence is a future option but adds disk-state-sync complexity for marginal benefit in this deployment.

## 11. Implementation Plan Sketch

This spec gates [glovebox-p1zx](.beads/issues.jsonl)'s implementation. The writing-plans skill produces the final wave-by-wave plan; below is the rough shape so the reviewer can sanity-check scope.

- **Wave A (parallel, 3 tasks)** -- foundations:
  - A1: Vault-token loader + reload (server-side). Mirrors the schoology refresher's Vault K8s auth pattern.
  - A2: Per-IP rate-limit middleware.
  - A3: `delivered_by` provenance plumbing in `internal/ingest/handler.go` and through to staging metadata.json. May require minor spec 06 §5 Identity-block touch.

- **Wave B (parallel, 2 tasks)** -- the protocol surface:
  - B1: tus.io HTTP handler with `creation`, `termination`, `checksum` extensions. Decision in the plan: embed `tusd`'s internals as a library OR roll a thin handler. Tradeoff: dep weight vs. protocol-correctness assurance.
  - B2: Untar dispatcher with safe-path / safe-link rejection, atomicity (`os.Rename` finalize), metadata.json sidecar writer.

- **Wave C (solo)** -- wire-up + integration test:
  - C1: Mount handlers in the framework; thread auth + rate-limit middleware. Quota measurement goroutine. Cleanup goroutine.
  - C2: Integration test: a tus client uploads a 50 MB mbox + a 5-file tarball; both stage correctly; corrupted sha256 rejected; tar-safety violation rejected.

- **Wave D (solo)** -- ops:
  - D1: Helm chart updates (PVC 50 GiB default, NetworkPolicy for recognizer ingress, Vault ClusterSecretStore reference for `ingest-tokens/`).
  - D2: Smoke test script.

Each wave follows the established spec-12 review pattern: implementer + 2 reviewers per task, single bundled cleanup commit. **A dedicated security review of the merged result is required** before declaring this work shippable, given the attack surface (multi-GB external-facing endpoint, tar extraction, token validation).

## 12. Related Specs

- **Spec 06 (Connector Auth and Provenance)** -- the Identity block, audit log, and `delivered_by` provenance pattern this spec emits into. §6 of this spec defers to spec 10 §6 for the integration details.
- **Spec 08 (HTTP Ingest API)** -- the existing `/v1/ingest` endpoint. This spec adds a sibling endpoint surface without modifying spec 08.
- **Spec 09 (Mbox Importer)** -- the first downstream consumer of an archive staged by this endpoint. The mbox-importer's `fsnotify` watch on `archives/` is the contract this spec satisfies.
- **Spec 10 (External Ingest Auth)** -- promoted from stub concurrently with this spec. Defines the bearer-token model §6 defers to.
- **Spec 11 (Data Subject and Audience)** -- not directly invoked. The staged archive's per-item metadata is the importer's concern, not this endpoint's.
