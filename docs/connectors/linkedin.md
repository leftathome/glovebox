# linkedin connector

Polls LinkedIn shares/posts for a person via the LinkedIn API and stages them
for scanning. Authenticated source; requires a LinkedIn OAuth token.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-linkedin` |
| Credential class | `test-account` — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs a test-account) |

## Authentication

LinkedIn requires an **OAuth token**. The connector calls the LinkedIn API
(`https://api.linkedin.com`) with a static token and reads:

- `LINKEDIN_TOKEN` — the OAuth access token (required).
- `LINKEDIN_PERSON_ID` — the person URN ID whose shares are polled (required).

For the integration harness this is a dedicated `test-account`, never a
personal account. The credential is provisioned in **Vault** and delivered to
the in-cluster CI job via an ESO **`ExternalSecret`**, surfaced as the
`LINKEDIN_TOKEN` / `LINKEDIN_PERSON_ID` env vars the connector reads — the same
mechanism the running connector uses. No secret values live in this repo or in
CI variables in cleartext.

- Vault path: TBD (private ci-templates)
- Secret shape: OAuth token

## Configuration

Sample: [`connectors/linkedin/config.json`](../../connectors/linkedin/config.json).
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
| `feed_types` | `[]string` | no | feeds to poll, e.g. `["posts", "shares"]`; each is referenced by `feed:<type>` rules |

```json
{
  "rules": [
    {
      "match": "feed:shares",
      "destination": "social-agent",
      "tags": {
        "source_type": "social",
        "priority": "low"
      }
    },
    {
      "match": "feed:posts",
      "destination": "social-agent",
      "tags": {
        "source_type": "social",
        "priority": "low"
      }
    }
  ],
  "feed_types": ["posts", "shares"]
}
```

## Routing

Match keys emitted by this connector:

- `feed:<type>` — an item from the named feed type, e.g. `feed:posts`,
  `feed:shares`.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  linkedin:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-linkedin
      tag: latest
    config:
      feed_types: ["posts", "shares"]
      rules:
        - match: "feed:shares"
          destination: "social-agent"
          tags: { source_type: "social", priority: "low" }
        - match: "feed:posts"
          destination: "social-agent"
          tags: { source_type: "social", priority: "low" }
    # test-account credential: an ESO ExternalSecret binds the Vault secret and
    # surfaces it as the LINKEDIN_TOKEN / LINKEDIN_PERSON_ID env vars.
```

## Integration test

Runs live only in-cluster (nightly/manual GitLab pipeline) with
`GLOVEBOX_INTEGRATION=1`; skips cleanly everywhere else. A live integration
test is a follow-up: it needs the `test-account` OAuth token provisioned via
Vault/ESO, and until that secret exists the job skips (and is logged), never
silently green.
