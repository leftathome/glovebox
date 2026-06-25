# Glovebox session handoff — 2026-06-25

Beads carry the authoritative status (`bd ready`, `bd show <id>`); this doc adds
narrative + a recommended pickup order for the next session.

## TL;DR

- **`main` is `8013c0b`, identical on GitLab (origin, primary) and GitHub.**
  Working tree clean except an untracked `mbox` test artifact (ignore it).
- `go test ./...` (+ `-race` on the touched packages) and `go vet` are green;
  `helm lint` passes on all charts.
- This session closed **7 beads** and filed several precise follow-ups. No live
  blockers — pick any ready bead below.

## Landed this session (closed)

- **`glovebox-npsj`** — gitlab-first CI pipeline. Pipeline 816 validated all 31
  jobs (test, build-base, 28 kaniko images, OCI chart). Fixed the chart job
  (`helm registry login --plain-http`, ci-templates **v0.1.1**). Merged; closed
  `glovebox-i8nd` (image-coverage gap) too.
- **`glovebox-0nzk`** — PII scrub of public artifacts. Removed household eids +
  de-pseudonymizing names from charts/configs/tests/docs (defaults now neutral:
  `subjects.json` empty/`enforce:false`, `data_subject_default:""`). **History
  rewritten** (git filter-repo) on GitLab (all refs+tags) and GitHub
  (main+branches); ghcr chart roster versions + eid-bearing image packages
  purged. Cut a clean **v0.6.1** to replace the withdrawn v0.6.0 (see gotcha
  below).
- **`glovebox-rbpt`** — scanner fsnotify-overflow → polling stall. Decoupled the
  watcher discovery loop from the blocking scan-pool dispatch (feeder goroutine
  + non-blocking enqueue); discovery never blocks → no overflow, polling keeps
  draining.
- **`glovebox-v815`** — ordered items were only delivered at SIGTERM. Result
  consumer now flushes ordered items every poll interval (`pipeline.ConsumeResults`).
- **`glovebox-lnzp`** — one hung delivery deadlocked the pipeline. `pipeline.WithTimeout`
  bounds each delivery (`DeliveryTimeoutSeconds`, default 30s, chart-configurable);
  on timeout the item is left in staging + counted via `glovebox_delivery_timeouts_total`.
- **`glovebox-544`** — mbox importer e2e integration test (`importers/mbox/e2e_test.go`,
  in-process via new `runCtx`). Surfaced two real bugs (92qr below + gtxt).
- **`glovebox-92qr`** — mbox resume dropped in-flight messages (at-most-once).
  Now resumes from a **low-water mark** over the inflight set → at-least-once.
  e2e `TestE2E_InterruptResume` asserts 13/13 delivered (was 10/13).

## Recommended pickup order (next session)

### Highest value, actionable code

1. **`glovebox-gtxt`** (P3 bug, **fresh context**) — `parser.NewScanner` reports
   byte offsets relative to the scanner start, so after a resume seek the
   `origin_archive` provenance tag (and a 2nd-interruption resume offset) are
   relative/wrong. Not data loss (92qr's low-water fix keeps single-interruption
   correct), but provenance is misleading. Fix: make offsets absolute (seed the
   scanner with the seek base, e.g. `NewScannerAt(r, startOffset)`), then
   `TestE2E_InterruptResume` can assert `origin_archive` uniqueness too (it
   currently keys on subject and documents the caveat). Self-contained.

2. **`glovebox-fslv`** (P2 bug, security) — `govulncheck` v1.4.0 segfaults on our
   generics under go1.26; security-scan is currently non-gating
   (`continue-on-error`). Worth re-checking whether a newer `govulncheck` /
   x/vuln release fixes the generics crash; if so, bump it and re-enable gating.
   May still be upstream-blocked — investigate first.

3. **`glovebox-afq4.12`** (P2, **needs operator sign-off first**) — HTTP-backend
   items are **completely unenriched** (NOT server-enriched, as the bead
   originally assumed — see the bead note from this session). The ingest server
   writes `content.raw`+`metadata.json` directly with no enrichment. The clear
   fix is option 1 (enrich connector-side in `commitHTTP`), which requires a
   multipart **wire-format change** (send `content.<name>.md` sidecars; ingest
   handler persists them) + spec 14 §4.3/§4.7 update + the HTTP-backend
   connectors carrying the enricher-runtime binaries. **Get sign-off on the wire
   change before implementing.**

### Needs cluster / operator

4. **`glovebox-3d4m`** (P1) — deploy + run `scripts/archive-smoke-test.sh`. This
   is a live-cluster operator task (deploy glovebox + run the smoke test); not
   safely autonomous from a dev session.

### Other ready P2/P3 (autonomous-friendly)

- `glovebox-c9zt` (P2) — mbox-importer archive-event watcher mode (spec 13 §5.3
  consumer); a feature.
- `glovebox-g499` (P2) — pprof + streaming-audit for multi-GiB upload memory
  profile; investigation/instrumentation.
- `glovebox-5o6v` (P2) — browserless NetworkPolicy allow-consumers → glovebox ns
  (k8s YAML; relates to `glovebox-txla`).
- `glovebox-afq4.13` (P2) — bump pandoc ≥3.5 in enricher-runtime. **Caution:**
  building the enricher-runtime image is OOM-prone on the johnny node under
  concurrent load (`glovebox-ozxr`); build it in isolation.
- `glovebox-ozxr` (P3) — enricher-runtime image builds OOMKill (exit 137) under
  concurrent load on johnny; raise per-job memory or cap heavy-build concurrency
  in `homelab/ci-templates`.
- `glovebox-afq4.14..17` (P3) — content-enrichment epic cleanup (LookPath pattern,
  pin enricher-runtime tag, CI wiring, per-attachment content-type sniffing).
- Smaller P3s: `ryvb`, `72gk`, `wilt`, `yrp5`, `glwf`, `nabc`.

## Conventions / gotchas

- **Dev workflow:** branch → commit → push to **origin (GitLab)** → `glab mr
  create` → `glab mr merge --remove-source-branch` → `git checkout main && git
  pull origin main` → `git push github main`. GitLab is primary; GitHub mirrors.
  Stage only the task's files (the tree may carry unrelated WIP).
- **GitHub immutable releases (IMPORTANT):** this repo permanently reserves a
  release tag name once published as an immutable release — and disabling the
  feature does **not** free already-burned names. You cannot move/recreate an
  old release tag (e.g. v0.6.0); cut the next version instead. Also,
  `release.yml` builds on **any** `v*` tag push — never push a `v*`-shaped probe
  tag (it spawns a real immutable release). See memory
  `reference_github_immutable_releases`.
- `.beads/` is gitignored; `bd` prints a benign `auto-export: git add failed`
  warning — ignore it.
- `gofmt -l` flags a few pre-existing files (untar.go, worker.go, some _test.go)
  with formatting that differs from the local gofmt version — not introduced by
  this session; leave them unless you're editing them.
- CI (`.gitlab-ci.yml` on main) builds 28 images + chart; runs are ~30-40 min on
  the johnny-pinned runner. MR `test` job is the gate.

## New follow-ups filed this session

`glovebox-gtxt` (relative byte offsets), `glovebox-ozxr` (enricher-build OOM),
`glovebox-afq4.12` note (HTTP items unenriched — corrected premise). All have
detailed reproduction + fix-direction notes in their bead descriptions.
