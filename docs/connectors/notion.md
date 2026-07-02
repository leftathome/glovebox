# notion connector

Polls Notion databases and pages and stages new content for scanning.
Authenticated source; requires an integration token.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-notion` |
| Credential class | `test-account` (integration token) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs a test-account) |

## Authentication

Notion is an authenticated source. The connector reads an integration token from
the `NOTION_TOKEN` environment variable; it exits if the variable is unset. The
token authorizes access to whichever databases and pages the integration has
been shared with in the Notion workspace.

For integration testing this is a dedicated `test-account` integration token
against a test workspace (safe to read against, no personal data). The secret is
provisioned via Vault -> ESO `ExternalSecret` in-cluster and surfaced as the
`NOTION_TOKEN` env var the connector reads — the same mechanism the running
connector uses. No secret values live in this repo or in CI variables in
cleartext.

- Vault path: TBD (private ci-templates)
- Secret shape: integration token (test workspace)

## Configuration

Sample: [`connectors/notion/config.json`](../../connectors/notion/config.json).
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
| `database_ids` | `[]string` | no | database IDs to poll; each is referenced by `database:<id>` rules |
| `page_ids` | `[]string` | no | page IDs to poll; each is referenced by `page:<id>` rules |

```json
{
  "rules": [
    {
      "match": "database:your-database-id",
      "destination": "knowledge-agent",
      "tags": {
        "source_type": "knowledge_base",
        "priority": "normal"
      }
    },
    {
      "match": "page:your-page-id",
      "destination": "knowledge-agent"
    }
  ],
  "database_ids": [
    "your-database-id"
  ],
  "page_ids": [
    "your-page-id"
  ]
}
```

## Routing

Match keys emitted by this connector:

- `database:<id>` — an item from the named database.
- `page:<id>` — the named page.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  notion:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-notion
      tag: latest
    config:
      rules:
        - { match: "database:your-database-id", destination: "knowledge-agent", tags: { source_type: "knowledge_base", priority: "normal" } }
        - { match: "page:your-page-id", destination: "knowledge-agent" }
      database_ids:
        - "your-database-id"
      page_ids:
        - "your-page-id"
    # Secret: integration token (test-account) via Vault -> ESO ExternalSecret,
    # surfaced as env var NOTION_TOKEN.
```

## Integration test

No live integration test yet; this is a follow-up that needs the `test-account`
integration token provisioned. When added, it will run live only in-cluster
(nightly/manual GitLab pipeline) with `GLOVEBOX_INTEGRATION=1` and skip cleanly
everywhere else; until the secret is provisioned the integration job skips (and
logs), never silently green.
