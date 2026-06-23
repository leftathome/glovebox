# Glovebox -> Memory Ingestion: Glovebox-side Prereqs

**Date:** 2026-06-12
**Owner:** Steven Wagner
**Tracking:** beads `glovebox-ptst`, `glovebox-qlkv` (both prereqs for
openclaw `openclaw-3wz`; openclaw spec
`docs/superpowers/specs/2026-06-01-glovebox-to-memory-ingestion-design.md`,
dep #5).

OpenClaw's glovebox->memory resolver assumes items arrive carrying an opaque
`data_subject = entity_id` (never a raw principal), and that glovebox
quarantines unregistered subjects itself rather than leaking them downstream.
These two beads put glovebox into that posture. All the *machinery* already
existed (spec 15 SP1 shipped the registry loader, resolver, and fail-closed
gate); this work is **populating real config + proving it with tests**.

## Decisions (confirmed with operator 2026-06-12)

- **Subjects:** Steve, Guardian (spouse), Child 1, Child 2.
- **entity_id style:** opaque ids + optional `display` label (spec 15 §5.1).
  Opaque ids are arbitrary-but-stable; `display` is operator legibility only
  and is never emitted into items/routing/audit.
- **default_audience:** Steve `[subject]`, Guardian `[subject]` (both private),
  Child 1 `[subject, guardians]`, Child 2 `[subject, guardians]` (spec 11 §9.1).
- **Stamping posture:** connectors stamp the **entity_id directly** in
  `data_subject` (the spec 15 §5.2 step-3 "already a registered entity_id"
  pass-through), so delivered items never carry a raw name/principal.
  `principals[]` is left empty for now; the walhelm health connector (SP2/SP3)
  will add `walhelm:<id>` principals when it lands.

| entity_id  | display | default_audience        |
|------------|---------|-------------------------|
| `e_111111` | Steve   | `["subject"]`           |
| `e_222222` | Guardian    | `["subject"]`           |
| `e_333333` | Child 1     | `["subject","guardians"]` |
| `e_444444` | Child 2 | `["subject","guardians"]` |

## glovebox-ptst — subjects.json + enforce

- [x] Populate `charts/glovebox/subjects.json` with `enforce: true` and the
      four subjects above.
- [x] Regression test on the **real** file: `internal/subject/realconfig_test.go`
      asserts enforce on, each entity_id pass-through-resolves to itself with
      its default_audience, and an unregistered principal is `OutcomeUnresolved`.
- [x] Routing test on the real file: an unregistered subject -> `GateQuarantine`
      with enforce on (`internal/routing/subject_gate_test.go`).

## glovebox-qlkv — per-connector data_subject/audience config

- [x] `charts/mbox-importer/values.yaml`: `dataSubjectDefault: e_111111`,
      `audienceDefault: [subject]` (Steve-private whole-archive attribution).
- [x] `connectors/gmail/config.json`: `data_subject_default: e_111111`,
      `audience_default: ["subject"]`.
- [x] `connectors/schoology/config.json`: per-kid rules stamp the entity_id in
      `data_subject` (k1->Child 1 `e_333333`, k2->Child 2 `e_444444`). Audience
      posture realized against the connector's *actual* match keys:
      `assignment -> [household]` (spec 11: assignments are household-wide),
      `feed`/`attachment` -> `[subject, guardians]` (a kid's personal activity /
      submitted work, i.e. spec's "grades/submitted" class), parent `message` /
      `parse-failure` -> `[guardians]` (unchanged; no specific kid).
- [x] Test: config defaults are stamped onto a committed item's metadata.json
      (`connector/staging_test.go`); and the real connector config.json files
      parse to the intended values (gmail + schoology test files).

## Notes / out of scope

- `connectors/schoology/config.json` `identity.tenant` left as `example-home`
  (placeholder). Tenant is provenance/identity, orthogonal to this bead's
  data_subject/audience scope; flagged for a follow-up if a real tenant label
  is wanted.
- `principals[]` intentionally empty (see Decisions). walhelm principals land
  with SP2/SP3.
- Schoology UIDs in config.json remain placeholders (`11111111`/`22222222`) —
  those are deployment-time values, not part of this bead.
