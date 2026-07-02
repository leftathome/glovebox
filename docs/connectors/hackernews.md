# hackernews connector

Polls Hacker News feeds via the public Firebase API and stages new stories
(optionally with top comments) for scanning. Public source; no credentials
required.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-hackernews` |
| Credential class | `none` (public source) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | `connectors/hackernews/live_integration_test.go` (`//go:build integration`) |

## Authentication

None. Hacker News is a public source, so no secret is provisioned and no
`ExternalSecret` / `secrets` reference is needed to run this connector.

## Configuration

Sample: [`connectors/hackernews/config.json`](../../connectors/hackernews/config.json).
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
| `feeds` | `[]string` | yes | feeds to poll; one or more of `top`, `new`, `best`, `ask`, `show`; referenced by `feed:<name>` rules |
| `follow_comments` | bool | no | fetch top-level comments for each story (default false) |
| `max_comments` | int | no | max comments to fetch per story when `follow_comments` is set (default 10) |

```json
{
  "feeds": ["top", "new", "best"],
  "follow_comments": true,
  "max_comments": 10,
  "rules": [
    {"match": "feed:top", "destination": "news-agent"},
    {"match": "feed:new", "destination": "news-agent"},
    {"match": "feed:best", "destination": "news-agent"},
    {"match": "feed:ask", "destination": "news-agent"},
    {"match": "feed:show", "destination": "news-agent"}
  ],
  "fetch_limits": {
    "per_source": 30,
    "per_poll": 100
  }
}
```

## Routing

Match keys emitted by this connector:

- `feed:<name>` — a story from the named feed (`top`, `new`, `best`, `ask`, `show`).
- `*` — fallback for any story.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  hackernews:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-hackernews
      tag: latest
    config:
      feeds: ["top", "new", "best"]
      follow_comments: true
      max_comments: 10
      rules:
        - { match: "feed:top", destination: "news-agent" }
        - { match: "feed:new", destination: "news-agent" }
        - { match: "feed:best", destination: "news-agent" }
        - { match: "feed:ask", destination: "news-agent" }
        - { match: "feed:show", destination: "news-agent" }
      fetch_limits:
        per_source: 30
        per_poll: 100
    # No secrets: Hacker News is a public source.
```

## Integration test

Runs live only in-cluster (nightly/manual GitLab pipeline) with
`GLOVEBOX_INTEGRATION=1`; skips cleanly everywhere else. No credentials needed.
