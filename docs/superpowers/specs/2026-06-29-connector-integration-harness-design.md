# Connector In-Cluster Integration/Smoke Harness — Design Specification

**2026-06-29**

*This spec defines sub-project B of the "ship every connector" initiative: a
repeatable, in-cluster test harness that proves each credentialed connector
actually works against its real upstream source, and whose configuration
becomes the canonical sample config shipped for that connector. It is the
load-bearing piece the other sub-projects derive from.*

---

## 1. Purpose

Glovebox has 23 source connectors + an auth helper (`schoology-auth-refresher`,
not itself a source) + 3 importers. Today the connectors' `_test.go` files are
unit tests driven by `httptest`/fakes; none drive a live upstream. The only
file carrying the `//go:build integration` tag is the repo-root
`integration_test.go`, which is a scanner-side *offline* test (no connector,
no live source). The GitLab `test` job is `go test -race` (unit only).

The broader initiative ("every connector ships a built image + a test-derived
sample config + docs; the Helm chart can deploy any published connector")
depends on a trustworthy answer to "does this connector actually work, and
what config makes it work?". This spec defines the harness that answers it by
running connectors against their real sources.

Because several connectors authenticate against credentialed APIs whose
secrets only exist inside the cluster (synced from Vault via ESO), these live
tests must run **in-cluster on GitLab** — GitHub-hosted runners cannot cross
that credential boundary. Image *publishing* (GitHub → ghcr) and credentialed
*integration testing* (GitLab, in-cluster) are therefore deliberately split.

## 2. Scope

### 2.1 In scope

- A shared test harness package (`connector/integrationtest`): helpers to
  stage a connector's output to a temp dir via a real `StagingWriter` and read
  the committed items back, assertion helpers over those items, and opt-in /
  credential skip guards.
- A per-connector **Go integration test** (`//go:build integration`) pattern
  asserting a full stage round-trip against the live source.
- An optional per-connector **container image smoke** (run the shipped image
  against the live source in-cluster), for connectors where artifact/runtime
  verification adds value.
- A **per-connector credential registry** describing each connector's
  test-credential source (dedicated test account vs. operator real account,
  read-only, vs. none), its Vault path, and the secret's shape.
- GitLab CI wiring: a new `integration` stage that runs on a **scheduled
  (nightly) pipeline + manual trigger**, in-cluster, with per-connector ESO
  secrets; unit tests remain the MR/main gate.
- First batch: add a **new live** integration test to `schoology` as the
  reference (its existing fake-driven `_test.go` stays), then the
  no-credential connectors (rss, hackernews, arxiv, semantic-scholar).

### 2.2 Out of scope

- Sample-config + per-connector docs generation (sub-project C) — consumes
  this harness's config as its source of truth; separate spec.
- Publishing all connector images to ghcr (sub-project A / `glovebox-i8nd`).
- Helm chart coverage for all connectors (sub-project D).
- Onboarding *every* connector's integration test in one pass. This spec
  ships the pattern + harness + CI stage + registry, and the first batch;
  remaining connectors onboard incrementally as test accounts/creds are
  provisioned (each a small, well-bounded task).
- Mutating/write operations against upstreams. All live tests are read-only on
  the source (staging is local to the test).

## 3. Vocabulary

**Integration test** — a `//go:build integration` Go test that runs a
connector's fetch/drain against its live upstream using real credentials and
asserts the staged result.

**Image smoke** — an in-cluster job that runs a connector's *shipped container
image* against the live upstream, asserting staged output on a mounted volume.

**Stage-and-readback** — the assertion technique (already used by schoology):
run the connector's drain with a real `connector.StagingWriter` rooted at
`t.TempDir()`, then read the committed item directories
(`<item>/metadata.json`, `content.raw`, sidecars) back from disk and assert on
them. This reuses the real `Commit()` path (so `Enrichments[]` and routing are
populated exactly as in production) and needs no access to `StagingItem`
internals.

**Effective config** — the connector configuration loaded through the *same
path the connector's `main.go` uses* (JSON unmarshal into the connector's
config struct + the framework's `connector.ValidateBaseConfig` /
`connector.Run` validation). For connectors that additionally implement their
own `ApplyDefaults`/`ValidateConfig` (currently only schoology), that runs too.
The **JSON config blob** — not a hand-built Go struct — is the canonical
artifact sub-project C sanitizes into the shipped sample config.

**Credential registry** — a documented table mapping each source connector to
its test-credential source, Vault path, and secret shape.

## 4. Architecture

Two layers; the Go integration test is the required bar, the image smoke is
additive where it matters ("hybrid").

### 4.1 Shared harness: `connector/integrationtest`

Rather than a bespoke in-memory backend (infeasible: `StagingBackend.NewItem`
returns the concrete `*connector.StagingItem` whose fields and `commitFunc`
are unexported, and content always lands in a `content.raw` file), the harness
uses **stage-and-readback** over a real `StagingWriter`:

- `StageToTempDir(t, connectorName) (*connector.StagingWriter, readback func() []StagedItem)`
  — creates a `StagingWriter` rooted at `t.TempDir()` (the name is required by
  `connector.NewStagingWriter(stagingDir, connectorName)`); the returned
  `readback` scans the staging dir and returns the committed items (parsed
  `metadata.json` + paths to `content.raw` and any sidecars).
- Assertion helpers over `[]StagedItem`:
  - `AssertStagedAtLeast(t, items, n)` — at least n items committed.
  - `AssertContentNonEmpty(t, item)` — `content.raw` is non-empty.
  - `AssertRouting(t, item, want)` — field equality on the already-resolved
    `metadata.json` routing fields `DataSubject`, `Audience`,
    `DestinationAgent` (the helper does not re-run the `RuleMatcher`; routing
    is resolved during `Commit`).
  - `AssertHasSidecar(t, item, name)` — optional, for enricher-runtime
    connectors that decompose MIME (asserts `Enrichments[]` / the sidecar file
    produced by the real `Commit` pipeline).
- Skip guards:
  - `RequireIntegration(t)` — `t.Skip` unless `GLOVEBOX_INTEGRATION=1`. Applied
    by **every** integration test (including public/no-credential ones) so the
    build-tagged suite never makes live network calls during an ordinary
    `go test -tags integration ./...` run.
  - `RequireCreds(t, envVars...)` — `t.Skip` (with a clear message) when any
    named credential env var is empty. Applied additionally by credentialed
    connectors.

The harness has unit tests (no live network): stage-and-readback round-trips a
hand-committed item, the assertion helpers pass/fail as expected, and the skip
guards skip when the relevant env is unset.

### 4.2 Per-connector Go integration test (required bar)

A new `//go:build integration` file per connector (name it
`live_integration_test.go` to avoid colliding with schoology's existing
`integration_test.go`), structured like schoology's existing test
(effective config + `RuleMatcher` + stage-and-readback) but driving the
**live** source. Note: for non-schoology connectors there is no single
exported "load+validate" call — validation lives partly inside
`connector.Run` — so each test replicates the small amount of `main.go` config
wiring (unmarshal the JSON config + the framework validation the connector
uses). Each test:

1. `RequireIntegration(t)`; then `RequireCreds(t, ...)` for any credential env.
2. Loads the connector's **effective config** (its sample JSON config through
   the connector's normal load+validate path).
3. Builds the connector's `RuleMatcher` (the routing a real deployment uses).
4. Runs the connector's fetch/drain against the live source, committing via a
   `StageToTempDir` `StagingWriter` (read-only on the source).
5. Asserts the round-trip via the harness helpers: `AssertStagedAtLeast(…,1)`,
   `AssertContentNonEmpty`, `AssertRouting` for the expected
   `data_subject`/`audience`/`destination_agent`, and (where applicable)
   `AssertHasSidecar`.

`schoology` **gains a new** live integration test as the reference. Its
existing fake-driven `integration_test.go` (which verifies on-disk atomic
rename, per-surface checkpoint advance, and cross-poll dedup) is valuable
offline coverage and **stays**; the new live test is additive (distinct file /
build tag).

Coexistence with the repo-root `integration_test.go`: same `//go:build
integration` tag, different scope (scanner-side, offline, no creds). It runs
under the same tag but does not require `GLOVEBOX_INTEGRATION`/creds; the
nightly stage runs the whole tagged suite, and the per-connector tests skip
themselves when their guards aren't satisfied.

### 4.3 Per-connector container image smoke (where it matters)

For connectors where verifying the *shipped artifact* (not just the code) is
valuable — bespoke runtime deps, entrypoint/config-mount behavior, etc. — an
in-cluster job runs the connector's **already-published** image (a fixed tag —
`latest` or a pinned `sha-<short>`; no fresh build happens on a scheduled
pipeline, see §6) against the live source with its ESO secret mounted at the
connector's expected path, asserting staged output on a mounted volume (the
`scripts/smoke-enrichment.sh` + `schoology/smoke_test.sh` pattern, including
the nonroot-owned-file cleanup learned in glovebox-afq4.16). Whether a
connector gets an image smoke is recorded in the registry (§5) and defaults to
"no" unless a reason is given.

## 5. Credential model and registry

Credentials are **mixed per connector**:

- **Dedicated test account** (preferred) where practical to create/maintain:
  github, gitlab, jira, trello, bluesky, x, meta, linkedin, notion, and the
  API-key connectors (steam, youtube) using a test/project key.
- **Operator real account, read-only** where a separate test account is not
  practical: gmail, imap, outlook, gcalendar, gdrive, onedrive, teams (and
  schoology, which already uses the household accounts).
- **No credentials** for public sources: rss, hackernews, arxiv,
  semantic-scholar.

The exact per-connector assignment (some, e.g. youtube/steam/bluesky, can go
either way) is recorded authoritatively in the registry, not guessed here.
`schoology-auth-refresher` is an auth helper, **not** a source connector, and
gets no integration test. Importers (apple, mbox, walhelm) are file-driven
(no live creds); they can reuse the same stage-and-readback harness against a
fixture archive and are tracked separately (lower priority).

All credentials live in **Vault** and reach the in-cluster CI job via an ESO
`ExternalSecret`, surfaced to the test as env vars (or a mounted file at the
connector's expected secret path) — the same mechanism the running connectors
use. No secret values appear in the repo or in CI variables in cleartext.

The **credential registry** (`docs/connectors/integration-credentials.md`)
records, per source connector:

| field | meaning |
|-------|---------|
| connector | name |
| cred source | `test-account` \| `real-readonly` \| `none` |
| vault path | where the test secret lives (omitted for `none`) |
| secret shape | env vars / file path + keys the connector reads |
| image smoke | `yes`/`no` (+ reason if yes) |

This table is the single source the CI stage uses to wire ESO secrets, and the
source sub-project C uses to know which config fields to replace with secret
placeholders/ESO-refs. Infra-sensitive specifics (exact ESO object names,
cluster Vault mounts, node selectors) live in the **private homelab
ci-templates**, not the public-mirrored files (per the don't-leak-infra rule);
the public registry names the logical secret, the private template binds it.

## 6. CI cadence and wiring

- The integration suite runs on a **scheduled GitLab pipeline (nightly)** plus
  **manual trigger** (`web`). It does **not** run on MR or main pushes — unit
  tests remain the merge gate, keeping live-API flakiness and rate limits out
  of the merge path.
- A new `integration` stage, gated by
  `rules: if $CI_PIPELINE_SOURCE == "schedule" || $CI_PIPELINE_SOURCE == "web"`.
- The current `test` and `build` jobs do **not** run on `schedule`/`web`
  (their rules cover MR/main/tags only), so integration jobs **must not**
  `needs:` them. Each integration job is self-contained: it checks out, sets
  `GLOVEBOX_INTEGRATION=1`, pulls its ESO secret, and runs
  `go test -tags integration ./connectors/<name>/...`. Image-smoke jobs
  **pull a previously-published image tag** (`latest` or a pinned
  `sha-<short>`) rather than build, since no `build` runs on the schedule.
- Jobs run in-cluster on the homelab runner (the only place the credential
  boundary is reachable), pinned per the existing runner/node conventions in
  the private ci-templates.
- A connector whose credentials are unavailable is **explicitly skipped and
  logged** (via the skip guards). The stage summary reports skipped connectors
  so coverage gaps are visible.

## 7. Assertion contract ("works")

A connector "works" when, against its live source and read-only:

1. It authenticates successfully.
2. It fetches ≥ 1 real item.
3. Each item is staged with non-empty `content.raw` and the expected metadata.
4. Routing (`data_subject` / `audience` / `destination_agent`) in the
   committed `metadata.json` matches the connector's intent.

This generalizes schoology's *structure* (production-shaped config +
`RuleMatcher` + stage-and-readback). Where a connector may legitimately return
zero items (e.g. an empty test inbox), the test selects/seeds a source
guaranteed to have content, or documents the minimum-fixture requirement in
the registry.

## 8. Linkage to sub-project C (sample configs + docs)

The connector's **effective config** — the JSON config blob loaded and
validated through the connector's normal path in the integration test (§4.2
step 2) — is the canonical sample config. Sub-project C ships that JSON with
secret fields replaced by placeholders / ESO references (driven by the
registry's "secret shape" column) and generates the per-connector doc. The
shipped sample config is therefore literally the configuration the live test
proved works — not a hand-written guess. Because the artifact is the JSON blob
(not a Go struct chain), C needs no per-connector Go knowledge.

## 9. Error handling and flakiness

- Transient network errors are retried a bounded number of times per test.
- **Rate limiting (HTTP 429 / provider throttle) results in skip-with-warning,
  not failure** — a nightly run must not go red because an upstream throttled
  us.
- Each connector test has a wall-clock timeout so one hung upstream can't stall
  the suite.
- All failure and skip paths emit a WHAT/CHECK/FIX line (the spec-14 §8
  convention already used across the repo).

## 10. Incremental rollout

The design ships, in order:

1. The shared harness (`connector/integrationtest`) + its unit tests.
2. The credential registry document (schema + the rows we can fill today).
3. The GitLab `integration` stage (scheduled + manual), wired in the private
   ci-templates, standing alone (no `needs:` on `test`/`build`).
4. A **new live** integration test for `schoology` (reference); its existing
   fake-driven test stays.
5. The no-credential connectors (rss, hackernews, arxiv, semantic-scholar).
   They need no secrets, so they validate the harness + CI stage end-to-end
   (gated behind `GLOVEBOX_INTEGRATION=1`) without waiting on account
   provisioning.

Remaining connectors onboard one at a time thereafter: add
`integration_test.go`, add the registry row, provision the Vault/ESO secret,
(optionally) add an image smoke. Each is a small, isolated task tracked as a
child bead.

## 11. Acceptance criteria

- `connector/integrationtest` exists with stage-and-readback + assertion
  helpers + `RequireIntegration`/`RequireCreds`, all unit-tested.
- `go test ./...` is green with no credentials present (integration tests are
  build-tag-excluded), and `go test -tags integration ./...` **skips cleanly
  with no credentials and `GLOVEBOX_INTEGRATION` unset** — i.e. it performs no
  live network calls (every per-connector test guards on `RequireIntegration`;
  the offline root `integration_test.go` may still run).
- The credential registry document exists with the agreed schema and a row for
  every source connector (cred source at minimum; Vault path/secret shape
  filled as provisioned); `schoology-auth-refresher` and importers are noted as
  excluded/separate.
- A GitLab `integration` stage runs only on schedule/manual, in-cluster, does
  not depend on `test`/`build`, and green-passes the no-credential connectors
  against their live sources (with `GLOVEBOX_INTEGRATION=1`), emitting a summary
  that lists skipped connectors.
- `schoology` has a new live integration test through the shared harness; its
  prior fake-driven coverage is intact.
- Spec-14 §8 WHAT/CHECK/FIX discipline holds on all failure/skip paths.

## 12. Open questions (flagged, not blocking)

- Per-connector jobs vs. a single matrix job for the `integration` stage, given
  runner concurrency limits — decide during implementation.
- Whether `AssertHasSidecar` should be required (not just available) for the
  enricher-runtime connectors that decompose MIME (gmail/imap/outlook/mbox) —
  lean toward required there. (Feasible because stage-and-readback uses the real
  `Commit`, which runs `runEnrichmentPipeline`.)
- Exact Vault path layout for `test-account` vs `real-readonly` secrets — owned
  by the homelab GitOps/secrets convention; recorded in the private template.
- Whether importers (apple/mbox/walhelm) fold into this harness now or in a
  follow-up — currently deferred.

## 13. Cross-references

- `connectors/schoology/integration_test.go` (fake-driven, offline) +
  `connectors/schoology/smoke_test.sh` — the *structural* model this
  generalizes into a live test.
- `integration_test.go` (repo root) — the pre-existing `//go:build integration`
  scanner-side offline test; coexists with the new per-connector convention.
- `scripts/smoke-enrichment.sh` — the container-smoke pattern (incl. the
  nonroot-owned-file cleanup from glovebox-afq4.16).
- `connector/staging.go` (`StagingWriter`, `StagingItem`, `Commit`),
  `connector/backend.go` (`StagingBackend`), `connector/runner.go`
  (`ValidateBaseConfig`/`Run`) — the framework this builds on.
- `docs/specs/05-connector-framework-design.md`,
  `docs/specs/06-connector-auth-and-provenance-design.md`.
- Sibling sub-projects: A (image coverage / `glovebox-i8nd`), C (sample
  configs + docs), D (Helm chart coverage).
