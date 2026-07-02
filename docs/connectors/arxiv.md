# arxiv connector

Polls the arXiv API for new papers matching configured search queries and
stages them (predominantly PDFs) for scanning. Public source; no credentials
required.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-arxiv` |
| Credential class | `none` (public source) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | yes (`FROM ${ENRICHER_BASE}`) |
| Live integration test | `connectors/arxiv/live_integration_test.go` (`//go:build integration`) |

## Authentication

None. arXiv is a public source, so no secret is provisioned and no
`ExternalSecret` / `secrets` reference is needed to run this connector.

## Configuration

Sample: [`connectors/arxiv/config.json`](../../connectors/arxiv/config.json).
This is the effective config the integration test drives.

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
| `queries` | `[]{name,query,max_results}` | yes | search queries to poll; `name` is referenced by `query:<name>` rules |
| `queries[].name` | string | yes | logical name of the query, used in `query:<name>` match keys |
| `queries[].query` | string | yes | arXiv search expression, e.g. `cat:cs.AI` or `all:transformer` |
| `queries[].max_results` | int | no | max results per query (default 25) |

```json
{
  "rules": [
    {
      "match": "query:ai-papers",
      "destination": "research"
    },
    {
      "match": "*",
      "destination": "inbox"
    }
  ],
  "queries": [
    {
      "name": "ai-papers",
      "query": "cat:cs.AI",
      "max_results": 25
    }
  ]
}
```

## Routing

Match keys emitted by this connector:

- `query:<name>` — a paper returned by the named query.
- `*` — fallback for any paper.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  arxiv:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-arxiv
      tag: latest
    config:
      rules:
        - { match: "query:ai-papers", destination: "research" }
        - { match: "*", destination: "inbox" }
      queries:
        - { name: "ai-papers", query: "cat:cs.AI", max_results: 25 }
    # No secrets: arXiv is a public source.
```

## Integration test

Runs live only in-cluster (nightly/manual GitLab pipeline) with
`GLOVEBOX_INTEGRATION=1`; skips cleanly everywhere else. No credentials needed.
