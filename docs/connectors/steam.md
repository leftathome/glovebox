# steam connector

Polls the Steam Web API for per-app reviews and news and stages new items for
scanning. Reads against a dedicated test account's Web API key; no personal
data.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-steam` |
| Credential class | `test-account` (Steam Web API key) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet (`connectors/steam/live_integration_test.go` is a follow-up) |

## Authentication

Steam requires a Web API key. It is provisioned in Vault and synced into the
cluster via an ESO `ExternalSecret`, then surfaced to the connector as the
`STEAM_API_KEY` environment variable (read by `connectors/steam/main.go`). No
secret values live in this repo. Vault path: TBD (private ci-templates).

## Configuration

Sample: [`connectors/steam/config.json`](../../connectors/steam/config.json).
This is the effective config the connector ships with.

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
| `apps` | `[]{id,name}` | yes | Steam apps to monitor; `id` is the Steam appid, `name` is the display name referenced by `app:<name>` rules |
| `fetch_reviews` | bool | no | poll the Steam reviews API for each app (default false) |
| `fetch_news` | bool | no | poll the Steam news API for each app (default false) |

```json
{
  "rules": [
    {
      "match": "app:*",
      "destination": "inbox"
    }
  ],
  "apps": [
    {
      "id": "440",
      "name": "team-fortress-2"
    }
  ],
  "fetch_reviews": true,
  "fetch_news": true
}
```

## Routing

Match keys emitted by this connector:

- `app:<name>` — a review or news item from the named app (both surfaces use
  the app's `name` as the match key).
- `app:*` — fallback across all monitored apps.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  steam:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-steam
      tag: latest
    config:
      rules:
        - { match: "app:*", destination: "inbox" }
      apps:
        - { id: "440", name: "team-fortress-2" }
      fetch_reviews: true
      fetch_news: true
    # Web API key from Vault via ESO, surfaced as STEAM_API_KEY.
```

## Integration test

No live integration test exists yet; adding
`connectors/steam/live_integration_test.go` (wiring `STEAM_API_KEY` from the
in-cluster ESO secret, running green only under `GLOVEBOX_INTEGRATION=1` and
skipping cleanly elsewhere) is a follow-up.
