# semantic-scholar connector

Polls the Semantic Scholar paper search API for papers matching configured
queries and stages them (predominantly PDFs) for scanning. Requires a free
API key: the keyless public tier returns HTTP 429 immediately.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-semantic-scholar` |
| Credential class | `test-account` (free API key) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | yes (`FROM ${ENRICHER_BASE}`) |
| Live integration test | `connectors/semantic-scholar/live_integration_test.go` (`//go:build integration`) |

## Authentication

The connector reads the environment variable `SEMANTIC_SCHOLAR_API_KEY` (a free
Semantic Scholar API key). Although Semantic Scholar is a public source, the
keyless tier returns HTTP 429 immediately and the connector treats the fetch
failure as an error rather than skipping, so a key is required. In-cluster the
key is provisioned in Vault and surfaced to the connector via an ESO
`ExternalSecret` (logical secret: free `SEMANTIC_SCHOLAR_API_KEY`; exact Vault
path / ESO object name live in the private `homelab/ci-templates`). The live
integration test skips cleanly until that key is provisioned.

## Configuration

Sample: [`connectors/semantic-scholar/config.json`](../../connectors/semantic-scholar/config.json).
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
| `queries` | `[]{name,query}` | yes | search queries to poll; `name` is referenced by `query:<name>` rules |
| `queries[].name` | string | yes | logical name of the query, used in `query:<name>` match keys |
| `queries[].query` | string | yes | Semantic Scholar search expression, e.g. `large language models` |

```json
{
  "rules": [
    {
      "match": "query:*",
      "destination": "research"
    }
  ],
  "queries": [
    {
      "name": "example-topic",
      "query": "large language models"
    }
  ]
}
```

## Routing

Match keys emitted by this connector:

- `query:<name>` — a paper returned by the named query.
- `query:*` — wildcard fallback for any query (as in the sample config).

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  semantic-scholar:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-semantic-scholar
      tag: latest
    config:
      rules:
        - { match: "query:*", destination: "research" }
      queries:
        - { name: "example-topic", query: "large language models" }
    # Free API key from Vault via ESO; surfaced as an env var.
    secrets:
      externalSecretName: glovebox-semantic-scholar   # ESO ExternalSecret -> Secret
    env:
      - name: SEMANTIC_SCHOLAR_API_KEY
        valueFrom:
          secretKeyRef:
            name: glovebox-semantic-scholar
            key: SEMANTIC_SCHOLAR_API_KEY
```

## Integration test

Runs live only in-cluster (nightly/manual GitLab pipeline) with
`GLOVEBOX_INTEGRATION=1`; skips cleanly everywhere else. Requires the free
`SEMANTIC_SCHOLAR_API_KEY` (Vault -> ESO); the job skips (and logs) until that
key is provisioned, never silently green.
