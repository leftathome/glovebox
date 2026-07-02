# gdrive connector

Polls the Google Drive changes feed via the Drive API and stages new/changed
files for scanning. Reads the operator's real Drive read-only; credentials are
provisioned as a secret.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-gdrive` |
| Credential class | `real-readonly` (operator's real account, read-only) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs `real-readonly` creds provisioned) |

## Authentication

`real-readonly`: an OAuth2 flow against the operator's Google account (Drive
read-only). The connector reads:

- `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` — the OAuth2 client credentials
  (both required; the connector exits if either is unset).
- `token.json` at `$GLOVEBOX_STATE_DIR/token.json` — the persisted OAuth2 token
  (`access_token` / `refresh_token` / `expiry`); the connector refreshes the
  access token against `https://oauth2.googleapis.com/token` and rewrites the
  file atomically. `GLOVEBOX_STATE_DIR` is required.

These come from **Vault** via an ESO **`ExternalSecret`** in-cluster (the same
mechanism the running connector uses), surfaced as the env vars above plus the
mounted `token.json`. Exact Vault path / secret shape: **TBD (private
ci-templates)** per the registry — do not hardcode them here.

## Configuration

Sample: [`connectors/gdrive/config.json`](../../connectors/gdrive/config.json).
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
| _(none)_ | | | The Drive connector adds no fields beyond `connector.BaseConfig`; it polls the account-wide changes feed. |

```json
{
  "rules": [
    {
      "match": "drive:changes",
      "destination": "file-agent",
      "tags": {
        "source_type": "cloud_storage",
        "priority": "normal"
      }
    }
  ]
}
```

## Routing

Match keys emitted by this connector:

- `drive:changes` — a new/changed file from the Drive changes feed. If no rule
  matches this key the poll is skipped.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing. Rules may also carry
`tags` (e.g. `source_type` / `priority`).

## Enabling in the Helm chart

```yaml
connectors:
  gdrive:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-gdrive
      tag: latest
    config:
      rules:
        - match: "drive:changes"
          destination: "file-agent"
          tags:
            source_type: "cloud_storage"
            priority: "normal"
    # real-readonly: OAuth2 credentials from Vault via an ESO ExternalSecret.
    # Surfaced as env GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET and the OAuth
    # token file at $GLOVEBOX_STATE_DIR/token.json. Vault path / secret shape:
    # TBD (private ci-templates).
```

## Integration test

No live integration test yet. This is a follow-up: it runs live only
in-cluster (nightly/manual GitLab pipeline) with `GLOVEBOX_INTEGRATION=1` once
the `real-readonly` OAuth credentials are provisioned in Vault/ESO. Until then
the `integration:gdrive` job is skipped (and logged), never silently green.
