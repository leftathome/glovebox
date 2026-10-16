# Design spec: record journal (stateful sources as keyed records and change events)

- **Status:** draft (brainstorm converged 2026-10-16)
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
| D8 | Audience vocabulary | **Opaque tokens**; no household roles in code. Readers are described by **grants** that the **calling service supplies**, capped per service | Glovebox holds a relationship directory |
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
| **Record** | One instance of a type, identified by `(record_type, key)`. `key` is a string the source guarantees stable. |
| **Fields** | A flat map of typed values: `string`, `text` (untrusted free text, scanned), `int`, `number`, `bool`, `date`, `timestamp`, `string_list`. Every field is nullable. |
| **Scope** | The unit a connector observes in one go, e.g. `(source=powerschool, surface=assignments, data_subject=X, course=Y)`. Written as an ordered list of `name=value` parts. |
| **Snapshot** | One observation of one scope at one time: the records seen, plus `complete` -- whether absence from this snapshot means the record no longer exists. |
| **Event** | One append-only change to one record: `created`, `updated`, `removed`, `restored`. |
| **Current state** | The latest version of every record, including removed ones (tombstoned). Always equal to a fold of the events. |
| **data_subject** | As in spec 11: the entity a record is about. Opaque string. |
| **Audience** | As in spec 11: a list of tokens naming who may see the record, relative to its `data_subject`. Opaque to the journal. |
| **Grant** | A pair `(subject, token)` held by a reader. `subject` may be `*` (any) or `""` (records with no `data_subject`). |

## 4. Part A -- the journal core (glovebox)

### 4.1 Components

```
connector process                         glovebox core service                   readers
-----------------                         ---------------------                   -------
records lib  -- builds snapshot           /v1/records  (write, SA token)
spool dir    -- one file per snapshot ->  journal.Apply (one transaction)  ---->  Postgres (CNPG)
                                          /v1/notes    (write, SA token)           record_type, snapshot,
                                          /v1/state, /v1/events,                   record_current, record_event,
                                          /v1/counts   (read, SA token + grants)   erasure, held_snapshot
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

**`snapshot`**
- `id uuid` (assigned by the connector; idempotency key), `record_type`,
  `schema_version`
- `scope text` (canonical form), `complete bool`, `approximate bool`
- `observed_at timestamptz`, `received_at timestamptz`, `writer text`
  (authenticated service)
- `status` -- `applied | duplicate | late | held | discarded | rejected`
- `stats jsonb` -- counts created/updated/removed/restored/unchanged

**`record_current`** -- primary key `(record_type, key)`
- `scope text`, `data_subject text`, `audience text[]`
- `fields jsonb`, `content_hash bytea`
- `scan jsonb` -- per `text` field: `{verdict, scanner_version}`
- `first_seen_at`, `last_seen_at`, `removed_at` (null unless tombstoned)
- `last_event_seq bigint`

**`record_event`** -- append-only, primary key `seq bigserial`
- `record_type`, `key`, `scope`, `kind`
- `data_subject`, `audience text[]`, `prev_audience text[]` (4.8)
- `fields jsonb` -- full fields for `created` and `restored`; null otherwise
- `changed jsonb` -- `{field: [old, new]}` for `updated`; last known fields for
  `removed`
- `observed_at`, `prev_observed_at` (4.6), `approximate bool`
- `snapshot_id uuid` (null for household writes), `actor text` (writer service,
  and for notes the person-agent on whose behalf it wrote)
- `importance text`, `rule text`, `group_key text` (section 4.10)

**`erasure`** -- `data_subject`, `erased_at`, `actor`, `rows jsonb` (counts).
Never erased itself; holds no field data.

**`held_snapshot`** -- the full payload of a snapshot held by the removal guard
(4.5), until confirmed or discarded.

Indexes: `record_current (data_subject)`, `record_current (scope)`, GIN on
`audience` in both record tables, `record_event (data_subject, seq)`,
`record_event (record_type, key, seq)`.

### 4.3 Wire format: `POST /v1/records`

```json
{
  "snapshot_id": "0b9f...-uuid",
  "record_type": "assignment",
  "schema_version": 1,
  "scope": [["source","powerschool"],["surface","assignments"],["data_subject","e_..."],["course","1234"]],
  "complete": true,
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
  the scope has one.
- Response: `200 {"status":"applied"|"duplicate"|"late"|"held", "stats":{...}}`,
  `422` with a machine-readable reason for schema violations, `403` for policy
  violations (4.9). `5xx` means "retry later" and the spool keeps the file.

### 4.4 Applying a snapshot

In one transaction, under a per-scope advisory lock:

1. **Idempotency.** If `snapshot_id` exists, return `duplicate` with the stored
   stats.
2. **Erasure guard.** If any record's `data_subject` has an `erasure` row with
   `erased_at > observed_at`, reject the snapshot (`rejected`, reason
   `erased_subject`).
3. **Ordering.** If a snapshot for the same scope with a later `observed_at`
   is already applied, store this one as `late`: kept for history, not diffed,
   no events. Current state never moves backwards.
4. **Schema.** Validate fields against the type's declared schema version;
   apply declared renames; reject on type mismatch; store undeclared fields
   but flag them in `stats`.
5. **Scan.** Run every `text` field through the existing scanner in-process;
   record verdicts. Scanning never blocks the write (4.11).
6. **Diff.** Load `record_current` for the scope. For each incoming record:
   - not present -> `created`
   - present and tombstoned -> `restored` (full fields)
   - present, same `content_hash` and same audience -> touch `last_seen_at`
     only; no event
   - otherwise compare field by field -> `updated` with only the changed fields
7. **Removal.** If `complete`, every live key in the scope that the snapshot
   does not contain is a candidate `removed`. The removal guard (4.5) decides.
8. **Write.** Insert events (assigning `importance`, `rule` and `group_key`,
   section 4.10), upsert `record_current`, insert the `snapshot` row.

A snapshot with `complete=false` never removes anything.

### 4.5 Removal guard

If a complete snapshot would remove more than `removal_guard.max_fraction`
(default 0.3) of the scope's live keys, or every key of a scope that had more
than `removal_guard.min_keys` (default 3), the snapshot is **held**: nothing is
applied, the payload goes to `held_snapshot`, a metric and an alert fire, and
the connector is told `held` (it does not retry). An operator confirms (apply
as received) or discards it through an admin endpoint. Later snapshots for the
same scope apply normally only if they do not trip the guard; a held snapshot
older than an applied one is discarded automatically as `late`.

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

A record type's schema version is bumped by a migration-like config change.
`renames` map old field names to new ones before diffing, so a rename is not a
remove-plus-add. A type change of an existing field requires a new field name.
Old events are never rewritten; readers interpret them with the schema version
recorded on their snapshot.

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
  services:
    openclaw/openclaw-gateway:
      write_types: [attendance_note]          # household writes, on behalf of an agent
      read: true
      grant_tokens: [subject, guardians, siblings, household]   # tokens it may present
    openclaw/glovebox-glovebox-powerschool:
      write_types: [course_mark, assignment, attendance_mark, attendance_day,
                    attendance_total, term_grade, term_comment]
      write_scopes: {source: powerschool}
      write_subjects: [e_..., e_...]          # its configured subjects only
    openclaw/glovebox-glovebox-schoology:
      write_types: [lms_assignment, lms_assignment_detail]
      write_scopes: {source: schoology}
      write_subjects: [e_..., e_...]
    trunchbull/trunchbull:
      read: true
      grant_tokens: [guardians]
      counts: true
```

**Grants.** Every read and every household write carries the reader's or
writer's grants, computed by the calling service from its own roster:

```json
{"reader": "e_parent1", "grants": [["e_child1","guardians"],["e_child2","guardians"],["e_parent1","subject"]]}
```

- A grant whose token is not in the service's `grant_tokens` is rejected
  (`403`), so a compromised dashboard cannot claim `subject` for anyone.
- **Visibility rule:** a row is visible if and only if the reader holds a
  grant `(s, t)` where `s` is the row's `data_subject` (or `*`) and `t` is in
  the row's `audience`. `public` in a row's audience is visible to any
  authenticated reader. `household` is matched literally: callers that want
  "household" visibility present `(s, household)` grants.
- **Write rule:** a household write is allowed only if the writer's grants
  cover the record's `data_subject` with at least one token in the record's
  audience.
- The journal never interprets tokens beyond this matching. The token
  vocabulary for records is the spec 11 enum by default and is configurable;
  the journal itself contains no household roles.

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

`text` fields go through the existing scanner during `Apply`. Each verdict is
stored per field with the scanner version. On read, a field whose latest
verdict is not `pass` is replaced by `"withheld: failed content scan"`, in both
state and events. A rescan job re-scans stored text after a scanner upgrade and
can release fields. `string` fields are not scanned and are therefore length-
and character-limited (4.3) and must not carry free text; the record type
schema decides which is which.

### 4.12 Read API

All reads require a service token and grants.

- `GET /v1/state?subject=...&type=...&scope=...&include_removed=false` --
  visible `record_current` rows, with `first_seen_at`, `last_seen_at`,
  `removed_at` and the snapshot `observed_at` that last confirmed them.
- `GET /v1/events?after=<seq>&subject=...&type=...&min_importance=...&limit=...`
  -- visible events with `seq > after`, ascending. The reader stores the last
  `seq` as its cursor. Gaps in visible `seq` are normal.
- `GET /v1/counts?subject=...` -- per subject and record type: live record
  counts and events by importance for a period; no field values. For a
  counts-only household view. Requires `counts: true` in the service policy.
- Admin (operator ServiceAccount only): `POST /v1/admin/held/{id}:confirm|discard`,
  `POST /v1/admin/erase {data_subject}`, `POST /v1/admin/rescan`.

### 4.13 Erase

`POST /v1/admin/erase` deletes, in one transaction, every `snapshot`,
`record_current`, `record_event` and `held_snapshot` row for the subject, and
writes an `erasure` row with counts. Later snapshots observed before the
erasure are rejected (4.4 step 2), so a spool replay cannot bring data back.
Reader cursors stay valid (sequence numbers are not reused). This is the only
operation that deletes event history.

### 4.14 Spool (connector side)

- Directory `spool/` on the connector's existing state volume. Each poll
  writes its snapshots there first (temp file then rename), named
  `<observed_at>-<snapshot_id>.json`, then sends pending files oldest first
  per scope, deleting each on `applied | duplicate | late | held` and keeping
  it on `5xx` or network failure.
- Bounds: `spool.max_age` (default 7 days) and `spool.max_bytes` (default
  256 MiB). On overflow the oldest intermediate snapshots of a scope are
  dropped; the newest of each scope is always kept. Only timing granularity is
  lost.
- `4xx` other than `409` is not retried: the file moves to `spool/rejected/`
  with the reason, and a metric fires.
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

| Record type | Key | Fields | Scope (complete?) |
|---|---|---|---|
| `course_mark` | course id; else `number-period` | `course` string, `period` string, `teacher` string, `term_marks` per term: `<term>_letter` string, `<term>_percent` number | `(powerschool, grades, subject)` -- yes |
| `assignment` | `<section>:<assignment_id>` (`#n` suffix kept for shared rows) | `course`, `category`, `title` string; `due` date; `score`, `max_score`, `percent` number; `letter` string; `missing`, `late`, `incomplete`, `exempt`, `collected` bool; `teacher_comment`, `description`, `category_description` text | `(powerschool, assignments, subject, course)` -- yes: the connector sends the course's full list. Today's lookback filter stops applying to records. |
| `attendance_total` | course | `absences`, `tardies` int | `(powerschool, attendance_totals, subject)` -- yes |
| `attendance_mark` | `<course>:<period>:<date>` | `date` date, `course`, `period`, `code`, `code_description` string, `kind` string (`absence_unexcused`, `absence_excused`, `tardy_unexcused`, `tardy_excused`, `left_early_excused`, ...), `excused` bool | `(powerschool, attendance_marks, subject, term)` -- yes |
| `attendance_day` | date | `date` date, `periods_met`, `absent_unexcused`, `absent_excused`, `tardies`, `left_early` int, `classification` string (`full_day`, `partial_day`, `tardy_only`, `present`), `any_unexcused` bool | `(powerschool, attendance_days, subject, term)` -- yes |
| `term_grade` | `<course_number>:<term>` | `course`, `term`, `grade` string; `earned`, `attempted` number | `(powerschool, term_grades, subject)` -- yes |
| `term_comment` | `<course>:<term>` | `course`, `term` string, `comment` text | `(powerschool, term_comments, subject)` -- yes; audience **`guardians` only** |

`attendance_day` is derived in the connector from the attendance grid, which
shows every period of every day (absence codes, present, no meeting):

- `periods_met` -- periods with a class meeting that day.
- `full_day` -- every period that met has an absence code.
- `partial_day` -- at least one, but not all, periods that met have an
  absence code.
- `tardy_only` -- no absence codes, at least one tardy or left-early code.
- `present` -- none of the above. Present days are emitted only when they
  replace an earlier non-present classification (so a correction is visible);
  otherwise they are not records, to keep volume down.

Classification is connector logic by design: the journal stays free of
source-specific rules.

### 5.2 Schoology record types

| Record type | Key | Fields | Scope (complete?) |
|---|---|---|---|
| `lms_assignment` | assignment id | `title`, `course`, `status`, `link` string; `due` timestamp | `(schoology, assignments, subject)` -- **no**: the overdue/upcoming listing is a window |
| `lms_assignment_detail` | assignment id | `teacher` string; `description`, `comments` text | `(schoology, assignment_detail, subject)` -- **no**: detail reads are budgeted |

Because snapshots carry the whole listing each poll, status changes are now
captured (today the connector's seen-set suppresses them). Posts and messages
remain ordinary items.

### 5.3 Household record type

| Record type | Key | Fields | Written by |
|---|---|---|---|
| `attendance_note` | `<data_subject>:<date>` | `date` date; `what_happened` text; `heard_from` string (`subject`, `guardian`, ...); `explained_to_school` string (`not_yet`, `yes`, `not_needed`) | the openclaw gateway on behalf of a person-agent, via `POST /v1/notes`; default audience `[subject, guardians]` |

`POST /v1/notes` takes `{record_type, key, data_subject, audience, fields,
on_behalf_of, grants}` and writes one `created` or `updated` event (actor =
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

- The **school patrol** reads `/v1/state` and `/v1/events` for each
  person-agent, presenting that agent's grants (computed from
  `household.json`), and stores one cursor per agent.
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
  gateway adds the agent's grants and `on_behalf_of`; the journal enforces the
  write rule.
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
  a matching grant (including 4.8); a service cannot present tokens outside
  its `grant_tokens`; after erase nothing remains for the subject and a replayed
  older snapshot is rejected.
- **Postgres in a container:** all journal tests run against a real Postgres
  (the CNPG image version in production) started in CI; migrations are tested
  from empty and from every previous version. Database-writing tests are rerun
  whenever the schema changes.
- **Connector snapshot builders:** against the existing recorded, redacted
  fixtures; attendance-day classification for full, partial, tardy-only,
  no-meeting and corrected days; scope completeness per surface.
- **Auth:** token audience, allow-list, fail-closed TokenReview, per-service
  write types, scopes, subjects and grant tokens.
- **Spool:** crash mid-write, replay order, overflow policy, `held` and
  `rejected` handling.
- **Shadow comparison** (section 9 step 4) is the end-to-end test against live
  data.

## 9. Rollout

Each step is its own reviewed change and release; each can stop without
affecting what came before.

1. **Journal core** (glovebox): schema, `Apply`, API, auth, guards, erase,
   metrics; off by default. Gitops: database on CNPG, credentials via Vault and
   ESO, policies and roles, NetworkPolicy.
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
2. Whether `attendance_day` should also be emitted for `present` days that
   follow a non-present one only within the same term (current rule) or for a
   longer window.
3. Whether the three-school-day window for `attendance_followup` should be
   configurable per subject.
