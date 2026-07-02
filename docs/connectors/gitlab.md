# gitlab connector

Polls GitLab projects for events and stages new activity for scanning.
Authenticated source; requires a personal access token.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-gitlab` |
| Credential class | `test-account` (PAT) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs a test-account) |

## Authentication

GitLab is an authenticated source. The connector reads a personal access token
(PAT) from the `GITLAB_TOKEN` environment variable; it exits if the variable is
unset.

For integration testing this is a dedicated `test-account` PAT (safe to read
against, no personal data). The secret is provisioned via Vault -> ESO
`ExternalSecret` in-cluster and surfaced as the `GITLAB_TOKEN` env var the
connector reads — the same mechanism the running connector uses. No secret
values live in this repo or in CI variables in cleartext.

- Vault path: TBD (private ci-templates)
- Secret shape: PAT

## Configuration

Sample: [`connectors/gitlab/config.json`](../../connectors/gitlab/config.json).
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
| `projects` | `[]{path}` | yes | projects to poll; each `path` (e.g. `mygroup/myproject`) is referenced by `project:<path>` rules |
| `base_url` | string | no | GitLab instance base URL (default `https://gitlab.com`) |

```json
{
  "base_url": "https://gitlab.com",
  "projects": [
    {
      "path": "mygroup/myproject"
    }
  ],
  "rules": [
    {
      "match": "project:mygroup/myproject",
      "destination": "dev-agent"
    }
  ]
}
```

## Routing

Match keys emitted by this connector:

- `project:<path>` — an event polled from the named project.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  gitlab:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-gitlab
      tag: latest
    config:
      base_url: "https://gitlab.com"
      projects:
        - { path: "mygroup/myproject" }
      rules:
        - { match: "project:mygroup/myproject", destination: "dev-agent" }
    # Secret: PAT (test-account) via Vault -> ESO ExternalSecret,
    # surfaced as env var GITLAB_TOKEN.
```

## Integration test

No live integration test yet; this is a follow-up that needs the `test-account`
PAT provisioned. When added, it will run live only in-cluster (nightly/manual
GitLab pipeline) with `GLOVEBOX_INTEGRATION=1` and skip cleanly everywhere else;
until the secret is provisioned the integration job skips (and logs), never
silently green.
