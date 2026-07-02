# rss connector

Polls RSS/Atom feeds and stages new entries for scanning. Public source; no
credentials required.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-rss` |
| Credential class | `none` (public source) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | `connectors/rss/live_integration_test.go` (`//go:build integration`) |

## Authentication

None. RSS is a public source, so no secret is provisioned and no
`ExternalSecret` / `secrets` reference is needed to run this connector.

## Configuration

Sample: [`connectors/rss/config.json`](../../connectors/rss/config.json). This
is the effective config the integration test drives.

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
| `feeds` | `[]{name,url}` | yes | feeds to poll; `name` is referenced by `feed:<name>` rules |
| `fetch_links` | bool | no | follow entry links to fetch link targets (default false) |
| `link_policy` | object | no | `default` (`safe`/…) + per-domain `rules` governing link following |

```json
{
  "rules": [
    { "match": "feed:example-blog", "destination": "reader" },
    { "match": "*", "destination": "inbox" }
  ],
  "feeds": [ { "name": "example-blog", "url": "https://example.com/feed.xml" } ],
  "fetch_links": false,
  "link_policy": { "default": "safe", "rules": [] }
}
```

## Routing

Match keys emitted by this connector:

- `feed:<name>` — an entry from the named feed.
- `*` — fallback for any entry.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  rss:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-rss
      tag: latest
    config:
      rules:
        - { match: "feed:example-blog", destination: "reader" }
        - { match: "*", destination: "inbox" }
      feeds:
        - { name: "example-blog", url: "https://example.com/feed.xml" }
      fetch_links: false
      link_policy: { default: "safe", rules: [] }
    # No secrets: RSS is a public source.
```

## Integration test

Runs live only in-cluster (nightly/manual GitLab pipeline) with
`GLOVEBOX_INTEGRATION=1`; skips cleanly everywhere else. No credentials needed.
