# imap connector

Polls IMAP folders for new messages and stages them for scanning. Reads the
operator's real mailbox read-only, so it requires host + user + app-password
credentials.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-imap` |
| Credential class | `real-readonly` (host + user + app password) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | yes (`FROM ${ENRICHER_BASE}`) |
| Live integration test | follow-up (only fake-driven `connector_test.go` exists today; no `live_integration_test.go` yet) |

## Authentication

The connector reads its IMAP credentials from environment variables
(`connectors/imap/client.go`):

| env var | required | description |
|---------|----------|-------------|
| `IMAP_HOST` | yes | IMAP server hostname |
| `IMAP_USERNAME` | yes | mailbox username |
| `IMAP_PASSWORD` | yes | app password / mailbox password |
| `IMAP_PORT` | no | server port (defaults applied by the client) |
| `IMAP_TLS` | no | toggle TLS for the connection |

`IMAP_HOST`, `IMAP_USERNAME`, and `IMAP_PASSWORD` are mandatory; the client
errors out if any is missing. In-cluster these are provisioned in Vault and
surfaced to the connector via an ESO `ExternalSecret` (logical secret: host +
user + app password; exact Vault path / ESO object name live in the private
`homelab/ci-templates`).

## Configuration

Sample: [`connectors/imap/config.json`](../../connectors/imap/config.json).
This is the effective config the tests drive.

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
| `folders` | `[]{name}` | yes | IMAP folders to poll; `name` is referenced by `folder:<name>` rules |
| `folders[].name` | string | yes | folder/mailbox name, e.g. `INBOX` |

```json
{
  "folders": [
    {"name": "INBOX"}
  ],
  "rules": [
    {"match": "folder:INBOX", "destination": "messaging"},
    {"match": "*", "destination": "general"}
  ]
}
```

## Routing

Match keys emitted by this connector:

- `folder:<name>` — a message from the named IMAP folder (e.g. `folder:INBOX`).
- `*` — fallback for any message.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  imap:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-imap
      tag: latest
    config:
      folders:
        - { name: "INBOX" }
      rules:
        - { match: "folder:INBOX", destination: "messaging" }
        - { match: "*", destination: "general" }
    # Real mailbox (read-only) credentials from Vault via ESO; surfaced as env vars.
    secrets:
      externalSecretName: glovebox-imap   # ESO ExternalSecret -> Secret
    env:
      - name: IMAP_HOST
        valueFrom: { secretKeyRef: { name: glovebox-imap, key: IMAP_HOST } }
      - name: IMAP_USERNAME
        valueFrom: { secretKeyRef: { name: glovebox-imap, key: IMAP_USERNAME } }
      - name: IMAP_PASSWORD
        valueFrom: { secretKeyRef: { name: glovebox-imap, key: IMAP_PASSWORD } }
      # Optional:
      # - name: IMAP_PORT
      #   valueFrom: { secretKeyRef: { name: glovebox-imap, key: IMAP_PORT } }
      # - name: IMAP_TLS
      #   valueFrom: { secretKeyRef: { name: glovebox-imap, key: IMAP_TLS } }
```

## Integration test

A live integration test is a follow-up: today only a fake-driven
`connector_test.go` exercises the connector logic against a stub `IMAPClient`,
and no `live_integration_test.go` exists yet. When added, it will run live only
in-cluster (nightly/manual GitLab pipeline) with `GLOVEBOX_INTEGRATION=1` and
skip cleanly everywhere else, wiring the real client from `IMAP_HOST` /
`IMAP_USERNAME` / `IMAP_PASSWORD` (Vault -> ESO). Until the secret is
provisioned the job skips (and logs), never silently green.
