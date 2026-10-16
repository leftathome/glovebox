# Design spec: record journal (stateful sources as keyed records and change events)

- **Status:** reviewed (three independent passes, 2026-10-16); awaiting owner review
- **Bead:** glovebox-wekc.18 (epic glovebox-wekc)
- **Repos:** glovebox (Parts A and B), openclaw (Part C, the first consumer)
- **Related:** spec 05 (connector framework), spec 08 (ingest API), spec 11
  (data_subject and audience), spec 12 (Schoology), spec 16 (PowerSchool),
  openclaw `docs/superpowers/specs/2026-10-09-school-patrol-design.md` section 15
  (the state projection this replaces)

## 1. Purpose

Glovebox treats everything as an immutable **item**: a connector stages bytes
plus metadata, the core scans and routes it, and a consumer receives a
directory. That fits event-like sources (mail, feeds, posts). It does not fit
**stateful** sources -- a gradebook, an LMS assignment list, a calendar, an
issue tracker -- whose records change in place.

For those sources today:

- An item id is random per commit. Nothing in the framework knows that two
  items are versions of the same record, or that one replaced another.
- Each connector invents its own change detection in a flat checkpoint map.
  The PowerSchool connector fingerprints records but never detects removal;
  the Schoology connector emits an assignment once and never again, so status
  changes are lost.
- Connectors hold typed values (a score as a number, an assignment id) and
  render them to text. Consumers then recover them heuristically from body
  lines. The openclaw school patrol folds items into a per-child "state file"
  with keys partly parsed out of body text, and its rows carry no stable id.
- There is no record of what changed, or when. "A new unexcused absence" and
  "a scored worksheet" arrive as indistinguishable items.

This spec adds a **record journal**: connectors describe stateful sources as
keyed records with typed fields; the core diffs each observation against
last-known state, appends **change events**, keeps **current state** as a fold
of those events, and serves both through an audience-filtered read API.

## 2. Decisions taken during brainstorming

| # | Decision | Chosen | Rejected |
|---|---|---|---|
| D1 | First delivery | School end to end (PowerSchool, Schoology, openclaw patrol, dashboard) on a **general** contract; two or three other connectors checked on paper only | General-from-day-one with a non-school migration; school-only implementation |
| D2 | Important changes | Typed, prioritised events; configured event types are **pushed** to the people they concern; the record's subject's own agent is prompted with a purpose | Record-only; push to everyone entitled |
| D3 | History | **Keep everything** (multi-year), with an explicit per-subject **erase**; seed from items already collected, marked approximate | One school year; rolling 90 days |
| D4 | Architecture | A **central journal in the glovebox core service** on the existing **CloudNativePG** Postgres | Journal inside the connector framework; snapshots only with consumers diffing |
| D5 | Messaging | **No broker.** Postgres is the log; readers keep a cursor | Kafka or similar |
| D6 | Household notes | Correctable; every version kept | Locked once written |
| D7 | Outage handling | **Per-connector spool** of snapshot files, replayed in order | No buffering; core-side buffering |
| D8 | Audience vocabulary and reader grants | **Opaque tokens**; no household roles in journal code. Readers are described by **grants**. The caller names only the **reader**; glovebox **computes the grants** from the **same household roster ConfigMap openclaw reads**, which neither application can modify and only a gitops deployment can change (4.9). The consumer re-checks every row before writing (defence in depth). | Caller-supplied grants (first choice, revised 2026-10-16: a caller bug that mixed up subjects would go undetected); a separate glovebox-owned directory (drift) |
| D9 | Service auth | Kubernetes ServiceAccount tokens checked by TokenReview (the login broker's pattern) | mTLS for the new endpoints |

### Prior art (open-source spike, 2026-10-15)

No project covers the whole job. Two models are borrowed:

- **dlt `scd2` / partition merge keys** -- removal is inferred only for the
  partitions (here: scopes) present and complete in a load.
- **simonw/git-history** -- `item` (current), `item_version` (only changed
  columns), a content hash to skip unchanged rows.

Rejected as runtimes: dlt and Airbyte (Python platforms, whole-row versions, no
audience model), event-sourcing libraries (log without diffing), pgMemento and
temporal-table extensions (capture database writes, cannot infer removal from
snapshots), XTDB, Dolt, TerminusDB (a second database), Debezium (reads database
logs, not snapshots). XTDB's split between when something was true and when it
was observed informs section 4.6.

## 3. Concepts

| Term | Meaning |
|---|---|
| **Record type** | A named kind of record (`assignment`, `attendance_mark`) with a schema version and typed field definitions. Defined in configuration, owned by the connector that writes it (or by `household` for notes). |
| **Record** | One instance of a type, identified by `(record_type, data_subject, key)`. `key` is a string the source guarantees stable **within one data subject**; two subjects may use the same key (two siblings in the same course). |
| **Fields** | A flat map of typed values: `string`, `text` (untrusted free text, scanned), `int`, `number`, `bool`, `date`, `timestamp`, `string_list`. Every field is nullable. |
| **Scope** | The unit a connector observes in one go, e.g. `(source=powerschool, surface=assignments, data_subject=X, course=Y)`. Written as an ordered list of `name=value` parts. |
| **Snapshot** | One observation of one scope at one time: the records seen, plus `complete` -- whether absence from this snapshot means the record no longer exists. |
| **Event** | One append-only change to one record: `created`, `updated`, `removed`, `restored`. |
| **Current state** | The latest version of every record, including removed ones (tombstoned). Always equal to a fold of the events. |
| **data_subject** | As in spec 11: the entity a record is about. Opaque string; `""` for records about no one in particular. Part of every record's identity. |
| **Audience** | As in spec 11: a list of tokens naming who may see the record, relative to its `data_subject`. Opaque to the journal. |
| **Reader** | A member of the household roster on whose behalf a read or household write is made. |
| **Grant** | A pair `(subject, token)` held by a reader, **computed by the journal from the roster** (4.9b), never supplied by a caller. `subject` is a data subject or `""` (records with no `data_subject`). |

## 4. Part A -- the journal core (glovebox)

### 4.1 Components

```
connector process                         glovebox core service                   readers
-----------------                         ---------------------                   -------
records lib  -- builds snapshot           /v1/records  (write, SA token)
spool dir    -- one file per snapshot ->  journal.Apply (one transaction)  ---->  Postgres (CNPG)
                                          /v1/notes    (write, SA token)           record_type, snapshot,
                                          /v1/state, /v1/events,                   record_current, record_event,
                                          /v1/counts   (read, SA token + grants)   erasure
                                                                                 <- openclaw patrol, notes tool
                                                                                 <- dashboard
```

- **`connector/records`** (new framework package): builds snapshots, writes
  them to the spool, sends pending snapshots in order, handles acknowledgements.
  No diffing.
- **`internal/journal`** (new core package): schema migrations, `Apply`
  (section 4.4), reads, erase, rescan, hold/confirm. Pure Go over `database/sql`
  with `pgx`; no ORM.
- **HTTP surface**: a separate listener on the core service (default port
  9095) so a NetworkPolicy can scope it independently of item ingest.

### 4.2 Storage

A dedicated database (`glovebox_journal`) on the existing CloudNativePG
cluster, created and credentialed through gitops (Vault + ESO, policies and
roles in gitops). Migrations are plain numbered SQL files embedded in the
binary and applied at start under an advisory lock; `schema_migrations`
records what ran. The schema is documented in `docs/journal-schema.md`, which
every migration updates.

Tables (types abbreviated):

**`record_type`** -- one row per (name, schema_version)
- `name text`, `schema_version int`, `owner text` (connector or `household`)
- `fields jsonb` -- `{name: {type, required}}`
- `renames jsonb` -- `{old_name: new_name}` applied before diffing (4.7)
- `default_audience text[]`, `created_at`

**`snapshot`** -- one row for **every** snapshot received, whatever its outcome
- `id uuid` (assigned by the connector; idempotency key), `record_type`,
  `schema_version`
- `scope text` (canonical form), `data_subject text` (the scope's subject
  part, `""` if none), `complete bool`, `approximate bool`
- `observed_at timestamptz`, `received_at timestamptz`, `writer text`
  (authenticated service)
- `status` -- `applied | duplicate | late | held | discarded`
- `payload jsonb` -- the snapshot as received. Kept for every status: it is
  the raw observation history, it lets a held snapshot be applied later, and
  it lets a late snapshot be inspected. Volume is small (section 1).
- `stats jsonb` -- counts created/updated/removed/restored/unchanged

Rejected snapshots (`4xx`) are not stored.

**`record_current`** -- primary key `(record_type, data_subject, key)`
- *event-derived columns:* `scope text`, `audience text[]`, `fields jsonb`,
  `removed bool`
- *observation columns (not event-derived):* `first_seen_at`, `last_seen_at`,
  `last_confirmed_snapshot uuid`, `content_hash bytea`, `last_event_seq bigint`
- `scan jsonb` -- per `text` field of the current value:
  `{verdict, scanner_version}`

**`record_event`** -- append-only, primary key `seq bigserial`
- `record_type`, `data_subject`, `key`, `kind`, `schema_version`
- `scope text`, `prev_scope text` (set when a record moves scope)
- `audience text[]`, `prev_audience text[]` (4.8)
- `fields jsonb` -- full fields for `created` and `restored`; null otherwise
- `changed jsonb` -- `{field: [old, new]}` for `updated`; the last known fields
  for `removed`
- `scan jsonb` -- a verdict for **every `text` value stored in this row**, old
  and new: `{"<field>": {"old": verdict, "new": verdict}}` (4.11)
- `observed_at`, `prev_observed_at` (4.6), `approximate bool`
- `snapshot_id uuid` (null for household writes), `actor text` (writer service,
  and for notes the person-agent on whose behalf it wrote)
- `importance text`, `rule text`, `group_key text` (section 4.10)

**`erasure`** -- `data_subject`, `erased_at`, `actor`, `rows jsonb` (counts).
Never erased itself; holds no field data.

Indexes: `record_current (data_subject)`, `record_current (scope)`, GIN on
`audience` in both record tables, `record_event (data_subject, seq)`,
`record_event (record_type, data_subject, key, seq)`,
`snapshot (scope, observed_at)`, `snapshot (data_subject)`.

**The fold invariant.** For every record, folding its events in `seq` order --
applying each event's `schema_version` renames (4.7) -- reproduces exactly the
event-derived columns of `record_current`: `scope`, `audience`, `fields` and
`removed`. Observation columns are bookkeeping about when the record was last
confirmed and are deliberately not events. The property tests in section 8
assert this invariant and nothing broader.

### 4.3 Wire format: `POST /v1/records`

```json
{
  "snapshot_id": "0b9f...-uuid",
  "record_type": "assignment",
  "schema_version": 1,
  "scope": [["source","powerschool"],["surface","assignments"],["data_subject","e_..."],["course","1234"]],
  "complete": true,
  "complete_range": null,
  "observed_at": "2026-10-16T13:00:41Z",
  "approximate": false,
  "records": [
    {
      "key": "1234-5:98765",
      "data_subject": "e_...",
      "audience": ["subject", "guardians"],
      "fields": {"title": "...", "due": "2026-10-20", "score": 8.5, "max_score": 10, "missing": false}
    }
  ]
}
```

- Body limit 8 MiB; at most 5000 records per snapshot; field names
  `[a-z][a-z0-9_]{0,63}`; `string` values at most 1024 bytes, `text` at most
  64 KiB. A snapshot that exceeds a limit is rejected whole (`413`/`422`), never
  truncated.
- Every record's `data_subject` must equal the scope's `data_subject` part when
  the scope has one. `(data_subject, key)` must be unique within a snapshot.
- `complete_range` (optional, only with `complete=true`). A connector sends it
  only when the page showed at least one date: a page that parsed with no
  dates at all gives no meaningful range, and is sent as `complete=false`. A
  page that showed dates but no records (every code corrected away) is a
  legitimate empty snapshot with a range; the removal guard (4.5) applies.
  Shape:
  `{"field": "<a date field of the type>", "from": "YYYY-MM-DD", "to": "YYYY-MM-DD"}`.
  It limits completeness to records whose `field` lies in `[from, to]`: only
  those can be removed by this snapshot. Use it when a source page covers a
  span of dates (an attendance grid for the current term) rather than
  everything in the scope.
- The scope's `source` part must equal the record type's `owner`, so one
  connector cannot write another source's records even when they share a
  ServiceAccount.
- Responses (the connector's spool acts on these exactly, 4.14):

| Code | Body `status` / `reason` | Meaning | Spool action |
|---|---|---|---|
| `200` | `applied`, `duplicate`, `late`, `held` | accepted (4.4) | delete the file |
| `410` | `erased_subject` | a subject in the snapshot was erased after `observed_at` | delete the file |
| `403` | `policy` | the service may not write this type or scope | move to `spool/rejected/` |
| `409` | `unknown_subject` | a `data_subject` is not in the current roster (4.9b) -- possibly a roster change in progress | keep, retry with backoff (bounded by the spool limits) |
| `413` | `too_large` | over a limit | move to `spool/rejected/` |
| `422` | `schema`, `future_observed_at`, `duplicate_key` | invalid snapshot | move to `spool/rejected/` |
| `5xx`, network error | -- | try again later | keep, retry with backoff |

### 4.4 Applying a snapshot

In one transaction, under a per-scope advisory lock:

1. **Idempotency.** If `snapshot_id` exists, return its stored status as
   `duplicate` (or `held` if it is still held) with the stored stats.
2. **Validation.** Schema, limits and policy; failures return `4xx` and
   store nothing.
3. **Erasure guard.** If any record's `data_subject` has an `erasure` row with
   `erased_at > observed_at`, return `410 erased_subject` and store nothing.
4. **Ordering.** If a snapshot for the same scope with a later `observed_at`
   is already applied, store this one as `late` (payload kept, no diff, no
   events). Current state never moves backwards.
5. **Renames.** Map declared old field names to new ones (4.7).
6. **Scan.** Run every `text` value through the existing scanner in-process;
   keep the verdicts for step 9. Scanning never blocks the write (4.11).
7. **Diff.** Load `record_current` rows for the scope's subject and type. For
   each incoming record, looked up by `(record_type, data_subject, key)`:
   - not present -> `created`
   - present and `removed` -> `restored` (full fields)
   - present in a **different scope** -> `updated` with `prev_scope` set (plus
     any changed fields); the record now belongs to the new scope
   - present, same `content_hash`, same audience, same scope -> observation
     columns only (`last_seen_at`, `last_confirmed_snapshot`); no event
   - otherwise compare field by field -> `updated` with only the changed fields
8. **Removal.** If `complete`, every live record whose stored `scope` equals
   this snapshot's scope, whose key the snapshot does not contain, and -- when
   `complete_range` is set -- whose range field lies within it, is a candidate
   `removed`. The removal guard (4.5) decides. A snapshot with
   `complete=false` never removes anything.
9. **Write.** Insert events (with scan verdicts, `importance`, `rule`,
   `group_key`, section 4.10), upsert `record_current`, insert the `snapshot`
   row as `applied`.

### 4.5 Removal guard

If a complete snapshot would remove more than `removal_guard.max_fraction`
(default 0.3) of the scope's live keys, or every key of a scope that had more
than `removal_guard.min_keys` (default 3), the snapshot is **held**. When the
snapshot has a `complete_range`, both tests count only the live keys whose
range field lies inside that range -- the keys the snapshot could remove --
not the whole scope. A held snapshot: nothing is
applied, the `snapshot` row is stored with status `held` (payload kept), a
metric and an alert fire, and the connector is told `held` (it deletes its
spool file; a resend gets `held` again by step 1). An operator confirms or
discards it through an admin endpoint. Rules:

- Later snapshots for the same scope are applied normally if they do not trip
  the guard. When one is applied, every older held snapshot of that scope
  becomes `discarded` automatically (it would now be `late`).
- If several snapshots of one scope are held, confirming one applies it and
  discards every older held snapshot of that scope. Confirming re-runs 4.4
  from step 4 against current state.

### 4.6 Time

- `observed_at` -- when the connector read the source (connector clock;
  rejected if more than 10 minutes in the future of the core's clock).
- `prev_observed_at` on an event -- the `observed_at` of the last snapshot in
  which the record was seen with its old value. The change happened somewhere
  in `(prev_observed_at, observed_at]`; readers show that window rather than a
  false precise time.
- `approximate` -- set on everything derived from seeding (section 7); readers
  label it.

### 4.7 Schema evolution

A record type's schema version is bumped by adding a new `record_type` row.
`renames` (`{old_name: new_name}`) belong to the new version. When the new
version is activated, a migration step rewrites `record_current.fields` of
that type under the new names, in one transaction and without events; after
that, incoming snapshots of the old version are renamed before diffing (4.4
step 5), so a rename never appears as a remove-plus-add. A type change of an
existing field requires a new field name. Old events are never rewritten:
each carries its `schema_version`, and the fold (and any reader) maps old
names forward through the chain of renames.

### 4.8 Audience on a record that changes audience

Audience is stamped per version. When an incoming version's audience differs
from the stored one:

- the `updated` event carries `audience` (new) and `prev_audience` (old);
- on read, old values in `changed` are returned only to a reader who matches
  **both** audiences; a reader matching only the new audience sees the new
  values and `"old": "withheld: different audience"`;
- a reader matching only the old audience sees nothing from this event onward.

So a reader never learns a value through a version they were not entitled to.

### 4.9 Authentication, policy and grants

**Service authentication.** Every journal endpoint requires a Kubernetes
ServiceAccount token with audience `glovebox-journal`, verified by TokenReview,
checked against the token's audience, matched against an exact
`namespace/name` allow-list, and failing closed when TokenReview is unavailable.
A NetworkPolicy limits who can reach the listener. (Same mechanism as the login
broker.)

**Per-service policy** (chart values, reviewed in gitops):

```yaml
journal:
  roster:                                  # 4.9b
    configMap: openclaw-household
    key: household.json
    format: openclaw-household-v1
  services:
    openclaw/openclaw-gateway:
      read: true
      readers: "*"                         # may read as any member of the roster
      write_types: [attendance_note]       # household writes, on behalf of a member
      counts: true
    openclaw/glovebox-connector-powerschool:   # one ServiceAccount per connector (4.9c)
      write_types: [course_mark, assignment, attendance_mark, attendance_day,
                    attendance_total, term_grade, term_comment]
      write_scopes: {source: [powerschool]}
      write_subjects: roster_subjects      # roster members, plus "" for types that declare it (no school type does)
    openclaw/glovebox-connector-schoology:
      write_types: [lms_assignment, lms_assignment_detail]
      write_scopes: {source: [schoology]}
      write_subjects: roster_subjects
    trunchbull/trunchbull:
      read: true
      readers: [e_parent1, e_parent2]      # the dashboard reads only as guardians
      counts: true
```

Every other workload -- the RSS connector, the core's own pods, the login
jobs -- has no journal write rights at all.

**Readers and grants.** Every read and every household write names **one
reader** (a roster member id) and nothing else about access:

```json
{"reader": "e_parent1"}
```

- The journal **computes** the reader's grants from the roster (4.9b). The
  caller cannot add, remove or alter grants; a request carrying any access
  field other than `reader` is rejected.
- A reader not in the roster, or not in the service's `readers`, is rejected
  (`403`).
- **Visibility rule:** a row is visible if and only if one of the reader's
  computed grants `(s, t)` has `s` equal to the row's `data_subject` and `t` in
  the row's `audience`. `public` in a row's audience is visible to any
  authenticated reader. The same rule filters `/v1/state`, `/v1/events` and
  `/v1/counts`.
- The journal still never interprets tokens beyond this matching: the mapping
  from roster to grants lives in one adapter (4.9b), not in the journal's
  visibility or apply code.

**What this protects against.** A caller that names the wrong reader gets
that reader's view, never more. A caller cannot claim access it was not
given: with caller-supplied grants (the first design), a consumer bug that
attached one child's grants to another child's request would have been
served without complaint. Naming the wrong reader is still possible inside
the calling service; that is what the consumer's own pre-write check (Part C)
and the isolation tests (section 8) are for.

#### 4.9b The household roster

- Glovebox mounts the roster ConfigMap **read-only** as a volume (not
  `subPath`, so changes from a gitops deployment reach the running process)
  and reloads it when the file changes. A reload is atomic: a roster that fails
  to parse is ignored and the previous one kept, with an error metric and an
  alert.
- An adapter selected by `format` turns the roster into grants per member.
  `openclaw-household-v1` is the file openclaw reads today:
  `{"people": {"<id>": {"agent": ..., "guardian_of": [ids], "siblings": [ids]}},
  "household_members": [ids]}`. Mapping for a member `m`:
  - `(m, subject)`;
  - `(c, guardians)` for every `c` in `m.guardian_of`, and `("", guardians)`
    if `m.guardian_of` is non-empty (spec 11 section 3.4: standalone
    `guardians` means the household's guardians);
  - `(s, siblings)` for every `s` in `m.siblings` (`m` is a sibling of `s`);
  - if `m` is in `household_members`: `(x, household)` for every `x` in
    `household_members`, and `("", household)`.
  No other grants exist; `caregivers` and `""`-subject `subject`/`siblings`
  grants are never produced. The adapter is the only code that knows these
  relationships; the mapping is pinned by tests against a fixture of this
  format, and the openclaw format is documented where openclaw defines it.
- **Roster changes.** A subject removed from the roster keeps its rows; with
  no grants for it they are unreadable until it returns or an operator erases
  it (4.13). Snapshots naming a subject not in the roster get `409` and stay
  in the spool (a removal in progress, or a parse window during reload, loses
  nothing). A household note written by a member later removed stays visible
  to its fixed audience; its `actor` still names the writer.
- **Reload timing.** The kubelet delivers a changed ConfigMap to each pod
  within about two minutes, so glovebox and openclaw can briefly hold
  different rosters. The consumer's pre-write check fails safe in that window
  (abort and retry next run). Hash mismatches shorter than ten minutes are
  expected and do not alert.
- **Immutability is a deployment precondition, not an assumption.** The
  journal may be enabled only when all of the following hold, and the rollout
  (section 9 step 1) verifies each:
  1. No ServiceAccount in the namespace can create, update, patch or delete
     **the roster ConfigMap** (`kubectl auth can-i ... configmap/openclaw-household`,
     verified 2026-10-16 for every ServiceAccount in the namespace). The chart's
     PowerSchool connector Role grants `update`/`patch` only on the
     login-request ConfigMap by `resourceNames`; any future Role granting
     ConfigMap writes must do the same.
  2. Both applications mount the roster read-only (the openclaw gateway does
     today, via a `readOnly` mount; ConfigMap volumes are read-only to the pod
     in any case).
  3. The ConfigMap is applied by Flux from gitops (verified: managed by the
     `apps` Kustomization, `prune: true`, 10-minute drift correction).
  4. **New:** a ValidatingAdmissionPolicy (Kubernetes 1.34 on this cluster)
     denies `CREATE`, `UPDATE` and `DELETE` of that ConfigMap to every
     requester except Flux's `flux-system/kustomize-controller`
     ServiceAccount. Without it a cluster administrator could edit the roster
     outside gitops until Flux reverts it; with it, the only path to a change
     is a gitops commit. Break-glass is removing the policy through gitops.
- The journal exposes the roster's content hash on `/v1/healthz` and as a
  metric; openclaw logs the hash of the roster it read. Equal hashes show both
  applications are using the same roster.

#### 4.9c One identity per connector (precondition)

Today the PowerSchool, Schoology and RSS connectors all run as the
`glovebox-glovebox` ServiceAccount, together with the core service. The RSS
connector processes untrusted content from the open internet; under a shared
identity, a compromise of it could write grades or attendance for any
child, and could trigger pushed alerts to guardians. The journal therefore
requires, before any connector is given write rights:

- **One ServiceAccount per connector Deployment**, named
  `<release>-connector-<name>` and rendered by the chart for every enabled
  connector, plus a separate one for the core. No connector uses the core's
  account or another connector's.
- `automountServiceAccountToken: false` on every connector pod; a connector
  with journal rights gets only a **projected token** with audience
  `glovebox-journal` and a short lifetime. A connector without journal rights
  gets no token at all.
- The journal's NetworkPolicy admits writes to its listener only from pods of
  connectors that hold journal write rights.
- The record-type `owner` versus scope `source` check (4.3) stays as a second
  line: even a correctly identified connector cannot write another source's
  records.

The chart change is independent of the journal and ships first (rollout step
0, bead to be filed); it is verified with `kubectl auth can-i` and by
inspecting each pod's `serviceAccountName`.

#### 4.9a Household writes

- The writer does **not** choose the audience. Each household record type
  declares a fixed `audience` (for `attendance_note`: `[subject, guardians]`)
  and the journal stamps it.
- A write names `on_behalf_of` (a roster member) as its reader. It is allowed
  only if that member's computed grants include `(data_subject, t)` for at
  least one `t` in the type's fixed audience.
- The record key includes the writer: `<date>:<on_behalf_of>` for
  `attendance_note`. Each person keeps their own note for a date; nobody can
  overwrite someone else's. Updating one's own note is an `updated` event
  (decision D6).

### 4.10 Importance, grouping and notification rules

Rules are configuration, evaluated when an event is written:

```yaml
journal:
  rules:
    - name: full_day_absence
      match: {record_type: attendance_day, kind: [created, updated],
              field_becomes: {classification: full_day}, field_in: {any_unexcused: [true]}}
      importance: high
      notify: [{token: guardians, mode: push},
                {token: subject, mode: prompt, purpose: attendance_followup}]
      group_by: [data_subject, field:date]
    - name: partial_day_absence
      match: {record_type: attendance_day, field_becomes: {classification: partial_day}, field_in: {any_unexcused: [true]}}
      importance: normal
      notify: [{token: guardians, mode: digest}, {token: subject, mode: prompt, purpose: attendance_followup}]
      group_by: [data_subject, field:date]
```

- `importance` is `high | normal | low`; events matching no rule are `low`.
- `notify` is advice to readers, not delivery: the journal stores the matched
  rule on the event; a reader (the patrol) decides how to deliver, for readers
  whose grants include the named token for that subject.
- `purpose` is a named instruction defined by the consumer (Part C).
- `group_by` produces `group_key` so a consumer can present several events
  (six period absences) as one notice.
- Rule changes apply to events written afterwards; old events keep the rule
  they matched.

### 4.11 Untrusted text

`text` values go through the existing scanner during `Apply` (and during
household writes). A verdict is stored **per stored value**: per field in
`record_current.scan`, and per old and new value in `record_event.scan`. On
read, every value is masked by its own verdict -- a value whose verdict is not
`pass` is replaced by `"withheld: failed content scan"` -- so an old value
that failed stays withheld even after the current value passes. A rescan job
re-scans every stored value after a scanner upgrade, updates the verdicts in
both tables, and can release values. `string` fields are not scanned and are therefore length-
and character-limited (4.3) and must not carry free text; the record type
schema decides which is which.

### 4.12 Read API

All reads require a service token and grants.

- `GET /v1/state?subject=...&type=...&scope=...&include_removed=false` --
  visible `record_current` rows, each with `first_seen_at`, `last_seen_at`,
  `confirmed_at` (the `observed_at` of the snapshot that last confirmed it)
  and `in_latest_snapshot` (whether the most recent applied snapshot of its
  scope contained it). For windowed scopes, `in_latest_snapshot=false` means
  "left the window; current status unknown" -- the record is not removed,
  because nothing says it was (section 5.2).
- `GET /v1/events?after=<seq>&subject=...&type=...&min_importance=...&limit=...`
  -- visible events with `seq > after`, ascending. The reader stores the last
  `seq` as its cursor. Gaps in visible `seq` are normal.
- `GET /v1/counts?subject=...` -- per subject and record type: live record
  counts and events by importance for a period; no field values. For a
  counts-only household view. Requires `counts: true` in the service policy and
  is filtered by the reader's grants like every other read.
- Admin (operator ServiceAccount only): `POST /v1/admin/held/{id}:confirm|discard`,
  `POST /v1/admin/erase {data_subject}`, `POST /v1/admin/rescan`.

### 4.13 Erase

`POST /v1/admin/erase` deletes, in one transaction, every `snapshot` row whose
`data_subject` is the subject (held ones included) and every `record_current`
and `record_event` row for the subject, and writes an `erasure` row with
counts. Later snapshots observed before the erasure get `410` (4.4 step 3) and
the connector deletes them, so a spool replay cannot bring data back.
Reader cursors stay valid (sequence numbers are not reused). This is the only
operation that deletes event history.

### 4.14 Spool (connector side)

- Directory `spool/` on the connector's existing state volume. Each poll
  writes its snapshots there first (temp file then rename), named
  `<observed_at>-<snapshot_id>.json`, then sends pending files oldest first
  per scope and acts on each response as in the table in 4.3.
- Bounds: `spool.max_age` (default 7 days) and `spool.max_bytes` (default
  256 MiB), applied to `spool/` and `spool/rejected/` alike. A file still
  answered `409 unknown_subject` when it reaches `max_age` is deleted and
  counted (a subject removed from the roster for good); `max_age` therefore
  applies to every file, newest included, so data at rest
  is always bounded. On overflow the oldest intermediate snapshots of a scope
  are dropped; the newest of each scope is always kept. For complete scopes
  this loses only timing granularity. For windowed scopes a record that
  appeared only in a dropped snapshot is lost; the drop counter makes that
  visible.
- Metrics: `glovebox_journal_spool_files`, `..._spool_oldest_seconds`,
  `..._spool_dropped_total`, `..._spool_rejected_total`. Alert when the spool
  has been non-empty for over an hour.

### 4.15 Observability

Metrics: snapshots by status, events by type and importance, removals, held
snapshots, scan withholdings, API requests by endpoint and status, apply
latency. Alerts: a held snapshot; a spool non-empty for over an hour; no
snapshot from a scope for more than two school days (configurable per scope
prefix); scan withholdings.

## 5. Part B -- the school connectors (glovebox)

Both connectors keep emitting today's items unchanged until rollout step 6
(section 9). Record emission is behind `records.enabled` per connector.

### 5.1 PowerSchool record types

Keys are unique **within one data subject**; the subject is part of every
record's identity (4.2), so siblings in the same course never collide.

| Record type | Key | Fields | Scope (complete?) |
|---|---|---|---|
| `course_mark` | course id; else `number-period` | `course`, `period`, `teacher` string; per term `<term>_letter` string and `<term>_percent` number | `(powerschool, grades, subject, course)` -- yes, one record per scope |
| `assignment` | `<course key>:<assignment_id>` (the `#n` suffix kept for rows shared by sections) | `course`, `category`, `title` string; `due` date; `score`, `max_score`, `percent` number; `letter` string; `missing`, `late`, `incomplete`, `exempt`, `collected` bool; `teacher_comment`, `description`, `category_description` text | `(powerschool, assignments, subject, course)` -- yes: the connector sends the course's full list. Today's lookback filter does not apply to records. |
| `attendance_total` | course key | `course` string; `absences`, `tardies` int | `(powerschool, attendance_totals, subject, course)` -- yes, one record per scope |
| `attendance_mark` | `<attendance course key>:<date>` -- the course key is the connector's existing `attendanceCourseKey` (course, expression, enrolment date), so a re-enrolment is a different record | `date` date; `course`, `period`, `code`, `code_description` string; `kind` string, one of the connector's configured kinds (`absence_unexcused`, `absence_excused`, `late_unexcused`, `late_excused`, `left_early_unexcused`, `left_early_excused`, `authorized`, `unknown`); `excused` bool | `(powerschool, attendance_marks, subject, school_year)` -- yes, with `complete_range` over `date` = the first and last date the grid showed |
| `attendance_day` | date | see below | `(powerschool, attendance_days, subject, school_year)` -- yes, with `complete_range` over `date` = the first and last settled date the grid showed |
| `term_grade` | `<course_number>:<term>` (`#n` on collision, as today) | `course`, `term`, `grade` string; `earned`, `attempted` number | `(powerschool, term_grades, subject)` -- yes |
| `term_comment` | `<course>:<term>` | `course`, `term` string; `comment` text | `(powerschool, term_comments, subject)` -- yes; audience **`guardians` only** |

`school_year` is derived from the date (1 August to 31 July, written
`2026-27`); the attendance page carries no term identifier, so scopes do not
use one.

Scopes with one record (`course_mark`, `attendance_total`) can never see a
removal from `complete=true`; tests should not expect one.

**Term changes.** Pages are course- or term-shaped, so completeness is limited
to what a page demonstrably covers:

- The attendance grid shows the current term's days. `complete_range` limits
  removal to the dates it showed, so at a term change last term's marks and
  days are untouched -- not removed.
- Assignment lists, course marks and attendance totals are scoped **per
  course**. When a term ends and its courses leave the pages, those scopes
  simply receive no more snapshots: their records stay as last seen, with
  their `confirmed_at` date. Nothing is removed by a term change. The same
  holds for a course dropped mid-term; readers see it as "last confirmed on
  <date>" (4.12). A later scope-retirement mechanism can tidy this if needed.

#### `attendance_day`

Derived in the connector from the attendance grid it already reads. Each cell
is one of: a **code** (`CellCode`, mapped to a kind through the configured
`attendance_days.kinds`), **blank** (present), **`.`** (`CellUnmarked`:
nothing marked) or **`-`** (`CellNoMeeting`: no class).

Fields: `date` date; `periods_met` (cells that are not `-`),
`periods_unmarked` (`.`), `absent_unexcused`, `absent_excused`,
`late`, `left_early`, `authorized`, `unknown_codes` int; `any_unexcused` bool
(any `*_unexcused` kind); `classification` string.

Classification, over the periods that met and were marked
(`periods_met - periods_unmarked`):

- `full_day` -- at least one, and every one, of those periods has an absence
  kind (`absence_unexcused` or `absence_excused`).
- `partial_day` -- at least one, but not every one, has an absence kind.
- `late_or_left_early` -- no absence kind; at least one `late_*` or
  `left_early_*` kind.
- `other` -- only `authorized` or `unknown` codes.

`authorized` (a field trip, a student elsewhere in the building) is never an
absence. `unknown` codes are counted and never ignored; a day with
`unknown_codes > 0` is flagged so a person can map the code.

Two rules keep the signal clean:

- **Settled days only.** A day becomes a record only once it is **settled**:
  in the past in the school's time zone, and either it has no unmarked (`.`)
  cells or it is at least three school days old. Days still filling in would
  otherwise read as `partial_day` and then `full_day` and fire rules twice.
  Their codes still arrive at once as `attendance_mark` records.
- **Only days with at least one code are records.** A fully present day is not
  a record. If every code on a day is later cleared, the day drops out of the
  complete scope and the journal records `removed` -- which is exactly "no
  longer an absence day". The connector keeps no memory for this.

Classification is connector logic by design: the journal stays free of
source-specific rules. The importance rules (4.10) match `classification` and
`any_unexcused`, so a full-day absence and a single-period absence or late
arrival are distinct signals.

### 5.2 Schoology record types

| Record type | Key | Fields | Scope (complete?) |
|---|---|---|---|
| `lms_assignment` | assignment id | `title`, `course`, `status`, `link` string; `due` timestamp | `(schoology, assignments, subject)` -- **no**: the overdue/upcoming listing is a window |
| `lms_assignment_detail` | assignment id | `teacher` string; `description`, `comments` text | `(schoology, assignment_detail, subject)` -- **no**: detail reads are budgeted |

What this does and does not capture, stated plainly:

- A change **while the assignment is in the window** (its status, due date,
  title) is captured as an `updated` event. Today the connector's seen-set
  suppresses these entirely.
- An assignment that **leaves the window** (typically because it was submitted
  or its upcoming date passed) produces no event: the listing does not say
  why it left. Its row keeps its last values with `in_latest_snapshot=false`
  (4.12), and readers present it as "no longer listed; status unknown".
- The authoritative record of submitted and scored work is PowerSchool's
  `assignment`, which is a complete scope.

Posts and messages remain ordinary items.

### 5.3 Household record type

| Record type | Key | Fields | Written by |
|---|---|---|---|
| `attendance_note` | `<date>:<on_behalf_of>` | `date` date; `what_happened` text; `heard_from` string (`subject`, `guardian`, ...); `explained_to_school` string (`not_yet`, `yes`, `not_needed`) | the openclaw gateway on behalf of a person-agent, via `POST /v1/notes`; fixed audience `[subject, guardians]` (4.9a) |

`POST /v1/notes` takes `{record_type, date, data_subject, fields,
on_behalf_of}`; the journal computes `on_behalf_of`'s grants from the roster,
forms the key, stamps the fixed audience and writes one `created` or `updated`
event (actor =
`openclaw/openclaw-gateway` plus `on_behalf_of`). It never spools: if the
journal is down the caller gets an error and must say so.

### 5.4 Paper check against other connectors

The contract must fit without change for: **gcalendar** (key: event id; scope:
calendar and time window, `complete=false` outside a fully listed window);
**jira** (key: issue key; scope: project and JQL, `complete` only for an
unfiltered project listing); **gdrive** (key: file id; text fields only for
titles, not document bodies, which stay items). No change to Part A was needed
for these three; noted here so later work starts from it.

## 6. Part C -- the first consumer (openclaw)

- The **school patrol** reads `/v1/state` and `/v1/events` once per
  person-agent, naming that agent's member id as `reader` (never a list, never
  grants), and stores one cursor per agent.
- **Pre-write check (kept from today's patrol).** Before writing anything into
  an agent's workspace, the patrol re-checks every row and event it is about to
  write against its own reading of `household.json`: the row's `data_subject`
  and `audience` must entitle that agent. Any mismatch aborts that agent's
  write entirely and raises an alert. This is independent of the journal's
  filter, so a patrol bug that names the wrong reader is caught before it
  reaches a workspace. The patrol logs the hash of the roster it read (4.9b).
- **Isolation tests** in openclaw: with two child agents and two guardian
  agents over a fixture journal, no child's files ever contain a row about the
  other child or a guardians-only row; a deliberately wrong `reader` is caught
  by the pre-write check.
- **`state.md` / `state.json`** are written from `/v1/state`: every row gets
  its record type and key, `as_of` from the confirming snapshot, removed rows
  are dropped, and the household note for a date is shown next to the school's
  attendance for that date.
- **Digests** are written from events since the cursor, grouped by
  `group_key`, high importance first, each change shown with its observed
  window.
- **Notifications:** events whose rule says `push` for a token the agent holds
  wake that agent through the patrol's existing wake path. `prompt` events
  wake the subject's own agent with the configured purpose. Both remain off
  until the wake-path safety gates (openclaw-16h2) are cleared; until then
  they appear at the top of the digest.
- **Purpose `attendance_followup`** (subject's own agent): find out what
  happened and record it with the notes tool; list assignments in the affected
  courses due within three school days either side of the date, so missed
  work can be collected. Whether and what to tell the school is a guardian
  decision; the agent does not raise it.
- **Notes tool** (`attendance_note`): available to person-agents only; the
  gateway sends the agent's member id as `on_behalf_of` and nothing else about
  access; the journal computes grants and enforces the write rule (4.9a).
- The item fold, body-line parsing and evidence-age rules in the patrol's
  state projection are deleted once the journal path is verified (section 9).

## 7. Seeding

A one-time importer converts school items retained since 2026-10-10 into
snapshots, oldest first, per scope:

- every snapshot is `approximate=true` and `complete=false` (so seeding can
  never remove anything);
- fields come from item tags where present and from body lines otherwise --
  the last use of that parsing, labelled as such;
- `observed_at` is the item's staging time;
- it runs as a Kubernetes Job with an operator ServiceAccount, is idempotent
  (deterministic snapshot ids derived from item ids), and reports counts.

## 8. Testing

- **Journal logic (property tests):** for generated sequences of snapshots --
  including out-of-order arrival, duplicates, windowed and complete scopes,
  declared renames, audience changes -- assert:
  - `record_current` equals a fold of `record_event`;
  - applying any snapshot twice changes nothing;
  - `complete=false` never produces `removed`;
  - a late snapshot never changes current state;
  - the removal guard holds what it should and nothing else.
- **Visibility (property tests):** no row or old value is ever returned without
  a matching grant (including 4.8); for every pair of distinct subjects in a
  generated roster, no read as one subject's own reader returns a row about
  the other unless the roster grants it; a guardians-only row never reaches a
  subject's own reader; a service cannot read as a reader outside its
  `readers`; a request carrying access fields other than `reader` is
  rejected; the roster adapter's mapping is pinned by fixture tests; a roster
  that fails to parse keeps the previous one; after erase nothing remains for
  the subject and a replayed older snapshot is rejected.
- **Roster immutability (deployment test, rollout step 1):** as a cluster
  administrator, an update to the roster ConfigMap is denied by the admission
  policy; every ServiceAccount in the namespace is denied ConfigMap writes
  (`kubectl auth can-i`); the roster hash reported by glovebox equals the one
  openclaw logs.
- **Postgres in a container:** all journal tests run against a real Postgres
  (the CNPG image version in production) started in CI; migrations are tested
  from empty and from every previous version. Database-writing tests are rerun
  whenever the schema changes.
- **Connector snapshot builders:** against the existing recorded, redacted
  fixtures; attendance-day classification for full, partial, late-or-left-early,
  no-meeting, unmarked-then-settled and corrected days; term rollover
  with `complete_range` (last term's records are not removed); scope completeness per surface.
- **Auth:** token audience, allow-list, fail-closed TokenReview, per-service
  write types and scopes; the RSS connector's and the core's accounts are
  refused every write; a connector's projected token is the only token in its
  pod; record-type owner versus scope source, unknown
  subjects (`409`), and allowed `readers`.
- **Spool:** crash mid-write, replay order, overflow policy, `held` and
  `rejected` handling.
- **Shadow comparison** (section 9 step 4) is the end-to-end test against live
  data.

## 9. Rollout

Each step is its own reviewed change and release; each can stop without
affecting what came before.

0. **Per-connector identities** (glovebox chart, 4.9c): one ServiceAccount per
   connector and one for the core, no default token mounts, projected tokens
   only where needed. Ships before step 1 and is useful on its own.

1. **Journal core** (glovebox): schema, `Apply`, API, auth, roster adapter,
   guards, erase, metrics; off by default. Gitops: database on CNPG,
   credentials via Vault and ESO, policies and roles, NetworkPolicy, and the
   roster admission policy (4.9b item 4). The journal is enabled only after the
   roster immutability test (section 8) passes on the cluster.
2. **Connector records and spool** (glovebox): PowerSchool and Schoology emit
   snapshots in shadow, alongside today's items.
3. **Seed** (Job).
4. **Shadow comparison** (openclaw): the patrol builds state from the journal
   into a side file and diffs it with today's state file for at least five
   school days. Every difference is explained before continuing.
5. **Switch the patrol** (openclaw): journal-backed state and digests; notes
   tool; rules. Pushes and prompts stay off until openclaw-16h2.
6. **Stop items for record surfaces** (glovebox): record surfaces stop staging
   items; posts and messages continue.
7. **Dashboard** reads the API.

## 10. Out of scope / later

- Linking the same assignment across PowerSchool and Schoology into one
  record (a later `link` record type; nothing here prevents it).
- Republishing events as items (an outbox) for consumers other than the patrol.
- Migrating non-school connectors.
- A user interface for confirming held snapshots (admin endpoint only).
- Changing the item pipeline's audience enum (records reuse it by default).

## 11. Open questions

1. Exact CNPG cluster and backup/retention settings for the new database
   (decided in the gitops change).
2. Whether three school days is the right settling time for days with unmarked
   cells (5.1), or whether it should be configurable.
3. Whether the three-school-day window for `attendance_followup` should be
   configurable per subject.
