# mbox archive-event watcher mode — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a long-running `--watch-archives` mode to the `mbox-importer` binary that picks up `archive/mbox` archives finalized into `staging/archives/` and drives the existing import pipeline against each, retiring processed archives to `archives/.done/`.

**Architecture:** A new `importers/mbox/watch.go` (package `main`) holds the watcher runtime: `runWatch` wires the existing `internal/watcher` (fsnotify + polling fallback + metadata.json readiness gate) to a per-archive `archiveHandler` that reads `metadata.json`, filters by `media_type`, takes an `O_EXCL` advisory lock, calls `importer.RunOneShot` against `raw/<filename>`, then moves the archive to `archives/.done/` on success (leaves it in place + releases the lock on failure). `main.go` gains the flags and a dispatch branch. Nothing in the existing one-shot path, parser, or pipeline changes.

**Tech Stack:** Go (`go test`, `go vet`, `staticcheck`); `github.com/fsnotify/fsnotify` via `internal/watcher`; OTel metrics via `connector.Metrics`; existing `importer.RunOneShot` pipeline.

**Spec:** `docs/superpowers/specs/2026-06-25-mbox-archive-watcher-design.md`

---

## File Structure

- **Create** `importers/mbox/watch.go` — watcher runtime: `runWatch`, `archiveHandler`, the `archiveMeta` struct + `readArchiveMeta`, `parseMediaTypes`, `safeRawFilename`, lock acquire/release helpers, `moveToDone`, and the `archiveMetrics` counters.
- **Create** `importers/mbox/watch_test.go` — unit tests for the pure helpers and an end-to-end integration test driving `runCtx --watch-archives` against the e2e mock ingest backend.
- **Modify** `importers/mbox/main.go` — add `--watch-archives`, `--media-types`, `--poll-interval` flags; enforce `--source` XOR `--watch-archives`; branch to `runWatch`.
- **Modify** `connector/metrics.go` — add a `Provider()` accessor (mirrors `internal/metrics.Provider()`) so the importer can register its own meter against the same Prometheus exporter.

**Reused unchanged:** `internal/watcher` (the `.done` dir is never dispatched because `archives/.done/metadata.json` does not exist; receipts live at `.done/<id>/metadata.json`), `importer.RunOneShot`, the mbox parser/pipeline, and the e2e mock harness (`ingestMock`, `newIngestMock`, `freePort`, `writeMbox`, `writeSupportFiles`).

**Conventions:** No emoji in any Go source or strings. Stage only the task's files when committing (the tree may carry unrelated WIP). Delivery is via GitLab branch + MR (see final task).

---

## Task 1: `Provider()` accessor on `connector.Metrics`

Enables the importer to attach its own OTel meter to the framework's existing Prometheus exporter (avoids a second `prometheus.New()` that would double-register on the default registry).

**Files:**
- Modify: `connector/metrics.go`
- Test: `connector/metrics_test.go` (create if absent)

- [ ] **Step 1: Write the failing test**

```go
package connector

import "testing"

func TestMetrics_Provider_NonNil(t *testing.T) {
	m, err := NewMetrics("test-connector")
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	t.Cleanup(func() { _ = m.Shutdown() })
	if m.Provider() == nil {
		t.Fatal("Provider() = nil, want the underlying MeterProvider")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./connector/ -run TestMetrics_Provider_NonNil`
Expected: build failure — `m.Provider undefined`.

- [ ] **Step 3: Add the accessor**

In `connector/metrics.go`, after the `Handler()` method, add:

```go
// Provider returns the underlying MeterProvider so importer-specific
// subsystems (e.g. the mbox archive watcher) can register their own meter
// against the same Prometheus exporter rather than creating a second one.
func (m *Metrics) Provider() *sdkmetric.MeterProvider {
	return m.provider
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./connector/ -run TestMetrics_Provider_NonNil`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add connector/metrics.go connector/metrics_test.go
git commit -m "feat(connector): expose Metrics.Provider() accessor (glovebox-c9zt)"
```

---

## Task 2: `parseMediaTypes` helper

Parses the `--media-types` CSV into a lookup set; defaults to `archive/mbox` when empty.

**Files:**
- Create: `importers/mbox/watch.go`
- Test: `importers/mbox/watch_test.go`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"reflect"
	"testing"
)

func TestParseMediaTypes(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]bool
	}{
		{"", map[string]bool{"archive/mbox": true}},
		{"archive/mbox", map[string]bool{"archive/mbox": true}},
		{" archive/mbox , archive/imap-export ", map[string]bool{"archive/mbox": true, "archive/imap-export": true}},
		{"archive/mbox,,archive/mbox", map[string]bool{"archive/mbox": true}},
	}
	for _, c := range cases {
		got := parseMediaTypes(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseMediaTypes(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./importers/mbox/ -run TestParseMediaTypes`
Expected: build failure — `parseMediaTypes` undefined.

- [ ] **Step 3: Implement in `watch.go`**

Create `importers/mbox/watch.go` with the package clause, imports, and:

```go
// defaultMediaType is the media_type the watcher claims when --media-types
// is not supplied. archive/imap-export shares the raw mbox shape and can be
// added by an operator via the flag without a code change (spec 13 sec 4.5).
const defaultMediaType = "archive/mbox"

// parseMediaTypes turns a comma-separated --media-types value into a lookup
// set, trimming whitespace and dropping empty entries. An empty input yields
// the default set {archive/mbox}.
func parseMediaTypes(csv string) map[string]bool {
	out := make(map[string]bool)
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	if len(out) == 0 {
		out[defaultMediaType] = true
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./importers/mbox/ -run TestParseMediaTypes`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add importers/mbox/watch.go importers/mbox/watch_test.go
git commit -m "feat(mbox): parseMediaTypes helper for watcher mode (glovebox-c9zt)"
```

---

## Task 3: `safeRawFilename` validation

Rejects a `raw_filename` that is not a single safe path element (defense in depth even though the server validated it at finalize).

**Files:**
- Modify: `importers/mbox/watch.go`
- Test: `importers/mbox/watch_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestSafeRawFilename(t *testing.T) {
	ok := []string{"archive.mbox", "All mail Including Spam and Trash.mbox", "a_b-c.mbox"}
	bad := []string{"", ".", "..", "sub/archive.mbox", "/etc/passwd", "a\x00b", "..\\x"}
	for _, s := range ok {
		if err := safeRawFilename(s); err != nil {
			t.Errorf("safeRawFilename(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range bad {
		if err := safeRawFilename(s); err == nil {
			t.Errorf("safeRawFilename(%q) = nil, want error", s)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./importers/mbox/ -run TestSafeRawFilename`
Expected: build failure — `safeRawFilename` undefined.

- [ ] **Step 3: Implement in `watch.go`**

```go
// safeRawFilename verifies name is a single, safe path element: non-empty,
// not "." or "..", containing no path separators or NUL. The server already
// validates archive_filename at finalize (spec 13 sec 4.2); this is a second
// gate before we join it onto a filesystem path.
func safeRawFilename(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("raw_filename %q is not a valid file name", name)
	}
	if strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("raw_filename %q contains a path separator or NUL", name)
	}
	if filepath.Base(name) != name {
		return fmt.Errorf("raw_filename %q is not a single path element", name)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./importers/mbox/ -run TestSafeRawFilename`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add importers/mbox/watch.go importers/mbox/watch_test.go
git commit -m "feat(mbox): safeRawFilename guard for watcher mode (glovebox-c9zt)"
```

---

## Task 4: `readArchiveMeta` — parse the archive's metadata.json

Reads the three fields the watcher needs from `<dir>/metadata.json`, decoupled from `internal/ingest/archives`.

**Files:**
- Modify: `importers/mbox/watch.go`
- Test: `importers/mbox/watch_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestReadArchiveMeta(t *testing.T) {
	dir := t.TempDir()
	good := `{"archive_id":"abc-123","media_type":"archive/mbox","raw_filename":"all.mbox","sha256":"deadbeef"}`
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := readArchiveMeta(dir)
	if err != nil {
		t.Fatalf("readArchiveMeta: %v", err)
	}
	if m.ArchiveID != "abc-123" || m.MediaType != "archive/mbox" || m.RawFilename != "all.mbox" {
		t.Errorf("got %+v", m)
	}

	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "metadata.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readArchiveMeta(bad); err == nil {
		t.Error("readArchiveMeta on malformed json = nil error, want error")
	}

	if _, err := readArchiveMeta(t.TempDir()); err == nil {
		t.Error("readArchiveMeta on missing file = nil error, want error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./importers/mbox/ -run TestReadArchiveMeta`
Expected: build failure — `readArchiveMeta` / `archiveMeta` undefined.

- [ ] **Step 3: Implement in `watch.go`**

```go
// archiveMeta is the minimal subset of the spec 13 sec 4.8 FinalizeReceipt
// (written to each archive's metadata.json) that the watcher consults. We do
// NOT import internal/ingest/archives.FinalizeReceipt: that would pull
// server-side dependencies into the importer binary. These field names are a
// stable subset of that struct's JSON.
type archiveMeta struct {
	ArchiveID   string `json:"archive_id"`
	MediaType   string `json:"media_type"`
	RawFilename string `json:"raw_filename"`
}

// readArchiveMeta loads and parses <dir>/metadata.json.
func readArchiveMeta(dir string) (archiveMeta, error) {
	var m archiveMeta
	b, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		return m, fmt.Errorf("read metadata.json: %w", err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("parse metadata.json: %w", err)
	}
	return m, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./importers/mbox/ -run TestReadArchiveMeta`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add importers/mbox/watch.go importers/mbox/watch_test.go
git commit -m "feat(mbox): readArchiveMeta parses archive metadata.json (glovebox-c9zt)"
```

---

## Task 5: lock + move-to-done helpers

`acquireLock` (O_EXCL create), `releaseLock` (remove), and `moveToDone` (rename into `archives/.done/<id>`).

**Files:**
- Modify: `importers/mbox/watch.go`
- Test: `importers/mbox/watch_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestAcquireReleaseLock(t *testing.T) {
	dir := t.TempDir()
	if err := acquireLock(dir); err != nil {
		t.Fatalf("first acquireLock: %v", err)
	}
	if err := acquireLock(dir); err == nil {
		t.Error("second acquireLock = nil, want already-held error")
	} else if !os.IsExist(err) {
		t.Errorf("second acquireLock err = %v, want os.IsExist", err)
	}
	if err := releaseLock(dir); err != nil {
		t.Fatalf("releaseLock: %v", err)
	}
	if err := acquireLock(dir); err != nil {
		t.Fatalf("acquireLock after release: %v", err)
	}
}

func TestMoveToDone(t *testing.T) {
	archives := t.TempDir()
	src := filepath.Join(archives, "arch-1")
	if err := os.MkdirAll(filepath.Join(src, "raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := moveToDone(archives, src, "arch-1"); err != nil {
		t.Fatalf("moveToDone: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source dir still exists after moveToDone")
	}
	if _, err := os.Stat(filepath.Join(archives, ".done", "arch-1", "raw")); err != nil {
		t.Errorf(".done/arch-1/raw missing: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./importers/mbox/ -run 'TestAcquireReleaseLock|TestMoveToDone'`
Expected: build failure — helpers undefined.

- [ ] **Step 3: Implement in `watch.go`**

```go
// lockName is the mbox-importer's per-importer advisory lock file inside an
// archive dir (spec 13 sec 5.3 step 4). Per-importer naming lets a future
// Takeout importer coexist on the same archive tree without lock races.
const lockName = ".mbox-importer.lock"

// doneDir is the retention subdirectory processed archives are moved into.
const doneDir = ".done"

// acquireLock creates the advisory lock with O_EXCL. A returned error for
// which os.IsExist is true means another importer/replica owns the archive.
func acquireLock(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// releaseLock removes the advisory lock. Missing-file is not an error.
func releaseLock(dir string) error {
	if err := os.Remove(filepath.Join(dir, lockName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// moveToDone retires a processed archive into archives/.done/<id> for
// retention (the spec 13 server cleanup goroutine deletes it after the
// retention window). archives/.done/ is not watched, so no re-pickup occurs.
func moveToDone(archivesDir, src, archiveID string) error {
	done := filepath.Join(archivesDir, doneDir)
	if err := os.MkdirAll(done, 0o700); err != nil {
		return fmt.Errorf("mkdir .done: %w", err)
	}
	if err := os.Rename(src, filepath.Join(done, archiveID)); err != nil {
		return fmt.Errorf("move to .done: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./importers/mbox/ -run 'TestAcquireReleaseLock|TestMoveToDone'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add importers/mbox/watch.go importers/mbox/watch_test.go
git commit -m "feat(mbox): lock + move-to-done helpers for watcher mode (glovebox-c9zt)"
```

---

## Task 6: `archiveMetrics` counters

Three OTel counters registered against the framework's exporter via `fw.Metrics.Provider()`.

**Files:**
- Modify: `importers/mbox/watch.go`
- Test: `importers/mbox/watch_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestNewArchiveMetrics(t *testing.T) {
	mp := otelnoop.NewMeterProvider()
	am, err := newArchiveMetrics(mp)
	if err != nil {
		t.Fatalf("newArchiveMetrics: %v", err)
	}
	// Recording must not panic with a real-but-noop provider.
	am.processed(context.Background())
	am.failed(context.Background())
	am.skipped(context.Background(), "media_type")
	am.skipped(context.Background(), "locked")
}
```

Add the import `otelnoop "go.opentelemetry.io/otel/metric/noop"` to the test file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./importers/mbox/ -run TestNewArchiveMetrics`
Expected: build failure — `newArchiveMetrics` undefined.

- [ ] **Step 3: Implement in `watch.go`**

`newArchiveMetrics` takes a `metric.MeterProvider` (interface) so the test can pass a noop provider and production can pass `fw.Metrics.Provider()`.

```go
// archiveMetrics holds the watcher's OTel counters. Registered against the
// framework's existing Prometheus exporter via fw.Metrics.Provider().
type archiveMetrics struct {
	processedTotal metric.Int64Counter
	failedTotal    metric.Int64Counter
	skippedTotal   metric.Int64Counter
}

func newArchiveMetrics(mp metric.MeterProvider) (*archiveMetrics, error) {
	meter := mp.Meter("mbox-importer")
	am := &archiveMetrics{}
	var err error
	if am.processedTotal, err = meter.Int64Counter("glovebox_mbox_archives_processed_total",
		metric.WithDescription("Archives processed and moved to .done")); err != nil {
		return nil, err
	}
	if am.failedTotal, err = meter.Int64Counter("glovebox_mbox_archives_failed_total",
		metric.WithDescription("Archive processing failures (left in place)")); err != nil {
		return nil, err
	}
	if am.skippedTotal, err = meter.Int64Counter("glovebox_mbox_archives_skipped_total",
		metric.WithDescription("Archives skipped, by reason")); err != nil {
		return nil, err
	}
	return am, nil
}

func (a *archiveMetrics) processed(ctx context.Context) { a.processedTotal.Add(ctx, 1) }
func (a *archiveMetrics) failed(ctx context.Context)    { a.failedTotal.Add(ctx, 1) }
func (a *archiveMetrics) skipped(ctx context.Context, reason string) {
	a.skippedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}
```

Imports needed in `watch.go`: `context`, `go.opentelemetry.io/otel/attribute`, `go.opentelemetry.io/otel/metric`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./importers/mbox/ -run TestNewArchiveMetrics`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add importers/mbox/watch.go importers/mbox/watch_test.go
git commit -m "feat(mbox): archive watcher OTel counters (glovebox-c9zt)"
```

---

## Task 7: `archiveHandler` + `runWatch` wiring

The per-archive handler and the watcher loop. Covered end-to-end by the integration test in Task 8; this task adds the code and confirms the build + existing tests stay green.

**Files:**
- Modify: `importers/mbox/watch.go`

- [ ] **Step 1: Implement `archiveHandler`**

A constructor closes over the dependencies and returns a `watcher.ItemHandler`:

```go
// archiveWatcher bundles the per-archive handler dependencies.
type archiveWatcher struct {
	ctx         context.Context
	fw          *connector.Framework
	imp         *mboxImporter
	archivesDir string
	mediaTypes  map[string]bool
	metrics     *archiveMetrics
}

// handle implements watcher.ItemHandler for one archives/<id>/ directory.
func (a *archiveWatcher) handle(dir string) {
	log := a.fw.Logger.With("archive_dir", dir)

	// Defense in depth: never act on a dot-named entry (.done, .tmp-*).
	if strings.HasPrefix(filepath.Base(dir), ".") {
		return
	}

	meta, err := readArchiveMeta(dir)
	if err != nil {
		log.Warn("skip archive: unreadable metadata.json", "err", err)
		return
	}
	log = log.With("archive_id", meta.ArchiveID, "media_type", meta.MediaType)

	if !a.mediaTypes[meta.MediaType] {
		log.Debug("skip archive: media_type not claimed")
		a.metrics.skipped(a.ctx, "media_type")
		return
	}

	if err := safeRawFilename(meta.RawFilename); err != nil {
		log.Error("skip archive: invalid raw_filename", "err", err)
		return
	}

	if err := acquireLock(dir); err != nil {
		if os.IsExist(err) {
			log.Debug("skip archive: lock held by another importer")
			a.metrics.skipped(a.ctx, "locked")
			return
		}
		log.Error("skip archive: lock error", "err", err)
		return
	}

	log.Info("processing archive")
	source := filepath.Join(dir, "raw", meta.RawFilename)
	cfg := importer.RunConfig{SourcePath: source}
	runErr := importer.RunOneShot(a.ctx, a.fw, a.imp, cfg)

	if runErr != nil {
		// Cancellation is a clean shutdown: RunOneShot left
		// manifest=interrupted; release the lock so the next pod resumes,
		// leave the archive in place. Do NOT count it as a failure.
		if a.ctx.Err() != nil {
			_ = releaseLock(dir)
			log.Warn("archive processing interrupted; left in place for resume", "err", runErr)
			return
		}
		_ = releaseLock(dir)
		a.metrics.failed(a.ctx)
		log.Error("archive processing failed; left in place", "err", runErr)
		return
	}

	if err := moveToDone(a.archivesDir, dir, meta.ArchiveID); err != nil {
		_ = releaseLock(dir)
		a.metrics.failed(a.ctx)
		log.Error("archive processed but move to .done failed; left in place", "err", err)
		return
	}
	a.metrics.processed(a.ctx)
	log.Info("archive processed and retired to .done")
}
```

- [ ] **Step 2: Implement `runWatch`**

```go
// defaultWatchPollInterval backs the fsnotify watch with a periodic poll.
// Archives are large and arrive infrequently, so a slow poll is plenty; it
// only needs to catch events fsnotify missed and retry backpressured items.
const defaultWatchPollInterval = 30 * time.Second

// runWatch runs the long-lived archive watcher until ctx is cancelled. It
// ensures archivesDir exists (the importer pod may boot before the ingest
// server creates it) so the fsnotify watch attaches immediately instead of
// falling back to polling.
func runWatch(ctx context.Context, fw *connector.Framework, imp *mboxImporter,
	archivesDir string, mediaTypes map[string]bool, poll time.Duration) error {

	if err := os.MkdirAll(archivesDir, 0o700); err != nil {
		return fmt.Errorf("ensure archives dir %s: %w", archivesDir, err)
	}
	am, err := newArchiveMetrics(fw.Metrics.Provider())
	if err != nil {
		return fmt.Errorf("init archive metrics: %w", err)
	}
	aw := &archiveWatcher{
		ctx: ctx, fw: fw, imp: imp,
		archivesDir: archivesDir, mediaTypes: mediaTypes, metrics: am,
	}
	fw.Logger.Info("archive watcher started", "archives_dir", archivesDir,
		"media_types", mediaTypeKeys(mediaTypes), "poll", poll.String())
	watcher.New(archivesDir, poll, aw.handle).Run(ctx)
	return nil
}

// mediaTypeKeys returns the claimed media types as a sorted slice for logging.
func mediaTypeKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
```

Add imports: `sort`, `time`, `github.com/leftathome/glovebox/connector`, `github.com/leftathome/glovebox/importer`, `github.com/leftathome/glovebox/internal/watcher`.

- [ ] **Step 3: Verify the build and existing tests stay green**

Run: `go build ./... && go test ./importers/mbox/`
Expected: build OK; all existing mbox tests PASS (no behavior change to the one-shot path yet — `runWatch` is not yet wired into `main.go`). Note: Go does not fail the build on unused package-level functions/types, so `runWatch`/`archiveWatcher` being uncalled here is fine. The build risk to watch for is an *unused import*; every import this task adds (`connector`, `importer`, `internal/watcher`, `sort`, `time`, plus `context`/`encoding/json`/`os`/`path/filepath`/`strings`/`fmt` from earlier tasks and the OTel packages) is referenced in the bodies above, so the build is clean.

- [ ] **Step 4: Commit**

```bash
git add importers/mbox/watch.go
git commit -m "feat(mbox): archiveHandler + runWatch watcher runtime (glovebox-c9zt)"
```

---

## Task 8: integration test — `runCtx --watch-archives` end to end

Drives the watcher through a real (in-process) archive pickup against the e2e mock ingest backend.

**Files:**
- Modify: `importers/mbox/watch_test.go`

- [ ] **Step 1: Write the failing test**

Add a helper that stages an archive on disk in the spec 13 layout, then the test:

```go
// stageArchive writes archives/<id>/{metadata.json, raw/<filename>} using the
// given mbox bytes, mirroring the spec 13 finalize layout.
func stageArchive(t *testing.T, archivesDir, id, mediaType, rawFilename, mboxPath string) {
	t.Helper()
	dir := filepath.Join(archivesDir, id)
	if err := os.MkdirAll(filepath.Join(dir, "raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mboxPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "raw", rawFilename), data, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"archive_id":%q,"media_type":%q,"raw_filename":%q}`, id, mediaType, rawFilename)
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestE2E_WatchArchives_PicksUpAndRetires(t *testing.T) {
	root := t.TempDir()
	archivesDir := filepath.Join(root, "archives")
	if err := os.MkdirAll(archivesDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Build the mbox the archive will contain from the shared fixture set.
	mboxPath := writeMbox(t, root, fixtureSpecs)
	filterPath, configPath, stateDir := writeSupportFiles(t, root)

	mock := newIngestMock()
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	t.Cleanup(srv.Close)

	stageArchive(t, archivesDir, "arch-001", "archive/mbox", "all.mbox", mboxPath)

	ctx, cancel := context.WithCancel(context.Background())
	args := []string{
		"--watch-archives", archivesDir,
		"--filter", filterPath,
		"--config", configPath,
		"--state-dir", stateDir,
		"--ingest-url", srv.URL,
		"--source-name", sourceName,
		"--poll-interval", "200ms",
		"--health-port", fmt.Sprintf("%d", freePort(t)),
	}

	done := make(chan int, 1)
	go func() { done <- runCtx(ctx, args) }()

	// Wait for all includable messages to be delivered, then for the archive
	// to be retired into .done. Poll with a timeout; no fixed sleeps.
	deadline := time.Now().Add(15 * time.Second)
	for {
		_, doneErr := os.Stat(filepath.Join(archivesDir, ".done", "arch-001"))
		if mock.count() >= wantIngested && doneErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout: delivered=%d (want %d), .done present=%v",
				mock.count(), wantIngested, doneErr == nil)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Original archive dir must be gone (moved, not copied).
	if _, err := os.Stat(filepath.Join(archivesDir, "arch-001")); !os.IsNotExist(err) {
		t.Error("archives/arch-001 still present after retire")
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("runCtx exit code = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runCtx did not return after cancel")
	}
}
```

Note: this test uses a short `--poll-interval` so it does not depend on fsnotify firing in the test sandbox (the periodic poll alone will discover the pre-staged archive). The `time.Sleep` calls here are test-side polling of an external process's side effects, not synchronization of the code under test.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./importers/mbox/ -run TestE2E_WatchArchives_PicksUpAndRetires -v`
Expected: FAIL — `--watch-archives` is not yet a recognized flag (exit code 2), so the archive is never picked up and the test times out. (This drives Task 9.)

- [ ] **Step 3: Commit the test**

```bash
git add importers/mbox/watch_test.go
git commit -m "test(mbox): e2e watch-archives pickup + retire (glovebox-c9zt)"
```

---

## Task 9: wire flags + dispatch in `main.go`

**Files:**
- Modify: `importers/mbox/main.go`

- [ ] **Step 1: Add the flags**

In `runCtx`, alongside the existing flag declarations:

```go
watchArchives := fs.String("watch-archives", "", "run in watcher mode against this archives/ dir (mutually exclusive with --source)")
mediaTypes := fs.String("media-types", "", "comma-separated media_type allow-list for watcher mode (default archive/mbox)")
pollInterval := fs.Duration("poll-interval", defaultWatchPollInterval, "watcher poll interval backing fsnotify")
```

(`fs.Duration` needs no extra import; `flag` already imported.)

- [ ] **Step 2: Enforce mutual exclusion and branch**

Replace the existing `if *source == ""` required-flag block with:

```go
if *watchArchives != "" && *source != "" {
	fmt.Fprintln(os.Stderr, "ERROR: --source and --watch-archives are mutually exclusive")
	return 2
}
if *watchArchives == "" && *source == "" {
	fmt.Fprintln(os.Stderr, "ERROR: one of --source or --watch-archives is required")
	return 2
}
```

After the framework + `imp` are constructed (just before the existing `cfg := importer.RunConfig{...}` / `RunOneShot` block), branch:

```go
if *watchArchives != "" {
	if err := runWatch(ctx, fw, imp, *watchArchives, parseMediaTypes(*mediaTypes), *pollInterval); err != nil {
		if ctx.Err() != nil {
			slog.Warn("watcher stopped", "err", err)
			return 0
		}
		slog.Error("watcher failed", "err", err)
		return 1
	}
	return 0
}
```

Leave the existing one-shot `RunOneShot` block unchanged for the `--source` path below the branch.

- [ ] **Step 3: Run the integration test to verify it passes**

Run: `go test ./importers/mbox/ -run TestE2E_WatchArchives_PicksUpAndRetires -v`
Expected: PASS.

- [ ] **Step 4: Run the full mbox suite (no regressions on the one-shot path)**

Run: `go test ./importers/mbox/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add importers/mbox/main.go
git commit -m "feat(mbox): --watch-archives flag + dispatch (glovebox-c9zt)"
```

---

## Task 10: handler edge-case tests (media-type mismatch, lock contention, failure)

Round out coverage of the §5.3 branches the happy-path integration test does not hit. These reuse `stageArchive` and the mock backend via short-lived `runCtx` runs, asserting on-disk state.

**Files:**
- Modify: `importers/mbox/watch_test.go`

- [ ] **Step 1: Write the tests**

```go
// Media-type mismatch: a non-mbox archive is ignored and left untouched.
func TestE2E_WatchArchives_IgnoresOtherMediaType(t *testing.T) {
	root := t.TempDir()
	archivesDir := filepath.Join(root, "archives")
	mboxPath := writeMbox(t, root, fixtureSpecs[:2])
	filterPath, configPath, stateDir := writeSupportFiles(t, root)
	mock := newIngestMock()
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	t.Cleanup(srv.Close)
	stageArchive(t, archivesDir, "arch-pst", "archive/pst", "x.pst", mboxPath)

	ctx, cancel := context.WithCancel(context.Background())
	go runCtx(ctx, []string{
		"--watch-archives", archivesDir, "--filter", filterPath, "--config", configPath,
		"--state-dir", stateDir, "--ingest-url", srv.URL, "--source-name", sourceName,
		"--poll-interval", "100ms", "--health-port", fmt.Sprintf("%d", freePort(t)),
	})
	// Give the watcher a few poll cycles, then assert nothing happened.
	time.Sleep(600 * time.Millisecond)
	cancel()
	if mock.count() != 0 {
		t.Errorf("delivered %d items for an unclaimed media_type, want 0", mock.count())
	}
	if _, err := os.Stat(filepath.Join(archivesDir, "arch-pst", "metadata.json")); err != nil {
		t.Errorf("archive should be left in place: %v", err)
	}
	if _, err := os.Stat(filepath.Join(archivesDir, ".done", "arch-pst")); !os.IsNotExist(err) {
		t.Error("unclaimed archive must not be retired to .done")
	}
}

// Lock contention: a pre-existing .mbox-importer.lock makes the watcher skip.
func TestE2E_WatchArchives_SkipsLockedArchive(t *testing.T) {
	root := t.TempDir()
	archivesDir := filepath.Join(root, "archives")
	mboxPath := writeMbox(t, root, fixtureSpecs[:2])
	filterPath, configPath, stateDir := writeSupportFiles(t, root)
	mock := newIngestMock()
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	t.Cleanup(srv.Close)
	stageArchive(t, archivesDir, "arch-locked", "archive/mbox", "all.mbox", mboxPath)
	// Pre-create the lock as if another replica owns the archive.
	if err := acquireLock(filepath.Join(archivesDir, "arch-locked")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go runCtx(ctx, []string{
		"--watch-archives", archivesDir, "--filter", filterPath, "--config", configPath,
		"--state-dir", stateDir, "--ingest-url", srv.URL, "--source-name", sourceName,
		"--poll-interval", "100ms", "--health-port", fmt.Sprintf("%d", freePort(t)),
	})
	time.Sleep(600 * time.Millisecond)
	cancel()
	if mock.count() != 0 {
		t.Errorf("delivered %d items for a locked archive, want 0", mock.count())
	}
	if _, err := os.Stat(filepath.Join(archivesDir, ".done", "arch-locked")); !os.IsNotExist(err) {
		t.Error("locked archive must not be retired to .done")
	}
}
```

Note on the failure-path branch: a clean way to force a `RunOneShot` failure without a flaky backend is to stage `metadata.json` referencing a `raw_filename` whose file is absent — but `safeRawFilename` passes and `RunOneShot`'s survey `os.Open` then fails, exercising the `runErr != nil` branch. Add `TestE2E_WatchArchives_FailureLeavesInPlaceAndUnlocks` that stages an archive whose `raw/` file is missing, runs the watcher briefly, then asserts: no delivery, `.mbox-importer.lock` removed (`os.IsNotExist`), and the archive dir still present (not in `.done`).

- [ ] **Step 2: Run the new tests**

Run: `go test ./importers/mbox/ -run 'TestE2E_WatchArchives_' -v`
Expected: PASS (all watcher e2e cases).

- [ ] **Step 3: Commit**

```bash
git add importers/mbox/watch_test.go
git commit -m "test(mbox): watcher edge cases — media-type, lock, failure (glovebox-c9zt)"
```

---

## Task 11: full feedback loop + delivery

**Files:** none (verification + delivery)

- [ ] **Step 1: Race, vet, staticcheck, gofmt**

```bash
go test ./importers/mbox/ ./connector/ -race
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./importers/mbox/ ./connector/
gofmt -l importers/mbox/watch.go importers/mbox/watch_test.go importers/mbox/main.go connector/metrics.go
```

Expected: tests PASS under `-race`; `vet` clean; `staticcheck` reports nothing new for the touched files (the pre-existing `workers.go:97 meterUse unused` finding is out of scope — do not fix here); `gofmt -l` prints nothing for the listed files.

- [ ] **Step 2: Update the bead with the outcome**

```bash
bd update glovebox-c9zt --notes "Implemented: --watch-archives mode (watch.go) reusing internal/watcher + RunOneShot; configurable --media-types (default archive/mbox); O_EXCL lock; move-to-.done on success; release-lock+mark-seen on failure. Helm Deployment workload deferred to a follow-up bead."
```

- [ ] **Step 3: Push the branch and open the MR (GitLab-first)**

The branch is `feat/glovebox-c9zt-archive-watcher`. Push to origin, then create the MR via the GitLab API (the `glab mr create` git step fails in the snap sandbox — use the API form that worked for glovebox-gtxt):

```bash
git push -u origin feat/glovebox-c9zt-archive-watcher
glab api projects/steve%2Fglovebox/merge_requests -X POST \
  -f source_branch=feat/glovebox-c9zt-archive-watcher \
  -f target_branch=main \
  -f title="feat(mbox): archive-event watcher mode (glovebox-c9zt)" \
  -f remove_source_branch=true \
  -f description="Spec 13 §5.3 importer pickup: --watch-archives mode picks up archive/mbox archives, drives RunOneShot per archive, retires to .done. Reuses internal/watcher. Closes glovebox-c9zt."
```

- [ ] **Step 4: Arm auto-merge (merge when the CI pipeline passes)**

```bash
glab api projects/steve%2Fglovebox/merge_requests/<iid>/merge -X PUT \
  -f merge_when_pipeline_succeeds=true -f should_remove_source_branch=true
```

- [ ] **Step 5: After the MR merges, mirror to GitHub and close the bead**

```bash
git checkout main && git pull origin main && git push github main
bd close glovebox-c9zt
```

---

## Notes / gotchas

- **No emoji** in any Go source or log strings (project rule).
- The existing `internal/watcher` is reused unchanged; do not modify it. `.done/` is never dispatched because `archives/.done/metadata.json` does not exist (receipts are at `.done/<id>/metadata.json`).
- `--source` and `--watch-archives` are mutually exclusive; the one-shot path is untouched.
- The Helm Deployment workload (running this as a long-lived Deployment vs. a Job) is **out of scope** — file a follow-up bead, sibling to `glovebox-3d4m`.
- Delivery follows the GitLab-first workflow; the `glab mr create` subcommand fails in this environment's snap sandbox, so use the `glab api` MR-create form shown in Task 11.
- `gofmt -l` flags some pre-existing files in this tree (untar.go, worker.go, etc.) — those are not ours; only ensure the files this plan touches are clean.
- **Per-archive manifest isolation holds (verified).** `ManifestPath`/`SurveyPath`/`CheckpointPath` (`importers/mbox/manifest.go:143`, `survey.go:106`, `checkpoint.go:45`) all return `sourcePath + suffix`. Each archive's `sourcePath` is `archives/<unique-archive_id>/raw/<filename>`, so two archives that happen to share a `raw_filename` (e.g. both `all.mbox`) still get distinct sidecar files. The `--state-dir` passed in tests is framework/connector state, not these importer sidecars, so there is no cross-archive collision.
- **watch_test.go imports:** the test snippets use `context`, `fmt`, `os`, `path/filepath`, `net/http`, `net/http/httptest`, `time`, `reflect`, `testing`, and `otelnoop "go.opentelemetry.io/otel/metric/noop"`. Add imports as the compiler requires (or run `goimports`).
