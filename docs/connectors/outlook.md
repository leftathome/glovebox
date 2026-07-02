# outlook connector

Polls Outlook mail folders via the Microsoft Graph API and stages new messages
(attachments included) for scanning. Reads the operator's real mailbox
read-only; credentials are provisioned as a secret.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-outlook` |
| Credential class | `real-readonly` (operator's real account, read-only) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | yes (glovebox-enricher-runtime: passthrough + html + pdf + ocr + office) |
| Live integration test | none yet — follow-up (needs `real-readonly` creds provisioned) |

## Authentication

`real-readonly`: an OAuth2 flow against the operator's Microsoft account,
granted the read-only `https://graph.microsoft.com/Mail.Read` scope. The
connector reads:

- `MS_CLIENT_ID` / `MS_CLIENT_SECRET` — the Azure AD app (OAuth2 client)
  credentials.
- `MS_TENANT_ID` — the Azure AD tenant, used to build the token endpoint
  `https://login.microsoftonline.com/<tenant>/oauth2/v2.0/token`.
- `token.json` at `$GLOVEBOX_STATE_DIR/token.json` — the persisted OAuth2 token
  (`access_token` / `refresh_token` / `expiry`); the connector refreshes the
  access token and rewrites the file atomically.

These come from **Vault** via an ESO **`ExternalSecret`** in-cluster (the same
mechanism the running connector uses), surfaced as the env vars above plus the
mounted `token.json`. Exact Vault path / secret shape: **TBD (private
ci-templates)** per the registry — do not hardcode them here.

## Configuration

Sample:
[`connectors/outlook/config.json`](../../connectors/outlook/config.json). This
is the effective config the connector loads (from
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
| `folder_ids` | `[]string` | no | Graph mail folder IDs to poll; each is referenced by a `folder:<id>` rule (default `["inbox"]`) |
| `max_results` | int | no | max messages fetched per folder per poll (default `25`) |

```json
{
  "folder_ids": ["inbox"],
  "max_results": 25,
  "rules": [
    {"match": "folder:inbox", "destination": "messaging"},
    {"match": "*", "destination": "general"}
  ]
}
```

## Routing

Match keys emitted by this connector:

- `folder:<id>` — a message from the named Graph mail folder (e.g.
  `folder:inbox`).
- `*` — fallback for any message.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  outlook:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-outlook
      tag: latest
    config:
      folder_ids: ["inbox"]
      max_results: 25
      rules:
        - { match: "folder:inbox", destination: "messaging" }
        - { match: "*", destination: "general" }
    # real-readonly: OAuth2 credentials from Vault via an ESO ExternalSecret.
    # Surfaced as env MS_CLIENT_ID / MS_CLIENT_SECRET / MS_TENANT_ID and the
    # OAuth token file at $GLOVEBOX_STATE_DIR/token.json. Vault path / secret
    # shape: TBD (private ci-templates).
```

## Integration test

No live integration test yet. This is a follow-up: it runs live only
in-cluster (nightly/manual GitLab pipeline) with `GLOVEBOX_INTEGRATION=1` once
the `real-readonly` OAuth credentials are provisioned in Vault/ESO. Until then
the `integration:outlook` job is skipped (and logged), never silently green.
