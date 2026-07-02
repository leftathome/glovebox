# schoology connector

Polls the Schoology LMS for each configured child's assignments, feed, and
messages (with attachments) and stages new items for scanning. This is the
reference **credentialed** connector: it reads the operator's real household
account read-only, driven from a saved browser session.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-schoology` |
| Credential class | `real-readonly` (household Schoology session) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | `connectors/schoology/live_integration_test.go` (`//go:build integration`, reference credentialed live test) |

## Authentication

Schoology has no first-class API key, so the connector authenticates with a
saved browser **session**. At startup `cmd/schoology/main.go` reads:

- `SCHOOLOGY_HOST` — the tenant subdomain (e.g. `yourschool.schoology.com`).
- `SCHOOLOGY_CREDENTIALS_FILE` — path to a JSON credentials file written by
  `schoology-go`'s `auth.SaveCredentials`, holding the session fields
  `SessID` / `CSRFToken` / `CSRFKey` / `UID`.

These are loaded via `schoologyauth.LoadCredentials` and passed to
`schoologylib.NewClient(host, WithSession(...))`. Any missing/invalid value is
a permanent startup error that exits non-zero and points at
`docs/AUTH-RECOVERY.md`.

The session JSON is **produced by the separate `schoology-auth-refresher`**
(an auth helper, not a source connector), which drives Browserless to log in
and refresh the cookie, writes the 5-field session to Vault, and lets ESO sync
it into the connector pod. The connector's own read path does **not** use
Browserless — it only consumes the materialized session. The secret is
provisioned in Vault and reaches the connector via an ESO `ExternalSecret`; no
secret values live in this repo. Vault path: TBD (private ci-templates).

Optional environment: `SCHOOLOGY_TIMEZONE` (scheduler timezone, default
`America/Los_Angeles`) and `SCHOOLOGY_TRIGGER_TOKEN` (bearer token for the
`POST /v1/poll` trigger endpoint). The live test additionally reads
`SCHOOLOGY_KID_UID`, a real child UID kept in env (not committed) since it is
account-specific PII.

## Configuration

Sample:
[`connectors/schoology/config.json`](../../connectors/schoology/config.json).
This is the effective config the integration test drives (through the
connector's normal `ApplyDefaults` + `ValidateConfig` path).

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
| `kids` | `[]{name,schoology_uid}` | yes | children to poll. `name` is an operator-chosen opaque label (`k1`, `k2`) — never a legal name, to keep PII out of metadata/audit logs — and is referenced by `schoology:<name>:*` rules. `schoology_uid` is the child's numeric Schoology UID (a `0` UID fails `ValidateConfig`). At least one kid required; names must be unique |
| `poll_schedule` | object | yes | `weekdays_only` (bool) + `windows` (`[]{start,end}`) daily polling windows. Times are `HH:MM` local-time strings in the connector timezone. At least one window required; each window's `end` must be strictly after its `start` |
| `trigger` | object | no | HTTP trigger endpoint. `debounce_seconds` (default 60) + `listen_port` (default 8081) |
| `attachments` | object | no | `max_size_mb` caps per-attachment download size (default 25) |
| `parse_failure_threshold` | int | no | consecutive parse failures tolerated before schema-drift escalation (default 10) |

```json
{
    "kids": [
        {"name": "k1", "schoology_uid": 11111111},
        {"name": "k2", "schoology_uid": 22222222}
    ],
    "poll_schedule": {
        "weekdays_only": true,
        "windows": [
            {"start": "07:00", "end": "09:00"},
            {"start": "15:30", "end": "17:30"}
        ]
    },
    "trigger": {
        "debounce_seconds": 60,
        "listen_port": 8081
    },
    "attachments": {
        "max_size_mb": 25
    },
    "parse_failure_threshold": 10,
    "rules": [
        {"match": "schoology:k1:assignment",        "data_subject": "e_333333", "audience": ["household"],            "destination": "school"},
        {"match": "schoology:k1:feed",              "data_subject": "e_333333", "audience": ["subject", "guardians"], "destination": "school"},
        {"match": "schoology:k1:attachment",        "data_subject": "e_333333", "audience": ["subject", "guardians"], "destination": "school"},
        {"match": "schoology:k2:assignment",        "data_subject": "e_444444", "audience": ["household"],            "destination": "school"},
        {"match": "schoology:k2:feed",              "data_subject": "e_444444", "audience": ["subject", "guardians"], "destination": "school"},
        {"match": "schoology:k2:attachment",        "data_subject": "e_444444", "audience": ["subject", "guardians"], "destination": "school"},
        {"match": "schoology:message",                                          "audience": ["guardians"],           "destination": "school"},
        {"match": "schoology:message-attachment",                               "audience": ["guardians"],           "destination": "school"},
        {"match": "schoology-parse-failure:*",                                  "audience": ["guardians"],           "destination": "school"}
    ],
    "identity": {
        "provider": "schoology",
        "auth_method": "session_cookie",
        "tenant": "example-home"
    }
}
```

## Routing

Match keys emitted by this connector:

- `schoology:<kid>:assignment` — an assignment for the named kid.
- `schoology:<kid>:feed` — a feed/course-update item for the named kid.
- `schoology:<kid>:attachment` — an attachment attached to the kid's content.
- `schoology:message` — an inbox message (account-level, not per kid).
- `schoology:message-attachment` — an attachment on an inbox message.
- `schoology-parse-failure:*` — a parse-failure receipt (schema-drift signal).

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing. Per-kid rules carry
`data_subject` so each child's school data routes under its own subject.

## Enabling in the Helm chart

Schoology is **not** wired through the generic `connectors:` map. It has
dedicated templates driven by two top-level values keys: `schoologyConnector:`
(the Deployment + Service + ConfigMap) and `schoologyAuthRefresher:` (the
session-refresh CronJob + ServiceAccount + ESO `ExternalSecret`s). In a real
install both are enabled, because the connector consumes the session Secret the
refresher produces.

```yaml
schoologyConnector:
  enabled: true
  image:
    repository: ghcr.io/leftathome/glovebox-schoology
    tag: ""            # defaults to .Chart.AppVersion when empty
  # host: ""           # empty => read SCHOOLOGY_HOST from the refresher Secret
  timezone: America/Los_Angeles
  config:
    rules:
      - { match: "schoology:k1:assignment", destination: school, data_subject: k1, audience: [operator] }
      - { match: "schoology:k1:feed",       destination: school, data_subject: k1, audience: [operator] }
      - { match: "schoology:message",       destination: school, data_subject: household, audience: [operator] }
      - { match: "*",                       destination: school, audience: [operator] }
    data_subject_default: household
    audience_default: [operator]
    kids:
      # REPLACE placeholder UIDs with each child's real Schoology UID before
      # enabling -- a 0 UID fails ValidateConfig at startup.
      - { name: k1, schoology_uid: 0 }
      - { name: k2, schoology_uid: 0 }
    poll_schedule:
      weekdays_only: true
      windows:
        - { start: "07:00", end: "19:00" }
    trigger: { debounce_seconds: 60, listen_port: 8081 }
    attachments: { max_size_mb: 25 }

# The session credentials this connector consumes are produced separately:
schoologyAuthRefresher:
  enabled: true
  schedule: "0 6 */12 * *"   # ~48h headroom before the 14-day cookie expiry
  image:
    repository: ghcr.io/leftathome/glovebox-schoology-auth-refresher
  vault:
    credentialsPath: "glovebox/schoology/default/credentials"  # {host,username,password}
    sessionPath: "glovebox/schoology/default/session"          # 5-field session JSON
  browserless:
    url: ""   # e.g. ws://browserless.browserless.svc.cluster.local:3000
```

The refresher drives Browserless, writes the session JSON to Vault, and ESO
syncs it into the connector pod as the `-schoology-session` Secret
(`SCHOOLOGY_CREDENTIALS_FILE` + `SCHOOLOGY_HOST`). Browserless belongs to the
refresher, not to the connector's read path.

## Integration test

Two tests cover this connector:

- `connectors/schoology/integration_test.go` — an offline, fake-driven
  integration test that exercises the production routing rules and stage
  round-trip against a fake Schoology client. Runs anywhere, no credentials.
- `connectors/schoology/live_integration_test.go` (`//go:build integration`) —
  the reference credentialed live test. It wires the real `schoology-go`
  client exactly as `cmd/schoology/main.go` does, from `SCHOOLOGY_HOST` /
  `SCHOOLOGY_CREDENTIALS_FILE` / `SCHOOLOGY_KID_UID`, and asserts a full stage
  round-trip. The session credentials only exist in-cluster (ESO), so it runs
  green only in the nightly/manual GitLab `integration` stage under
  `GLOVEBOX_INTEGRATION=1` and SKIPs cleanly everywhere else. Its CI job is
  `allow_failure: true` temporarily (a child's Schoology surfaces can be
  legitimately empty, and the session path is still being brought up); drop
  `allow_failure` once the ESO secret is wired and a min-content source is
  agreed.
