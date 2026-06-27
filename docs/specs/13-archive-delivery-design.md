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
   (Upload-Length,                            |- 401 on bad token
    Upload-Metadata,                          |
    Authorization)                            v
                                              Pre-flight idempotency
                                              (scoped by source-id)
                                              |
                                              |- absent -> create upload state
                                              |- match  -> 303 Location
                                              |- conflict (same source-id,
                                              |            different sha256) -> 409
   201 + Location              <----------    |
                                              |
   PATCH .../<upload-id>       ---------->    Per-upload-id mutex
   (Upload-Offset + chunk)                    Append + rolling sha256
                                              Update offset
   204 + Upload-Offset         <----------    |
   ...                                         |
   PATCH (final chunk:         ---------->    Offset reaches Upload-Length
    offset+len == length)                     |- sha256 verify (rolling)
                                              |- size verify
                                              |- media-type dispatch:
                                              |   |- raw: rename tmp -> .tmp-archives/<id>.finalize/raw/
                                              |   |- tar: stream-untar -> .tmp-archives/<id>.finalize/tree/
                                              |- write metadata.json sidecar
                                              |- atomic os.Rename:
                                              |    .tmp-archives/<id>.finalize -> archives/<archive_id>
                                              |
   204 + Upload-Offset         <----------    (importer fsnotify wakes; reads
   (== Upload-Length)                          archives/<archive_id>/metadata.json)
                                              |
   GET /v1/archives/<archive_id> ---------->  Read staged metadata.json
                                              |
   200 + receipt JSON         <----------     |
```

### 3.1 Endpoint Surface

A single HTTP listener (the existing ingest listener on port 9091 per spec 08 §2.2) hosts two distinct handler trees:

- `/v1/ingest` -- unchanged. Multipart with 64 MB cap. No auth in v1 (per spec 10 §7).
- `/v1/archives*` -- new. tus.io protocol. Bearer-token auth required.

The two handlers are siblings in the same `http.ServeMux`. There is no shared protocol code between them: `/v1/ingest` is connector-scale, `/v1/archives` is archive-scale, and conflating their request paths would require runtime branching on body size that complicates both.

### 3.2 Listener Configuration

The existing listener's `http.Server.MaxBytesReader` (or equivalent body cap) applies to `/v1/ingest` only. `/v1/archives*` requests bypass the cap and instead enforce `Tus-Max-Size` (50 GiB v1, configurable) at protocol level and per-source quota at storage level (§5.4).

### 3.3 Identity Flow

After authentication (per spec 10) sets the request context's `delivered_by = <source-id>`, the source-id is the canonical client identity for the rest of the request. It is recorded in:

- Each chunk's audit log entry (per spec 06 §8.3).
- The finalized archive's `metadata.json` sidecar (§4.8).
- The staged archive's Identity block (per spec 06 §5.2) -- `auth_method: "bearer_token"`, `provider: "ingest"`, `account_id: <source-id>`.
- Every metric label that includes `source_id`.

The client's `Upload-Metadata` MUST NOT contain the keys `delivered_by` or `delivered_at`. The server REJECTS any `POST /v1/archives` whose `Upload-Metadata` carries either key with 400. This is a hard rejection rather than a silent server-side override: a client that thinks it can set these fields has a bug worth surfacing, not a behavior to silently paper over.

## 4. Endpoint Contract (tus.io)

### 4.1 Methods

The endpoint speaks tus.io v1.0.0 with these extensions: `creation`, `termination`, `checksum`. The full method matrix:

| Method | Path | Purpose | Required headers | Response |
|---|---|---|---|---|
| `OPTIONS` | `/v1/archives` | Capability discovery | `Tus-Resumable: 1.0.0` | 200 + `Tus-Version`, `Tus-Max-Size`, `Tus-Extension`, `Tus-Resumable` |
| `POST` | `/v1/archives` | Initiate upload | `Upload-Length`, `Upload-Metadata`, `Tus-Resumable: 1.0.0`, `Authorization: Bearer <token>` | 201 + `Location: /v1/archives/<upload-id>` + `Tus-Resumable: 1.0.0`; OR (pre-flight idempotent hit) 303 + `Location: /v1/archives/<archive_id>` |
| `HEAD` | `/v1/archives/<upload-id>` | Read offset | `Tus-Resumable: 1.0.0`, `Authorization: Bearer <token>` | 200 + `Upload-Offset`, `Upload-Length`, `Tus-Resumable: 1.0.0`, `Tus-Expires` (deadline before §5.5 cleanup) |
| `PATCH` | `/v1/archives/<upload-id>` | Append chunk | `Tus-Resumable: 1.0.0`, `Upload-Offset`, `Content-Type: application/offset+octet-stream`, `Content-Length`, `Authorization: Bearer <token>` | 204 + `Upload-Offset` (new total) + `Tus-Resumable: 1.0.0` |
| `DELETE` | `/v1/archives/<upload-id>` | Abort upload | `Tus-Resumable: 1.0.0`, `Authorization: Bearer <token>` | 204 |
| `GET` | `/v1/archives/<archive_id>` | Read finalize receipt | `Authorization: Bearer <token>` | 200 + receipt JSON per §4.8; 404 if not finalized; 404 if archive_id belongs to a different source-id |

**Upload-ID format.** Server-generated upload-ids are 128-bit cryptographically random values, hex-encoded (32 lowercase hex chars). The namespace is large enough that collision probability is negligible at any realistic upload volume; a colliding ID at POST time would trigger an internal 500.

**Upload-ID binding.** Each upload-id is bound at creation to the source-id that POSTed it. The server REJECTS any `HEAD`/`PATCH`/`DELETE` against an upload-id whose binding does not match the requesting source-id with 404 (NOT 403 — we do not leak existence of upload-ids across source-ids).

**Acknowledged fingerprinting limitation (intra-source).** The §4.4 `409 upload_busy` response (returned to the upload's OWNER while another PATCH is in flight on that upload-id) is distinguishable from a 404 (returned to non-owners). A client that holds a valid token can therefore probe its OWN upload-ids to detect concurrent activity. This is a deliberate accepted disclosure: a token-holder's ability to know about their own uploads is below the meaningful-disclosure threshold (they already have the token). Cross-source fingerprinting is the case we close: an unauthorized source-id cannot distinguish "upload-id doesn't exist" from "upload-id belongs to another source-id" because both return 404.

The finalize step is NOT a separate method per tus.io: when a client's cumulative PATCH offset reaches `Upload-Length` exactly, the server transitions the upload to finalize automatically and responds with 204 + the new offset. The server's response to the final PATCH may take seconds (untar) and the client MUST tolerate that latency.

A separate `POST .../<upload-id>?finalize=1` endpoint is rejected: tus.io's "PATCH-reaches-length" semantic is sufficient and avoids inventing a custom verb.

### 4.2 `Upload-Metadata` Schema

The `Upload-Metadata` header carries `key1 value1,key2 value2,...` pairs where each value is base64-encoded per the tus.io spec. Required keys (server REJECTS the `POST` with 400 if any are missing or malformed):

| Key | Type | Description |
|---|---|---|
| `archive_id` | string | Client-supplied stable identifier. Format suggestion: `<8-hex-of-content-hash>-<original-archive-stem>-<sequence>`. MUST match `^[a-zA-Z0-9._-]{1,128}$`. **The regex check happens BEFORE any logging or metric emission**: an unvalidated archive_id never reaches an audit-log call site, which prevents log-line injection via crafted newlines/control chars. |
| `archive_filename` | string | Original archive name as the user delivered it. Used only for provenance AND (for raw-file media types) as the on-disk filename under `raw/`. MUST match `^[A-Za-z0-9._-]+$` and MUST NOT contain `..`, `/`, `\`, NUL, or any control character. Max 256 bytes. Server rejects at POST. |
| `subtree_relative_path` | string | Path within the unpacked archive: `.` for raw-file deliveries (mbox), `<subdir>` for tarball subtrees. UTF-8, max 1024 bytes, no NUL or control chars. Recorded; not used as a filesystem path. |
| `media_type` | string | Stable media-type identifier. Server enforces against a static allow-list (§4.5); unknown values reject at `POST` BEFORE any bytes flow. |
| `matcher_id` | string | The recognizer matcher that claimed this subtree (e.g., `google-takeout/mail`). MUST match `^[A-Za-z0-9._/-]{1,256}$` (no NUL, no control chars). Recorded; not interpreted. |
| `provider` | string | Upstream provider (`google`, `meta`, etc.). MUST match `^[a-z][a-z0-9-]{0,63}$`. Recorded; not interpreted. |
| `sha256` | string | Hex-encoded sha256 of the content bytes the client will send. 64 lowercase hex chars. Verified at finalize. |
| `size_bytes` | string | Decimal-encoded size in bytes. MUST equal the `Upload-Length` header. **Verified at POST** (not at finalize): mismatch returns 400 before any upload state is allocated, so a client never wastes a multi-GB upload on a header bug. |

**Validation ordering** (MANDATORY): all `Upload-Metadata` regex / format checks happen BEFORE any audit-log entry or metric emission that references the field. The validator either accepts the entire `Upload-Metadata` block (proceeding to logging + state allocation) or rejects it whole with a single 400 + a non-revealing error code. There is NO partial-validation flow that logs some fields while rejecting others.

Server-set fields (client-supplied values are ignored):

| Key | Source | Description |
|---|---|---|
| `delivered_by` | Validated bearer token | Source-id; canonical client identity per spec 10. |
| `delivered_at` | Finalize timestamp | RFC 3339 UTC. |

Total `Upload-Metadata` header length MUST NOT exceed 4 KiB. Exceeding returns 431.

### 4.3 Pre-flight Idempotency

Before accepting a `POST /v1/archives`, the server checks for an existing finalized archive at `archives/<archive_id>/`. **Idempotency is scoped by the authenticated source-id**: the lookup is `(source_id, archive_id) -> metadata.json`. A cross-source archive_id collision never returns the "matching sha256" branch; this prevents one source-id from probing what another source-id has uploaded by enumerating guessable archive_ids.

The three branches:

- **No `archives/<archive_id>/` exists** -- normal tus.io flow proceeds. Server allocates an `upload-id`, binds it to the requesting source-id, creates `.tmp-archives/<upload-id>`, returns 201 + `Location`.
- **`archives/<archive_id>/` exists, belongs to the requesting source-id, sha256 matches** -- duplicate of a prior successful delivery from the same source. Server returns 303 See Other + `Location: /v1/archives/<archive_id>`. No upload state created; no bytes flow.
- **`archives/<archive_id>/` exists AND (belongs to a different source-id OR has a different sha256)** -- conflict. Server returns 409 Conflict with body `{"error":"archive_id_conflict"}`. The response body deliberately does NOT echo the existing archive's sha256 or source-id; a probing client learns only "this archive_id is taken," not the contents of the conflicting record.

The 303 fast-path is a deviation from vanilla tus.io's "POST always creates" semantics, justified by the recognizer's at-least-once delivery semantics: a network failure between server-finalize and client-receives-201 leaves the client unsure whether to retry, and the idempotent fast-path makes the safe answer "yes, always retry."

Authentication (per spec 10) is checked BEFORE the idempotency lookup. Unauthenticated callers see 401 regardless of whether the archive_id exists, so the existence-leak surface is bounded by the bearer-token set.

**Acknowledged timing-side-channel.** The idempotency check is a filesystem `stat(archives/<archive_id>)` followed by a metadata.json read; its latency depends on filesystem-cache state. The on-disk directory layout (§5.1) is FLAT — `archives/<archive_id>/` with no source-id subdirectory — so an authenticated token-holder can detect the existence of an archive_id BY ANY source-id via stat-timing probes, then receive the 409 conflict response (which intentionally does NOT echo sha256 or source-id, §4.3 above) to confirm. The information leak is bounded: a token-holder learns "this archive_id is taken somewhere in the system" but not by whom or with what content. For the v1 homelab scope (tokens map to explicitly-trusted clients), this is below the threshold for restructuring the layout to `archives/<source_id>/<archive_id>/`. Future revisions that admit untrusted external sources should reconsider.

### 4.4 PATCH Validation

For each `PATCH /v1/archives/<upload-id>`:

1. Validate the upload-id is bound to the requesting source-id (§4.1). Mismatch returns 404.
2. Acquire the per-upload-id mutex. The server maintains one `sync.Mutex` per active upload-id; PATCH, HEAD, and DELETE all serialize through it. If the mutex is already held when the request arrives, return 409 + `{"error":"upload_busy"}` immediately (do NOT block waiting). This prevents two concurrent PATCHes (from the same client with two TCP sessions, or from a buggy retry loop) from interleaving bytes into the tmp file and corrupting the rolling sha256.
3. Validate `Upload-Offset` matches the server's current stored offset for this upload. Mismatch returns 409 + `{"error":"offset_mismatch","expected":N}`.
4. Validate `Content-Type: application/offset+octet-stream` (per tus.io). Wrong type returns 415.
5. Append the body bytes to `.tmp-archives/<upload-id>` and update the rolling sha256 (`hash/sha256.New()` writer wrapped around the bytes-to-disk).
6. Update the server's stored offset.
7. If the new offset equals `Upload-Length`, transition to finalize (§4.6) WHILE STILL HOLDING the mutex. The finalize-step's response (204 with `Upload-Offset == Upload-Length`) is sent only after finalize completes (or fails).
8. Respond 204 + new `Upload-Offset`.

**Idle timeout.** A PATCH that does not produce body bytes for `patchIdleTimeoutSeconds` (default 300 s = 5 min) is terminated by the server with the connection closed; the upload-id remains valid and the client may resume via HEAD + PATCH. This bounds slowloris-style resource holding.

**Overshoot tolerance.** If the body bytes pushed the cumulative offset beyond `Upload-Length`, the server discards the surplus, sets the offset to `Upload-Length`, and proceeds to finalize. The client SHOULD NOT send more than `Upload-Length` total bytes, but the server tolerates a small overshoot rather than failing the upload.

**Short-body resume.** If a PATCH body terminates short of its declared `Content-Length` (client connection dropped), the server reads what arrived, updates the offset, persists state, releases the mutex, and waits for the client to resume via HEAD + a subsequent PATCH.

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

For tarball `media_type` values, the server applies an **allow-list** approach: only entries that pass every rule below are extracted. Any violation REJECTS the entire upload (delete tmp + finalize dir, return 400 + identifying detail). The rules are evaluated in the order listed; the first failing rule determines the rejection reason.

**Step 1: Resolve effective entry name (defends against pax-header overrides).** Go's `archive/tar` silently merges pax extension records (`TypeXHeader`, `TypeXGlobalHeader`) into the regular `Name` field as the reader iterates. Safety checks MUST be applied to the resolved `header.Name` AFTER pax merging, NOT to any pre-merge value. The server MUST additionally reject any pax record whose key is `path` or `linkpath` (those are the recognized pax keys that override Name and Linkname) -- pax overrides are a known tar-extraction bypass and we do not need them. Other pax keys (`mtime`, `atime`, `uid`, `gid`, `comment`) are ignored.

**Step 2: Typeflag allow-list.** The entry's `Typeflag` MUST be one of:
- `TypeReg` (regular file)
- `TypeDir` (directory)

Any other Typeflag is rejected, including: `TypeSymlink` (symlinks), `TypeLink` (hardlinks), `TypeChar` / `TypeBlock` / `TypeFifo` (device files / FIFOs), `TypeGNUSparse` (sparse files), `TypeGNULongName` / `TypeGNULongLink` (deprecated GNU extensions). Each rejection is logged with the specific Typeflag value as `entry_type` so operators can identify what showed up.

**Step 3: Name validity.** The resolved `Name` MUST:
- Be valid UTF-8. Invalid UTF-8 → reject.
- Be non-empty after trimming.
- NOT contain NUL bytes (`\x00`). NUL truncates at the kernel boundary, allowing path-traversal bypass against substring checks.
- NOT contain any C0 control character (bytes `< 0x20`) other than nothing — newline, tab, etc. are all rejected. CRLF in entry names is a vector for log-line injection downstream.
- Be ≤ 4,096 bytes (PATH_MAX on Linux).
- Have every path component ≤ 255 bytes (NAME_MAX on Linux ext4 / xfs).

**Step 4: Path safety.** The resolved `Name` MUST:
- NOT start with `/` (absolute path).
- NOT contain `:` followed by `/` (Windows drive prefix forms).
- NOT contain `..` as a path component, even if it normalizes safely (rejecting `a/../b` is intentional: simpler rule, no surprises). `..` appearing as a substring inside a filename (e.g., `my..backup.tar`) is allowed.
- NOT contain double-slashes (`//`) or trailing slash on a `TypeReg` entry.

**Step 5: Mode hygiene.** The server EXTRACTS files with mode `0600` and directories with mode `0700`, IGNORING the tar entry's mode bits entirely. The tar `Uid`/`Gid` fields are also ignored; extraction runs as the glovebox process UID. Set-uid / set-gid / sticky bits are never honored.

**Step 6: Size caps.**
- Each entry's declared `Size` (from `header.Size`) MUST be ≤ `Upload-Length`. (No single entry can exceed the whole archive.) Header-declared size is the upper bound on what `tar.Reader` will yield from the entry's `Read()` calls; this check happens at entry boundary.
- The CUMULATIVE extracted size MUST be ≤ `2 * Upload-Length`. **The check is evaluated against bytes actually written to disk, NOT against header-declared sizes**, and is enforced incrementally during the per-entry write loop: the server tracks a running `bytes_written_total` and rejects the upload the moment the next chunk would push it over the cap. (A tar can declare honest header sizes and produce a stream that yields fewer bytes per entry, never more — Go's `tar.Reader` enforces the upper bound at entry level. We check against actual writes anyway because that's the resource we actually care about: disk bytes consumed.) The factor of 2 accommodates legitimate tarballs with mild metadata overhead but rejects pathological tar-bomb inputs.
- Total entry COUNT MUST be ≤ 1,000,000. Belt-and-suspenders against entry-count attacks even when individual sizes are tiny.

**Step 7: Storage cap.** Extraction proceeds only if it won't push the per-source `archives/` usage beyond the soft cap (§5.4). The soft cap is enforced here even though §5.4 calls it "informational for the upload itself": the difference is that the OVERALL upload's `Tus-Max-Size` admission isn't blocked by soft cap, but per-entry extraction during untar IS gated to prevent a tarball from blasting past the cap entry-by-entry.

**Rejection logging.** The reject log line is structured (`glovebox archive upload tar safety reject`) with fields: `archive_id`, `source_id`, `entry_path` (truncated to 256 bytes + sanitized to `^[A-Za-z0-9._/-]+$` BEFORE logging so a malicious path can't inject into the log line), `entry_type`, `reason` (one of the step labels above: `pax_path_override`, `typeflag_disallowed`, `name_invalid_utf8`, `name_contains_nul`, `name_contains_control`, `name_too_long`, `name_traversal`, `name_absolute`, `size_too_large`, `cumulative_size_too_large`, `entry_count_too_large`, `soft_cap_exceeded`). The reason set is closed; operators dashboard against it.

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

The single committing event is the `os.Rename(".tmp-archives/<upload-id>.finalize", "archives/<archive_id>")` in §4.6 step 5. This rename is atomic ONLY on a single-filesystem POSIX volume. If `.tmp-archives/` and `archives/` are on different filesystems (`stat -c %d` returns different device IDs), `os.Rename` returns `EXDEV` and a naive fallback copy+unlink would publish a partially-formed `archives/<archive_id>/` directory to importers mid-copy.

**Startup precondition.** On boot, before binding the `/v1/archives` listener, the server `stat()`s `<staging_root>/.tmp-archives/` and `<staging_root>/archives/` and compares their device IDs (`Stat_t.Dev`). If they differ — or if either path is missing and can't be created on the same volume — the server logs ERROR `glovebox archive listener unavailable: tmp and final dirs on different filesystems` and refuses to serve `/v1/archives*` (returns 503 with `Retry-After: 60`). The process continues to serve `/v1/ingest`, `/healthz`, `/readyz`, `/metrics`. The operator must remount the staging PVC such that both subdirectories share a filesystem.

If the process dies between sha256 verification (§4.6 step 1) and the atomic rename (step 5), the `.tmp-archives/<upload-id>.finalize/` dir is left orphaned. The cleanup job (§5.5) deletes such orphans.

`metadata.json` is written to `.tmp-archives/<upload-id>.finalize/metadata.json` AFTER all `raw/` or `tree/` content is in place, so it never appears partially. Within the finalize dir, the metadata.json is the last file written and the rename publishes everything together.

**Permissions.** The server creates `.tmp-archives/` with mode `0700`, files within at mode `0600`, and the post-rename `archives/<archive_id>/` directory at mode `0700` with files at `0600`. Extracted tarball directories are also mode `0700`. This prevents another process on the same pod (debug sidecar, init container with shared volume, etc.) from reading in-flight or finalized archive content; the only reader is the glovebox process UID. Operators MUST NOT mount the staging PVC into any container that doesn't share glovebox's UID.

### 5.3 Importer Pickup

Staged archives at `<staging_root>/archives/<archive_id>/` are picked up by media-type-specific importers via an `fsnotify` watch on the `archives/` directory.

**Current state (v1).** Spec 09's mbox-importer is a CLI-invoked K8s Job (`--source <file>` flag); it does NOT today implement the watcher mode this spec depends on. The watcher mode is named V2 in spec 09 §6 and is tracked as **`glovebox-c9zt`** ("spec 09 mbox-importer: archive-event watcher mode"), which MUST land alongside or before the spec 13 implementation reaches production for any `archive/mbox` delivery to be processed automatically.

**Contract (for `glovebox-c9zt` and future importers).** The importer:

1. Watches `<staging_root>/archives/` with an `fsnotify` `IN_MOVED_TO` watch (the spec 13 finalize uses `os.Rename`, which generates `IN_MOVED_TO` on the destination directory; `IN_CREATE` covers the fallback case where an operator manually stages an archive). MUST NOT react to anything under `.tmp-archives/`.
2. On event for `archives/<archive_id>/`, waits up to 2 seconds for `metadata.json` to be readable (paranoia against observing the rename before the kernel flushes the destination-directory-entry update; in practice the rename is single-syscall and atomic but the 2-second timeout costs nothing on the happy path).
3. Reads `metadata.json`. If `media_type` does not match the importer's allow-list, ignores the archive (a different importer will handle it; the v1 allow-list for mbox-importer is `archive/mbox`).
4. Acquires an importer-specific advisory lock on `archives/<archive_id>/.importer-lock` (an existence-check + create with `O_EXCL`) so a single archive is not double-picked. Locks are per-importer; e.g., the mbox-importer's lock is `.mbox-importer.lock`. This lets multiple importers (future Takeout subtree importer alongside mbox-importer) coexist without races even when both handle the same archive — they'd be looking at different `media_type`s anyway, but the per-importer lock leaves room.
5. Processes the archive (mbox → per-message items into the connector-staging surface via the existing spec 09 pipeline; Takeout subtree → per-file items via the future Takeout importer).
6. On success, moves `archives/<archive_id>/` to `archives/.done/<archive_id>/` for retention (configurable; default 7 days then deleted by the cleanup goroutine).
7. On failure, leaves the archive in place; the importer's failure log identifies the archive_id. Manual operator intervention recovers.

The retention dir `archives/.done/` is operator-visible for forensic audit but is NOT watched by importers (no `IN_MOVED_TO` event for moves into it because `fsnotify` watches are scoped to the immediate parent directory, not subdirectories).

### 5.4 Sizing + Quota

**Default PVC size: 50 GiB.** Operator override via the Helm chart's `staging.size` value. Justification: recognizer's data PVC is 100 GiB and currently holds 4 Takeout zips + a 12 GB mbox; once those land in glovebox at archive scale the equivalent need at glovebox is roughly half (importers consume archives and shrink the on-disk footprint as they emit per-item content; the archives themselves are deleted after retention).

**Per-upload protocol cap.** `Tus-Max-Size` is advertised as 30 GiB (32,212,254,720 bytes). This is below the PVC ceiling so even an extreme single upload can't fill the volume, and well above the 12 GiB current-real-world mbox so the recognizer's expected workload fits with comfortable headroom. Operator-configurable via `ingest.archives.maxUploadSize`.

**Per-source soft cap.** Each `source_id` is given a default soft cap of 40% of total PVC capacity (= 20 GiB at default sizing). The soft cap is **enforced for per-entry untar admission** (§4.7 step 7 will reject the next tarball entry if extracting it would push the source over the soft cap) but is **NOT enforced at upload admission**: a source can have a soft-cap-tripping `POST /v1/archives` complete to the rolling-sha256 verification stage; the cap only blocks the per-entry write loop when it actually crosses the threshold. The server logs `glovebox archive storage near cap` at WARN with `source_id, used_bytes, soft_cap_bytes` when the cap is crossed, and exposes `glovebox_archive_storage_source_bytes{source_id}` as a Prometheus gauge for alerting. (Earlier draft text said the soft cap was "informational only" — that was inconsistent with §4.7's enforcement; this version is the canonical contract.)

**Global hard cap.** When `archives/` + `.tmp-archives/` COMBINED usage exceeds 95% of PVC capacity, the server returns 503 Service Unavailable on new `POST /v1/archives` with `Retry-After: 600`. Including `.tmp-archives/` in the calculation is mandatory: otherwise a malicious source-id can hold open thousands of in-flight uploads to slowly fill the PVC without ever triggering the hard cap (which would only see `archives/` after finalize). In-flight uploads in progress when the hard cap trips are allowed to complete (terminating them would lose work that's already on disk), but new POSTs are rejected. The 503 lifts when combined usage drops below 85% (hysteresis).

**Per-source concurrent-upload cap.** Each source-id may have at most 4 concurrent in-flight uploads (configurable via `ingest.archives.perSourceMaxConcurrent`). New `POST /v1/archives` from a source-id already at the cap returns 429 + `Retry-After: 60`. This bounds the slowloris vector where one client opens thousands of uploads to reserve upload-id state without sending bytes.

**Global concurrent-upload cap.** No more than 32 concurrent in-flight uploads across all source-ids (configurable). Same 429 + `Retry-After` semantics. Belt-and-suspenders against a compromised single source-id exceeding its individual cap via a bug.

**Storage measurement.** A background goroutine sums `archives/` + `.tmp-archives/` directory sizes on a 60-second interval and publishes the totals. Each subdirectory's size is attributed to a `source_id` (read from the in-memory upload-id binding for tmp dirs, from the staged `metadata.json` for finalized dirs). The measurement is cheap on a homelab-scale tree (low hundreds of subdirectories).

**Memory sizing of an in-flight upload (glovebox-g499).** The PATCH path streams (`io.Copy` over a 32 KiB buffer + rolling sha256), so the Go heap stays a flat ~4 MiB and total process anon memory ~10 MiB **independent of upload size**. The container's memory footprint is therefore almost entirely OS **page cache** from writing the archive to the staging file (cgroup memory accounting includes page cache). Profiled on the orac storage tier: a 2 GiB upload peaks at ~2.1 GiB (≈2.0 GiB cache), a 12 GiB upload at ~3.0 GiB (≈2.9 GiB cache). The cache is reclaimable, but **dirty** pages accumulate under sustained write and plateau at the kernel dirty-page ceiling (`vm.dirty_ratio`) rather than at the file size — so peak memory does **not** scale with archive size beyond that ceiling. Consequently the pod memory limit must accommodate the dirty-page working set times the expected concurrent-upload count (≈3 GiB/upload on this tier), **not** the archive size; and `GOMEMLIMIT` is ineffective for this path (it bounds the Go heap, which is ~10 MiB, not page cache). A 512 MiB limit OOM-killed a 12 GiB upload (glovebox-5ud9) because dirty pages outran writeback under the cap. An optional code-side reduction — `sync_file_range(2)` / `posix_fadvise(POSIX_FADV_DONTNEED)` during the write to bound the dirty/cached set — is tracked on glovebox-jolp.

### 5.5 Orphan Cleanup

On startup AND on a 60-minute interval, the server walks `.tmp-archives/` and deletes:

- Any `<upload-id>` file (no `.finalize` sibling) whose mtime is older than 72 hours. These are stale in-flight uploads where the client never resumed. The 72-hour threshold accommodates legitimate workstation clients that suspend a multi-GB upload overnight (laptop sleep) and resume the next day; the prior 24-hour threshold was too aggressive for that use case.
- Any `<upload-id>.finalize/` directory whose mtime is older than 1 hour. These are processes that died mid-finalize (the legitimate finalize is sub-second to seconds; an hour-old `.finalize` is wreckage).
- Any `archives/.done/<archive_id>/` directory whose mtime is older than the configured retention window (default 7 days).

The cleanup is logged at INFO with the count of items removed. The interval is configurable but the defaults are reasonable for homelab cadence.

**`Tus-Expires` header.** The server's `HEAD /v1/archives/<upload-id>` response includes a `Tus-Expires` header carrying the RFC 1123 UTC timestamp at which the upload's tmp file becomes eligible for cleanup (i.e., creation-time + 72 hours, refreshed on each successful PATCH). Clients that need to pause uploads for extended periods MUST check this header and resume before it elapses. The server also includes the same header on the `POST /v1/archives` 201 response.

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
| `glovebox_archive_concurrent_uploads_rejected_total` | counter | `source_id, scope` (`per_source`/`global`) | 429-on-concurrent-cap from §5.4 |
| `glovebox_archive_patch_idle_timeout_total` | counter | `source_id` | PATCH terminated by §4.4 idle timeout |
| `glovebox_ingest_token_load_errors_total` | counter | `source_id` | Spec 10 §4.1 per-source token-load errors (malformed Vault entry); fires on each reload attempt that skips an entry |

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

The recognizer namespace MUST be granted ingress to the glovebox ingest listener port. Chart change: add a `NetworkPolicy` rule (or extend the existing one) allowing TCP/9091 from `namespaceSelector: { matchLabels: { name: openclaw-recognizer } }` (the recognizer's namespace label).

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
    enabled: false                       # opt-in
    maxUploadSize: 32212254720           # 30 GiB (Tus-Max-Size)
    perSourceSoftCapPct: 40              # of total PVC (default 20 GiB at 50 GiB PVC)
    perSourceMaxConcurrent: 4
    globalMaxConcurrent: 32
    globalHardCapPct: 95                 # 503 threshold (archives/ + .tmp-archives/)
    globalHardCapHysteresisPct: 85       # 503-lift threshold
    patchIdleTimeoutSeconds: 300         # 5-min slowloris guard per PATCH
    cleanupIntervalSeconds: 3600
    cleanupTmpAgeHours: 72               # accommodates overnight laptop suspends
    cleanupFinalizeAgeHours: 1
    retention:
      doneArchiveDays: 7
  auth:                                  # specified by spec 10
    tokensPath: secret/glovebox/ingest-tokens
    reloadIntervalSeconds: 300
    trustedProxyCIDRs:                   # X-Forwarded-For honored ONLY from these peers (spec 10 §5.3)
      - "10.244.0.0/16"                  # placeholder: replace with the cluster's Traefik pod CIDR
    perIPRateLimit:
      window: 60s
      maxRejected: 10
      lruCapacity: 1000                  # smaller bucket to reduce eviction-bypass surface
    globalRateLimit:
      window: 60s
      maxRejected: 100                   # backstop when LRU evicts the real attacker
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
| stuck upload | Client connection lost mid-PATCH | -- | Client: HEAD to read offset, resume from there. Auto-cleanup after 72h via §5.5; HEAD response carries `Tus-Expires` so clients can detect impending cleanup |
| process dies mid-finalize | Server crash | -- | On restart, `.tmp-archives/<id>.finalize/` is orphaned; cleanup removes it; client retries with the same archive_id (the original tmp file is gone, so this is a full re-send) |

## 10. Out of Scope

- Bearer-token rotation with overlap window. v1 requires a coordinated rotation; spec 10 §11 documents the future schema extension.
- Auth on `/v1/ingest`. Existing connector-scale path keeps its no-auth behavior; future spec extends spec 10 to cover it.
- Multi-tenant token scoping (per-source allowed media_types). All authenticated source-ids can upload any allowed media_type. Adding per-token scope is a spec 10 future extension.
- Streaming untar during PATCH. v1 untars at finalize after the full file is on disk. The few seconds of extra I/O is acceptable for homelab cadence.
- Server-side compression. Tarballs MUST be uncompressed tar (no .tar.gz, no .tar.zst) in v1. The recognizer's pipeline produces uncompressed tar artifacts; if compressed support is wanted later, it lands as a media-type-aware decompression in the untar dispatch and MUST include a decompression-bomb defense: `Tus-Max-Size` is enforced against the COMPRESSED upload size, but the decompressed extract size MUST be additionally capped (`decompressed_bytes <= 4 * Tus-Max-Size` is a reasonable bound) so a small compressed input cannot expand into PVC exhaustion. Per spec §4.7 step 6 (cumulative size cap) provides a similar guardrail for uncompressed tarballs; the compressed case needs its own variant of the same defense.
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
  - C2: Integration test: a tus client uploads a 50 MB mbox + a 5-file tarball; both stage correctly; corrupted sha256 rejected; tar-safety violation rejected. The 12 GiB end-to-end smoke test (bead acceptance criterion) is exercised in Wave D2 against the built container image, not in Go-level integration tests, because a 12 GiB tmpfile in `go test` is impractical.

- **Wave D (solo)** -- ops:
  - D1: Helm chart updates (PVC 50 GiB default, NetworkPolicy for recognizer ingress, Vault ClusterSecretStore reference for `ingest-tokens/`).
  - D2: Smoke test script. Drives a single tus.io upload against the built container image: 12 GiB synthetic mbox content (zeros are fine; we verify sha256 + finalize + the staged `metadata.json`, not content). Bead glovebox-p1zx's acceptance criterion ("12GB mbox delivers without 413") lives here.

Each wave follows the established spec-12 review pattern: implementer + 2 reviewers per task, single bundled cleanup commit. **A dedicated security review of the merged result is required** before declaring this work shippable, given the attack surface (multi-GB external-facing endpoint, tar extraction, token validation).

## 12. Related Specs

- **Spec 06 (Connector Auth and Provenance)** -- the Identity block, audit log, and `delivered_by` provenance pattern this spec emits into. §6 of this spec defers to spec 10 §6 for the integration details.
- **Spec 08 (HTTP Ingest API)** -- the existing `/v1/ingest` endpoint. This spec adds a sibling endpoint surface without modifying spec 08.
- **Spec 09 (Mbox Importer)** -- the first downstream consumer of an archive staged by this endpoint. The mbox-importer's `fsnotify` watch on `archives/` is the contract this spec satisfies.
- **Spec 10 (External Ingest Auth)** -- promoted from stub concurrently with this spec. Defines the bearer-token model §6 defers to.
- **Spec 11 (Data Subject and Audience)** -- not directly invoked. The staged archive's per-item metadata is the importer's concern, not this endpoint's.
