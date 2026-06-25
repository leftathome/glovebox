# mbox importer: archive-event watcher mode — design

**Bead:** `glovebox-c9zt`
**Date:** 2026-06-25
**Status:** approved (pending spec review)
**Implements:** spec 13 (`docs/specs/13-archive-delivery-design.md`) §5.3 importer-pickup
contract; promotes spec 09 (`docs/specs/09-mbox-importer-design.md`) §6 deferred
"archive-event listener mode" to V2.

## 1. Problem

Spec 13's archive-delivery endpoint stages finished, multi-GB mbox archives at
`<staging_root>/archives/<archive_id>/` via a single atomic `os.Rename` from
`.tmp-archives/<upload-id>.finalize`. The on-disk shape is:

```
archives/<archive_id>/
  metadata.json        (the §4.8 FinalizeReceipt: archive_id, media_type, raw_filename, ...)
  raw/<raw_filename>   (the mbox file itself, for raw-file media types)
```

The spec 09 mbox-importer today is a **one-shot, CLI-invoked K8s Job**
(`--source <file>`). It cannot pick up a staged archive without an operator
manually creating a Job per archive. Spec 13 §5.3 explicitly names this gap and
makes the watcher mode (this bead, `glovebox-c9zt`) a precondition for any
`archive/mbox` delivery to be processed automatically.

## 2. Goals / non-goals

**Goals**

- A long-running watcher mode on the existing `mbox-importer` binary that picks
  up `archive/mbox` archives as they are finalized into `archives/`, drives the
  existing per-mbox import pipeline against each, and retires processed archives
  to `archives/.done/<archive_id>/`.
- Honor the spec 13 §5.3 contract steps 1–7 (watch, readiness wait, media-type
  filter, per-importer advisory lock, process, move-to-done on success, leave in
  place on failure).
- Reuse existing building blocks: `internal/watcher` and `importer.RunOneShot`.

**Non-goals (explicit, with rationale)**

- **The Helm Deployment workload manifest.** Wiring a long-running Deployment
  (vs. the current Job) and its probes/resources is an operator/chart change,
  tracked as a follow-up sibling to `glovebox-3d4m`. This bead delivers the
  binary capability + tests.
- **Concurrent multi-archive processing.** v1 processes archives sequentially
  (the `internal/watcher` feeder is a single goroutine). At homelab scale
  (low-hundreds of archives, processed in minutes each) this is acceptable; a
  worker pool is a future optimization.
- **Enrichment wire-format changes** (`glovebox-afq4.12`) and **`.done`
  retention cleanup** (owned by the spec 13 server cleanup goroutine, §5.5).
- **`archive/imap-export` handling.** The media-type set is operator-configurable
  (see §4) so an operator can opt in without a code change, but the default is
  `archive/mbox` only.

## 3. Architecture

A single new file, `importers/mbox/watch.go` (`package main`), holds the
watcher-mode runtime. `importers/mbox/main.go`'s `runCtx` gains two flags and a
branch; everything else (framework bootstrap, the one-shot `--source` path,
`RunOneShot`, the parser/pipeline) is unchanged.

```
runCtx(--watch-archives <dir> [--media-types csv])
  └─ runWatch(ctx, fw, imp, archivesDir, mediaTypes)
       └─ watcher.New(archivesDir, pollInterval, archiveHandler).Run(ctx)
            └─ archiveHandler(dir)            // per archives/<archive_id>/
                 1. read metadata.json (minimal local struct)
                 2. media_type filter
                 3. O_EXCL lock .mbox-importer.lock
                 4. RunOneShot(SourcePath = dir/raw/<raw_filename>)
                 5. success → rename dir → archives/.done/<archive_id>/
                 6. failure → unlock, log, leave in place
```

### 3.1 Flags and dispatch (`main.go`)

- `--watch-archives <dir>` — when non-empty, run watcher mode against `<dir>`
  (the `archives/` directory). Mutually exclusive with `--source`; supplying
  both is a usage error (exit 2). Supplying neither is the existing
  "`--source` required" error.
- `--media-types <csv>` — comma-separated allow-list of `media_type` values the
  watcher claims. Default `archive/mbox`. Parsed into a `map[string]bool`.
- `--poll-interval <dur>` — periodic poll cadence backing the fsnotify watch
  (default reuses the existing watcher default; surfaced for tuning/tests).

When `--watch-archives` is set, `runCtx` bootstraps the framework exactly as the
one-shot path does (same logger / staging backend / matcher / health server /
metrics), constructs the `mboxImporter`, and calls `runWatch` instead of
`importer.RunOneShot`. The health server staying up is what makes the process a
viable long-lived Deployment with liveness/readiness probes.

### 3.2 `runWatch` (`watch.go`)

```
func runWatch(ctx, fw, imp, archivesDir string, mediaTypes map[string]bool, poll time.Duration) error
```

Constructs `watcher.New(archivesDir, poll, handler)` where `handler` closes over
`ctx`, `fw`, `imp`, `mediaTypes`. Calls `w.Run(ctx)` (blocks until ctx
cancellation). Returns nil on clean shutdown.

### 3.3 `archiveHandler` (`watch.go`)

Signature adapts to `watcher.ItemHandler` (`func(dirPath string)`). Steps, mapping
to spec 13 §5.3:

1. **Read metadata** (§5.3.2–3). `os.ReadFile(filepath.Join(dir, "metadata.json"))`
   → `json.Unmarshal` into a minimal local struct:

   ```go
   type archiveMeta struct {
       ArchiveID   string `json:"archive_id"`
       MediaType   string `json:"media_type"`
       RawFilename string `json:"raw_filename"`
   }
   ```

   This is a deliberate decoupling: the importer does not import
   `internal/ingest/archives` (which would pull in server-side deps). The three
   fields are a stable subset of `archives.FinalizeReceipt`. Unreadable or
   malformed metadata → log WARN with `dir`, return (leave in place; operator
   recovers). The watcher's readiness gate already guarantees the file exists
   before dispatch.

2. **Media-type filter** (§5.3.3). If `meta.MediaType` not in `mediaTypes`,
   increment `_skipped_total{reason="media_type"}` at debug and return. A
   different importer owns this archive.

3. **Advisory lock** (§5.3.4). `os.OpenFile(filepath.Join(dir, lockName),
   O_CREATE|O_EXCL|O_WRONLY, 0o600)`; `lockName = ".mbox-importer.lock"`. On
   `os.IsExist(err)` → another replica/importer owns it; increment
   `_skipped_total{reason="locked"}` and return. On other error → log ERROR,
   return. On success, the file handle is closed immediately (existence is the
   lock); a `defer`/explicit removal governs release per the outcome below.

4. **Process** (§5.3.5). `rawFilename` is validated to be a single path element
   (no separators, not `.`/`..`) before use — defense in depth even though the
   server already validated it at finalize. `sourcePath =
   filepath.Join(dir, "raw", meta.RawFilename)`. Build `importer.RunConfig{
   SourcePath: sourcePath}` and call `importer.RunOneShot(ctx, fw, imp, cfg)`.
   Survey/manifest/checkpoint sidecars derive from `sourcePath`, so they land in
   `dir/raw/` and travel with the archive into `.done`.

5. **Success** (§5.3.6). Ensure `archives/.done/` exists (`os.MkdirAll`, 0o700).
   `os.Rename(dir, filepath.Join(archivesDir, ".done", meta.ArchiveID))`.
   Increment `_processed_total`. The lock file moves with the directory (it is
   now under `.done`, harmless). If the rename fails, log ERROR, remove the lock,
   leave in place.

6. **Failure** (§5.3.7). `RunOneShot` returned a non-nil error and `ctx.Err()`
   is nil: remove `.mbox-importer.lock`, increment `_failed_total`, log ERROR
   with `archive_id`. Leave the archive in place. The watcher has already marked
   the path `seen`, so the running process will not re-pick it (no hot retry
   loop); a pod restart starts with a fresh `seen` map and the lock gone, so the
   archive is retried then. This realizes the agreed "release lock, mark seen"
   behavior.

7. **Shutdown mid-run.** `ctx` cancelled during `RunOneShot`: `RunOneShot`
   returns a context error and leaves `manifest.status=interrupted` +
   checkpoint. The handler removes the lock and leaves the archive in place
   (does NOT move to `.done`). The next pod re-picks it and `RunOneShot` resumes
   from the persisted byte offset.

### 3.4 Why `internal/watcher` needs no change

- It is pointed at `archivesDir` only; `.tmp-archives/` is a sibling and is
  never watched (§5.3.1 "MUST NOT react to anything under `.tmp-archives/`").
- `archives/.done/` is a child dir, but the watcher only dispatches a `<entry>/`
  whose `<entry>/metadata.json` exists. `.done/metadata.json` does not exist
  (receipts live one level deeper at `.done/<id>/metadata.json`), so `.done` is
  never dispatched. The handler additionally skips any base name starting with
  `.` as defense in depth.
- The metadata.json readiness gate (`dispatchIfNew`) already implements §5.3.2's
  "wait until metadata.json is readable" via leave-unseen-and-retry-on-poll,
  which is equivalent to (and strictly more robust than) a fixed 2-second wait.
- fsnotify maps a rename-into-watched-dir (`IN_MOVED_TO`, the spec 13 finalize
  signal) to `fsnotify.Create`, which the watcher already handles, alongside the
  `IN_CREATE` operator-manual-stage fallback.

## 4. Configuration summary

| Flag | Default | Purpose |
|------|---------|---------|
| `--watch-archives <dir>` | `""` (one-shot mode) | Enable watcher mode against the `archives/` dir |
| `--media-types <csv>` | `archive/mbox` | media_type allow-list the watcher claims |
| `--poll-interval <dur>` | watcher default | Periodic poll cadence behind fsnotify |

`--source` and `--watch-archives` are mutually exclusive.

## 5. Observability

New Prometheus counters on the framework registry:

- `glovebox_mbox_archives_processed_total` — archives moved to `.done`.
- `glovebox_mbox_archives_failed_total` — processing failures (left in place).
- `glovebox_mbox_archives_skipped_total{reason}` — `reason ∈ {media_type, locked}`.

Per-archive INFO logs on pickup (`archive_id`, `media_type`) and outcome.

## 6. Error handling

| Condition | Handling |
|-----------|----------|
| metadata.json missing/unreadable/malformed | WARN, skip, leave in place |
| media_type not claimed | debug, skip (another importer owns it) |
| lock already held | skip (`_skipped_total{locked}`) |
| `raw_filename` not a single safe path element | ERROR, skip |
| `RunOneShot` error (ctx live) | unlock, ERROR, leave in place (`_failed_total`) |
| ctx cancelled mid-run | unlock, leave in place (manifest=interrupted; resumes) |
| rename to `.done` fails | ERROR, unlock, leave in place |

## 7. Testing

All tests are in-process (no container) against the existing e2e mock ingest
backend harness (`importers/mbox/e2e_test.go`'s mock server + fixture builders).

**Handler unit tests** (`watch_test.go`), each over a temp `archives/` tree with
a synthesized `<id>/metadata.json` + `raw/<file>.mbox`:

1. **Happy path** — `archive/mbox` archive → items delivered to the mock
   backend, `<id>` moved to `.done/<id>`, original path gone.
2. **Media-type mismatch** — `metadata.json` with `media_type: archive/pst` →
   no delivery, archive untouched, `_skipped_total{media_type}` incremented.
3. **Lock contention** — pre-create `.mbox-importer.lock` → handler skips, no
   delivery, archive untouched.
4. **Processing failure** — induce a `RunOneShot` failure (e.g. a backend that
   returns 5xx, or a `raw/` file referenced by metadata that is absent) →
   `.mbox-importer.lock` removed, archive left in place, `_failed_total`
   incremented.
5. **Malformed metadata** — non-JSON `metadata.json` → skip, archive untouched.

**Integration test** (`watch_test.go` or `e2e_test.go`) — drive `runCtx` with
`--watch-archives <dir>` in a goroutine, stage one `archive/mbox` archive,
assert end-to-end delivery + move-to-`.done`, then cancel ctx and assert clean
exit (code 0).

`go test ./importers/mbox/ -race`, `go vet`, and `staticcheck` are part of the
feedback loop; `gofmt` clean on touched files.

## 8. Rollout / follow-ups

- **Chart Deployment workload** (follow-up bead): a long-running Deployment
  running `mbox-importer --watch-archives /staging/archives` with the staging
  PVC mounted, liveness/readiness on the health port, and resource requests
  sized for multi-GB mbox parsing. Sibling to `glovebox-3d4m`.
- **Worker pool for concurrent archives** (future): replace the single-feeder
  sequential processing if archive arrival outpaces per-archive processing.
