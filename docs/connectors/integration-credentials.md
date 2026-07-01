# Connector Integration-Test Credential Registry

This is the single source that the in-cluster GitLab `integration` stage uses
to wire per-connector test credentials, and that sub-project C uses to know
which config fields to replace with secret placeholders / ESO references when
generating shipped sample configs.

See the design: `docs/superpowers/specs/2026-06-29-connector-integration-harness-design.md`
(sub-project B). Harness: `connector/integrationtest`.

## Credential model (per spec §5)

Each **source connector** gets one of three credential sources:

- **`test-account`** — a dedicated test account/app/API key for that source
  (preferred; safe to read against, no personal data).
- **`real-readonly`** — the operator's real account, used read-only, where a
  separate test account isn't practical.
- **`none`** — public source, no credentials needed.

All non-`none` credentials live in **Vault** and reach the in-cluster CI job
via an **ESO `ExternalSecret`**, surfaced as env vars or a mounted file at the
connector's expected secret path — the same mechanism the running connectors
use. **No secret values live in this repo or in CI variables in cleartext.**

**Infra-sensitive specifics** (exact ESO object names, cluster Vault mounts,
node selectors) live in the **private `homelab/ci-templates`**, not here. The
`vault path` / `secret shape` columns below name the *logical* secret; the
private template binds it. Cells marked `TBD` are filled as the secret is
provisioned.

`image smoke` defaults to `no` (spec §4.3 — image smoke is added per connector
later only where artifact/runtime verification is justified).

## Exclusions

- **`schoology-auth-refresher`** — an auth *helper* (refreshes the schoology
  session via Browserless), **not** a source connector. No integration test.
- **Importers** (`apple`, `mbox`, `walhelm`) — file-driven (read a local
  archive; no live credentials). They can reuse the `connector/integrationtest`
  stage-and-readback harness against a committed fixture archive; tracked
  separately (lower priority), not in this credential registry.

## Registry (23 source connectors)

| connector | cred source | vault path | secret shape | image smoke |
|-----------|-------------|------------|--------------|-------------|
| rss | none | n/a | n/a | no |
| hackernews | none | n/a | n/a | no |
| arxiv | none | n/a | n/a | no |
| gmail | real-readonly | TBD (private ci-templates) | TBD (OAuth token/creds file) | no |
| imap | real-readonly | TBD (private ci-templates) | TBD (host + user + app password) | no |
| outlook | real-readonly | TBD (private ci-templates) | TBD (OAuth token/creds file) | no |
| gcalendar | real-readonly | TBD (private ci-templates) | TBD (OAuth token/creds file) | no |
| gdrive | real-readonly | TBD (private ci-templates) | TBD (OAuth token/creds file) | no |
| onedrive | real-readonly | TBD (private ci-templates) | TBD (OAuth token/creds file) | no |
| teams | real-readonly | TBD (private ci-templates) | TBD (OAuth token/creds file) | no |
| schoology | real-readonly | TBD (private ci-templates) | env `SCHOOLOGY_HOST` + `SCHOOLOGY_CREDENTIALS_FILE` (session JSON: SessID/CSRFToken/CSRFKey/UID) + `SCHOOLOGY_KID_UID` | no |
| github | test-account | TBD (private ci-templates) | TBD (PAT) | no |
| gitlab | test-account | TBD (private ci-templates) | TBD (PAT) | no |
| jira | test-account | TBD (private ci-templates) | TBD (API token + email) | no |
| trello | test-account | TBD (private ci-templates) | TBD (key + token) | no |
| bluesky | test-account | TBD (private ci-templates) | TBD (handle + app password) | no |
| x | test-account | TBD (private ci-templates) | TBD (API/bearer token) | no |
| meta | test-account | TBD (private ci-templates) | TBD (OAuth app token) | no |
| linkedin | test-account | TBD (private ci-templates) | TBD (OAuth token) | no |
| notion | test-account | TBD (private ci-templates) | TBD (integration token, test workspace) | no |
| steam | test-account | TBD (private ci-templates) | TBD (Web API key) | no |
| youtube | test-account | TBD (private ci-templates) | TBD (Data API key) | no |
| semantic-scholar | test-account | TBD (private ci-templates) | free SEMANTIC_SCHOLAR_API_KEY | no |

Tally: 3 `none` + 8 `real-readonly` + 12 `test-account` = 23 source connectors.

## Onboarding order

Per the spec's incremental rollout: the `none` connectors
(rss, hackernews, arxiv) onboard first (no secret provisioning needed),
validating the harness + the `integration` stage end-to-end. Credentialed
connectors follow as their Vault/ESO secret is provisioned; fill this table's
`vault path` / `secret shape` cells at that time. A connector whose
credentials are not yet provisioned is skipped (and logged) by the integration
job, never silently green.

Note: `schoology` is the reference **credentialed** connector (lyku.4). Its
live test (`connectors/schoology/live_integration_test.go`) wires the real
`schoology-go` client exactly as `cmd/schoology/main.go` does, from the env in
the table above; the session credentials only exist in-cluster (ESO), so the
test SKIPs clean everywhere else. The `integration:schoology` CI job is
`allow_failure: true` **temporarily**: a child's Schoology surfaces can be
legitimately empty at any moment (so a real run may assert-fail on `staged>=1`
until a min-content window/fixture is chosen), and the session path is still
being brought up. Drop `allow_failure` once the ESO secret is wired and a
reliable min-content source is agreed. The connector's read path does **not**
use Browserless -- that belongs to the separate `schoology-auth-refresher`,
which produces the session JSON this test consumes.

Note: `semantic-scholar` was originally scoped as `none`, but its keyless
public tier returns HTTP 429 immediately and the connector swallows the
fetch error (so a keyless run fails rather than skips); it is therefore
`test-account` with the free `SEMANTIC_SCHOLAR_API_KEY`, and its live test
skips cleanly until that key is provisioned.
