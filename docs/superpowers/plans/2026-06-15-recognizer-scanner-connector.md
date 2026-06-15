# recognizer-scanner Connector — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Register an authenticated `recognizer-scanner` ingest source that delivers scanned documents to the openclaw *operator* lane — stamping `source`, a `data_subject` default, and an `operator` audience marker into `metadata.json`, and producing `content.extracted.md` from recognizer's pre-extracted OCR text — while rejecting any other source that tries to impersonate the lane.

**Architecture:** This rides the existing tus.io archive-delivery path (`internal/ingest/archives/`). The anti-spoof identity is the **authenticated bearer-token source-id** (`UploadState.SourceID`), never a producer-asserted field. A new config-driven **source registry** (`internal/source`, mirroring `internal/subject`) maps an authorized source-id to its lane policy (`kind`, `data_subject_default`, `audience_default`). At finalize, when the authenticated source is a registered `recognizer-scanner`, glovebox: (a) stamps `source` + applies the `data_subject`/`audience` defaults, and (b) renders `tree/ocr.txt` (recognizer pre-extracts the OCR text — locked decision 2026-06-15) into `content.extracted.md`. A request that asserts the scanner media type but is NOT the authorized source-id is rejected 403. Prompt-injection scanning of the extracted text is the existing downstream pipeline's job and is out of scope here.

**Tech Stack:** Go (standard `go test`, `go vet`, `staticcheck`). No new third-party dependencies — recognizer pre-extracts OCR, so glovebox needs no PDF/A text-extraction library.

**Contract source:** `/mnt/c/Users/steve/Code/openclaw/docs/superpowers/specs/2026-06-14-operator-scanner-lane-design.md` §4.2. Bead: `glovebox-9s60`.

---

## Locked decisions (this plan)

1. **Extraction owner = recognizer.** Recognizer pre-extracts OCR text and includes it in the delivered tarball as a UTF-8 plaintext file at tar-root `ocr.txt`. glovebox renders it to `content.extracted.md`. glovebox adds NO PDF-parsing dependency. *(Must be echoed in the recognizer `archiver-tso` delivery bead — flag if that bead specifies a different path/field.)*
2. **Identity = authenticated token source-id.** The lane policy keys off `UploadState.SourceID` (proven by bearer token). Producer-asserted source claims are ignored. This is what makes "spoof rejected" real.
3. **`operator` is a new standalone audience token** (must appear alone, no `data_subject` required), added to the spec-11 audience vocabulary. It is the marker the openclaw per-person resolver skips.
4. **New media type `archive/recognizer-scan` (tar shape).** Required to pass the POST allow-list. Only a registered `recognizer-scanner` source-id may deliver it.

## Inter-repo contract notes (do NOT implement here — flag only)

- recognizer (`archiver-tso`): delivery POSTs `media_type=archive/recognizer-scan`, a tar whose root contains `manifest.json`, the OCR'd PDF/A, WebP proxies, and **`ocr.txt`**. Auth as source-id `recognizer-scanner`.
- openclaw (`openclaw-3wz`): per-person triage resolver must skip items whose `audience` contains `operator`.

## File structure

| File | Responsibility | New/Modify |
|---|---|---|
| `internal/staging/audience.go` | add `operator` token + standalone rule | Modify |
| `internal/source/registry.go` | source-policy registry (Load/Lookup/Enforce) | Create |
| `internal/source/registry_test.go` | registry unit tests | Create |
| `internal/config/config.go` | load+validate `sources_file` | Modify |
| `internal/config/config_test.go` | config wiring tests | Modify |
| `internal/ingest/archives/metadata.go` | allow `archive/recognizer-scan` media type | Modify |
| `internal/ingest/archives/scan_extract.go` | render `ocr.txt` -> `content.extracted.md` | Create |
| `internal/ingest/archives/scan_extract_test.go` | extraction unit tests | Create |
| `internal/ingest/archives/finalize.go` | `Source` receipt field; source-policy stamping; spoof gate; extract hook | Modify |
| `internal/ingest/archives/finalize_test.go` | finalize policy + spoof + extract tests | Modify |
| `internal/ingest/archives/handler.go` | thread source registry into `FinalizeConfig` | Modify |
| `internal/ingest/archives/integration_test.go` | end-to-end authorized + spoof + extracted-md + operator-marker | Modify |
| `charts/glovebox/sources.json` | the `recognizer-scanner` registry entry | Create |

---

## Task 1: Add the `operator` audience token

**Files:**
- Modify: `internal/staging/audience.go`
- Test: `internal/staging/audience_test.go`

- [ ] **Step 1: Write the failing tests.** In `audience_test.go`, add cases to the existing `ValidateAudience` table tests:
  - valid: `{"operator-alone", []string{"operator"}, false}` (no data_subject required) → expect nil error.
  - valid: `{"operator-alone-with-subject", []string{"operator"}, true}` → nil.
  - invalid: `{"operator-with-subject-token", []string{"operator", "subject"}, true, "operator must appear alone"}`.
  - invalid: `{"operator-with-household", []string{"operator", "household"}, true, "operator must appear alone"}`.

- [ ] **Step 2: Run to verify they fail.** Run: `go test ./internal/staging/ -run ValidateAudience -v`. Expected: FAIL — `unknown audience token "operator"` for the valid cases.

- [ ] **Step 3: Implement.** In `audience.go`:
  - Add `AudienceOperator = "operator"` to the const block.
  - Add `AudienceOperator: true` to `validAudienceTokens`.
  - Add a standalone rule mirroring `public`: track `hasOperator` in the loop, and after the loop add `if hasOperator && len(audience) > 1 { return fmt.Errorf("operator must appear alone in audience") }`. Do NOT add it to `householdSubsetTokens` or `subjectRelativeTokens` (it stands alone and needs no data_subject).

- [ ] **Step 4: Run to verify pass.** Run: `go test ./internal/staging/ -run ValidateAudience -v`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add internal/staging/audience.go internal/staging/audience_test.go
git commit -m "feat(audience): add standalone 'operator' audience token (glovebox-9s60)"
```

---

## Task 2: Source-policy registry package

Mirrors `internal/subject/registry.go`. Maps an authorized ingest source-id to its lane policy. `sources.json` wire format:
```json
{
  "enforce": true,
  "sources": [
    { "source_id": "recognizer-scanner", "kind": "recognizer-scanner",
      "data_subject_default": "e_111111", "audience_default": ["operator"] }
  ]
}
```

**Files:**
- Create: `internal/source/registry.go`
- Test: `internal/source/registry_test.go`

- [ ] **Step 1: Write failing tests.** In `registry_test.go`:
  - `TestLoad_EmptyPath`: `Load("")` returns a non-nil, non-enforcing registry, nil error; `Lookup("anything")` returns `(_, false)`.
  - `TestLoad_RecognizerScanner`: write a temp `sources.json` (the JSON above) via `t.TempDir()`; `Load` it; `Lookup("recognizer-scanner")` returns `ok==true`, `Kind()=="recognizer-scanner"`, `DataSubjectDefault()=="e_111111"`, `AudienceDefault()==["operator"]`; `Enforce()==true`.
  - `TestLoad_UnknownSource`: `Lookup("rss")` returns `(_, false)`.
  - `TestLoad_RejectsBadAudience`: a `sources.json` whose `audience_default` is `["subject"]` (subject-relative, would need data_subject) — assert `Load` returns an error wrapping a validation failure. (Reuse `staging.ValidateAudience(aud, def != "")`.)
  - `TestLoad_RejectsDuplicateSourceID`: two entries with the same `source_id` → error.
  - `TestLoad_RejectsEmptySourceID`: entry with `""` source_id → error.

- [ ] **Step 2: Run to verify fail.** Run: `go test ./internal/source/ -v`. Expected: FAIL — package/symbols undefined.

- [ ] **Step 3: Implement `registry.go`.** Package `source`. Define:
  - unexported `sourcePolicy struct { sourceID, kind, dataSubjectDefault string; audienceDefault []string }`.
  - `type Registry struct { enforce bool; bySourceID map[string]*sourcePolicy }`.
  - `func (r *Registry) Enforce() bool { return r != nil && r.enforce }`.
  - `func (r *Registry) Lookup(id string) (*Policy, bool)` — return an exported read-only view `Policy` with methods `Kind() string`, `DataSubjectDefault() string`, `AudienceDefault() []string` (return a copy of the slice). Nil-receiver safe.
  - wire structs `wireSource{ SourceID, Kind, DataSubjectDefault string; AudienceDefault []string }` with json tags `source_id`,`kind`,`data_subject_default`,`audience_default`; `wireRegistry{ Enforce bool; Sources []wireSource }` with tag `sources`.
  - `func Load(path string) (*Registry, error)`: empty path → `&Registry{bySourceID: map...}`, nil. Else read+unmarshal; for each entry validate non-empty `source_id`, no duplicates, and `staging.ValidateAudience(s.AudienceDefault, s.DataSubjectDefault != "")`; build map. Wrap errors with `fmt.Errorf("source registry %s: %w", path, err)`.

- [ ] **Step 4: Run to verify pass.** Run: `go test ./internal/source/ -v`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add internal/source/
git commit -m "feat(source): add ingest source-policy registry (glovebox-9s60)"
```

---

## Task 3: Wire `sources_file` into config

**Files:**
- Modify: `internal/config/config.go` (struct field near `SubjectsFile` line ~107; env load near line ~248; validate near line ~253)
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Write failing test.** In `config_test.go`, mirror the existing `SubjectsFile` test: a config with `sources_file` pointing at a valid temp `sources.json` validates clean; one pointing at a malformed file returns an error from `Validate()`. Add an accessor expectation: `cfg.SourcesFile` round-trips the env/JSON value.

- [ ] **Step 2: Run to verify fail.** Run: `go test ./internal/config/ -run Sources -v`. Expected: FAIL — `SourcesFile` undefined.

- [ ] **Step 3: Implement.** In `config.go`:
  - Add `SourcesFile string \`json:"sources_file"\`` to `Config` next to `SubjectsFile`.
  - In the env-loading block (next to where `cfg.SubjectsFile = v` is set, ~line 248), load `GLOVEBOX_SOURCES_FILE` into `cfg.SourcesFile` following the exact surrounding idiom.
  - In `Validate()` (next to the `SubjectsFile` block ~line 253), add: `if c.SourcesFile != "" { if _, err := source.Load(c.SourcesFile); err != nil { return ... } }`. Add the `internal/source` import.

- [ ] **Step 4: Run to verify pass.** Run: `go test ./internal/config/ -v`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): load+validate sources_file source registry (glovebox-9s60)"
```

---

## Task 4: Allow the scanner media type + add `Source` receipt field

**Files:**
- Modify: `internal/ingest/archives/metadata.go` (`mediaAllowList` ~line 82)
- Modify: `internal/ingest/archives/finalize.go` (`FinalizeReceipt` ~line 60)
- Test: `internal/ingest/archives/metadata_test.go` (or the existing parse test file)

- [ ] **Step 1: Write failing test.** Add a parse test asserting `media_type=archive/recognizer-scan` is accepted by `ParseUploadMetadata` and `Shape()` returns `MediaTar`.

- [ ] **Step 2: Run to verify fail.** Run: `go test ./internal/ingest/archives/ -run Metadata -v`. Expected: FAIL — `ErrMetadataUnknownMediaType`.

- [ ] **Step 3: Implement.**
  - In `metadata.go` add to `mediaAllowList`: `"archive/recognizer-scan": MediaTar,`. Add a comment noting it is gated to the registered `recognizer-scanner` source at finalize.
  - In `finalize.go` add to `FinalizeReceipt` after `MediaType`: `Source string \`json:"source,omitempty"\``. Document it carries the authenticated source-id for registered lanes.

- [ ] **Step 4: Run to verify pass.** Run: `go test ./internal/ingest/archives/ -run Metadata -v`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add internal/ingest/archives/metadata.go internal/ingest/archives/finalize.go internal/ingest/archives/metadata_test.go
git commit -m "feat(ingest): allow archive/recognizer-scan media type + Source receipt field (glovebox-9s60)"
```

---

## Task 5: OCR -> markdown extraction helper

Pure function so it can be unit-tested without the full finalize machinery.

**Files:**
- Create: `internal/ingest/archives/scan_extract.go`
- Test: `internal/ingest/archives/scan_extract_test.go`

- [ ] **Step 1: Write failing tests.** In `scan_extract_test.go`:
  - `TestRenderExtractedMarkdown_Basic`: input OCR bytes `"Invoice 2026\nTotal: $40"` → output contains a `# Scanned document` heading line AND the verbatim OCR body.
  - `TestRenderExtractedMarkdown_Empty`: empty OCR input → returns `ErrScanMissingOCR` (so an empty/absent OCR layer fails loudly rather than emitting an empty recall doc).
  - `TestWriteExtractedMarkdown`: given a `t.TempDir()` finalize dir containing `tree/ocr.txt`, call the writer; assert `content.extracted.md` exists at the finalize-dir root, mode `0600`, and contains the OCR body. Missing `tree/ocr.txt` → `ErrScanMissingOCR`.

- [ ] **Step 2: Run to verify fail.** Run: `go test ./internal/ingest/archives/ -run Extract -v`. Expected: FAIL — undefined.

- [ ] **Step 3: Implement `scan_extract.go`.**
  - `var ErrScanMissingOCR = errors.New("recognizer-scan: missing or empty ocr.txt")`.
  - `func renderExtractedMarkdown(ocr []byte) ([]byte, error)`: trim; if empty → `ErrScanMissingOCR`; else return `[]byte("# Scanned document\n\n" + string(ocr) + "\n")`.
  - `func writeExtractedMarkdown(finalizeDir string) error`: read `filepath.Join(finalizeDir, "tree", "ocr.txt")` (os.ErrNotExist → `ErrScanMissingOCR`); render; write to `filepath.Join(finalizeDir, "content.extracted.md")` via `atomicWriteSibling(path, md, finalizeFileMode)` (reuse the helper already in `finalize.go`).

- [ ] **Step 4: Run to verify pass.** Run: `go test ./internal/ingest/archives/ -run Extract -v`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add internal/ingest/archives/scan_extract.go internal/ingest/archives/scan_extract_test.go
git commit -m "feat(ingest): render recognizer OCR text to content.extracted.md (glovebox-9s60)"
```

---

## Task 6: Finalize — source-policy stamping, spoof gate, extract hook

This is the integration point. `FinalizeConfig` gains the registry; `Finalize` applies policy and gates the media type.

**Files:**
- Modify: `internal/ingest/archives/finalize.go`
- Test: `internal/ingest/archives/finalize_test.go`

- [ ] **Step 1: Write failing tests.** In `finalize_test.go`, build a `*source.Registry` via `source.Load` of a temp `sources.json` (recognizer-scanner → e_111111 / ["operator"]). Construct an `UploadState` for a tar containing `ocr.txt`, drive `Finalize` with `FinalizeConfig{StagingRoot, Sources}`:
  - `TestFinalize_ScannerStampsPolicy`: `SourceID="recognizer-scanner"`, media `archive/recognizer-scan`, metadata supplies NO data_subject/audience → published `metadata.json` has `source=="recognizer-scanner"`, `data_subject=="e_111111"`, `audience==["operator"]`; `content.extracted.md` exists at archive root with the OCR body.
  - `TestFinalize_ScannerDataSubjectDefaultHonored`: same but the sources.json default is `household` → `data_subject=="household"`. (Proves the per-connector knob.)
  - `TestFinalize_ScannerExplicitDataSubjectWins`: metadata supplies `data_subject=e_333333` → receipt keeps `e_333333` (per-item override > connector default), audience still defaults to `["operator"]`.
  - `TestFinalize_SpoofRejected`: `SourceID="rss"` (valid token, NOT in registry as scanner kind) delivering `media_type=archive/recognizer-scan` → `Finalize` returns `ErrSourceNotAuthorized`; nothing published; tmp cleaned.
  - `TestFinalize_SpoofRejectedNilRegistry`: `Sources==nil` (registry unconfigured) + `media_type=archive/recognizer-scan` → `Finalize` returns `ErrSourceNotAuthorized`; nothing published. **The media-type gate is fail-CLOSED.**
  - `TestFinalize_ScannerForcesOperatorMarker`: scanner source whose metadata asserts a non-operator audience (e.g. `["household"]`) → published `audience` is forced to `["operator"]` (the lane marker is guaranteed, not merely defaulted). The connector lane overrides a producer audience that lacks `operator`.
  - `TestFinalize_ScannerMissingOCR`: scanner source, tar WITHOUT `ocr.txt` → `Finalize` returns an error wrapping `ErrScanMissingOCR`; nothing published.
  - `TestFinalize_NonScannerUnaffected`: existing `archive/mbox` from a normal source with `Sources==nil` → behaves exactly as today (no source/extracted.md/operator marker). Guards backward-compat.

- [ ] **Step 2: Run to verify fail.** Run: `go test ./internal/ingest/archives/ -run Finalize -v`. Expected: FAIL — `Sources` field / `ErrSourceNotAuthorized` undefined.

- [ ] **Step 3: Implement.** In `finalize.go`:
  - Add `var ErrSourceNotAuthorized = errors.New("source not authorized for recognizer-scan lane")`.
  - Add `Sources *source.Registry` to `FinalizeConfig`. Import `internal/source`.
  - **Spoof gate (early, before building .finalize/) — FAIL-CLOSED:** if `st.Meta.MediaType == "archive/recognizer-scan"`, then unless `cfg.Sources != nil` AND a lookup of `st.SourceID` yields `ok && Kind()=="recognizer-scanner"`, do `cleanupTmp(...); return nil, ErrSourceNotAuthorized`. A nil/unconfigured registry or any non-scanner source-id MUST be rejected — the gate is NOT nil-guarded. (Rationale: an unset `GLOVEBOX_SOURCES_FILE` or missing chart mount must never let the scanner media type publish unchecked.)
  - **Policy stamping (after receipt skeleton built, before write) — for the scanner kind, the operator marker is GUARANTEED, not merely defaulted:** when `cfg.Sources != nil` and the lookup yields the scanner kind: set `receipt.Source = st.SourceID`; if `receipt.DataSubject == ""` set it to `pol.DataSubjectDefault()` (per-item override wins when present); ALWAYS set `receipt.Audience = pol.AudienceDefault()` (i.e. `["operator"]`) — overriding any producer-asserted audience, since a scanner item that escaped the operator marker would be auto-routed by the openclaw resolver, violating spec §4.2.
  - **Extract hook (in the `MediaTar` branch, after a successful untar, only for the scanner kind):** call `writeExtractedMarkdown(finalizeDir)`; on error `cleanupTmp(...); return nil, fmt.Errorf("extract: %w", err)`. Place it before step 5 (metadata write) so a missing OCR aborts before publish.
  - The stamping and extraction branches are guarded on `cfg.Sources != nil` so existing callers/tests with a nil registry and non-scanner media types are unaffected; ONLY the fail-closed media-type gate runs regardless of `cfg.Sources`.

- [ ] **Step 4: Run to verify pass.** Run: `go test ./internal/ingest/archives/ -run Finalize -v`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add internal/ingest/archives/finalize.go internal/ingest/archives/finalize_test.go
git commit -m "feat(ingest): recognizer-scanner finalize policy + spoof gate + OCR extract (glovebox-9s60)"
```

---

## Task 7: Thread the registry through handler + 403 mapping (test-level)

> NOTE on the boot path: the handler is NOT constructed directly in production — it is built deep in the listener chain. Task 7 covers the handler + error mapping (exercised by the in-package integration test); **Task 7b covers the production boot wiring** that the integration test does NOT reach. Both are required, or production silently runs with `Sources==nil` (and, given the fail-closed gate, would 403 every real scanner delivery).

**Files:**
- Modify: `internal/ingest/archives/handler.go` (`HandlerConfig` struct at ~line 82; `FinalizeConfig{...}` construction at line 916; `recordFinalizeFailure` at ~line 960)
- Test: `internal/ingest/archives/integration_test.go` (handler stood up directly via `NewHandler`, ~line 177)

- [ ] **Step 1: Write failing end-to-end test.** In `integration_test.go`, stand up the handler via `NewHandler` with a `HandlerConfig` carrying a loaded `*source.Registry`. Drive a full POST+PATCH tus upload of a small tar (containing `manifest.json` + `ocr.txt`) as source-id `recognizer-scanner`, media `archive/recognizer-scan`:
  - assert 204 finalize; `archives/<id>/metadata.json` has `source`, `data_subject`, `audience:["operator"]`; `archives/<id>/content.extracted.md` present with OCR body.
  - second sub-test: same upload authenticated as a non-scanner source-id → finalize maps to **403** with error code `source_not_authorized`.

- [ ] **Step 2: Run to verify fail.** Run: `go test ./internal/ingest/archives/ -run Integration -v`. Expected: FAIL — `HandlerConfig` has no `Sources`; no 403 mapping.

- [ ] **Step 3: Implement.**
  - Add `Sources *source.Registry` to `HandlerConfig` (handler.go:82). Import `internal/source`.
  - At `handler.go:916` pass `Sources: h.cfg.Sources` into the `FinalizeConfig{StagingRoot: ...}` literal.
  - In `recordFinalizeFailure` (handler.go:~960), add a case: `errors.Is(err, ErrSourceNotAuthorized)` → `(http.StatusForbidden, "source_not_authorized")`, and emit `h.tel.RecordUploadFailed(ctx, sourceID, mediaType, "failed_authz")` to match the telemetry of every other case in that switch. Keep existing mappings intact.

- [ ] **Step 4: Run to verify pass.** Run: `go test ./internal/ingest/archives/ -v`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add internal/ingest/archives/handler.go internal/ingest/archives/integration_test.go
git commit -m "feat(ingest): HandlerConfig.Sources + 403 spoof mapping (glovebox-9s60)"
```

---

## Task 7b: Wire the registry through the listener boot chain (production path)

The construction chain is `archive_listener.go:bootstrapArchivesWithSource` (~line 70) → builds `ArchiveListenerConfig` (`internal/ingest/archives/listener.go:51`) → `StartArchiveListener` builds `HandlerConfig` and calls `NewHandler` (`listener.go:147-154`). The registry must travel this whole path or `HandlerConfig.Sources` stays nil in prod. `SubjectsFile` is NOT a precedent here — it is only wired into the file-watch routing pipeline in `main.go:54`, never into the archive listener.

**Files:**
- Modify: `internal/ingest/archives/listener.go` (`ArchiveListenerConfig` ~line 51; `HandlerConfig` build ~line 149)
- Modify: `archive_listener.go` (`bootstrapArchivesWithSource` ~line 70; `listenerCfg` build ~line 98)

- [ ] **Step 1: Write failing test.** Add a listener-level test (in `archive_listener_test.go` or `internal/ingest/archives/listener_test.go`) asserting that when `ArchiveListenerConfig.Sources` is set, the handler it constructs carries it (e.g. a smoke upload of `archive/recognizer-scan` from `recognizer-scanner` succeeds through the listener-built handler; and the same from a non-scanner source 403s). If the handler is not exported from the listener, assert via behavior (the 403 vs 204) rather than field inspection.

- [ ] **Step 2: Run to verify fail.** Run: `go test ./internal/ingest/archives/ -run Listener -v` and `go test . -run ArchiveListener -v`. Expected: FAIL — `ArchiveListenerConfig` has no `Sources`.

- [ ] **Step 3: Implement.**
  - Add `Sources *source.Registry` to `ArchiveListenerConfig` (listener.go:51) and pass it into the `HandlerConfig{...}` literal at listener.go:149.
  - In `archive_listener.go:bootstrapArchivesWithSource`: call `source.Load(cfg.SourcesFile)` (where `cfg` is the app config carrying `SourcesFile` from Task 3), handle the error (fail boot — a malformed registry must not start), and set the result on the `listenerCfg` (~line 98). Import `internal/source` and ensure the app config is threaded to this function (it already carries `StagingRoot`, etc.).

- [ ] **Step 4: Run to verify pass.** Run: `go test ./internal/ingest/archives/ -v && go test . -v`. Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add internal/ingest/archives/listener.go archive_listener.go internal/ingest/archives/listener_test.go archive_listener_test.go
git commit -m "feat(ingest): load+thread source registry through archive listener boot (glovebox-9s60)"
```

---

## Task 8: Chart registry entry, full verification, bead close

**Files:**
- Create: `charts/glovebox/sources.json`

- [ ] **Step 1: Add the registry entry.** Write `charts/glovebox/sources.json`:
```json
{
  "enforce": true,
  "sources": [
    {
      "source_id": "recognizer-scanner",
      "kind": "recognizer-scanner",
      "data_subject_default": "e_111111",
      "audience_default": ["operator"]
    }
  ]
}
```
(Confirm the deployment wires `GLOVEBOX_SOURCES_FILE` to this path; if `subjects.json` is mounted via the chart, add `sources.json` the same way. Note this in the bead if a chart/values change is needed — that may be a separate gitops bead.)

- [ ] **Step 2: Full test + lint sweep.** Run, and confirm each is clean:
```bash
gofmt -l .          # expect no output; gofmt -w any listed files (config.go:106-108 was already misaligned)
go test ./...
go vet ./...
staticcheck ./...   # or: golangci-lint run
```
Expected: all PASS / no findings. Fix anything before proceeding.

- [ ] **Step 3: Confirm acceptance criteria** against the bead, by test name:
  - source authorized in registry → Task 2 + Task 7 authorized sub-test.
  - spoof rejected → `TestFinalize_SpoofRejected` + integration 403.
  - `content.extracted.md` produced → `TestWriteExtractedMarkdown` + finalize/integration.
  - operator audience marker + data_subject stamped → `TestFinalize_ScannerStampsPolicy` + integration metadata assertions.
  - per-connector `data_subject_default` honored → `TestFinalize_ScannerDataSubjectDefaultHonored`.

- [ ] **Step 4: Commit + close.**
```bash
git add charts/glovebox/sources.json docs/superpowers/plans/2026-06-15-recognizer-scanner-connector.md
git commit -m "feat(charts): recognizer-scanner source registry entry; close (glovebox-9s60)"
```
Then `bd close glovebox-9s60` with a note pointing at the contract §4.2 and the recognizer/openclaw sibling beads.

---

## Out of scope / flag to siblings
- Prompt-injection scanning of `content.extracted.md` — existing downstream pipeline.
- recognizer delivery client + idempotent re-delivery — `archiver-tso`.
- openclaw operator-inbox lane, `memorySearch.extraPaths`, resolver `operator`-skip — `openclaw-3wz`.
- Chart/gitops mounting of `sources.json` + `GLOVEBOX_SOURCES_FILE` env — confirm during Task 8; may be a gitops bead.
- Optional hardening (not required for acceptance): reject an unauthorized `archive/recognizer-scan` at POST/`create` time (both media_type and authenticated source-id are known there) to avoid transferring a full multi-GiB body before the finalize gate rejects it. The fail-closed finalize gate is sufficient for correctness; this only saves bandwidth.
