# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
- Readiness gate: watcher checks for metadata.json before dispatching items,
  supporting networked/virtualized mounts (NFS, iSCSI, 9P, virtiofs)
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
- First-party connectors
  - IMAP connector with Poll + Watch (IMAP IDLE), folder-based routing, MIME
    content extraction
  - RSS connector with poll-based RSS/Atom feed parsing, configurable link
    fetching with safe-by-default link policy
- Scaffold generator: `go run ./generator new-connector <name>` creates a new
  connector directory from templates
- Multi-stage Dockerfile with distroless runtime for glovebox and each connector
- Helm chart with Deployment, NetworkPolicy, PVCs, and ConfigMap
- GitHub Actions CI
  - Test, vet, and build on push/PR
  - Multi-arch binaries: linux/amd64, linux/arm64, darwin/arm64, windows/amd64,
    windows/arm64
  - Multi-arch Docker images: linux/amd64 + linux/arm64
  - SBOMs via anchore/sbom-action (syft)
  - SLSA provenance attestations via actions/attest-build-provenance
  - Security scanning: govulncheck + Trivy
  - Test reporting with JUnit annotations and job summary
  - Push Docker images to ghcr.io on main/tag
- Dependabot for Go modules, Dockerfiles, and GitHub Actions
- Apache License 2.0
- Documentation
  - README with architecture overview, quickstart (Docker Compose), and badges
  - Deployment guide: Docker Compose, Kubernetes, binary install with systemd
  - Connector author guide with API reference and testing patterns
  - AGENTS.md with machine-readable connector development instructions
  - Design specifications for glovebox and connector framework

[Unreleased]: https://github.com/leftathome/glovebox/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/leftathome/glovebox/releases/tag/v0.1.0
