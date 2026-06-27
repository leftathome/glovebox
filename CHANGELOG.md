# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.6.4] - 2026-06-26

### Fixed

- **archive listener no longer caps multi-GB uploads at 60s (glovebox-dddn)**:
  `/v1/archives*` shares the ingest `http.Server`, which set
  `ReadTimeout`/`WriteTimeout` to `request_timeout_seconds` (default 60s).
  `http.Server.ReadTimeout` bounds the *entire request including the body*, so
  any archive PATCH upload taking longer than 60s was force-closed (curl
  `(55) Send failure: Broken pipe`) -- impossible to deliver the advertised
  `Tus-Max-Size: 30 GiB`, and the handler's own 5-min `patchBodyReader` idle
  timeout was overridden. Now the server sets only `ReadHeaderTimeout`
  (slowloris protection) with `ReadTimeout`/`WriteTimeout` unbounded; per-route
  body bounds remain (`/v1/ingest` via `http.MaxBytesReader`, `/v1/archives`
  via the idle timeout). Verified: a 12 GiB mbox + 2 GiB tarball-subtree upload
  completes under default config (was a broken pipe at 60s).
- **archive-smoke-test.sh can actually run its 12 GiB criterion (glovebox-3d4m)**:
  three fixes -- (1) the container metrics port now follows `METRICS_PORT` so a
  host with something already on 9090 (e.g. Prometheus) can run the test;
  (2) the archive-listener-mounted check polls instead of grepping once (was
  racing the metrics-ready probe to a false FAIL on fast boots); (3) the PATCH
  body streams via `--upload-file` instead of `--data-binary @file`, which
  loaded the whole archive into memory (`curl: option --data-binary: out of
  memory` on 12 GiB). The full 12 GiB + 2 GiB acceptance run now passes.

### Changed

- **re-enable govulncheck gating (glovebox-fslv)**: the security-scan job's
  govulncheck step was non-gating (`continue-on-error`) because v1.4.0 segfaulted
  on our generics under go1.26 (x/tools `ForEachElement` / `*types.TypeParam`
  panic). govulncheck v1.5.0 fixes the crash (verified clean under go1.26), so
  the step is gating again and pinned to `@v1.5.0` (not `@latest`) so a future
  tool regression can't silently re-break the gate.

## [0.6.3] - 2026-06-26

### Security

- **bump golang.org/x/net to v0.55.0 (glovebox-auq4)**: clears 7 govulncheck
  findings + 3 Trivy CVEs (GO-2026-5025/5026/5027/5028/5029/5030 in
  `x/net/html` + `x/net/idna`, GO-2026-4918 in net/http2;
  CVE-2026-25680/-33814/-39821). All were reachable (schoology `html.Parse`,
  `connector/httpclient.go` idna/http2). govulncheck now reports
  "No vulnerabilities found".

### Fixed

- **enricher Dockerfile ARG scope (CI image builds)**: the 8 enricher-runtime
  based Dockerfiles (mbox/apple/walhelm importers; arxiv/gmail/imap/outlook/
  semantic-scholar connectors) declared `ARG ENRICHER_BASE` after the first
  `FROM`, so it was stage-scoped and resolved blank in `FROM ${ENRICHER_BASE}`.
  Newer BuildKit (pulled by the docker/build-push-action v7 bump) rejects this
  with `UndefinedArgInFrom`, breaking every enricher image build (the cause of
  the failed v0.6.2 image publish). Moved the `ARG` to global (pre-`FROM`)
  scope; verified `docker buildx build` resolves the base again.

## [0.6.2] - 2026-06-25

### Added

- **mbox-importer archive-event watcher mode (glovebox-c9zt)**: a long-running
  `--watch-archives <dir>` mode on the mbox-importer that picks up `archive/mbox`
  archives finalized into `staging/archives/` by the spec-13 delivery endpoint,
  drives the existing per-message import pipeline against each, and retires
  processed archives to `archives/.done/` (spec 13 sec 5.3). Reuses the fsnotify
  watcher (polling fallback + metadata.json readiness gate); configurable
  `--media-types` (default `archive/mbox`); per-archive `O_EXCL` advisory lock so
  multiple replicas/importers never double-pick; on failure the lock is released
  and the archive left in place for operator recovery.
- **mbox-importer watcher Deployment (glovebox-j2s0)**: `charts/mbox-importer`
  gains an opt-in long-running Deployment (`watch.enabled`) that runs the watcher
  mode in-cluster with `/healthz` + `/readyz` probes and a `Recreate` strategy
  (RWO archive-storage PVC), coexisting with the existing one-shot import Job.

### Fixed

- **mbox-importer absolute byte offsets across resume (glovebox-gtxt)**: after a
  resume seek the parser reported byte offsets relative to the seek base, so the
  `origin_archive` provenance tag and any second-interruption resume offset were
  wrong. Offsets are now absolute archive positions (`NewScannerAt`); the
  interrupt/resume e2e test asserts `origin_archive` uniqueness.

## [0.6.1] - 2026-06-24

### Changed

- **gitlab-first release pipeline (glovebox-npsj)**: `.gitlab-ci.yml` now builds
  and publishes every connector/importer container image (kaniko) and packages
  the Helm chart as an OCI artifact to the in-cluster registry, with
  gitlab.orac.local established as the primary build/release target ahead of
  GitHub. Closes the CI image-coverage gap (glovebox-i8nd).

### Security

- **PII scrub of public artifacts (glovebox-0nzk)**: removed household entity_ids
  and de-pseudonymizing name comments that were baked as DEFAULTS into the public
  Helm charts, connector/importer configs, tests, and docs. Public defaults are
  now neutral (`subjects.json` ships empty with `enforce: false`;
  `data_subject_default` defaults to `""`, falling through to the safe household
  audience); real subject bindings belong only in operator-controlled values.

### Note

- Supersedes **v0.6.0**, which was withdrawn from GitHub (its published artifacts
  carried the identity defaults scrubbed above). v0.6.0 remains available, clean,
  on the primary GitLab remote. No functional/source code changed between the
  intended v0.6.0 and this release beyond the scrub.

## [0.6.0] - 2026-06-19

### Added

- **Content enrichment framework (spec 14)** -- a pluggable pipeline that
  derives clean, model-ready text sidecars (`content.<name>.md`) from
  binary/rich attachments during staging. See
  `docs/specs/14-content-enrichment-design.md`.
  - **Enricher interface + registry** (`connector/enrich/`): enrichers are
    registered by media type and run from `StagingItem.Commit()` between
    metadata build and atomic rename. Per-source artifacts are recorded in
    `metadata.json` as `Enrichments[]`; per-enricher failures write
    `content.<name>.error.md` markers without failing the commit (additive
    schema -- old metadata without `Enrichments` still parses).
  - **Enrichers shipped:** passthrough (identity copy), a pure-Go HTML text
    extractor, and binary-dependent PDF (pdftotext), OCR (tesseract), and
    Office/OOXML (pandoc) enrichers.
  - **enricher-runtime base image** -- a shared Debian-based image bundling
    poppler-utils, tesseract-ocr, and pandoc for the binary enrichers; the
    attachment-heavy connectors (gmail, imap, outlook, mbox, arxiv,
    semantic-scholar) are rebased onto it and wire the full enricher set.

- **Recognizer-scanner ingest lane (glovebox-9s60)** -- a push ingest
  source for the OpenClaw recognizer's document scanner, riding the spec-13
  tus.io archive path. The authenticated bearer-token source-id is the
  anti-spoof identity: a config-driven source registry (`internal/source/`,
  `charts/glovebox/sources.json`, env `GLOVEBOX_SOURCES_FILE`) holds each
  connector's `data_subject_default` and `audience_default`, and a
  fail-closed gate in `Finalize` rejects the `archive/recognizer-scan`
  media type from any non-scanner source (403 `source_not_authorized`).
  Adds a standalone `operator` audience token (must appear alone) that marks
  items for OpenClaw's operator lane, and renders the recognizer's
  pre-extracted `ocr.txt` to `content.extracted.md`.

- **Pluggable ingest token-source (glovebox-4ypk)** -- the archive
  listener's bearer-token store is now selectable via `ingest.auth.source`
  (`vault` | `env` | `file`). Vault remains the production default; the
  env/file sources are opt-in and dev-only, enabling single-node and
  in-container smoke testing of the auth + archives path without a
  Kubernetes cluster.

- **Health-data provenance + subject resolution (spec 15, SP1)** -- the
  Glovebox-side foundation for ingesting health data fetched from
  credentialed sources (initially Kaiser Permanente WA via the recognizer
  using the walhelm-go library). See
  `docs/specs/15-health-provenance-and-subject-resolution-design.md`.
  - **Archive contract extension:** new `archive/walhelm-export` media type
    (tar) and producer-asserted provenance keys on the spec-13
    `Upload-Metadata`: acquisition identity (`acq_provider`,
    `acq_account_id`, `acq_auth_method`) and an opaque subject principal
    (`data_subject`) plus optional `audience`. The finalize receipt
    (`metadata.json`) now records an `acquisition` identity block and the
    producer-asserted `data_subject`/`audience`. Other media types are
    unchanged (the provenance keys are required only for walhelm-export).
  - **Known-subjects registry** (`internal/subject/`): an operator-maintained
    allowlist mapping opaque source principals (e.g. `walhelm:<id>`) to an
    opaque Glovebox `entity_id`. PHI/PII firewall -- the data plane (staged
    items, routing, audit log) carries only opaque `entity_id`s; an optional
    `display` label is non-functional and never emitted. Cross-connector
    normalization (one entity, many principals); rejects principal/entity_id
    collisions at load.
  - **Fail-closed subject-resolution gate** at the routing decision: items
    carrying a `data_subject` are resolved to their `entity_id` (rewriting
    the staged metadata) before delivery; subjects that do not resolve are
    quarantined with reason `subject_unresolved` when the registry enforces.
    Enforcement lives in the registry file's `enforce` field (default
    **false**) -- with an empty registry and enforcement off, behavior is
    unchanged and subjectless items (every existing connector) bypass the
    gate untouched.
  - **walhelm importer** (`importers/walhelm/`): a one-shot importer that
    reads a finalized `archive/walhelm-export` directory and stages one item
    per tree file, stamping each with the receipt's subject/audience/
    acquisition-identity (the rule matcher chooses only the destination
    agent). Ships with an enricher-runtime Dockerfile, CI binary + image
    build, and a Helm-delivered `subjects.json` registry ConfigMap.

- **mbox importer + archive media types** -- a one-shot importer for
  `mbox` email archives (the 20-year backfill use case), plus two new
  archive media types on the spec-13 ingest path: `archive/generic-tarball`
  and `archive/imap-export` (glovebox-4enb, glovebox-7ey). Previously
  shipped under the out-of-order `v0.4.1`-`v0.4.3` tags; documented here.

- **Schoology connector** (`connectors/schoology/`) -- ingests
  assignments, faculty feed posts, inbox messages, and attachments from
  a parent Schoology account via the
  [schoology-go](https://github.com/leftathome/schoology-go) library.
  Single-container deployment serving all kids in a household. Browser-
  session-cookie authentication (spec 06's pattern for unusual auth
  flows); credentials provisioned via K8s Secret + External Secrets
  Operator + 1Password. Window-scheduled polling with splay (07:00-09:00
  and 15:30-17:30 local on weekdays) plus an authenticated `POST
  /v1/poll` trigger endpoint with 60-second debounce. Implements the
  framework's `Connector` + `Watcher` + `Listener` interfaces. See
  `docs/specs/12-schoology-connector-design.md`.
- Routing-layer tag-based quarantine: items with `tags.parse_status`
  set to `degraded` or `failure_receipt` are routed to quarantine
  regardless of scanner verdict. Audit log records
  `QuarantineReason: "parse_status_tag"`. Enables forensic preservation
  of parse failures for bug-patrol.
- `docs/AUTH-RECOVERY.md` -- operator procedure for Schoology session
  expiry recovery (detect via `kubectl logs`, re-auth on workstation
  via `auth.Login`, update 1Password item, wait for ESO sync, verify).

### Fixed

- **Per-source `data_subject` routing (privacy)** -- the mbox importer and
  the gmail, imap, outlook, linkedin, x, meta, bluesky, jira, and trello
  connectors dropped the matched routing rule's `data_subject`/`audience`
  when building items, so personal data (e.g. one person's Gmail Takeout)
  defaulted to the shared **household** audience group -- recallable by
  every household agent. All ten now carry the rule's
  `data_subject`/`audience` through the staging merge chain so an item
  routes to the intended person's agent (`glovebox-hyvp`, `glovebox-do3z`).
  The framework also logs a startup warning when a connector has no
  `data_subject` configured at any level (empty `data_subject_default` and
  no rule sets one), since such a connector silently defaults to household.
- **Windows cross-compilation** -- `internal/ingest/archives` (st_dev check)
  and the `stagingCapacityBytes` quota gauge used `syscall.Stat_t` /
  `syscall.Statfs` inline, breaking `GOOS=windows` release builds. Both are
  now split into `//go:build unix` implementations with non-Unix stubs, so
  the full release matrix (linux, darwin, windows; amd64/arm64) builds.

### Notes

- This release consolidates all work since `v0.5.0` into a single `v0.6.0`
  tag: the Schoology connector (previously drafted as an untagged `0.6.0`
  changelog entry), the mbox/media-type work shipped under the out-of-order
  `v0.4.1`-`v0.4.3` tags, and the first tagged appearance of the spec-14
  enrichment, spec-15 provenance, and recognizer-scanner features. The
  earlier `v0.3.1`-`v0.3.5` / `v0.4.1`-`v0.4.3` patch tags were never
  documented here individually.
- Schoology session cookies expire approximately every 14 days; the
  connector surfaces expiry as `PermanentError` with a recovery-
  instruction message and exits non-zero so K8s reports
  `CrashLoopBackOff` for alerting. There is no headless refresh path
  for SSO-fronted tenants -- recovery is a manual operator action.
- Uses spec 11 v1.2 audience vocabulary (`guardians`, `caregivers`);
  inbox messages route with `audience: ["guardians"]` standalone
  (parent-level, no specific kid).
- Per-kid `data_subject` values are operator-chosen opaque labels
  (`k1`/`k2`) to avoid placing PII (nicknames, legal names) into
  metadata and audit logs.
- Introduces `auth_method: "session_cookie"` to spec 06's open
  `auth_method` enum.
- Marks several patterns (window scheduler with splay, trigger endpoint
  with debounce, parse-failure receipt synthesis, per-kid opaque
  labels) as candidates for extraction to a future "connector primitive
  base type" when PowerSchool (spec 13) lands.

## [0.5.0] - 2026-05-19

### Added

- New audience enum token `caregivers` -- delegated supervisors and care
  providers (tutors, nannies, AI agents in caretaking roles,
  out-of-household relatives on duty). Orthogonal to `household`; the
  combination `[household, caregivers]` is permitted. See spec 11 v1.2
  §3.4 and the §3.1 glossary.

### Changed (breaking)

- Renamed audience enum token `parents` → `guardians`. Same semantics
  (spec 11 v0.4.0's §3.4 table already documented the token as
  "parents/guardians" parenthetically); the new name matches school and
  legal terminology and is inclusive of bio/adoptive/foster parents and
  legal guardians. The Go constant `AudienceParents` was renamed to
  `AudienceGuardians`. v0.4.0 was less than 24 hours old with no
  external consumers when this change landed; in-repo callers were
  migrated in the same release.
- `guardians` and `caregivers` may now appear standalone in `audience`
  with empty `data_subject` (household-scope interpretation). Prior to
  v0.5.0, role-relative tokens uniformly required `data_subject` to be
  set. `subject` and `siblings` retain that requirement -- they are
  inherently subject-relative.

### Notes

- Spec 11 §3.1 was extended with a `guardians`-vs-`caregivers` glossary
  entry, an architectural stance documenting Glovebox audience as
  coarse (with fine-grained authorization deferred to downstream
  agents), and an "Audience is a snapshot, not a permanent ACL"
  subsection clarifying that lifecycle-dependent access (juvenile →
  adult transition, caregiver contract endings, retention horizons) is
  the downstream agent's responsibility to apply against frozen
  audit-log audience tokens.
- Spec 11 §2.2 explicitly defers medical-care role tokens (`spouse`,
  `medical_providers`, HIPAA-grade sensitivity escalators) until a
  medical-content connector lands with concrete use cases to validate
  against.

## [0.4.0] - 2026-05-18

### Added

- `data_subject` (string) and `audience` ([]string enum) fields on
  `metadata.json`, `ItemOptions`, `Rule`, `MatchResult`, `BaseConfig`
  defaults, and `AuditEntry`. See
  `docs/specs/11-data-subject-and-audience-design.md`.
- Audience enum tokens: `subject`, `parents`, `siblings`, `household`,
  `public`, with validated combinations (spec 11 §3.5).
- `staging.EffectiveAudience()` reader-side helper that applies the
  default `["household"]` when audience is omitted.
- `staging.HasControlChars()` exported wrapper enabling consistent
  control-char policy across connector and staging packages.
- Commit-time validation of `data_subject` length/control-chars and
  `audience` enum + cross-field rules.
- Config-load-time validation of `data_subject_default` and
  `audience_default`: malformed defaults fail startup, not first-item
  commit.
- End-to-end integration test `TestIntegration_DataSubjectAudienceEndToEnd`
  exercising the full spec-11 path: rule -> match -> staging -> metadata.json
  for both data-subject-bearing and subjectless items.

### Notes

- Purely additive schema extension. Existing connectors produce
  byte-identical `metadata.json` files with no code changes.
- V1 is metadata-only: Glovebox validates and stamps these fields but
  does not filter or route on them. Audience-aware routing and
  enforcement are deferred to later specs.

## [0.3.0] - 2026-04-05

### Added

- **HTTP ingest API** (spec 08): scanner accepts content items via POST
  `/v1/ingest` on a dedicated port (9091), replacing the shared staging PVC
  between connectors and the scanner. Connectors POST multipart
  (metadata JSON + content bytes) instead of writing to a shared filesystem.
  Eliminates RWX PVC requirement, co-location constraints, and fsGroup
  permission issues in Kubernetes deployments.
- `StagingBackend` interface: abstracts item delivery mechanism.
  `StagingWriter` (filesystem) and `HTTPStagingBackend` (HTTP ingest) both
  implement it. Backend selected automatically by `connector.Run` based on
  `GLOVEBOX_INGEST_URL` (HTTP mode) or `GLOVEBOX_STAGING_DIR` (filesystem mode).
- Ingest handler with atomic write (`.ingest-tmp/` rename), backpressure via
  atomic counter (429 with Retry-After), startup readiness gate (503 until
  initialized), strict multipart validation (reject missing/duplicate/unexpected
  parts), configurable size limits (256KB metadata, 64MB body).
- `HTTPStagingBackend` with exponential backoff + jitter retry on 429/5xx/network
  errors. Honors Retry-After header. Returns PermanentError on 400/413.
  `X-Glovebox-Connector` header on every request.
- Unified receive metrics: `glovebox_items_received_total` (source, status),
  `glovebox_receive_duration_seconds`, `glovebox_receive_bytes_total`,
  `glovebox_staging_queue_depth` (atomic counter). `source` label threads
  through entire pipeline for end-to-end traceability.
- 5 integration tests proving full HTTP ingest pipeline (end-to-end, identity
  merge, backpressure recovery, validation rejection, server restart).
- Design specification: `docs/specs/08-ingest-api-design.md`

### Changed

- **Helm chart v0.3.0**: major overhaul
  - Connectors default to HTTP ingest (`GLOVEBOX_INGEST_URL`); staging PVC mount
    removed. Per-connector `ingestMode` toggle (default: `http`, option:
    `filesystem`) for backward compatibility.
  - New ingest Service (ClusterIP, port 9091) for scanner
  - Scanner NetworkPolicy: port 9091 restricted to connector pods, port 9090
    (metrics) unrestricted. Separate ports prevent NetworkPolicy bypass.
  - Standard `app.kubernetes.io/*` labels on all resources
  - `podSecurityContext` (runAsNonRoot, runAsUser, fsGroup) on all deployments
  - `containerSecurityContext` (allowPrivilegeEscalation: false, drop ALL) on all containers
  - ServiceAccount with `automountServiceAccountToken: false`
  - `helm.sh/resource-policy: keep` on all PVCs (prevents data loss on uninstall)
  - Configurable `accessMode` per PVC (staging defaults to ReadWriteMany for
    filesystem mode, ReadWriteOnce sufficient for HTTP mode)
  - `nodeSelector`, `affinity`, `tolerations` on scanner and all connectors
    (connectors inherit from top-level values, overridable per-connector)
  - Config checksum annotations for automatic rollout on ConfigMap changes
  - Liveness/readiness probes on scanner deployment
  - Startup probe on ingest port
  - `nameOverride` / `fullnameOverride` support
  - Consistent naming via `glovebox.fullname` helper across all resources
  - `existingClaim` support for connector state PVCs
  - Per-connector `imagePullPolicy` configuration
  - Ingest config in scanner ConfigMap (port, size limits, backpressure threshold)
  - Removed dead rules.json fallback path
- `ConnectorContext.Writer` deprecated in favor of `ConnectorContext.Backend`
- `connector_items_produced_total` metric deprecated (scanner-side
  `glovebox_items_received_total` is the authoritative counter)
- `StagingItem.Commit()` delegates to backend via `commitFunc` dispatch
- Shared `buildMetadata()` method on `StagingItem` used by both filesystem
  and HTTP backends (eliminates code duplication)
- Chart version bumped to 0.3.0, appVersion to 0.2.3

## [0.2.3] - 2026-04-05

### Fixed

- Add missing source files for Outlook, Teams, OneDrive connectors (v0.2.2
  shipped test files without source code, causing `go vet` failures)
- Teams test reading wrong filename (`content` instead of `content.raw`)

## [0.2.2] - 2026-04-05 [BROKEN]

> **This release is broken.** Use v0.2.3 instead.

### Added

- ClientCredentials token source for service-to-service OAuth
- 6 new connectors: Notion, Semantic Scholar, arXiv, Steam, Hacker News, LinkedIn
- YouTube comments (commentThreads API) and caption language metadata
- Gmail connector (OAuth + MIME decoding)
- Google Calendar connector (event polling with updatedMin checkpoint)
- Google Drive connector (delta token change tracking)
- Outlook mail connector (Microsoft Graph)
- Teams messages connector (Microsoft Graph)
- OneDrive activity connector (Microsoft Graph delta API)

### Fixed

- Redact API keys from Steam and YouTube error messages
- staging-tmp path for container deployments
- Helm: existingClaim support for all PVCs, bundled default rules

## [0.2.1] - 2026-04-01

### Added

- Helm chart: `existingClaim` option for all PVCs (staging, quarantine, audit,
  failed, agents, shared) to support bring-your-own persistent volumes

## [0.2.0] - 2026-03-31

### Added

- Unified rules config: `routes` replaced by `rules` with destination + tags
  per rule (backward compatible -- `routes` accepted with deprecation warning)
- Identity and data provenance: metadata.json gains `identity` object
  (account_id, provider, auth_method, scopes, tenant) and `tags` map
- TokenSource interface for authenticated API access
  - StaticTokenSource for PATs, API keys, app passwords
  - RefreshableTokenSource for OAuth2 with atomic token file persistence,
    automatic refresh, 5-minute wait cap, and concurrent-safe access
- WebhookVerifier: HMAC-SHA256 signature verification for GitHub, Meta, X
- RuleMatcher: first-match-wins routing with tags (replaces Router)
- FetchCounter: configurable per-source and per-poll fetch limits to control
  throughput cost on large backlogs
- HTTPClient: standardized GloveboxBot User-Agent via RoundTripper, applied
  to all HTTP requests across all connectors
- RateLimiter: reads X-RateLimit-*, RateLimit-*, and Retry-After headers;
  sleeps when exhausted (capped at 5 minutes); pre-emptive slowdown
- RobotsChecker: robots.txt compliance for web-fetching connectors (RSS link
  fetching), with LRU cache, crawl-delay support, SSRF-safe redirect handling
- Round 2 connectors: GitHub (Poll + Listener), GitLab (Poll with pagination),
  Jira (Poll with JQL), Trello (Poll with query param auth)
- Round 3 connectors: LinkedIn (Poll), Meta (Poll + Listener with HMAC),
  Bluesky (Poll with AT Protocol XRPC), X (Poll + Listener with CRC)
- Helm chart v0.2.0: connector deployments via values.yaml, Prometheus scrape
  annotations on all pods, optional ServiceMonitor CRDs
- Community health files: CODE_OF_CONDUCT.md (Contributor Covenant 2.1),
  SECURITY.md (vulnerability reporting), CONTRIBUTING.md (DCO, standards)
- Executable demos in examples/ (showboat format)
- Design specifications for auth/provenance (06) and fetch controls (07)

### Changed

- BaseConfig accepts both `rules` and `routes` (routes deprecated)
- ConnectorContext gains Matcher (was Router), FetchCounter, and Metrics fields
- StagingWriter merges rule tags and config identity into metadata on Commit
- ItemOptions gains Identity, Tags, and RuleTags fields
- Glovebox validates identity sub-fields and tags in metadata
- Audit log entries include identity and tags
- All 10 connectors use standardized GloveboxBot User-Agent
- All 10 connectors enforce FetchCounter limits in poll loops
- Generator templates use `rules`/`RuleMatcher` (was `routes`/`Router`)

### Removed

- Old Router/Route types (replaced by RuleMatcher/Rule)

### Fixed

- Watcher readiness gate: metadata.json presence check before dispatching
  items, with periodic poll fallback for networked/virtualized mounts
- Meta connector: access token moved from URL query param to Authorization
  header (prevents token leaking into error messages)
- RoundTrip: clone request before setting headers (http.RoundTripper contract)
- robots.txt: SSRF prevention (http/https only), bounded read (512KB cap)
- Generator: templates use package main (was package name, wouldn't compile)
- Meta webhook: reflected XSS via hub.challenge (set Content-Type text/plain)
- CI: CodeQL action bumped to v4 (Node.js 24 compatible)
- CI: explicit CodeQL workflow for Go only (was auto-detecting Ruby)
- CI: Docker builds parallelized via matrix (11 concurrent vs sequential)
- Contact emails updated in SECURITY.md and CODE_OF_CONDUCT.md

## [0.1.0] - 2026-03-29

Initial public release of the glovebox content scanning service and connector
framework.

### Added

- Deterministic content scanning engine with weighted signal scoring
  - Substring, case-insensitive substring, and regex pattern matchers
  - Custom detectors: encoding anomaly, template structure, language detection
  - Content pre-processing: NFKC normalization, zero-width character stripping,
    HTML tag stripping
  - Configurable quarantine threshold with boost multiplier support
- Staging item protocol with metadata validation and field constraints
- Parallel scan worker pool with per-item timeout (quarantine on expiry)
- Ordered delivery router preserving item sequence per destination
- Routing verdicts: PASS (to agent workspace), QUARANTINE (with sanitization
  and notification), REJECT (with typed reasons and cleanup)
- Append-only JSONL audit logger with fail-closed degraded mode
- Filesystem watcher with fsnotify (primary) and polling (fallback)
- OpenTelemetry instrumentation with Prometheus exporter (10 metrics)
- Connector framework library (`connector/`)
  - Core interfaces: Connector (poll), Watcher (long-lived), Listener (webhook)
  - Execution engine with poll-once, poll-loop, watch-loop, and listener modes
  - Atomic staging writer with metadata validation
  - JSON-backed checkpoint persistence with per-item saves
  - Config-based routing with wildcard support
  - Health endpoints: `/healthz` (liveness), `/readyz` (readiness), `/metrics`
  - OTel metrics for connectors (6 instruments)
  - Content helpers: MIME multipart decoder, HTML-to-text extractor, link policy
  - Error classification: transient (retry) vs permanent (exit)
- First-party connectors: IMAP (Poll + Watch/IDLE), RSS (Poll with link fetching)
- Scaffold generator for new connectors
- Multi-stage Dockerfile with distroless runtime
- Helm chart with Deployment, NetworkPolicy, PVCs, and ConfigMap
- GitHub Actions CI with multi-arch builds, SBOMs, provenance, security scanning
- Dependabot for Go modules, Dockerfiles, and GitHub Actions
- Apache License 2.0
- Documentation: README, deployment guide, connector author guide, AGENTS.md

[Unreleased]: https://github.com/leftathome/glovebox/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/leftathome/glovebox/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/leftathome/glovebox/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/leftathome/glovebox/releases/tag/v0.1.0
