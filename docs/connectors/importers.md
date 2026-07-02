# importers (apple, mbox, walhelm)

Importers are file-driven: they read a local archive (an Apple/iCloud export, an
mbox mail file, or a walhelm archive directory) and ingest its contents once,
then exit. Unlike connectors — long-running Deployments that poll a live source
— importers are one-shot batch **Jobs**.

| | |
|---|---|
| Images | `ghcr.io/leftathome/glovebox-apple-importer`, `ghcr.io/leftathome/glovebox-mbox-importer`, `ghcr.io/leftathome/glovebox-walhelm-importer` |
| Credential class | `none` — file-driven, no live upstream credentials |
| Enricher runtime | yes (all three build `FROM ${ENRICHER_BASE}`) |

## Invocation contract

All three importers share the same CLI (`ENTRYPOINT ["/importer"]`):

| flag / env | meaning |
|------------|---------|
| `--source` | path to the input: an archive **directory** (apple, walhelm) or the `.mbox` **file** (mbox). Required. |
| `--source-name` | value stamped on ingested items' `source` metadata (default: the importer name) |
| `--config` / `$GLOVEBOX_IMPORTER_CONFIG` | config.json path (default `/etc/importer/config.json`) |
| `--state-dir` / `$GLOVEBOX_STATE_DIR` | checkpoint dir (enables resume across runs) |
| `--ingest-url` / `$GLOVEBOX_INGEST_URL` | glovebox ingest endpoint; if unset, `--staging-dir` / `$GLOVEBOX_STAGING_DIR` is used instead |
| `--filter` | optional filter JSON |
| `--concurrency` | parallel ingest workers (default 8) |
| `--survey-only`, `--regenerate-survey`, `--resume`, `--fixed-tags` | see `--help` |

## Enabling in the Helm chart

Importers live under the `importers:` map (separate from `connectors:`), each
rendered as a Job + a config ConfigMap. Enable one, point `input.existingClaim`
at a PVC that holds the archive, and set `source` to the path inside the
container (the PVC is mounted at `/data/import`):

```yaml
importers:
  mbox:
    enabled: true
    image:
      repository: ghcr.io/leftathome/glovebox-mbox-importer
      tag: latest
    source: /data/import/archive.mbox   # mbox: the .mbox FILE
    sourceName: mbox
    config:
      rules:
        - { match: "*", destination: "inbox" }
    input:
      existingClaim: my-mbox-pvc         # REQUIRED: PVC holding the archive
      readOnly: true
    # state.existingClaim: ""            # optional; emptyDir when unset
    # args: ["--concurrency", "4"]       # extra flags
```

For `apple` and `walhelm`, `source` is a **directory** (e.g. `/data/import`),
not a file. The chart fails template rendering if an importer is enabled without
`input.existingClaim`, since there would be nothing to import.

The Job wires `GLOVEBOX_INGEST_URL` to the in-cluster ingest Service by default
(HTTP ingest mode), so imported items flow through the same ingest path as
connector output. Re-run an import by re-applying the release (or deleting and
recreating the Job).

## Note

Importers are not part of the connector integration-test credential registry
(they are file-driven, no live creds). They can reuse the
`connector/integrationtest` stage-and-readback harness against a committed
fixture archive; that coverage is tracked separately.
