# youtube connector

Polls the YouTube Data API for new uploads on watched channels and stages
video metadata (optionally comments and caption metadata) for scanning. Reads
against a dedicated test account's Data API key; no personal data.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-youtube` |
| Credential class | `test-account` (YouTube Data API key) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet (`connectors/youtube/live_integration_test.go` is a follow-up) |

## Authentication

YouTube requires a Data API key. It is provisioned in Vault and synced into the
cluster via an ESO `ExternalSecret`, then surfaced to the connector as the
`YOUTUBE_API_KEY` environment variable (read by `connectors/youtube/main.go`,
which exits non-zero if it is unset). No secret values live in this repo.
Vault path: TBD (private ci-templates).

## Configuration

Sample:
[`connectors/youtube/config.json`](../../connectors/youtube/config.json). This
is the effective config the connector ships with.

Framework fields (from `connector.BaseConfig`, shared by every connector):

| field | type | required | description |
|-------|------|----------|-------------|
| `rules` | `[]Rule` | yes | routing rules; each has `match`, `destination`, optional `tags`, `data_subject`, `audience` |
| `identity` | object | no | `provider` / `auth_method` / `tenant` identity stamped on staged items |
| `fetch_limits` | object | no | `per_source` / `per_poll` caps (0 = unlimited) |
| `data_subject_default` | string | no | default data subject when a rule sets none |
| `audience_default` | `[]string` | no | default audience when a rule sets none |

Connector-specific fields:

| field | type | required | description |
|-------|------|----------|-------------|
| `channel_ids` | `[]string` | yes | channel IDs to poll; each is referenced by `channel:<id>` rules |
| `fetch_comments` | bool | no | fetch top-level comments per video (default true) |
| `max_comments` | int | no | cap on comments fetched per video (default 25) |
| `fetch_captions` | bool | no | fetch caption metadata per video (default true) |

```json
{
  "rules": [
    {
      "match": "channel:UC_EXAMPLE_CHANNEL_ID",
      "destination": "media-agent",
      "tags": {
        "source_type": "video",
        "priority": "normal"
      }
    }
  ],
  "channel_ids": [
    "UC_EXAMPLE_CHANNEL_ID"
  ]
}
```

## Routing

Match keys emitted by this connector:

- `channel:<id>` — a video from the named channel (the channel ID is the match
  key).

Each matched rule sets the `destination` agent and, optionally, `tags`,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  youtube:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-youtube
      tag: latest
    config:
      rules:
        - match: "channel:UC_EXAMPLE_CHANNEL_ID"
          destination: "media-agent"
          tags: { source_type: "video", priority: "normal" }
      channel_ids:
        - "UC_EXAMPLE_CHANNEL_ID"
    # Data API key from Vault via ESO, surfaced as YOUTUBE_API_KEY.
```

## Integration test

No live integration test exists yet; adding
`connectors/youtube/live_integration_test.go` (wiring `YOUTUBE_API_KEY` from
the in-cluster ESO secret, running green only under `GLOVEBOX_INTEGRATION=1`
and skipping cleanly elsewhere) is a follow-up.
