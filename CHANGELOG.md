# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/leftathome/glovebox/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/leftathome/glovebox/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/leftathome/glovebox/releases/tag/v0.1.0
