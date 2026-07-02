# trello connector

Polls Trello boards and stages new activity for scanning. Authenticated
source; requires an API key and token.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-trello` |
| Credential class | `test-account` (key + token) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs a test-account) |

## Authentication

Trello is an authenticated source. The connector reads an API key from the
`TRELLO_API_KEY` environment variable and a token from `TRELLO_TOKEN`; it exits
if either is unset. The two are sent together as query parameters on Trello REST
API requests.

For integration testing these are dedicated `test-account` credentials (safe to
read against, no personal data). The secret is provisioned via Vault -> ESO
`ExternalSecret` in-cluster and surfaced as the `TRELLO_API_KEY` and
`TRELLO_TOKEN` env vars the connector reads — the same mechanism the running
connector uses. No secret values live in this repo or in CI variables in
cleartext.

- Vault path: TBD (private ci-templates)
- Secret shape: key + token

## Configuration

Sample: [`connectors/trello/config.json`](../../connectors/trello/config.json).
This is the effective config the connector loads.

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
| `boards` | `[]{id,name}` | yes | boards to poll; each `name` is referenced by `board:<name>` rules |

```json
{
  "boards": [],
  "rules": [
    {
      "match": "*",
      "destination": "home-agent"
    }
  ]
}
```

## Routing

Match keys emitted by this connector:

- `board:<name>` — activity from the named board.
- `*` — fallback for any item.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  trello:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-trello
      tag: latest
    config:
      boards:
        - { id: "abc123", name: "My Board" }
      rules:
        - { match: "board:My Board", destination: "home-agent" }
        - { match: "*", destination: "home-agent" }
    # Secret: key + token (test-account) via Vault -> ESO ExternalSecret,
    # surfaced as env vars TRELLO_API_KEY and TRELLO_TOKEN.
```

## Integration test

No live integration test yet; this is a follow-up that needs the `test-account`
credentials provisioned. When added, it will run live only in-cluster
(nightly/manual GitLab pipeline) with `GLOVEBOX_INTEGRATION=1` and skip cleanly
everywhere else; until the secret is provisioned the integration job skips (and
logs), never silently green.
