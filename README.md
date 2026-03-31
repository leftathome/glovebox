# glovebox

Content scanning service for the OpenClaw Home Agent.

[![CI](https://github.com/leftathome/glovebox/actions/workflows/ci.yml/badge.svg)](https://github.com/leftathome/glovebox/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](https://go.dev/)

## What is glovebox?

Glovebox is a deterministic content scanning service that sits between external
data connectors and the domain agents in an OpenClaw Home Agent deployment. It
inspects every piece of incoming content -- email, RSS entries, webhooks -- for
prompt injection attacks and other adversarial patterns before that content can
reach an agent workspace.

Glovebox has no LLM dependency. It uses configurable weighted pattern matching
(regex, substring, and custom detectors) to score content against a threat
threshold. Items that score above the threshold are quarantined for human review;
everything else is delivered to the destination agent. Content is never modified
-- it arrives and leaves byte-identical.

The project includes a connector framework: a Go library that handles staging
writes, checkpoint persistence, health checks, and metrics, so connector authors
only need to implement the fetch logic for their data source.

```
                                 +-------------------+
  +-----------+                  |    glovebox       |
  |   IMAP    |--+               |                   |     +----------------+
  +-----------+  |  staging/     | validate           |--->| agents/        |
                 +-------------->| pre-process        |    | (workspaces)   |
  +-----------+  |               | scan (worker pool) |    +----------------+
  |   RSS     |--+               | score & route      |
  +-----------+                  |                   |     +----------------+
                                 |                   |--->| quarantine/    |
  +-----------+                  +-------------------+    | (human review) |
  |  custom   |--+                                        +----------------+
  +-----------+
```

## Key features

- Multi-rule scanning engine with regex, substring, and custom detector support
- Weighted scoring with configurable quarantine threshold
- Connector framework library with poll, watch, and listen execution modes
- Atomic handoff protocol -- connectors write to staging; glovebox picks up
- First-party connectors for IMAP and RSS (Round 1)
- Scaffold generator for creating new connectors from templates
- OpenTelemetry metrics with Prometheus exporter (`/metrics`)
- Append-only JSONL audit log for all scan verdicts
- Quarantine with notification placeholders for human review
- Parallel scan workers with per-item timeout (quarantine on expiry)

## Quickstart

This example runs glovebox with the RSS connector using Docker Compose. It
pulls an RSS feed every 15 minutes, scans each entry, and delivers clean items
to the `messaging` agent workspace.

Create a file called `docker-compose.yml`:

```yaml
services:
  glovebox:
    build: .
    volumes:
      - staging:/data/glovebox/staging
      - quarantine:/data/glovebox/quarantine
      - audit:/data/glovebox/audit
      - failed:/data/glovebox/failed
      - agents:/data/agents
      - shared:/data/shared
    ports:
      - "9090:9090"  # Prometheus metrics

  rss-connector:
    build:
      context: .
      dockerfile: connectors/rss/Dockerfile
    environment:
      GLOVEBOX_STAGING_DIR: /data/glovebox/staging
      GLOVEBOX_STATE_DIR: /state
      GLOVEBOX_CONNECTOR_CONFIG: /etc/connector/config.json
    volumes:
      - staging:/data/glovebox/staging
      - rss-state:/state
      - ./quickstart-rss.json:/etc/connector/config.json:ro

volumes:
  staging:
  quarantine:
  audit:
  failed:
  agents:
  shared:
  rss-state:
```

Create `quickstart-rss.json` with an RSS feed to poll:

```json
{
  "rules": [
    { "match": "*", "destination": "messaging" }
  ],
  "feeds": [
    {
      "name": "engadget",
      "url": "https://www.engadget.com/rss.xml"
    }
  ],
  "fetch_links": false,
  "link_policy": { "default": "safe", "rules": [] }
}
```

Start the stack and inspect results:

```sh
docker compose up -d --build
# Wait a minute for the first poll cycle, then check:
docker compose logs rss-connector    # connector fetch logs
docker compose logs glovebox         # scan verdicts
curl -s http://localhost:9090/metrics # Prometheus metrics
```

Scanned items land in the `agents` volume under `messaging/workspace/inbox/`.
Anything flagged lands in `quarantine`.

## Configuration

Glovebox reads a JSON config file (default: `/etc/glovebox/config.json`):

| Key                      | Purpose                                    |
|--------------------------|--------------------------------------------|
| `staging_dir`            | Directory connectors write items to        |
| `quarantine_dir`         | Flagged items held for review              |
| `audit_dir`              | Append-only JSONL scan verdict logs        |
| `failed_dir`             | Items that failed validation (retried)     |
| `agents_dir`             | Root of agent workspace directories        |
| `shared_dir`             | Shared directory for quarantine notifications |
| `rules_file`             | Path to the scanning rules JSON            |
| `scan_workers`           | Number of parallel scan goroutines         |
| `scan_timeout_seconds`   | Per-item timeout (quarantine on expiry)    |
| `scan_chunk_size_bytes`  | Content chunk size for scanning            |
| `metrics_port`           | Port for Prometheus `/metrics` endpoint    |
| `watch_mode`             | `fsnotify` (default) or `polling`          |
| `poll_interval_seconds`  | Polling interval for staging watcher       |
| `agent_allowlist`        | Which agent names are valid destinations   |

The rules file (`configs/default-rules.json`) defines pattern rules, weights,
and the `quarantine_threshold`. See `docs/` for the full configuration reference.

## Installation

### Pre-built binaries

Download from [GitHub Releases](https://github.com/leftathome/glovebox/releases).
Archives are available for Linux (amd64, arm64), macOS (arm64), and Windows
(amd64, arm64). Each archive contains glovebox and all connector binaries.

### Docker images

Published to GitHub Container Registry for linux/amd64 and linux/arm64:

```sh
docker pull ghcr.io/leftathome/glovebox:latest
docker pull ghcr.io/leftathome/glovebox-rss:latest
docker pull ghcr.io/leftathome/glovebox-imap:latest
# Also: glovebox-github, glovebox-gitlab, glovebox-jira, glovebox-trello,
#        glovebox-linkedin, glovebox-meta, glovebox-bluesky, glovebox-x
```

### Helm chart

```sh
helm install glovebox oci://ghcr.io/leftathome/charts/glovebox --version 0.2.0
```

See `docs/deployment.md` for full Kubernetes deployment instructions including
connector configuration via `values.yaml`.

### Building from source

```sh
# Glovebox scanner
go build -o glovebox .

# Connectors (all 10)
for c in rss imap github gitlab jira trello linkedin meta bluesky x; do
  go build -o "${c}-connector" "./connectors/${c}/"
done

# Docker images
docker build -t glovebox .
docker build -t glovebox-rss -f connectors/rss/Dockerfile .

# Run tests
go vet ./...
go test ./... -count=1 -race
```

## Writing custom connectors

Generate a new connector scaffold:

```sh
go run ./generator new-connector <name>
```

This creates a directory under `connectors/<name>/` with starter code, config,
Dockerfile, and README. Implement the `Connector` interface (a single `Poll`
method) and the framework handles staging writes, checkpointing, health
endpoints, and metrics.

See `docs/connector-guide.md` (coming soon) for the full walkthrough.

## License

Apache License 2.0 -- see [LICENSE](LICENSE) for details.
