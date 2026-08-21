# Glovebox State Assessment — 2026-08-20 (agent raw output)

**Repo:** `/home/user/glovebox` · HEAD `a0a6a58` (2026-08-05) · latest CHANGELOG release **0.6.4** (2026-06-26), plus an Unreleased section · Helm chart version 0.7.0.

**Bottom line:** This is a mature, well-tested project whose *code* is substantially ahead of its *front-door docs*. Every numbered spec (04–15) plus the three superpowers specs is implemented and tested; all 24 connector dirs contain real, working connectors. The main debt is staleness: README, `docs/deployment.md`, release/CI binary lists, examples, and three root PLAN files still describe the ~10-connector, v0.2-era project.

---

## 1. Stated goals and roadmap

**Mission** (README.md:9–25, docs/specs/04-glovebox-design.md §1): a deterministic, LLM-free content-scanning service between external data connectors and OpenClaw agent workspaces. Weighted pattern matching (regex/substring/custom detectors) scores content; above-threshold items are quarantined for human review; content is never modified; the scanner has no network egress (amended by spec 08's inbound ingest API); audit log is append-only; nothing reaches an agent unscanned. Plus a connector framework library so authors write only fetch logic.

**Documented roadmap** — numbered specs (04–15 live here; 01–03 live in the parent OpenClaw repo, per spec 04's header):

| Spec | Subject |
|---|---|
| 04 | Core scanner design (watcher, engine, routing, audit, OCI/Helm) |
| 05 | Connector framework |
| 06 | Connector auth & provenance (incl. §12 Schoology auth refresher) |
| 07 | Fetch controls (rate limits, robots, link policy) |
| 08 | Ingest API (`/v1/ingest`) |
| 09 | Mbox importer |
| 10 | External ingest auth (Vault-backed bearer tokens) |
| 11 | Data subject & audience |
| 12 | Schoology connector |
| 13 | Archive delivery (tus `/v1/archives`) |
| 14 | Content enrichment |
| 15 | Health provenance & subject resolution (SP1) |

Superpowers specs (dated, post-15 work): mbox archive watcher (2026-06-25), connector integration harness (2026-06-29), sanitize gate `POST /v1/sanitize` (2026-07-02). 13 implementation plans in `docs/superpowers/plans/` track these. `docs/specs/nagus-connector-integration.md` is a design record for the glovebox↔nagus boundary.

**Explicitly-future work per specs** (planned, not promised-and-missing): Phase 2 LLM classifier, rules hot-reload, per-source staging rate limits (spec 04 §16); ETag/If-Modified-Since (spec 07 §3.4); more importer formats + external Windows-side invocation (spec 09 §6); medical-care audience vocabulary and audience-aware routing lanes (spec 11 §2.2, spec 15 §2.2); walhelm SP2/SP3 (walhelm-go, recognizer fetch loop), per-item subject manifests, automatic quarantine re-drive (spec 15 §10).

---

## 2. Connector inventory (24 dirs)

All 24 are **real implementations, not scaffold stubs** — each hits a genuine API (verified endpoints in code), has table-driven tests against fake/mock servers, and passes `go test` (all 24 packages green, run 2026-08-20). None contains TODO/stub fetch logic (only 4 benign "candidate for extraction" comments in schoology).

| Connector | LOC (go) | Tests | Modes | Dockerfile | In-dir README | docs/connectors/*.md | Doc↔code auth match |
|---|---|---|---|---|---|---|---|
| arxiv | 794 | 430 lines + **live** | Poll | Y | **N** | Y | Y (no creds) |
| bluesky | 733 | 379 | Poll | Y | N | Y | Y (`BLUESKY_ID`/`APP_PASSWORD`) |
| gcalendar | 635 | 354 | Poll | Y | N | Y | Y (Google OAuth) |
| gdrive | 662 | 370 | Poll | Y | N | Y | Y |
| github | 699 | 370 | Poll + Listen (webhook HMAC) | Y | N | Y | Y |
| gitlab | 715 | 417 | Poll | Y | N | Y | Y |
| gmail | 1016 | 614 | Poll | Y | N | Y | Y |
| hackernews | 844 | 497 + **live** | Poll | Y | N | Y | Y (no creds) |
| imap | 1055 | 613 | Poll + Watch (IDLE) | Y | N | Y | Y (`IMAP_HOST/USERNAME/PASSWORD`) |
| jira | 699 | 385 | Poll | Y | N | Y | Y |
| linkedin | 582 | 324 | Poll | Y | N | Y | Y |
| meta | 757 | 398 | Poll + Listen | Y | N | Y | Y |
| notion | 875 | 472 | Poll | Y | N | Y | Y |
| onedrive | 631 | 357 | Poll (Graph delta) | Y | N | Y | Y (MS_* vars) |
| outlook | 849 | 508 | Poll | Y | N | Y | Y |
| rss | 1211 | 758 + **live** | Poll | Y | N | Y | Y (no creds) |
| schoology | 6276 | 15 files, 3563 lines + **live** | Poll + Watch + Listen | Y | N | Y | Y (`SCHOOLOGY_TRIGGER_TOKEN`, Vault session cookies) |
| schoology-auth-refresher | 683 | 338 | n/a (CronJob auth helper) | Y | N | **N** (intentional) | — |
| semantic-scholar | 809 | 487 + **live** | Poll | Y | N | Y | Y |
| steam | 873 | 479 | Poll | Y | N | Y | Y |
| teams | 728 | 388 | Poll | Y | N | Y | Y |
| trello | 763 | 464 | Poll | Y | N | Y | Y |
| x | 800 | 430 | Poll | Y | N | Y | Y |
| youtube | 1184 | 732 | Poll | Y | N | Y | Y |

Doc quality is high: `docs/connectors/README.md` accurately lists all 23 source connectors with credential classes; per-connector docs honestly state "live integration test: none yet — follow-up" for the 19 without one. **No auth-method or capability discrepancies** found between docs and code (spot-checked all 24 via env-var and mode comparison).

**README staleness confirmed**: README.md:170–173 names only 10 connector images; README.md:191 says "Connectors (all 10)"; README.md:48 says "First-party connectors for IMAP and RSS (Round 1)". Reality: 24 connector dirs, 28 images built in CI.

**Root PLAN files vs reality**:
- `PLAN-linkedin-connector.md` — all boxes checked; matches implementation. Accurate but redundant with docs.
- `PLAN-onedrive-connector.md` — only the test file is checked; config.go/connector.go/main.go/config.json/Dockerfile boxes are **unchecked**, yet all exist, tested, imaged, documented. Stale.
- `PLAN-teams-connector.md` — files checked but Status (RED/GREEN/verify) all **unchecked**; implementation complete and green. Stale.

---

## 3. Core capabilities: specs vs code

`go build ./...` ✓, `go vet ./...` ✓, `go test ./internal/... ./connector/... ./connectors/...` all green.

| Capability | Spec | Status | Evidence |
|---|---|---|---|
| Staging watcher (fsnotify + polling) | 04 | **Implemented** | `internal/watcher/`, overflow fix (0.6.1, glovebox-rbpt) |
| Heuristic engine, preprocessing, streaming scan | 04 | **Implemented** | `internal/engine/` (matcher, preprocess, scoring, stream), `internal/detector/` (encoding, language via lingua-go, templates), `internal/scan/` shared path |
| Routing: PASS/QUARANTINE/REJECT, pending placeholders, failed retry | 04 | **Implemented** | `internal/routing/` (pass, quarantine, reject, pending, failed, notify) |
| Quarantine notifications | 04 | **Implemented** (not a placeholder) | `internal/routing/notify.go` writes structured JSON to the shared notify dir. README.md:52 still calls these "notification placeholders" — undersells it; actual human-review agent is out-of-repo by design (spec 04 §2) |
| Audit JSONL append-only | 04 | **Implemented** | `internal/audit/logger.go` |
| Metrics (OTel→Prometheus), health | 04/15 | **Implemented** | `internal/metrics/`, `health.go` — active `/healthz` (delivery-mount write probe) + `/readyz` (Unreleased) |
| Connector framework (poll/watch/listen, checkpoints, staging, health, metrics) | 05 | **Implemented** | `connector/` — `Connector`/`Watcher`/`Listener` interfaces at connector.go:10–18 |
| Auth & provenance, identity stamping | 06 | **Implemented** | `connector/identity.go`, `connector/token.go`; schoology-auth-refresher CronJob (§12) |
| Fetch controls (rate limit, robots, link policy, fetch limits) | 07 | **Implemented** | `connector/ratelimit.go`, `robots.go`, `fetchlimit.go`, `connector/content/linkpolicy.go`. ETag remains future per spec §3.4 (consistent) |
| Ingest API `/v1/ingest` | 08 | **Implemented** | `internal/ingest/` wired in main.go:161–162, bearer auth (archives), metrics, queue depth |
| Mbox importer (+ watcher mode) | 09 + 2026-06-25 spec | **Implemented** | `importers/mbox/` incl. `watch.go`, e2e tests; also `importers/apple/`, `importers/walhelm/` |
| External ingest auth (Vault) | 10 | **Implemented** | `internal/ingest/auth/`, Vault k8s auth in `internal/config` |
| Data subject & audience | 11 | **Implemented** | `internal/staging/audience.go`, `internal/subject/` (registry, resolve, fail-closed gate) |
| Schoology connector | 12 | **Implemented** | largest connector (6.3k LOC, 15 test files, live test), parse_status quarantine routing |
| Archive delivery (tus, 30 GiB) | 13 | **Implemented** | `archive_listener.go`, `internal/ingest/archives/`; 12 GiB acceptance run verified (CHANGELOG 0.6.4) |
| Content enrichment | 14 | **Implemented** | `connector/enrich/` (sniff, html, pdf, ocr, office, passthrough), `Dockerfile.enricher-runtime`, CI smoke harness |
| Health provenance & subject resolution (SP1) | 15 | **Implemented** | `internal/source/registry.go`, acquisition identity in archives/finalize.go; SP2/SP3 deliberately deferred |
| Recognizer-scanner lane (`archive/recognizer-scan`, `operator` audience) | plan 2026-06-15 | **Implemented** | `internal/ingest/archives/finalize.go:101–128`, `scan_extract.go`, `internal/staging/audience.go:19–24` |
| Connector integration harness | 2026-06-29 spec | **Implemented** | `connector/integrationtest/`, live tests behind `//go:build integration` in 5 connectors |
| Sanitize gate `POST /v1/sanitize` | 2026-07-02 spec | **Implemented** | `api/openapi.yaml` (OpenAPI 3.0.3), `internal/sanitizeapi/` (oapi-codegen, conformance test, fail-closed), main.go mount behind bearer auth, `docs/sanitize-gate.md`, codegen drift gate in CI |
| Phase 2: LLM classifier, rules hot-reload, per-source staging rate limit | 04 §16 | **Unimplemented (planned-only, by design)** | no reload/SIGHUP path in engine/main; extension points documented |
| nagus work items (`cmd/connector-ebay`, Craigslist feed) | nagus-connector-integration.md | **Deliberately dropped** — sanitize-gate spec §2 records the scope decision; the nagus doc was never updated to say so |

---

## 4. Quality signals

- **Tests:** 168 `_test.go` files vs 208 source files. All packages pass (internal, connector framework + enrich, all 24 connectors). Live/integration tests gated behind build tags; `integration_test.go` + `container_test.sh` at root; enrichment smoke harness in CI.
- **CI (`.github/workflows/ci.yml`):** test job with codegen-drift gate (`scripts/check-codegen.sh`), `go vet`, junit; multi-platform binary builds with SBOM + SLSA provenance attestation; **28-image Docker matrix**; Helm chart packaged/pushed tag-stamped; `security` job with **gating govulncheck** (pinned v1.5.0) + Trivy; `codeql.yml`. A parallel GitLab CI is primary per handoffs (GitLab origin, GitHub mirror).
- **Release process:** `release.yml` on `v*` tags builds archives + checksums + changelog notes. **But** builds only glovebox + the original 10 connectors (release.yml:39) — see gaps.
- **Helm:** `charts/glovebox` (version 0.7.0, appVersion 0.6.1, 25+ templates, 1400-line values.yaml covering all 22 generic connectors + bespoke schoology templates + ESO/Vault, NetworkPolicies, ServiceMonitor); plus `charts/mbox-importer`, `apple-importer`, `mbox-importer-pvc`.
- **Examples:** 4 Showboat-recorded demos, all dated **2026-03-31** (v0.2 era). Commands still match current code, but `examples/03-helm-deployment.md:180` renders image tag `0.2.0`.

---

## 5. Gap list

1. **HIGH — README materially understates the project and contains wrong install instructions.** README.md:170–173 lists 10 connector images (24 exist, 28 built — ci.yml:156–247); README.md:191 "Connectors (all 10)"; README.md:178 `helm install ... --version 0.2.0` vs chart 0.7.0 (charts/glovebox/Chart.yaml:5); README.md:48 "IMAP and RSS (Round 1)"; README.md:44–53 key-features omit the ingest API, archive delivery, sanitize gate, enrichment, subject/audience enforcement, and importers.
2. **HIGH — `release.yml` contradicts README's release claim.** README.md:160–162: "Each archive contains glovebox and all connector binaries"; `.github/workflows/release.yml:39` builds only the 10 original connectors — 14 connectors and all importers missing from release archives. Same 10-connector list in ci.yml:110 (mbox-importer binary absent).
3. **MED — README.md:217 says `docs/connector-guide.md` is "coming soon"; it exists and is a full guide** (9 sections).
4. **MED — `docs/deployment.md:39–50` lists only the 10 original connector images**, omitting the 14 newer ones and enricher-runtime/importer images.
5. **MED — CHANGELOG has no entry for the sanitize gate.** `/v1/sanitize` merged 2026-07-03 (141434e, glovebox-t6fz series) but `[Unreleased]` (CHANGELOG.md:8–44) mentions only health probes and chart registry values.
6. **MED — no release since 2026-06-26 despite significant merged work.** Sanitize gate (07-03), daemon health probes (07-26), chart registry override (08-05) unreleased; chart appVersion pinned at 0.6.1 (Chart.yaml:6) while latest release is 0.6.4.
7. **MED — stale PLAN files at repo root.** PLAN-onedrive (5 of 6 boxes unchecked) and PLAN-teams (all 3 status boxes unchecked) describe finished work as in-progress.
8. **MED — `docs/specs/nagus-connector-integration.md` work items superseded but unmarked.** Sanitize-gate spec §2 records the drop decision; older doc has no status amendment.
9. **MED — `.github/workflows/docker.yml` legacy partial duplicate.** On `v*` tags builds only 3 images while ci.yml builds all 28 — redundant double-builds, confusing source of truth.
10. **LOW — examples/ are v0.2-era recordings** (2026-03-31; helm example shows tag `0.2.0`); no example for ingest, archives, sanitize, or enrichment.
11. **LOW — no connector dir contains a README.md**, though the generator emits one (generator/generate.go:36) and README.md:212 promises it.
12. **LOW — live integration tests cover only 5 of 23 source connectors** (arxiv, hackernews, rss, schoology, semantic-scholar). Honestly documented per-connector as follow-ups.
13. **LOW — README.md:52 "Quarantine with notification placeholders"** understates implemented behavior (`internal/routing/notify.go` writes full structured JSON notifications).
14. **LOW — Phase 2 items from spec 04 §16 remain unimplemented** (LLM classifier, rules hot-reload, per-source staging rate limits) and spec 07 §3.4 ETag caching absent — explicitly labeled future, roadmap not drift.

**Overall:** capabilities ≥ stated goals everywhere checked; documentation drift runs one direction (docs behind code) except the two misleading install-path items (#1, #2), the unchecked PLAN files (#7), and the superseded nagus work items (#8).
