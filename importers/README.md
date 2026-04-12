# importers/

One-shot handlers for **finished archive files** (Google Takeout mbox, Slack
exports, WhatsApp chat dumps, etc.). Distinct from `connectors/`, which poll
**live remote sources** on a schedule.

See `docs/specs/09-mbox-importer-design.md` §2 for the rationale and the full
taxonomy of how the two families differ.

In short:

| Aspect            | `connectors/`                    | `importers/`                       |
|-------------------|----------------------------------|------------------------------------|
| Source            | Live remote service              | Finished local archive file        |
| Lifetime          | Long-running polling loop        | One-shot; run to completion, exit  |
| Scale pattern     | Steady trickle                   | Burst (whole archive at once)      |
| Checkpoint        | Source-specific cursor           | Byte offset in the archive file    |
| Filter timing     | Post-ingest                      | Pre-ingest (for scale)             |
| Trigger           | Scheduled poll / push            | CLI invocation or archive event    |

Both families share the framework library under `connector/` (staging backend,
rules engine, metrics, identity, checkpoints).

V1 members:

- `mbox/` -- Google Takeout / generic RFC 4155 mbox archives.
