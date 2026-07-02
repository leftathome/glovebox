# bluesky connector

Polls a Bluesky (AT Protocol) author feed and stages new posts for scanning.
Authenticated source; requires a Bluesky handle plus an app password.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-bluesky` |
| Credential class | `test-account` — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs a test-account) |

## Authentication

Bluesky requires a **handle + app password**. The connector authenticates
against the AT Protocol service (`https://bsky.social` by default) and reads
two env vars:

- `BLUESKY_IDENTIFIER` — the account handle / identifier (required).
- `BLUESKY_APP_PASSWORD` — an app password issued for that account (required).

For the integration harness this is a dedicated `test-account`, never a
personal account. The credential is provisioned in **Vault** and delivered to
the in-cluster CI job via an ESO **`ExternalSecret`**, surfaced as the
`BLUESKY_IDENTIFIER` / `BLUESKY_APP_PASSWORD` env vars the connector reads —
the same mechanism the running connector uses. No secret values live in this
repo or in CI variables in cleartext.

- Vault path: TBD (private ci-templates)
- Secret shape: handle + app password

## Configuration

Sample: [`connectors/bluesky/config.json`](../../connectors/bluesky/config.json).
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
| `service` | string | no | AT Protocol service base (default `https://bsky.social`) |
| `feed_uris` | `[]string` | no | feed URIs to consider, e.g. `at://did:plc:.../app.bsky.feed.getAuthorFeed` |

```json
{
  "service": "https://bsky.social",
  "feed_uris": [],
  "rules": [
    {
      "match": "feed:timeline",
      "destination": "home-agent",
      "tags": {
        "source_type": "social",
        "platform": "bluesky"
      }
    }
  ]
}
```

## Routing

Match keys emitted by this connector:

- `feed:timeline` — a post from the authenticated account's author feed.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  bluesky:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-bluesky
      tag: latest
    config:
      service: "https://bsky.social"
      feed_uris: []
      rules:
        - match: "feed:timeline"
          destination: "home-agent"
          tags: { source_type: "social", platform: "bluesky" }
    # test-account credential: an ESO ExternalSecret binds the Vault secret and
    # surfaces it as the BLUESKY_IDENTIFIER / BLUESKY_APP_PASSWORD env vars.
```

## Integration test

Runs live only in-cluster (nightly/manual GitLab pipeline) with
`GLOVEBOX_INTEGRATION=1`; skips cleanly everywhere else. A live integration
test is a follow-up: it needs the `test-account` handle + app password
provisioned via Vault/ESO, and until that secret exists the job skips (and is
logged), never silently green.
