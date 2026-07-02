# gmail connector

Polls Gmail labels via the Gmail API and stages new messages (raw RFC822,
attachments included) for scanning. Reads the operator's real mailbox
read-only; credentials are provisioned as a secret.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-gmail` |
| Credential class | `real-readonly` (operator's real account, read-only) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | yes (glovebox-enricher-runtime: passthrough + html + pdf + ocr + office) |
| Live integration test | none yet — follow-up (needs `real-readonly` creds provisioned) |

## Authentication

`real-readonly`: an OAuth2 flow against the operator's Google account, granted
the read-only `https://www.googleapis.com/auth/gmail.readonly` scope. The
connector reads:

- `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` — the OAuth2 client credentials.
- `token.json` at `$GLOVEBOX_STATE_DIR/token.json` — the persisted OAuth2 token
  (`access_token` / `refresh_token` / `expiry`); the connector refreshes the
  access token against `https://oauth2.googleapis.com/token` and rewrites the
  file atomically. If the file is missing the connector exits with a permanent
  error instructing the operator to run the auth setup flow.

These come from **Vault** via an ESO **`ExternalSecret`** in-cluster (the same
mechanism the running connector uses), surfaced as the env vars above plus the
mounted `token.json`. Exact Vault path / secret shape: **TBD (private
ci-templates)** per the registry — do not hardcode them here.

## Configuration

Sample: [`connectors/gmail/config.json`](../../connectors/gmail/config.json).
This is the effective config the connector loads (from
`$GLOVEBOX_CONNECTOR_CONFIG`, default `/etc/connector/config.json`).

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
| `label_ids` | `[]string` | no | Gmail label IDs to poll; each is referenced by a `label:<id>` rule (default `["INBOX"]`) |
| `max_results` | int | no | max messages fetched per label per poll (default `25`) |

```json
{
  "label_ids": ["INBOX"],
  "max_results": 25,
  "data_subject_default": "",
  "audience_default": ["household"],
  "rules": [
    {"match": "label:INBOX", "destination": "messaging"},
    {"match": "*", "destination": "general"}
  ]
}
```

## Routing

Match keys emitted by this connector:

- `label:<id>` — a message from the named Gmail label (e.g. `label:INBOX`).
- `*` — fallback for any message.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  gmail:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-gmail
      tag: latest
    config:
      label_ids: ["INBOX"]
      max_results: 25
      data_subject_default: ""
      audience_default: ["household"]
      rules:
        - { match: "label:INBOX", destination: "messaging" }
        - { match: "*", destination: "general" }
    # real-readonly: OAuth2 credentials from Vault via an ESO ExternalSecret.
    # Surfaced as env GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET and the OAuth
    # token file at $GLOVEBOX_STATE_DIR/token.json. Vault path / secret shape:
    # TBD (private ci-templates).
```

## Integration test

No live integration test yet. This is a follow-up: it runs live only
in-cluster (nightly/manual GitLab pipeline) with `GLOVEBOX_INTEGRATION=1` once
the `real-readonly` OAuth credentials are provisioned in Vault/ESO. Until then
the `integration:gmail` job is skipped (and logged), never silently green.
