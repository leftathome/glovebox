# x connector

Polls the X (Twitter) API for mentions and ingests Account Activity webhook
events, staging them for scanning. Authenticated source; requires an X
API/bearer token.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-x` |
| Credential class | `test-account` — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs a test-account) |

## Authentication

X requires an **API / bearer token**. The connector calls the X API
(`https://api.x.com`) with a static bearer token and reads:

- `X_BEARER_TOKEN` — the API/bearer token (required).
- `X_WEBHOOK_SECRET` — signing secret used to verify Account Activity webhook
  payloads (optional).

For the integration harness this is a dedicated `test-account`, never a
personal account. The credential is provisioned in **Vault** and delivered to
the in-cluster CI job via an ESO **`ExternalSecret`**, surfaced as the
`X_BEARER_TOKEN` (and optional `X_WEBHOOK_SECRET`) env vars the connector reads
— the same mechanism the running connector uses. No secret values live in this
repo or in CI variables in cleartext.

- Vault path: TBD (private ci-templates)
- Secret shape: API/bearer token

## Configuration

Sample: [`connectors/x/config.json`](../../connectors/x/config.json). This is
the effective config the integration test drives.

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
| `user_id` | string | yes | X user ID whose mentions are polled |
| `feed_types` | `[]string` | no | poll feeds to consider, e.g. `["mentions", "timeline"]` |

```json
{
  "rules": [
    {
      "match": "feed:mentions",
      "destination": "social-agent",
      "tags": {
        "source_type": "social",
        "priority": "normal"
      }
    },
    {
      "match": "event:tweet_create_events",
      "destination": "social-agent"
    },
    {
      "match": "event:favorite_events",
      "destination": "social-agent"
    }
  ],
  "user_id": "YOUR_USER_ID",
  "feed_types": ["mentions"]
}
```

## Routing

Match keys emitted by this connector:

- `feed:mentions` — a mention polled from the X API for `user_id`.
- `event:<event_type>` — an Account Activity webhook event, e.g.
  `event:tweet_create_events`, `event:favorite_events`.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  x:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-x
      tag: latest
    config:
      user_id: "YOUR_USER_ID"
      feed_types: ["mentions"]
      rules:
        - match: "feed:mentions"
          destination: "social-agent"
          tags: { source_type: "social", priority: "normal" }
        - { match: "event:tweet_create_events", destination: "social-agent" }
        - { match: "event:favorite_events", destination: "social-agent" }
    # test-account credential: an ESO ExternalSecret binds the Vault secret and
    # surfaces it as the X_BEARER_TOKEN (and optional X_WEBHOOK_SECRET) env vars.
```

## Integration test

Runs live only in-cluster (nightly/manual GitLab pipeline) with
`GLOVEBOX_INTEGRATION=1`; skips cleanly everywhere else. A live integration
test is a follow-up: it needs the `test-account` API/bearer token provisioned
via Vault/ESO, and until that secret exists the job skips (and is logged),
never silently green.
