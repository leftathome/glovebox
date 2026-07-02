# github connector

Polls GitHub repositories and (optionally) verifies inbound webhooks, staging
new activity for scanning. Authenticated source; requires a personal access
token.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-github` |
| Credential class | `test-account` (PAT) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs a test-account) |

## Authentication

GitHub is an authenticated source. The connector reads a personal access token
(PAT) from the `GITHUB_TOKEN` environment variable; it exits if the variable is
unset. If a `webhook_secret_env` is configured, the connector reads the named
env var (by default `GITHUB_WEBHOOK_SECRET`) and uses its value to verify the
`X-Hub-Signature-256` HMAC on inbound webhooks.

For integration testing this is a dedicated `test-account` PAT (safe to read
against, no personal data). The secret is provisioned via Vault -> ESO
`ExternalSecret` in-cluster and surfaced as the `GITHUB_TOKEN` (and, where
webhooks are exercised, `GITHUB_WEBHOOK_SECRET`) env vars the connector reads —
the same mechanism the running connector uses. No secret values live in this
repo or in CI variables in cleartext.

- Vault path: TBD (private ci-templates)
- Secret shape: PAT (plus optional webhook secret)

## Configuration

Sample: [`connectors/github/config.json`](../../connectors/github/config.json).
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
| `repos` | `[]{owner,repo}` | yes | repositories to poll; each emits `repo:<owner>/<repo>` and `event:<type>` match keys |
| `webhook_secret_env` | string | no | name of the env var holding the webhook HMAC secret (e.g. `GITHUB_WEBHOOK_SECRET`) |

```json
{
  "rules": [
    {
      "match": "repo:myorg/myrepo",
      "destination": "dev-agent",
      "tags": {
        "source_type": "vcs",
        "priority": "normal"
      }
    },
    {
      "match": "event:push",
      "destination": "dev-agent"
    },
    {
      "match": "event:pull_request",
      "destination": "dev-agent"
    }
  ],
  "repos": [
    {
      "owner": "myorg",
      "repo": "myrepo"
    }
  ],
  "webhook_secret_env": "GITHUB_WEBHOOK_SECRET"
}
```

## Routing

Match keys emitted by this connector:

- `repo:<owner>/<repo>` — activity polled from the named repository.
- `event:<type>` — an inbound webhook of the given type (e.g. `event:push`,
  `event:pull_request`).

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  github:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-github
      tag: latest
    config:
      rules:
        - { match: "repo:myorg/myrepo", destination: "dev-agent", tags: { source_type: "vcs", priority: "normal" } }
        - { match: "event:push", destination: "dev-agent" }
        - { match: "event:pull_request", destination: "dev-agent" }
      repos:
        - { owner: "myorg", repo: "myrepo" }
      webhook_secret_env: "GITHUB_WEBHOOK_SECRET"
    # Secret: PAT (test-account) via Vault -> ESO ExternalSecret,
    # surfaced as env vars GITHUB_TOKEN (and GITHUB_WEBHOOK_SECRET).
```

## Integration test

No live integration test yet; this is a follow-up that needs the `test-account`
PAT provisioned. When added, it will run live only in-cluster (nightly/manual
GitLab pipeline) with `GLOVEBOX_INTEGRATION=1` and skip cleanly everywhere else;
until the secret is provisioned the integration job skips (and logs), never
silently green.
