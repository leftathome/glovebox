# meta connector

Polls a Facebook Page feed via the Meta Graph API and ingests webhook events,
staging them for scanning. Authenticated source; requires a Meta OAuth app
access token.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-meta` |
| Credential class | `test-account` — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs a test-account) |

## Authentication

Meta requires an **OAuth app access token**. The connector calls the Graph API
(`https://graph.facebook.com`) with a static access token and reads:

- `META_ACCESS_TOKEN` — the OAuth app access token (required).
- `META_APP_SECRET` — app secret used to sign/verify webhook payloads (optional).
- `META_VERIFY_TOKEN` — token echoed during webhook subscription verification
  (optional).

For the integration harness this is a dedicated `test-account`, never a
personal account. The credential is provisioned in **Vault** and delivered to
the in-cluster CI job via an ESO **`ExternalSecret`**, surfaced as the
`META_ACCESS_TOKEN` (and optional `META_APP_SECRET` / `META_VERIFY_TOKEN`) env
vars the connector reads — the same mechanism the running connector uses. No
secret values live in this repo or in CI variables in cleartext.

- Vault path: TBD (private ci-templates)
- Secret shape: OAuth app token

## Configuration

Sample: [`connectors/meta/config.json`](../../connectors/meta/config.json).
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
| `page_id` | string | yes | Facebook Page ID whose feed is polled |
| `fetch_posts` | bool | no | fetch Page posts/feed (default false) |
| `fetch_comments` | bool | no | fetch comments on Page posts (default false) |

```json
{
  "rules": [
    {
      "match": "platform:facebook",
      "destination": "social-agent",
      "tags": {
        "source_type": "social",
        "platform": "facebook"
      }
    },
    {
      "match": "event:feed",
      "destination": "social-agent"
    },
    {
      "match": "event:messages",
      "destination": "social-agent"
    }
  ],
  "page_id": "123456789",
  "fetch_posts": true,
  "fetch_comments": false
}
```

## Routing

Match keys emitted by this connector:

- `platform:facebook` — a post/item polled from the Page feed.
- `event:<object_type>` — a Graph API webhook event, e.g. `event:feed`,
  `event:messages`.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  meta:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-meta
      tag: latest
    config:
      page_id: "123456789"
      fetch_posts: true
      fetch_comments: false
      rules:
        - match: "platform:facebook"
          destination: "social-agent"
          tags: { source_type: "social", platform: "facebook" }
        - { match: "event:feed", destination: "social-agent" }
        - { match: "event:messages", destination: "social-agent" }
    # test-account credential: an ESO ExternalSecret binds the Vault secret and
    # surfaces it as the META_ACCESS_TOKEN (and optional META_APP_SECRET /
    # META_VERIFY_TOKEN) env vars.
```

## Integration test

Runs live only in-cluster (nightly/manual GitLab pipeline) with
`GLOVEBOX_INTEGRATION=1`; skips cleanly everywhere else. A live integration
test is a follow-up: it needs the `test-account` OAuth app token provisioned
via Vault/ESO, and until that secret exists the job skips (and is logged),
never silently green.
