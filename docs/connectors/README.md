# Connectors

Glovebox ships one container image per connector (`ghcr.io/leftathome/glovebox-<name>`)
and a Helm chart that can deploy any of them. Each connector polls a source,
applies routing rules, and stages content for scanning.

Every connector below has:
- a built image published to ghcr on `main`/tag (GitHub CI) and to the internal
  registry (GitLab CI);
- a test-derived sample config at `connectors/<name>/config.json`, shipped as the
  default `config:` in the chart's `connectors.<name>` values;
- a per-connector doc (linked below): auth setup, config fields, routing, and how
  to enable it in the chart.

## Enabling a connector

In your Helm values, set `connectors.<name>.enabled: true`. Provide credentials
one of two ways:
- **Pre-existing Secret**: set `connectors.<name>.secrets: <secret-name>` (injected
  via `envFrom`); or
- **ESO / Vault**: set `connectors.<name>.externalSecret.enabled: true` with a
  `secretStoreRef` and `data` map — the chart renders an ExternalSecret that
  materializes the Secret from Vault and injects it. See any connector doc for the
  exact env vars / keys.

Public connectors (`none` credential class) need neither.

## Source connectors

| connector | credential class | doc |
|-----------|------------------|-----|
| rss | none | [rss.md](rss.md) |
| hackernews | none | [hackernews.md](hackernews.md) |
| arxiv | none | [arxiv.md](arxiv.md) |
| semantic-scholar | test-account | [semantic-scholar.md](semantic-scholar.md) |
| gmail | real-readonly | [gmail.md](gmail.md) |
| gcalendar | real-readonly | [gcalendar.md](gcalendar.md) |
| gdrive | real-readonly | [gdrive.md](gdrive.md) |
| outlook | real-readonly | [outlook.md](outlook.md) |
| onedrive | real-readonly | [onedrive.md](onedrive.md) |
| teams | real-readonly | [teams.md](teams.md) |
| imap | real-readonly | [imap.md](imap.md) |
| schoology | real-readonly | [schoology.md](schoology.md) |
| github | test-account | [github.md](github.md) |
| gitlab | test-account | [gitlab.md](gitlab.md) |
| jira | test-account | [jira.md](jira.md) |
| trello | test-account | [trello.md](trello.md) |
| notion | test-account | [notion.md](notion.md) |
| bluesky | test-account | [bluesky.md](bluesky.md) |
| x | test-account | [x.md](x.md) |
| meta | test-account | [meta.md](meta.md) |
| linkedin | test-account | [linkedin.md](linkedin.md) |
| steam | test-account | [steam.md](steam.md) |
| youtube | test-account | [youtube.md](youtube.md) |

23 source connectors. Credential classes and secret shapes are defined
authoritatively in [integration-credentials.md](integration-credentials.md).

## Importers

File-driven, one-shot batch Jobs (not connectors): [importers.md](importers.md)
covers apple, mbox, and walhelm.

## Notes

- `schoology` is configured via the bespoke `schoologyConnector` /
  `schoologyAuthRefresher` values keys (dedicated templates), not the generic
  `connectors:` map — see [schoology.md](schoology.md).
- `schoology-auth-refresher` is an auth helper, not a source connector, so it has
  no sample config or integration test.
