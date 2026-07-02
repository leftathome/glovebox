# teams connector

Polls Microsoft Teams channels via the Microsoft Graph API and stages new
channel messages for scanning. Reads the operator's real Teams read-only;
credentials are provisioned as a secret.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-teams` |
| Credential class | `real-readonly` (operator's real account, read-only) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet — follow-up (needs `real-readonly` creds provisioned) |

## Authentication

`real-readonly`: an OAuth2 flow against the operator's Microsoft account,
granted the read-only `https://graph.microsoft.com/ChannelMessage.Read.All`
scope. The connector reads:

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

Sample: [`connectors/teams/config.json`](../../connectors/teams/config.json).
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
| `channels` | `[]{team_id,channel_id,name}` | no | Teams channels to poll; `name` is the display name referenced by `channel:<name>` rules, `team_id` / `channel_id` are the Graph identifiers |

```json
{
  "channels": [],
  "rules": [
    {"match": "channel:*", "destination": "messaging"},
    {"match": "*", "destination": "general"}
  ]
}
```

## Routing

Match keys emitted by this connector:

- `channel:<name>` — a message from the named channel (the channel's `name`
  display value; matched by patterns like `channel:*`).
- `*` — fallback for any message.

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing.

## Enabling in the Helm chart

```yaml
connectors:
  teams:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-teams
      tag: latest
    config:
      channels:
        - { team_id: "<team-guid>", channel_id: "<channel-guid>", name: "general" }
      rules:
        - { match: "channel:*", destination: "messaging" }
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
the `integration:teams` job is skipped (and logged), never silently green.
