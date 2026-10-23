# Record journal schema

The record journal (spec `docs/superpowers/specs/2026-10-16-record-journal-design.md`)
keeps its state in its own PostgreSQL database, `glovebox_journal`, on the
CloudNativePG cluster. This document describes every table and column. **Every
migration updates it in the same commit**; `internal/journal/schemadoc_test.go`
fails when a migration or a column is not described here.

Migrations are numbered SQL files in `internal/journal/migrations/`, embedded
in the binary and applied at start under a session advisory lock
(`hashtextextended('glovebox_journal:migrate', 0)`). A binary refuses a
database whose schema is newer than its own.

`schema_migrations` records each applied migration's name and the SHA-256
checksum of its file, and every start verifies them: an applied migration that
was edited or renamed is refused, so migrations are immutable once released
and changes go in a new one. Each migration runs inside one transaction
together with its `schema_migrations` row, so a migration file must not
contain `BEGIN`/`COMMIT` or statements that cannot run in a transaction such
as `CREATE INDEX CONCURRENTLY`. Database tests run against the
CloudNativePG image at the production PostgreSQL version
(`scripts/journal-test-db.sh`); rerun them after any schema change.

## Invariants

- **The fold invariant.** For every record, folding its `record_event` rows
  in `seq` order -- mapping each event's field names forward through the
  renames of later schema versions -- reproduces exactly the event-derived
  columns of its `record_current` row: `scope`, `audience`, `fields`,
  `removed`. Observation columns are not events. Property tests in
  `internal/journal/property_test.go` assert this.
- **Event shape.** CHECK constraints: `created` and `restored` events carry
  `fields`; `updated` and `removed` carry `changed`; `prev_scope` is set only
  on `updated` and `restored` events (a removed record that returns in another
  scope is restored there).
- **Append-only events.** A trigger refuses any `UPDATE` of `record_event`
  that changes anything but `scan`, with one exception: erase's rewrite of a
  kept note (see `erasure` below), admitted only inside a transaction that set
  `glovebox_journal.erase_rewrite = on` (transaction-local) and only in its
  exact shape (migration `0002_erase_rewrite`). Rows are deleted only by
  erase.
- **Ordered sequence numbers.** Every transaction that inserts events holds
  the advisory lock `glovebox_journal:events` from its first insert to its
  commit, so a reader that has seen `seq = n` will never later see a
  smaller `seq` appear.
- **One writer per subject.** Apply, confirm, notes and erase for one
  `data_subject` serialize on the advisory lock
  `glovebox_journal:subject:<data_subject>` (the household's is
  `glovebox_journal:subject:`). Erase is the only writer that takes more than
  one: the erased subject's, the household's and those of every subject the
  erased member wrote notes about, all in sorted order before any row lock,
  so it cannot deadlock with Apply, notes or another erase.

## Tables

### `schema_migrations`

| Column | Type | Meaning |
|---|---|---|
| `version` | integer | Migration number. |
| `name` | text | Migration name (file name without number and extension). |
| `checksum` | bytea | SHA-256 of the migration file as applied. |
| `applied_at` | timestamptz | When it ran. |

### `record_type`

One row per record type and schema version, written at start from the
`journal.record_types` configuration. A row never changes: changing a type
means adding a version. A version stored here must stay in the configuration.

Activating a version that has `renames` rewrites `record_current.fields`,
`scan` and `content_hash` of that type to the new names, in one transaction
under the migration lock, without events. Rolling-deploy hazard: a replica
still running the previous configuration could apply a snapshot after the
rewrite and write the old names back. Activate a version with renames only
with the old replicas gone first (a single replica, or the Recreate
strategy for that release).

| Column | Type | Meaning |
|---|---|---|
| `name` | text | Type name, `[a-z][a-z0-9_]{0,63}`. |
| `schema_version` | integer | 1, 2, ... without gaps. The highest is the active version. |
| `owner` | text | The source that may write it (`powerschool`), or `household` for types written through `/v1/notes`. A snapshot's scope `source` must equal it. |
| `fields` | jsonb | `{name: {"type": t, "required": bool}}`; `t` is one of `string`, `text`, `int`, `number`, `bool`, `date`, `timestamp`, `string_list`. |
| `renames` | jsonb | `{old_name: new_name}` from the previous version's names to this version's. |
| `default_audience` | text[] | Audience stamped on a record that names none. |
| `fixed_audience` | text[] | Household types only: the audience every note gets; writers cannot choose it. NULL otherwise. |
| `allow_no_subject` | boolean | Whether records with `data_subject = ""` are accepted. |
| `created_at` | timestamptz | When this version was first activated. |

### `snapshot`

One row for every snapshot received and accepted (status 200), whatever it
did. Refused snapshots (4xx) are not stored. `duplicate` is a response, not a
stored status: a resend is answered from the original row.
A resend is a duplicate only when its writer, record type and canonical scope
equal the stored row's. A resend whose canonical scope or writer name differs
(a change of the scope's canonical form, a renamed connector ServiceAccount) is
refused as an id reuse (422 `snapshot_id_reused`) rather than answered as a
duplicate; the data was already applied.

| Column | Type | Meaning |
|---|---|---|
| `id` | uuid | Assigned by the connector; the idempotency key. |
| `record_type` | text | |
| `schema_version` | integer | The version the snapshot was written in (before renames). |
| `scope` | text | Canonical scope: `name=value` parts joined by `/`, with `%`, `/` and `=` in names and values escaped as `%25`, `%2F`, `%3D`. |
| `data_subject` | text | The scope's `data_subject` part, `""` if none. Every record in the snapshot has this subject. |
| `complete` | boolean | Whether absence from the snapshot means removal. |
| `approximate` | boolean | Seeded data (spec section 7). |
| `observed_at` | timestamptz | When the connector read the source (connector clock). |
| `received_at` | timestamptz | When the journal received it (core clock). |
| `writer` | text | Authenticated ServiceAccount, `namespace/name`. |
| `status` | text | `applied`, `late` (an applied snapshot of the scope was observed later; nothing changed), `held` (removal guard; waiting for an operator), `discarded` (a held snapshot overtaken or rejected). |
| `payload` | jsonb | The request body as received. Kept for every status. |
| `stats` | jsonb | `{"created","updated","removed","restored","unchanged","stale"}` counts. For `held`, what applying would have done. |

Indexes: partial `(record_type, scope, observed_at) WHERE status = 'applied'`,
`(data_subject)`, partial `(record_type, scope) WHERE status = 'held'`.

### `record_current`

The latest version of every record, removed ones included (tombstones).
Primary key `(record_type, data_subject, key)`.

| Column | Type | Meaning |
|---|---|---|
| `record_type` | text | |
| `data_subject` | text | Part of the identity: two subjects may use the same key. |
| `key` | text | Stable within one subject, chosen by the source. |
| `scope` | text | Event-derived. The scope the record now belongs to. |
| `audience` | text[] | Event-derived. Sorted, distinct tokens. |
| `fields` | jsonb | Event-derived. Canonical values under the active version's names; null fields are absent. |
| `removed` | boolean | Event-derived. Tombstone. |
| `first_seen_at` | timestamptz | Observation. `observed_at` of the snapshot that created it. |
| `last_seen_at` | timestamptz | Observation. Newest `observed_at` at which it was present. |
| `last_confirmed_snapshot` | uuid | Observation. The snapshot that last confirmed this row (present, or removed by it). NULL for household notes. |
| `last_confirmed_at` | timestamptz | Observation. That snapshot's `observed_at` (for notes, the write time). It is the `prev_observed_at` of the next event. |
| `content_hash` | bytea | SHA-256 over the canonical fields; an unchanged record costs no event. |
| `last_event_seq` | bigint | The newest event of this record. |
| `scan` | jsonb | Per `text` field of the current value: `{"verdict": v, "scanner_version": s}`. |

Indexes: `(data_subject)`, `(scope)`, GIN on `audience`.

### `record_event`

Append-only change log; the reader cursor is `seq`.

| Column | Type | Meaning |
|---|---|---|
| `seq` | bigint | Sequence number. Never reused, also not after erase. |
| `record_type` | text | |
| `data_subject` | text | |
| `key` | text | |
| `kind` | text | `created`, `updated`, `removed`, `restored`. |
| `schema_version` | integer | The version whose field names this row uses. Old rows are never rewritten; readers map names forward. |
| `scope` | text | Scope after the event. |
| `prev_scope` | text | Set when the record moved scope. |
| `audience` | text[] | Audience after the event. |
| `prev_audience` | text[] | Set when the audience changed (4.8). |
| `fields` | jsonb | Full fields for `created` and `restored`; NULL otherwise. |
| `changed` | jsonb | `updated`: `{field: [old, new]}` (null for absent). `removed`: the last known fields. NULL otherwise. |
| `scan` | jsonb | A verdict for every `text` value in this row: `{field: {"old": v, "new": v, "scanner_version": s}}`. |
| `observed_at` | timestamptz | When the change was observed. |
| `prev_observed_at` | timestamptz | When the old value was last confirmed; the change happened in `(prev_observed_at, observed_at]`. NULL for `created`. For `restored` it is the `observed_at` of the snapshot that removed the record (the removal is its last confirmation). |
| `approximate` | boolean | Seeded. |
| `snapshot_id` | uuid | The snapshot that caused it; NULL for household writes. |
| `actor` | text | Writer ServiceAccount; for notes also `;on_behalf_of=<member>`. |
| `importance` | text | `high`, `normal`, `low` (no rule matched). |
| `rule` | text | Name of the matching rule. |
| `notify` | jsonb | That rule's `notify` list as it was when the event was written. |
| `group_key` | text | From the rule's `group_by`. |
| `written_at` | timestamptz | Core clock at insert. |

Indexes: `(data_subject, seq)`, `(record_type, data_subject, key, seq)`, GIN on
`audience`.

### `erasure`

One row per erase (4.13). Never erased itself; holds no field data.

| Column | Type | Meaning |
|---|---|---|
| `id` | bigint | |
| `data_subject` | text | The erased subject. Snapshots of it observed before `erased_at` plus the maximum future skew (10 minutes, the writer-clock allowance of validation) are refused with 410; one observed exactly `erased_at` plus the skew or later is accepted. |
| `erased_at` | timestamptz | Core clock. |
| `actor` | text | Admin ServiceAccount. |
| `rows` | jsonb | Counts only: `{"snapshot": n, "record_current": n, "record_event": n, "notes_deleted_as_author": n, "notes_pseudonymised": n}`. The first three are rows deleted (the member's own notes about others included); `notes_deleted_as_author` is the `record_current` rows among them that were notes the member wrote; `notes_pseudonymised` is the notes kept (not deleted). No pseudonym and no other member's id is recorded. Rows written before migration `0002_erase_rewrite` have the first three keys only. |

Index: `(data_subject, erased_at)`.

**What erase does with household notes (decision 2026-10-23, spec 4.13).** A
note belongs to its author, the member in its key `<date>:<on_behalf_of>` (and
in its events' actor `writer;on_behalf_of=<id>`).

1. Erasing a member deletes every note they wrote, about anyone: other
   subjects, the household, themselves, and notes of theirs kept from an
   earlier erasure.
2. Notes someone else wrote about the erased member are kept when their
   audience has a `guardians` part, because the observation is the author's.
   Each is moved, rows and events alike, to the household: `data_subject`
   becomes `''`, `key` becomes `<date>:<author>:erased:<token>`, `scope` the
   household scope of its type (`source=household/record_type=<type>`),
   `audience` and `prev_audience` keep only `guardians`, `prev_scope` (when
   set) the household scope, `group_key` NULL (it may embed the subject or
   the scope). The fields, the free text included, are kept exactly as
   written: text that names the member is the author's observation and is not
   scrubbed. A note with no `guardians` part has no reader left once its
   subject part is gone and is deleted.
3. Everything else of the member is deleted, as before.

The pseudonym is `erased:` and 128 random bits (32 hex digits), drawn per
erasure and stored nowhere else. A hash of the id, even keyed, was rejected: a
household has a handful of ids, so an unkeyed hash is reversed by trying them,
and a keyed one is as safe as its key, would need key management, and makes
two erasures of one member linkable. A random token cannot be recomputed from
the id at all. It can still be linked by elimination: the `erasure` row names
the member and its time, so with one erasure in the database its kept notes
are evidently that member's. The pseudonym guarantees that the id is gone from
the rows, not that a reader with database access cannot infer whom a kept note
was about, and the text itself may say so.

Kept notes are read under the unchanged visibility rule: a household row with
audience `[guardians]` is visible to holders of the grant `("", guardians)`,
which the roster gives every guardian (spec 11 3.4). No grant names a
pseudonym, so nothing else matches it, and no member holds `("", subject)` (a
subject grant names the member's own id). The erased member reads it only if
they are still in the roster as a guardian; erase is meant for members who
have left it. Readers' cursors stay valid: the events keep their `seq`, and
the fold invariant holds for the moved record. The new keys cannot collide:
the moved rows' keys were unique per (type, subject), the token is new for
each erasure, and no note written through `POST /v1/notes` has a key with a
third `:`-part (roster ids contain no `:`).

## Design notes

Choices this schema makes where the spec leaves room (also in the plan,
`docs/superpowers/plans/2026-10-16-record-journal-core.md`):

- Old values across an audience change fail closed. A NULL `prev_audience`
  means the audience did not change; `access.OldValue` takes that as an
  explicit `audienceChanged` flag, not as a nil slice. When the flag is set,
  an empty or undecodable previous audience matches nobody, so the old value
  is withheld (a decode failure while reading events is an error).
- Record type versions evolve additively: a later version may not add a
  required field or make an optional one required (`NewRegistry` refuses it),
  because writes from older versions are not checked against later
  versions. An empty `audience` on a record takes the type default, since a
  record visible to nobody is never intended.
- `snapshot` is indexed by `(record_type, scope, observed_at)` restricted to
  applied snapshots, not `(scope, observed_at)`: the lateness check and
  `in_latest_snapshot` look only at applied snapshots, per record type and
  scope.
- `record_current.last_confirmed_at` and `record_event.notify` are additions
  to the spec's column lists; neither is event-derived.
- Locks: per subject for writes, one global lock around event inserts (see
  Invariants). The global lock serialises every transaction that writes
  events, from its first event insert to its commit. That costs concurrency
  only between writers of different subjects, and at household scale (a
  handful of connectors, snapshots of at most a few thousand records) it is
  not a bottleneck; if it ever is, the alternative is a reader-side
  watermark (serve only seq below the oldest in-flight transaction's).
- Rows are read `FOR UPDATE` by Apply and by each rescan batch, so verdicts
  written by one are never overwritten from a stale read by the other.
- Record types: a field keeps its type across versions, renames keep the
  type, and a text field is never dropped (4.7); values that carry a scan
  verdict are masked by it whatever the active type says (4.11).

- Rules (spec 4.10): the first matching rule in configured order wins. With
  several `field_becomes` entries the condition is their conjunction: it
  matches when all entries hold after the event and not all held before
  (a missing before counts as not holding), so excused -> unexcused on a
  full day fires and staying full-unexcused does not refire. `field_in`
  tests the value after the event (last known value for `removed`). Rule
  JSON is decoded strictly (unknown keys are errors). A `group_key` is
  `entry=value` pairs joined by `/`, values escaped; an absent field is
  `%00`; a key over 512 bytes is replaced by `sha256=<hex of the key>`.
  Notify tokens are syntax-checked only. `group_by` may not name a `text`
  field (`rules.Compile` refuses it): a group key is stored and returned as
  written, outside the per-value scan verdicts, so it must carry no untrusted
  text and no digest of it. As defence in depth the read path drops an
  event's `group_key` when any text value of the event is withheld.

- Reads (spec 4.12): the reader is a roster member; the journal computes the
  grants from the roster in use and refuses a reader who is not in it
  (`ErrUnknownReader`, 403) or any read while no roster is loaded
  (`ErrNoRoster`, 503). Every row is filtered in SQL and checked again in Go
  (counts per group of subject, type, audience); a disagreement drops the
  row and counts `VisibilityMismatch`. A page the re-check empties entirely
  fails closed, stalls that reader's cursor and fires the alert. The state,
  events and counts scans over the rows the audience index leaves are
  accepted as cheap at household scale.
- Shape leaks the spec accepts (4.8): a reader who matches the new audience
  but not the old one still sees, on an `updated` event, which field names
  changed (the keys of `changed`), and a rule's `rule`, `importance` and
  `notify` advice fires on a `field_becomes` match whose before-value that
  reader may not see. Their old values, `prev_scope` and `prev_observed_at`
  are withheld, and `prev_audience` is never on the wire.

## Migration history

| Migration | Change |
|---|---|
| `0001_init` | All tables above, their indexes and the append-only trigger. |
| `0002_erase_rewrite` | Replaces the append-only trigger's function: besides `scan`, it admits erase's in-place rewrite of a kept note's events (decision 2026-10-23), only under the transaction-local setting `glovebox_journal.erase_rewrite = on` and only from a subject to the household, with the key gaining an `:erased:<token>` suffix, scopes without a `data_subject=` part, audiences that only shrink and `group_key` cleared. No table changes. |
