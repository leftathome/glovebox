# jira connector

Polls Jira projects for issues and stages them for scanning. Authenticated
source; requires an API token and account email.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-jira` |
| Credential class | `test-account` (API token + email) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs a test-account) |

## Authentication

Jira is an authenticated source. The connector reads the account email from the
`JIRA_EMAIL` environment variable and an API token from `JIRA_API_TOKEN`; it
exits if either is unset. The two are used together for Atlassian basic auth
against the Jira Cloud REST API.

For integration testing these are dedicated `test-account` credentials (safe to
read against, no personal data). The secret is provisioned via Vault -> ESO
`ExternalSecret` in-cluster and surfaced as the `JIRA_EMAIL` and
`JIRA_API_TOKEN` env vars the connector reads — the same mechanism the running
connector uses. No secret values live in this repo or in CI variables in
cleartext.

- Vault path: TBD (private ci-templates)
- Secret shape: API token + email

## Configuration

Sample: [`connectors/jira/config.json`](../../connectors/jira/config.json).
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
| `base_url` | string | yes | Jira Cloud base URL (e.g. `https://mycompany.atlassian.net`) |
| `projects` | `[]string` | yes | project keys to poll (e.g. `["PROJ", "OPS"]`); each is referenced by `project:<key>` rules |

```json
{
  "base_url": "https://mycompany.atlassian.net",
  "projects": ["PROJ", "OPS"],
  "rules": [
    {
      "match": "project:PROJ",
      "destination": "project-agent"
    },
    {
      "match": "project:OPS",
      "destination": "ops-agent"
    },
    {
      "match": "*",
      "destination": "default-agent"
    }
  ]
}
```

## Routing

Match keys emitted by this connector:

- `project:<key>` — an issue from the named project.
- `*` — fallback for any issue.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  jira:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-jira
      tag: latest
    config:
      base_url: "https://mycompany.atlassian.net"
      projects: ["PROJ", "OPS"]
      rules:
        - { match: "project:PROJ", destination: "project-agent" }
        - { match: "project:OPS", destination: "ops-agent" }
        - { match: "*", destination: "default-agent" }
    # Secret: API token + email (test-account) via Vault -> ESO ExternalSecret,
    # surfaced as env vars JIRA_EMAIL and JIRA_API_TOKEN.
```

## Integration test

No live integration test yet; this is a follow-up that needs the `test-account`
credentials provisioned. When added, it will run live only in-cluster
(nightly/manual GitLab pipeline) with `GLOVEBOX_INTEGRATION=1` and skip cleanly
everywhere else; until the secret is provisioned the integration job skips (and
logs), never silently green.
