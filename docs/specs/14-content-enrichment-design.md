# Content Enrichment — Design Specification

**Version 1.0 — June 2026**

*This document specifies the content-enrichment pipeline for glovebox: a framework hook that runs alongside the existing staging flow and produces text-form sidecar artifacts (e.g., `content.extracted.md`) from raw item content. v1 ships text extraction for HTML, plain text, PDF, image OCR, and office documents. The mechanism is designed as an extensible class — future members include vision-LLM image captions, speech-to-text transcripts, and document-structure extraction.*

---

## 1. Purpose

Glovebox connectors and importers write staging items as `inbox/<id>/{content.raw, metadata.json}`. Downstream consumers (notably the OpenClaw memory subsystem via its `memorySearch.extraPaths` watcher) index markdown files for recall and reasoning. Today there is no text-form sidecar for non-text content: a PDF attached to an email lands as a binary `content.raw`; an HTML newsletter body lands as raw HTML; an image attachment is opaque.

The top-level glovebox→memory ingestion design (`docs/superpowers/specs/2026-06-01-glovebox-to-memory-ingestion-design.md` in the openclaw repo, §8) defines the contract this spec satisfies: glovebox writes a `content.extracted.md` sidecar alongside `content.raw` during ingestion, and the enrichment mechanism is designed as the first member of an extensible class so future enrichments (vision-LLM captions, audio transcripts, etc.) can plug in without re-architecting the pipeline.

This spec defines that class's interface, the v1 first-party members, the per-connector image strategy required to support binary-dependent enrichers under glovebox's distroless build pattern, and the test plan.

## 2. Scope

### 2.1 In scope

- A new `connector/enrich/` package defining the `Enricher` interface, an `Artifact` value type, a `Registry`, and a process-global `Default` registry.
- A pipeline hook inside `StagingItem.Commit()` that runs the applicable enrichers before atomic rename to staging.
- A `staging.ItemMetadata.Enrichments []EnrichmentRecord` field describing what was produced (or attempted and failed) per item.
- Five v1 first-party enrichers: `passthrough`, `html`, `pdf`, `ocr`, `office`.
- A new base image `glovebox-enricher-runtime` (debian:bookworm-slim + apt: poppler-utils, tesseract-ocr-eng, pandoc) for connectors that need binary-dependent enrichers.
- Per-connector Dockerfile rebases for `gmail`, `imap`, `outlook`, and the `mbox` importer.
- Unit tests, container tests, and an end-to-end smoke script.

### 2.2 Out of scope

- Vision-LLM image captions. Anticipated as the next class member; separate spec when implemented.
- Speech-to-text on audio attachments. Same shape; deferred.
- Sensitivity inference from extracted content. The triage step in the downstream openclaw pipeline owns sensitivity decisions; enrichment is text-shape neutral.
- Connector-side opt-out for high-throughput backfill (e.g., "skip OCR for this drain"). v1 always runs every applicable enricher. Flag added in a future iteration if real throughput pressure demands it.
- LibreOffice. The `pandoc` tool covers the office-doc formats we care about (.docx, .xlsx with tabular content, .pptx text); LibreOffice headless is rejected for v1 on image-size grounds (~500 MB).
- Multi-language OCR. v1 ships English (`tesseract-ocr-eng`); adding languages is an apt install in the enricher-runtime image, documented in a runbook entry.

## 3. Vocabulary

**Enricher** — a unit that consumes one source file (e.g., `content.raw` or an attachment) plus its `ItemMetadata` and emits zero or more sidecar artifacts. Each enricher has a stable `Name()`, an `Applies()` predicate, and an `Enrich()` action.

**Artifact** — a sidecar file produced by an enricher. Carries the producer's name, the artifact's semantic kind (e.g., `extracted-text`, `image-caption`), and the filename relative to the item directory.

**Registry** — the set of enrichers known to the running process. Subpackages register their enrichers via `init()`. Connectors include or exclude enrichers via blank imports in `main.go`.

**Pipeline** — the registry's `ApplyAll(...)` method that iterates over enrichers and runs the applicable ones against a staging item.

**Pure-Go enricher** — implementation that runs entirely in-process without invoking external binaries (e.g., `passthrough`, `html`). Distroless-compatible.

**Binary-dependent enricher** — implementation that shells out to an external CLI tool (e.g., `pdf` invokes `pdftotext`). Requires the binary in the running image; gracefully disabled if absent.

## 4. Framework

### 4.1 Enricher interface

```go
package enrich

import (
    "context"
    "github.com/leftathome/glovebox/internal/staging"
)

// Enricher produces sidecar artifacts from a source file + its metadata.
type Enricher interface {
    // Name is a stable identifier used in metadata and error markers.
    // Convention: lowercase, hyphen-separated, package-aligned (e.g., "pdf", "html", "ocr").
    Name() string

    // Applies reports whether this enricher should run on a given source file.
    // Implementations dispatch primarily on meta.ContentType, with optional
    // file-sniffing fallback via connector/content/mime.go for ambiguous cases.
    Applies(meta staging.ItemMetadata, sourcePath string) bool

    // Enrich produces sidecar artifacts. Implementations write files into
    // outputDir (the item directory) and return the artifacts they produced.
    // Errors are non-fatal for the item commit: see §4.4.
    Enrich(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error)
}

// Artifact describes one sidecar file produced by an enricher.
type Artifact struct {
    Filename string  // relative to outputDir, e.g. "content.extracted.md"
    Kind     string  // "extracted-text" | "image-caption" | "ocr-text" | "transcript" | ...
    Producer string  // enricher Name()
}
```

### 4.2 Registry

```go
// Registry holds the set of enrichers active in the process.
type Registry struct { /* unexported fields */ }

func NewRegistry() *Registry
func (r *Registry) Register(e Enricher)

// ApplyAll runs every Enricher whose Applies() returns true on the given
// source file. Returns the artifacts produced (across all enrichers) and a
// per-enricher error slice. Errors do NOT abort iteration.
func (r *Registry) ApplyAll(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) (artifacts []Artifact, errs []EnricherError)

// EnricherError pairs an Enricher.Name() with the error it returned.
type EnricherError struct {
    Producer string
    Err      error
}

// Default is the process-global registry. Subpackages register via init().
var Default = NewRegistry()
```

Subpackages register themselves with `Default`:

```go
// connector/enrich/html/html.go
func init() { enrich.Default.Register(&Enricher{}) }
```

Connectors enable specific enrichers via blank imports in `main.go`:

```go
// connectors/gmail/main.go
import (
    _ "github.com/leftathome/glovebox/connector/enrich/passthrough"
    _ "github.com/leftathome/glovebox/connector/enrich/html"
    _ "github.com/leftathome/glovebox/connector/enrich/pdf"
    _ "github.com/leftathome/glovebox/connector/enrich/ocr"
    _ "github.com/leftathome/glovebox/connector/enrich/office"
)
```

Connectors that don't import a subpackage simply don't have its enricher in the registry — this is the compile-time lean/fat decision.

### 4.3 Pipeline hook in StagingItem.Commit()

`StagingItem.Commit()` runs the pipeline between content finalization and atomic rename to staging:

```go
func (si *StagingItem) Commit() error {
    if si.commitFunc != nil {
        return si.commitFunc()
    }

    meta, err := si.buildMetadata()
    if err != nil {
        os.RemoveAll(si.dir)
        return err
    }

    // ---- enrichment pipeline ----
    // Primary source: content.raw (the connector-written body)
    contentPath := filepath.Join(si.dir, "content.raw")
    primaryArts, primaryErrs := enrich.Default.ApplyAll(si.ctx, contentPath, meta, si.dir)

    // Per-attachment sources: any "attachment-*" file in the item dir.
    // enrich.Artifact and enrich.EnricherError are the types from §4.1/§4.2.
    var attachmentArts []enrich.Artifact
    var attachmentErrs []enrich.EnricherError
    attachments, _ := filepath.Glob(filepath.Join(si.dir, "attachment-*"))
    for _, ap := range attachments {
        arts, errs := enrich.Default.ApplyAll(si.ctx, ap, meta, si.dir)
        attachmentArts = append(attachmentArts, arts...)
        attachmentErrs = append(attachmentErrs, errs...)
    }

    // Record what was produced and what failed. summarize() and
    // writeErrorMarkers() are local helpers in this package; see §4.6 for
    // the EnrichmentRecord shape and §4.4 for the marker file semantics.
    meta.Enrichments = summarize(primaryArts, primaryErrs, attachmentArts, attachmentErrs)
    writeErrorMarkers(si.dir, primaryErrs, attachmentErrs)
    // ---- end pipeline ----

    data, err := json.Marshal(meta)
    if err != nil {
        return fmt.Errorf("marshal metadata: %w", err)
    }
    if err := os.WriteFile(filepath.Join(si.dir, "metadata.json"), data, 0644); err != nil {
        return fmt.Errorf("write metadata: %w", err)
    }

    destDir := filepath.Join(si.stagingDir, filepath.Base(si.dir))
    if err := os.Rename(si.dir, destDir); err != nil {
        return fmt.Errorf("atomic rename: %w", err)
    }
    return nil
}
```

After Commit() returns successfully, the item directory in `staging/<item-id>/` contains:

- `content.raw` (immutable original)
- `content.extracted.md` (primary text-form sidecar, if a primary-content enricher applied)
- `content.<enricher>.error.md` (one per failed enricher, optional)
- `attachment-<n>-<filename>` (any attachments, unchanged)
- `content.attachment-<n>.extracted.md` (per-attachment text sidecar, if applicable)
- `metadata.json` (now includes `Enrichments[]`)

### 4.4 Failure semantics

Per-enricher errors do NOT abort the item commit. The pipeline:

1. Calls each applicable enricher in order.
2. Collects artifacts AND errors.
3. For each error, writes a marker file `content.<enricher-name>.error.md` containing the failure message in the WHAT/CHECK/FIX shape specified by the shared error-message discipline (see §8).
4. Records the failure in `meta.Enrichments[]` with `Status: "failed"`.
5. Proceeds to metadata marshal and atomic rename.

Rationale: a corrupt PDF shouldn't prevent the email body from being indexed. The email's `content.extracted.md` (from HTML extraction) is still produced; only `content.pdf.error.md` records the attachment-extraction failure. Memory-core sees the email text; the downstream triage pass gets an error marker it can decide to act on (re-extract later, ask the operator, ignore).

### 4.5 Concurrency

Within a single item: enrichers run sequentially. Items are small; the marginal latency of running 3 enrichers in series is dwarfed by the connector's fetch cycle. Sequential ordering is deterministic and easy to test.

Across items: the existing connector concurrency model (fetch loop, worker pool, framework runner) is unchanged. Enrichment piggybacks on whatever the connector already does.

### 4.6 Metadata schema additions

Extends `internal/staging.ItemMetadata`:

```go
type ItemMetadata struct {
    // ... existing fields ...

    // Enrichments is the list of sidecar artifacts produced (or attempted
    // and failed) by the enrichment pipeline. Populated by StagingItem.Commit().
    // Empty for items committed before this field existed (additive schema change).
    Enrichments []EnrichmentRecord `json:"enrichments,omitempty"`
}

type EnrichmentRecord struct {
    Producer   string `json:"producer"`              // enricher Name()
    Kind       string `json:"kind"`                  // artifact kind
    Source     string `json:"source"`                // source filename (e.g., "content.raw" or "attachment-2-report.pdf")
    Filename   string `json:"filename,omitempty"`    // produced sidecar filename (empty if Status="failed")
    Status     string `json:"status"`                // "ok" | "failed"
    Error      string `json:"error,omitempty"`       // present iff Status="failed"
}
```

Downstream consumers can introspect `Enrichments[]` to know what's available without scanning the directory.

### 4.7 Backend parity: enrichment always runs connector-side

A connector commits items through one of two `StagingBackend`s: the
filesystem backend (writes the item directory directly into the staging
dir) or the `HTTPStagingBackend` (POSTs the item to the scanner's
`/v1/ingest` endpoint as multipart/form-data). Enrichment **always runs
connector-side**, in `StagingItem.Commit()` (filesystem) and in
`HTTPStagingBackend.commitHTTP()` (HTTP), via the same
`runEnrichmentPipeline`. The ingest server does **not** enrich; it stores
received artifacts verbatim.

Rationale: the ingest handler historically wrote only `content.raw` +
`metadata.json` and never invoked the pipeline, so HTTP-backend items were
committed **completely unenriched** while filesystem items were enriched —
an asymmetry downstream consumers could not detect (glovebox-afq4.12). The
fix runs the pipeline on both paths so `metadata.json.Enrichments[]` is
populated for **both** backends; openclaw's triage pass (top-level spec
§7.1) can rely on the field regardless of how the item was delivered.

Wire format (HTTP backend). The multipart request gains zero or more
`sidecar` parts in addition to the existing `metadata` (JSON) and `content`
(`content.raw`) parts:

```
metadata   : application/json          (ItemMetadata incl. Enrichments[])
content    : application/octet-stream  filename=content.raw
sidecar    : application/octet-stream  filename=content.extracted.md
sidecar    : application/octet-stream  filename=content.<enricher>.error.md
sidecar    : ...                       (one per enrichment artifact/marker)
```

The connector sends every file in the item directory except `content.raw`
and `metadata.json` (which are their own parts) as a `sidecar` part. The
ingest handler validates each sidecar filename strictly (a bare filename:
no path separators, no `..`, not a reserved name) before writing it into
the item directory, so a malformed or hostile filename cannot escape the
staging tree. The result is that the staged item directory is identical
whichever backend produced it.

Binary-enricher availability. Connectors using the HTTP backend run the
same enrichers as the filesystem path; a connector whose image lacks a
binary enricher's tool (e.g. a distroless connector without `pandoc`)
degrades exactly as the filesystem path does — the pure-Go enrichers
(passthrough, html) still run and the binary enrichers write WHAT/CHECK/FIX
error markers (§4.4) rather than failing the commit. Connectors that need
binary enrichment of attachments should base on `glovebox-enricher-runtime`
(§6.1).

## 5. First-party enrichers (v1)

| Package | Mode | Applies-on | Output | Implementation |
|---|---|---|---|---|
| `connector/enrich/passthrough` | pure-Go | `meta.ContentType` is `text/plain*`, `text/markdown`, or empty + sniffs as text | `content.extracted.md` (identity copy with .md extension; lets the downstream `**/*.md` glob index uniformly) | reads source, writes to `<source-basename>.extracted.md` |
| `connector/enrich/html` | pure-Go | `text/html*` | `content.extracted.md` (text-only) | wraps existing `connector/content/html.go` HTML-to-text |
| `connector/enrich/pdf` | requires `pdftotext` | `application/pdf` | `content.extracted.md` (text content) | `os/exec` invokes `pdftotext -layout -enc UTF-8 - -` (stdin → stdout) |
| `connector/enrich/ocr` | requires `tesseract` | `image/jpeg`, `image/png`, `image/heic`, `image/tiff` | `content.extracted.md` (OCR'd text; may be empty if image has no text) | `os/exec` invokes `tesseract - - -l eng` |
| `connector/enrich/office` | requires `pandoc` | OOXML: `application/vnd.openxmlformats-officedocument*` (.docx, .xlsx, .pptx) | `content.extracted.md` (markdown rendering) | `os/exec` invokes `pandoc -f <fmt> -t markdown`. Legacy `.doc` / `.xls` / `.ppt` (non-OOXML `application/msword`, `application/vnd.ms-excel`) are NOT supported by pandoc directly; they fall through to no-enrichment in v1 and produce a `content.office.error.md` marker explaining the limitation and the FIX (convert to OOXML at the source or wait for a future libreoffice-headless enricher). |

### 5.1 Per-enricher init() check

Each binary-dependent subpackage performs a one-time `exec.LookPath` check in its `init()`:

- Binary present → register with `Default`. Enricher is active.
- Binary absent → log a structured warning at process start; do NOT register. The enricher is silently inactive for this image.

Init warning follows the error-message discipline:

```
enrich/pdf: pdftotext not found in PATH; the PDF enricher is disabled for this connector.
  CHECK: docker inspect <image> for /usr/bin/pdftotext
  FIX:   rebase this connector on glovebox-enricher-runtime, or accept that PDFs will not be text-extracted.
```

### 5.2 Multi-part / attachments

Email connectors (gmail, imap, outlook) and the mbox importer write the message body to `content.raw` and each attachment as `attachment-<n>-<safe-filename>` (existing pattern). The pipeline:

1. Runs the registry on `content.raw` → may produce primary `content.extracted.md`.
2. Iterates over `attachment-*` files, running the registry on each → may produce `content.attachment-<n>.extracted.md` per attachment.

`meta.Enrichments[]` records the source filename for each artifact so the consumer can correlate sidecar → source.

## 6. Image strategy

### 6.1 New base image: `glovebox-enricher-runtime`

`Dockerfile.enricher-runtime` (in glovebox repo root or `image/` subdir; final path is an implementation-plan decision):

```dockerfile
FROM docker.io/debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      poppler-utils \
      tesseract-ocr \
      tesseract-ocr-eng \
      pandoc \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN groupadd -g 65532 nonroot && useradd -u 65532 -g 65532 -s /usr/sbin/nologin nonroot
USER nonroot:nonroot
```

Expected size: ~150–200 MB. The image is published as `registry.orac.local/glovebox/enricher-runtime:<tag>` (or whatever the glovebox image registry convention is; confirm during implementation).

### 6.2 Per-connector rebase matrix (v1)

| Connector | Base image | Pure-Go enrichers | Binary enrichers | Rationale |
|---|---|---|---|---|
| gmail | enricher-runtime | passthrough, html | pdf, ocr, office | live email, attachment-heavy |
| imap | enricher-runtime | passthrough, html | pdf, ocr, office | live email, attachment-heavy |
| outlook | enricher-runtime | passthrough, html | pdf, ocr, office | live email, attachment-heavy |
| mbox-importer | enricher-runtime | passthrough, html | pdf, ocr, office | 20-year backfill is the driver use case |
| arxiv | enricher-runtime | passthrough, html | pdf | items are predominantly PDFs |
| semantic-scholar | enricher-runtime | passthrough, html | pdf | same |
| rss | distroless/static | passthrough, html | — | HTML-heavy, no attachments |
| hackernews | distroless/static | passthrough, html | — | HTML/text only |
| github, gitlab | distroless/static | passthrough | — | structured API responses |
| linkedin, bluesky, x | distroless/static | passthrough, html | — | text + HTML |
| notion, jira, schoology, gcalendar, gdrive, onedrive, teams, steam, meta | (decide per connector; default: stay distroless) | depends | depends | each Dockerfile change carries a one-line rationale |

Default for ambiguous cases: stay distroless. Promote a connector to enricher-runtime only when there's a concrete need.

## 7. Test plan

### 7.1 Unit tests (go test, in CI)

Per-enricher table-driven tests under `connector/enrich/<name>/<name>_test.go` with fixtures at `connector/enrich/<name>/testdata/<scenario>/`. Each scenario has an input file plus the expected `content.extracted.md`.

Per-enricher scenario coverage:

- **passthrough:** text/plain identity; empty content; UTF-8 with non-ASCII; very large content (chunking semantics if any).
- **html:** simple paragraph; nested lists; tracking pixels stripped; broken HTML; HTML with embedded scripts (assert scripts dropped).
- **pdf:** text PDF; image-only PDF (assert empty output + no error); password-protected PDF (assert produces an error marker with the WHAT/CHECK/FIX message); corrupt PDF (assert error marker, not panic).
- **ocr:** image with English text; image with no text (assert empty output, no error); image too small for OCR (assert documented message).
- **office:** .docx text; .xlsx tabular content; .pptx text; corrupt file (assert error marker).

Binary-dependent tests are guarded by a `//go:build enrichruntime` tag so they only run in CI on the enricher-runtime image. Pure-Go tests run on the default base. Both run under the same `go test ./connector/enrich/...` invocation; the build tag handles selection.

**Pipeline / framework tests** at `connector/enrich/pipeline_test.go`:

- Empty registry → no sidecars, no errors.
- Registered enricher matches → expected sidecar appears; artifact metadata correct.
- Registered enricher does NOT apply → no sidecar.
- Enricher returns error → marker file written; `meta.Enrichments[]` records `Status: "failed"`; other enrichers still run.
- Multiple enrichers match the same source → both produce sidecars with distinct filenames.
- Multi-part item with attachments → primary `content.extracted.md` from body; per-attachment sidecars; `meta.Enrichments[].Source` correctly distinguishes.

**Error-message tests** (cross-cutting): every documented error message has a test asserting the exit/return-error AND that the message string matches the WHAT/CHECK/FIX shape.

### 7.2 Container test extensions

Extend the existing `container_test.sh` with:

- For each connector rebased on enricher-runtime: assert `pdftotext`, `tesseract`, `pandoc` are present in the running image (`docker run ... which pdftotext`).
- For each connector: run a synthetic fetch (mocked source or test-fixture archive), inspect the resulting staging item directory, assert the expected sidecar set.
- Negative case: assert distroless connectors do NOT have the binaries available (catches an accidental rebase that bloats the image).

### 7.3 End-to-end smoke

Script `scripts/smoke-enrichment.sh` (this repo):

1. Spins up the gmail connector container against a fixture archive (small mbox with a known PDF, image, and HTML email).
2. Waits for staging items to appear.
3. Asserts each item dir has the expected sidecar set.
4. Hash-compares sidecar contents to expected fixtures.
5. Cleans up.

Runs in CI nightly OR on-demand. Exit non-zero on any FAIL; each PASS/FAIL line obeys the error-message discipline.

### 7.4 Test discipline going forward

Every commit touching `connector/enrich/`, the staging schema, or a rebased connector Dockerfile MUST update or add the relevant test case (go test for code, container_test.sh for image content, smoke for end-to-end). A commit without a verifiable, automatable test plan does not ship.

## 8. Error-message discipline

Every non-zero return error, every assertion-failure message in tests, and every init-time warning MUST follow the shape:

```
<scope>: WHAT failed[: WHY (if known)]
  CHECK <command-to-diagnose>  (optional, when applicable)
  FIX   <command-or-action>     (optional, when there's an obvious next step)
```

Examples:

```
enrich/pdf: pdftotext returned exit code 2 on attachment-3-report.pdf.
  Likely cause: password-protected PDF, no extractable text.
  CHECK pdfinfo /staging/<id>/attachment-3-report.pdf
  FIX   if the PDF is operator-meaningful, decrypt at the source connector before staging.
```

```
enrich/ocr: tesseract returned no text from image-only attachment.
  This is normal for decorative images. Not an error.
```

```
smoke: FAIL pdf-extraction
  fixture     /testdata/email-with-pdf.mbox  produced no content.extracted.md from attachment-3
  expected    sidecar present, size > 0
  CHECK docker logs <gmail-container>
  FIX   inspect the pdf enricher init() output in the container start logs for missing-binary warnings.
```

A shared helper package (`internal/errfmt` or similar) provides the constructors; direct `fmt.Errorf` with a bare string in enricher code is rejected in code review.

## 9. Dependencies and phasing

### 9.1 Hard dependencies (none external)

This spec is level-2 in the OpenClaw memory-ingestion dependency chain; its consumer is the top-level openclaw spec at `docs/superpowers/specs/2026-06-01-glovebox-to-memory-ingestion-design.md` (in the openclaw repo). This spec has no specs above it.

### 9.2 Phasing (bottom-up TDD)

1. Add the `enrich.Enricher`, `Artifact`, `Registry`, `Default` types + tests for the registry semantics (register, apply-all, error collection). Red → green.
2. Wire `enrich.Default.ApplyAll(...)` into `StagingItem.Commit()`. Add the `meta.Enrichments[]` schema. Tests for the empty-registry and one-enricher cases.
3. Implement `passthrough` enricher + tests.
4. Implement `html` enricher (wrap existing helper) + tests.
5. Build `Dockerfile.enricher-runtime` + CI job. Verify image size and binary presence.
6. Implement `pdf`, `ocr`, `office` enrichers + binary-tag-guarded tests. CI runs them against the enricher-runtime image.
7. Rebase gmail, imap, outlook, mbox-importer Dockerfiles on enricher-runtime. Add the binary-dependent enrichers to their main.go imports.
8. Rebase arxiv, semantic-scholar on enricher-runtime; add `pdf` enricher to their imports.
9. Extend `container_test.sh` and write `scripts/smoke-enrichment.sh`.
10. Branch + MR for the framework + pure-Go enrichers (per the project's MR policy for non-trivial changes).
11. Separate branch + MR for the enricher-runtime image + binary enrichers + connector rebases.
12. After merge: smoke against a real gmail or imap test mailbox to flush out content-type edge cases.

## 10. What this spec retires

| Thing | Status after this spec |
|---|---|
| Per-connector ad-hoc HTML-to-text invocation (where it exists) | The `html` enricher is the canonical caller of `connector/content/html.go`; per-connector code that bypassed the framework can be removed |
| The implicit "attachments are opaque binaries" assumption in downstream consumers | Retired by `meta.Enrichments[]` exposing what was extracted from each |
| The OpenClaw-side `patrols/glovebox-finds-*.md` brittle bridge | Indirectly: the downstream openclaw spec's memory-wiki ingestion path replaces this once enrichment is producing `content.extracted.md` |

## 11. Acceptance criteria

This spec's implementation plan is done when ALL of the following hold:

- Every item written by gmail, imap, outlook, and the mbox importer has a `content.extracted.md` sidecar after `Commit()` returns successfully, AND `meta.Enrichments[]` records what was produced.
- Enrichment failures produce a `content.<enricher>.error.md` marker, populate `meta.Enrichments[].Status="failed"`, and DO NOT abort the item commit. The marker message follows the WHAT/CHECK/FIX shape.
- Pure-Go enrichers (`passthrough`, `html`) run on distroless connector images without any binary dependencies.
- Binary-dependent enrichers (`pdf`, `ocr`, `office`) run on enricher-runtime connector images and are gracefully disabled (with an init-time warning, not a per-item failure) on images that lack the binaries.
- `go test ./connector/enrich/...` passes in CI on both base images (build-tag selection working correctly).
- `container_test.sh` asserts binary presence/absence per-connector matching the rebase matrix.
- `scripts/smoke-enrichment.sh` runs end-to-end against a fixture archive and asserts the expected sidecars.

## 12. Open questions (flagged, not blocking)

1. **Vision-LLM caption enricher.** Anticipated as the next class member after v1 lands. Will require LiteLLM connectivity from inside the connector container — adds a network dependency to enrichment for the first time. Separate small spec.
2. **Speech-to-text enricher (audio attachments).** Same shape; requires whisper.cpp or a hosted ASR model. Defer until there's an actual audio use case.
3. **Per-language OCR.** v1 ships English-only tesseract. Adding other languages is an apt install in the enricher-runtime image; documented in a runbook entry, not a spec change.
4. **Per-attachment sensitivity propagation.** If an attachment's content is more sensitive than the parent email's body (e.g., a medical PDF attached to a casual email), enrichers could surface a sensitivity hint in the artifact. v1 leaves this entirely to the downstream consumer; flagged as future work.
5. **OCR opt-out for high-throughput backfill.** Tesseract is the slowest enricher (~1–3s per image). A bulk-import flag to skip OCR is plausible later; v1 always runs every applicable enricher.

## 13. Cross-references

- `docs/superpowers/specs/2026-06-01-glovebox-to-memory-ingestion-design.md` (openclaw repo) — top-level consumer of this spec's output. §8 of that spec defines the contract this spec satisfies.
- `docs/specs/05-connector-framework-design.md` — connector framework architecture being extended here.
- `docs/specs/09-mbox-importer-design.md` — the mbox importer; one of the connectors rebased on enricher-runtime.
- `docs/specs/13-archive-delivery-design.md` — archive-scale delivery used by the mbox importer; unchanged by this spec.
- `connector/content/html.go`, `connector/content/mime.go` — existing pure-Go helpers leveraged by the new enrichers.
- Saved beads memories: `workflow-glovebox-mr-policy`, `informative-error-messages-with-next-steps`.

---

Open for review.
