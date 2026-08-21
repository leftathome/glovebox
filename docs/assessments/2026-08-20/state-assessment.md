# Glovebox — State Assessment: Goals vs. Capabilities

**Date:** 2026-08-20 · **HEAD:** `a0a6a58` (2026-08-05) · **Latest release:**
0.6.4 (2026-06-26) · **Helm chart:** 0.7.0 (appVersion pinned 0.6.1).

## Executive summary

Glovebox is a **mature, well-tested project whose code substantially exceeds
its front-door documentation.** Every numbered design spec (04–15) plus the
three dated "superpowers" specs is implemented and tested; all 24 connector
directories contain real, working connectors (not scaffold stubs); the build,
vet, and full test suite are green. The project **meets or exceeds its stated
goals on capability** — the gaps are almost entirely *documentation drift in one
direction* (docs lag behind code), plus a set of **scanner-efficacy weaknesses**
(covered in the security review) where the implementation is weaker than the
design claims.

The strategic picture (competitive report): glovebox's niche —
deterministic, offline, connector-integrated ingestion scanning with human
quarantine and byte-identical delivery, packaged for self-hosted OpenClaw — is
**partially served but not superseded**. OpenClaw core declined to build this
(issue #7705, closed not-planned); the two closest 2026 active projects
(pipelock, Vault/vaultmcp) guard *network traffic* and *MCP responses*, not
out-of-band connector feeds; and two prior analogs (llm-guard, Rebuff) were
archived in 2025–2026, widening the gap. But the field's consensus has moved:
pattern detection is understood to be bypassable and best used as
telemetry+quarantine-triage, with the real boundary being capability/policy
control outside the model. Glovebox's quarantine-for-review framing fits that,
but it must not market itself as a complete defense.

## 1. Stated goals (from README + spec 04)

- Deterministic, **LLM-free** content firewall between external connectors and
  OpenClaw agent workspaces.
- Weighted pattern matching (regex/substring/custom detectors); above-threshold
  → quarantine for human review.
- **Content never modified** — arrives and leaves byte-identical.
- **No scanner egress** (amended by spec 08's inbound ingest API).
- **Append-only audit log**; nothing reaches an agent unscanned.
- A **connector framework** so authors write only fetch logic.

## 2. Documented roadmap

Numbered specs 04–15 (01–03 live in the parent OpenClaw repo): core scanner
(04), connector framework (05), connector auth & provenance (06), fetch
controls (07), HTTP ingest API (08), mbox importer (09), external ingest auth
(10), data subject & audience (11), Schoology connector (12), archive delivery
/ tus (13), content enrichment (14), health provenance & subject resolution
(15). Post-15 dated specs: mbox archive watcher, connector integration harness,
sanitize gate (`POST /v1/sanitize`). 13 implementation plans track these.

**Explicitly-future (planned, not missing):** Phase-2 LLM classifier, rules
hot-reload, per-source staging rate limits (spec 04 §16); ETag/If-Modified-Since
(spec 07); more importer formats (spec 09); medical-care audience vocabulary and
audience-aware routing (specs 11, 15); walhelm SP2/SP3, per-item subject
manifests, automatic quarantine re-drive (spec 15).

## 3. Capabilities vs. goals — verdict per spec

Every spec 04–15 and all three superpowers specs are **Implemented and tested**
(full evidence table in the detailed appendix `state-assessment-raw.md`).
Highlights:

- **Scanner core (04):** watcher (fsnotify+polling), engine + detectors
  (encoding, language via lingua-go, template), routing (pass/quarantine/reject,
  pending placeholders, failed-retry), append-only audit, OTel→Prometheus
  metrics, active `/healthz` (delivery-mount write probe) + `/readyz`.
- **Framework (05–07):** Connector/Watcher/Listener interfaces, checkpoints,
  identity/token, rate-limit/robots/link-policy/fetch-limit.
- **Ingest & scale (08–13):** `/v1/ingest`, Vault-backed bearer auth for
  archives, subject/audience (fail-closed gate), Schoology (6.3k-LOC connector),
  tus archive delivery verified to 12 GiB.
- **Enrichment & health (14–15):** text extraction (HTML/PDF/OCR/office) with a
  CI smoke harness; source registry + acquisition identity. SP2/SP3 deferred by
  design.
- **Post-15:** mbox watcher mode, connector integration harness (5 connectors
  with live tests behind build tags), sanitize gate (OpenAPI 3.0.3 +
  oapi-codegen + conformance test + codegen-drift CI gate).

**Only genuinely unimplemented items are the explicitly-future Phase-2 set**
(LLM classifier, hot-reload, per-source staging rate limit) and spec 07 ETag —
all labeled future in the specs, so roadmap, not drift.

## 4. Connector inventory (24 dirs)

All 24 are real, API-hitting implementations with offline fake-driven tests
(168 `_test.go` files against 208 sources). **No auth-method or capability
discrepancy** found between `docs/connectors/*.md` and code across all 24.
Modes: 20 Poll-only, imap adds Watch (IDLE), github/meta add Listen (webhook),
schoology does all three; schoology-auth-refresher is a CronJob helper.
Live/integration tests exist for 5 (arxiv, hackernews, rss, schoology,
semantic-scholar); the other 19 are honestly documented as "live test:
follow-up."

## 5. Gap list (documentation & process; severity in [ ])

1. **[HIGH]** README materially understates the project and has **wrong install
   instructions**: lists 10 connector images (24 exist, 28 built in CI); says
   "Connectors (all 10)" and "IMAP and RSS (Round 1)"; `helm install
   --version 0.2.0` vs chart 0.7.0; key-features omit ingest API, archive
   delivery, sanitize gate, enrichment, subject/audience, importers.
   (README.md:44–53, 160–162, 170–173, 178, 191)
2. **[HIGH]** `release.yml:39` builds only glovebox + the original 10
   connectors, contradicting README's "each archive contains all connector
   binaries" — 14 connectors and all importers are missing from release
   archives.
3. **[MED]** README:217 calls `docs/connector-guide.md` "coming soon"; it
   exists as a full 9-section guide.
4. **[MED]** `docs/deployment.md:39–50` lists only the 10 original images.
5. **[MED]** CHANGELOG has **no sanitize-gate entry** though `/v1/sanitize`
   merged 2026-07-03 — it would ship unmentioned in the next release notes.
6. **[MED]** No release since 2026-06-26 despite sanitize gate, daemon health
   probes, and chart registry override all merged; chart appVersion pinned at
   0.6.1 while latest release is 0.6.4.
7. **[MED]** Stale root PLAN files: `PLAN-onedrive-connector.md` (5/6 boxes
   unchecked) and `PLAN-teams-connector.md` (all status boxes unchecked)
   describe finished, tested, imaged work as in-progress.
8. **[MED]** `docs/specs/nagus-connector-integration.md` still directs building
   `cmd/connector-ebay` + a Craigslist feed; the sanitize-gate spec §2 records
   that decision was reversed, but the older doc has no amendment.
9. **[MED]** `.github/workflows/docker.yml` is a legacy partial duplicate
   (builds 3 images on `v*` tags while ci.yml builds all 28) — redundant
   double-builds, confusing source of truth.
10. **[LOW]** examples/ are v0.2-era recordings (2026-03-31; helm example shows
    tag 0.2.0); no example for ingest, archives, sanitize, or enrichment.
11. **[LOW]** No connector dir has a README.md though the generator emits one
    and README:212 promises it.
12. **[LOW]** Live integration tests cover only 5 of 23 source connectors
    (honestly tracked as follow-ups).
13. **[LOW]** README:52 "notification placeholders" understates the implemented
    structured-JSON notifications (`internal/routing/notify.go`).
14. **[LOW]** Phase-2 items (spec 04 §16) and spec 07 ETag unimplemented —
    explicitly future, roadmap not drift.

**Overall:** capabilities ≥ stated goals everywhere checked; documentation
drift runs one direction (docs behind code, never over-claiming) except the two
misleading install-path items (#1, #2), the unchecked PLAN files (#7), and the
superseded nagus work items (#8). A full evidence appendix — per-connector
table and per-spec status with file citations — is in `state-assessment-raw.md`.
