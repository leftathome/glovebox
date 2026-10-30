# Record journal API

The record journal keeps stateful sources -- a gradebook, an assignment
list, an attendance grid -- as keyed records with typed fields, appends a
change event whenever a record changes, and serves current state and the
change log to readers, filtered by what each reader is entitled to see.

- Design: `docs/superpowers/specs/2026-10-16-record-journal-design.md`.
- Database schema, invariants and what erase does to each table:
  [journal-schema.md](journal-schema.md).
- The household roster (ownership, format, grants, reloads, erase and the
  roster, the enabling checklist): [journal-roster.md](journal-roster.md).
- What the chart renders when the journal is enabled, and the gitops
  prerequisites: [upgrading.md, "Record journal"](upgrading.md#record-journal).
- The wire types: `internal/journal/journalapi` (Go). Where this document
  and the code disagree, the code is right; please fix this document.

The journal runs in the glovebox core, on its own listener (default port
9095), and is **off** unless `journal.enabled` is set in the chart. Every id
below (`e_parent1`, `e_child1`, ...) is a placeholder.

- [Requests and refusals](#requests-and-refusals)
- [Authentication](#authentication)
- [Authorization: the policy](#authorization-the-policy)
- [Readers and grants](#readers-and-grants)
- [POST /v1/records](#post-v1records)
- [POST /v1/notes](#post-v1notes)
- [GET /v1/state](#get-v1state)
- [GET /v1/events](#get-v1events)
- [GET /v1/counts](#get-v1counts)
- [GET /v1/healthz](#get-v1healthz)
- [Operator endpoints](#operator-endpoints)
- [Audit logs](#audit-logs)
- [Configuration](#configuration)
- [Metrics and alerts](#metrics-and-alerts)
- [Connector side: the spool](#connector-side-the-spool)
- [Known follow-ups](#known-follow-ups)

---

## Requests and refusals

Every answer is JSON (`Content-Type: application/json`, `Cache-Control:
no-store`). Every refusal, on every endpoint, has the same shape:

```json
{"status": "rejected", "reason": "policy", "detail": "this service may not read"}
```

`reason` is a fixed code (`journalapi.Reason*`); `detail` is a fixed text or
names fields and positions, never a value the caller sent. Lists in answers
are always `[]`, maps `{}`, never `null`. Times are RFC 3339 in UTC.

Each request goes through these steps in this order; the first that fails
answers:

1. **Route.** An unknown path, or a known path with another method, is
   `404 not_found` (no token needed).
2. **Ready.** Until the journal has warmed its scanner, loaded a roster,
   migrated its database and stored its record types, every request is
   `503 unavailable` -- before authentication.
3. **Bearer token** (`401 unauthenticated`, with `WWW-Authenticate: Bearer`).
4. **An in-flight slot.** At most 16 requests do work at once (TokenReview
   plus the handler). Beyond that: `503 unavailable` with `Retry-After: 1`.
   The limit is not configurable.
5. **TokenReview** (`401`, `403 forbidden`, `503 auth_unavailable`).
6. **The endpoint's own checks** (below).

Each request runs under `journal.requestTimeoutSeconds` (default 60), the
TokenReview included.

Rules shared by the endpoints:

- **Every POST refuses a query string** (`400 bad_request`), before the body
  is read: a write carries everything in its body. `GET /v1/healthz` takes
  no query parameters either.
- **Reads admit only their own named parameters, each at most once**
  (`400`). A request names its reader and its filters and nothing else about
  access: a grant, an audience or a token in the query is refused, not
  ignored.
- **Bodies are decoded strictly**: unknown, duplicate and case-variant keys
  and trailing data are refused. A body needs `Content-Type:
  application/json` (`400`).
- **Body and key caps are checked before the body is decoded** (`413
  too_large`, and the connection is closed when the body is too long).

| Body | Byte cap | Key caps (per object / per document) |
|---|---|---|
| `POST /v1/records` | 8 MiB | widest record type version's field count (at least the 9 top-level keys) + 16 / `12 + 5000 x (4 + that width) + 1024` |
| `POST /v1/notes` | derived: the widest household type version's fields at their widest JSON encoding (every string byte `\u`-escaped) + 16 KiB, never above 8 MiB | `max(5, widest household field count) + 4` / `5 + widest + 4` |
| `POST /v1/admin/erase` | 3,454 bytes | 4 / 4 |
| `POST /v1/admin/rescan` | 283 bytes | 3 / 3 |
| `POST /v1/admin/held/...` | 256 bytes (no body, or `{}`) | 2 / 2 |

With record types of at most 9 fields (or none) the snapshot key caps are
25 and 66,036. With the test fixtures' `attendance_note` (a text, two
strings and a date) the note caps are 422,442 bytes and 9 / 13 keys. The
caps follow the configured record types, so they change when a wider type
is added.

## Authentication

Every endpoint needs exactly one `Authorization: Bearer <token>` header (the
scheme in any case, one space, a token68 token of at most 8 KiB). The token
is a Kubernetes ServiceAccount token projected for the audience
`glovebox-journal` (`journalapi.Audience`). The journal asks the API server
to review it (TokenReview, through the core's own ServiceAccount, which may
create `tokenreviews` and nothing else), checks the audience, and matches
the ServiceAccount against `journal.services`, which is the allow-list. If
the API server cannot be asked, nobody is let in.

| Status | `reason` | Meaning |
|---|---|---|
| 401 | `unauthenticated` | no token, a malformed `Authorization` header, an invalid token, or one for another audience |
| 403 | `forbidden` | a valid token of a ServiceAccount with no entry in `journal.services` |
| 403 | `policy` | the policy does not allow this request (see each endpoint) |
| 503 | `auth_unavailable` | TokenReview could not be asked (fails closed) |
| 503 | `unavailable` | the journal is not ready (starting, no database, no roster), or at its in-flight limit (with `Retry-After`) |

Tokens are never logged or echoed.

## Authorization: the policy

`journal.services` maps `namespace/serviceaccount` to what that account may
do. The keys inside an entry are the journal's own (snake_case), passed to
the core as they are and decoded strictly: a misspelled key is an error at
start, never a silently narrower or wider grant.

```yaml
journal:
  services:
    <namespace>/<serviceaccount>:
      read: true                 # GET /v1/state, /v1/events
      counts: true               # GET /v1/counts
      readers: "*"               # or [e_parent1, e_parent2]
      write_types: [assignment]  # record types it may write
      write_scopes: {source: [powerschool]}
      write_subjects: roster_subjects
      admin: true                # /v1/admin/*
```

| Field | Meaning |
|---|---|
| `read` | May use `GET /v1/state` and `GET /v1/events`, as one of its `readers`. |
| `counts` | May use `GET /v1/counts`, as one of its `readers`. |
| `readers` | Whom the service may read, count or write notes as: `"*"` (any roster member) or a list of distinct member ids. Required with `read`, `counts` or a household `write_types` entry, refused without them. |
| `write_types` | Record types it may write. A source type (any owner but `household`) is written through `POST /v1/records`; a household type through `POST /v1/notes`. Every name must be a configured record type. |
| `write_scopes` | For snapshots: each listed scope part must be present in the snapshot's scope with one of the listed values. Needs a source type in `write_types`. |
| `write_subjects` | `roster_subjects`, the only mode, and required for a writer of a source type: snapshots only about subjects on the roster (`409 unknown_subject` otherwise). |
| `admin` | The operator endpoints. Nothing else grants them, and `admin` grants nothing else. |

The core refuses to start with a policy that names nobody, an entry that
grants nothing, an entry whose key is not `namespace/serviceaccount`, or any
of the combinations above. The chart also refuses an entry for the core's
own ServiceAccount.

**The gateway's `"*"`.** The openclaw gateway reads on behalf of every
member, so its entry has `readers: "*"`. `"*"` is not "anyone": it admits
any id at the policy step only. The journal then requires the reader (or
`on_behalf_of`) to be a member of the roster in use and not erased, and
computes the grants from the roster; a reader off the roster or erased is
`403 policy`. A narrower consumer, such as a dashboard for two guardians,
lists its members instead (`readers: [e_parent1, e_parent2]`).

## Readers and grants

Reads and notes name exactly one member: `reader=` on a read,
`on_behalf_of` in a note. The journal computes that member's grants --
`(subject, token)` pairs -- from the roster on every request, and a row is
visible when a grant names the row's `data_subject` and one of its
`audience` tokens, or when its audience contains `public`. For
`openclaw-household-v1`, member `m` holds `(m, subject)`; `(c, guardians)`
for each `c` it is guardian of, and `("", guardians)` if it is guardian of
anyone; `(s, siblings)` for each `s` in its `siblings`; and, if it is a
household member, `(x, household)` for every household member `x` and
`("", household)`. [journal-roster.md, section 2](journal-roster.md#2-format-openclaw-household-v1)
is the full rule. Kept notes of an erased member follow their own rule
([Erase](#post-v1adminerase)).

Rows are filtered twice, in SQL and again in Go. A row the two disagree on
is never served, and each one counts
`glovebox_journal_visibility_mismatch_total` (a critical alert).

## POST /v1/records

A connector's snapshot of one scope. Needs a source type in `write_types`;
any other service is `403 policy` before anything else, even before a
resend is looked up.

```json
{
  "snapshot_id": "0b9f6c8e-6a64-4c49-9f55-6f4cf1b3b0a1",
  "record_type": "assignment",
  "schema_version": 1,
  "scope": [["source","powerschool"],["surface","assignments"],["data_subject","e_child1"],["course","c1"]],
  "complete": true,
  "complete_range": null,
  "observed_at": "2026-10-16T13:00:41Z",
  "approximate": false,
  "records": [
    {"key": "c1:98765", "data_subject": "e_child1", "audience": ["subject", "guardians"],
     "fields": {"title": "Worksheet 3", "due": "2026-10-20", "score": 8.5}}
  ]
}
```

- `snapshot_id` is a UUID, the idempotency key. Sending the same id again
  for the same writer, record type and scope returns the stored outcome as
  `duplicate` (or `held`), even if the configuration changed since. The
  same id for another writer, type or scope is `422 snapshot_id_reused`.
- `record_type` and `schema_version` name a configured type version. A
  household type is refused here (`422 schema`; use `/v1/notes`).
- `scope` has 1 to 8 `[name, value]` parts, starts with `source`, which must
  equal the record type's `owner` (`422 schema`); names are distinct,
  values 1 to 256 bytes without control characters. The part order is part
  of the scope's identity. The canonical form is `name=value` parts joined
  by `/` (`%`, `/`, `=` escaped).
- Every record's `data_subject` equals the scope's `data_subject` part, or
  is `""` when the scope has none (only for types with `allow_no_subject`).
- `complete: true` means a live record of this scope missing from the
  snapshot has been removed; `complete_range` (`{"field", "from", "to"}`, a
  date field, `from <= to`, needs `complete`) limits that to records whose
  field lies in the range.
- `observed_at` is when the source was read; more than 10 minutes ahead of
  the journal's clock is `422 future_observed_at`.
- At most 5000 records; keys 1 to 512 bytes without control characters,
  distinct within the snapshot (`422 duplicate_key`).
- `audience`: at most 16 tokens; absent or empty takes the type's
  `default_audience`.
- Field values by type: `string` (at most 1024 bytes, no control
  characters), `text` (at most 64 KiB, content-scanned), `int` (int64),
  `number` (finite), `bool`, `date` (`YYYY-MM-DD`), `timestamp` (RFC 3339),
  `string_list` (at most 256 strings); no NUL anywhere; any field may be
  null unless `required`. Too long is `413 too_large`, anything else
  invalid `422 schema`.
- `approximate: true` marks seeded data.

`record.ValidateSnapshot` applies exactly these rules and can be called by a
connector's tests.

### Answers, and what the connector's spool does

| Code | `status` / `reason` | Meaning | Spool action |
|---|---|---|---|
| 200 | `applied`, `duplicate`, `late`, `held` | accepted | delete the file |
| 410 | `erased_subject` | the subject was erased: an erase bans all later data about the member, whatever its `observed_at` | delete the file |
| 403 | `policy` | this service may not write this type or scope | move to `spool/rejected/` |
| 413 | `too_large` | over a limit | move to `spool/rejected/` |
| 422 | `schema`, `future_observed_at`, `duplicate_key` | invalid snapshot | move to `spool/rejected/` |
| 422 | `snapshot_id_reused` | the id already identifies a snapshot of another writer, record type or scope | move to `spool/rejected/` |
| 409 | `unknown_subject` | the subject is not in the roster in use | keep, retry with backoff |
| 400 | `bad_request` | a query string, or not `application/json` | keep, retry with backoff, alert |
| 401 | `unauthenticated` | no token, a malformed header, or a token TokenReview does not vouch for with audience `glovebox-journal` | keep, retry with backoff, alert |
| 403 | `forbidden` | a valid token of a ServiceAccount with no journal policy | keep, retry with backoff, alert |
| 404 | `not_found` | no such endpoint or method | keep, retry with backoff, alert |
| 503 | `unavailable` | the journal is not ready, or at its limit of 16 concurrent requests (with `Retry-After`) | keep, retry with backoff |
| 503 | `auth_unavailable` | TokenReview could not be asked (fails closed) | keep, retry with backoff |
| 5xx | `internal` or none | try again later (also a body that could not be read) | keep, retry with backoff |

**The spool keys on `reason`, not on the status code alone.** Only the
reasons marked "move to `spool/rejected/`" are permanent: `policy`,
`too_large`, `schema`, `future_observed_at`, `duplicate_key` and
`snapshot_id_reused`. Both 403s show why: `403 policy` is permanent, but
`403 forbidden`, like `401 unauthenticated` and `400 bad_request`, is a
deployment fault (a token audience, an allow-list, a client bug) and every
`503` is a journal that is starting or busy -- those are kept, retried with
backoff and alerted on while they persist, never a reason to drop data.
`410 erased_subject` is final too, but nothing is kept: the data must not be
sent again.

A 200 body:

```json
{"status": "applied", "stats": {"created": 1, "updated": 0, "removed": 0, "restored": 0, "unchanged": 4, "stale": 0}}
```

- `late`: a snapshot of the same scope observed later was already applied;
  nothing changed (the snapshot is stored).
- `held`: the removal guard stopped it ([Held snapshots](#held-snapshots));
  `stats` say what applying would do.
- `stale` counts records whose stored state was confirmed by a later
  observation, which this snapshot does not overwrite.

## POST /v1/notes

A household note, written on behalf of a roster member. Needs a household
type in `write_types` (`403 policy` before the body is read otherwise).

```json
{"record_type": "attendance_note", "date": "2026-10-14", "data_subject": "e_child1",
 "on_behalf_of": "e_parent1", "fields": {"what_happened": "...", "heard_from": "subject"}}
```

- The body has exactly these five keys. Any other key -- `audience`,
  `grants`, `reader` -- is `400 bad_request`.
- `date` is given once, at the top level (a `date` inside `fields` is `422`).
- The audience is the type's `fixed_audience`; the key is
  `<date>:<on_behalf_of>`, so each member keeps their own note per date and
  subject, and a note belongs to its author (see [Erase](#post-v1adminerase)).
- `data_subject` may be `""` (the household) only for a type with
  `allow_no_subject`.

The checks, in order: the service may write the type and `on_behalf_of` is
in its `readers` (`403 policy`); a ban on the subject or on the author
(`410 erased_subject`); `on_behalf_of` is on the roster (`403 policy`);
`data_subject` is on the roster (`409 unknown_subject`, also when no roster
is loaded); `on_behalf_of`'s grants cover the subject for one of the fixed
audience's tokens (`403 policy`).

Answer: `{"status": "created" | "updated" | "unchanged", "key": "...", "seq": n}`
(`seq` omitted when unchanged). `409 conflict` means the stored note is newer
than the journal's clock (the clock stepped back); retry later. Notes are not
spooled: on an error the caller must say so to the person.

## GET /v1/state

`?reader=<member>[&subject=<id>][&type=<record type>][&scope=<canonical scope>][&include_removed=true|false]`

Needs `read: true` and `reader` in `readers`. `subject=` present and empty
selects the household's rows.

```json
{"rows": [{"record_type": "assignment", "data_subject": "e_child1", "key": "c1:98765",
  "scope": "source=powerschool/surface=assignments/data_subject=e_child1/course=c1",
  "audience": ["guardians", "subject"], "fields": {"title": "Worksheet 3"},
  "removed": false, "first_seen_at": "...", "last_seen_at": "...", "confirmed_at": "...",
  "in_latest_snapshot": true, "approximate": false}],
 "roster_sha256": "..."}
```

- `confirmed_at`: the `observed_at` of the snapshot that last confirmed the
  row.
- `in_latest_snapshot`: whether the newest applied snapshot of its scope
  contained it; `false` for a windowed scope means "left the window, status
  unknown", not removed. Absent for household notes.
- `approximate`: the current value came from seeding.
- `roster_sha256`: the hash of the roster that filtered these rows.

## GET /v1/events

`?reader=<member>[&after=<seq>][&subject=<id>][&type=<record type>][&min_importance=high|normal|low][&limit=1..1000]`

Needs `read: true` and `reader` in `readers`. `after` defaults to 0,
`min_importance` to `low`, `limit` to 200.

Visible events with `seq > after`, ascending, and `next`: store it and send
it as `after` next time (an empty page returns `after` unchanged). Gaps in
`seq` are normal (other readers' events, erased events).

Each event has `seq`, `record_type`, `data_subject`, `key`, `kind`
(`created`, `updated`, `removed`, `restored`), `schema_version`, `scope`,
`prev_scope` (a scope move), `audience`, `fields` (created, restored),
`changed` (`{field: {"old", "new"}}`, updated), `last_fields` (removed),
`observed_at` and `prev_observed_at` (the change happened between them),
`approximate`, `actor` (the writer; for a note
`<writer>;on_behalf_of=<member>`), `importance`, `rule`, `group_key`, and
`notify` -- the rule's advice (`token`, `mode` push/prompt/digest,
`purpose`) for the tokens this reader holds. Field names are always the
active schema version's.

Withheld values: a text value that failed the content scan reads
`"withheld: failed content scan"`; an old value from before an audience
change that the reader was not entitled to reads
`"withheld: different audience"`. There is no `prev_audience` on the wire.

## GET /v1/counts

`?reader=<member>[&subject=<id>][&since=<RFC 3339>]` (default `since`: 7
days ago). Needs `counts: true` and `reader` in `readers`.

`{"since": "...", "counts": [{"data_subject": "e_child1", "record_type": "assignment", "live": 12, "events": {"high": 1, "normal": 3}}]}`

Per subject and record type: live records and events since `since` by
importance. No values.

## GET /v1/healthz

Any caller with a policy entry, no query parameters.
`{"status": "ok", "roster_sha256": "...", "schema_version": n}`. Like every
endpoint it answers `503 unavailable` until the journal is ready. Compare
`roster_sha256` with the hash openclaw logs: equal hashes mean both use the
same roster.

## Operator endpoints

Only a service with `admin: true`. The operator check comes first: anyone
else gets `403 policy` ("this service is not an operator", logged as a
warning) whatever else is wrong with the request. Then: no query string
(`400`), the request's own checks, then the journal.

### POST /v1/admin/held/{snapshot_id}:confirm and :discard

`snapshot_id` must be a UUID and the action `confirm` or `discard` (`400`,
before the database). No body, or `{}`.

- `:confirm` applies a held snapshot without the guard. Its writer is
  checked again under today's policy, and the snapshot is validated again;
  a refusal comes back as it is (`403 policy`, `410 erased_subject`, `409
  unknown_subject`, `422 schema`) and the snapshot stays held -- discard it.
  Answer: the snapshot's status and stats, as for `POST /v1/records`.
- `:discard` answers `{"status": "discarded"}`.
- Unknown id: `404 not_found`. No longer held: `409 conflict`.

### Held snapshots

A complete snapshot is held when the scope has more than
`journal.removalGuard.minKeys` (default 3) live keys (within
`complete_range`) and the snapshot would remove all of them or more than
`journal.removalGuard.maxFraction` (default 0.3) of them. A scope with
`minKeys` live keys or fewer is never held. The connector is told `held`
and deletes its spool file; `GloveboxJournalSnapshotHeld` fires after 5
minutes.

A later snapshot of the scope that passes the guard is applied and discards
the older held ones by itself, so the alert may clear at the next poll.
Otherwise look at the held snapshot
(`SELECT id, scope, stats, payload FROM snapshot WHERE status = 'held'`;
`stats` says what applying would do), compare with the source, and confirm
or discard it.

### POST /v1/admin/erase

```json
{"data_subject": "e_child1", "confirm": "e_child1"}
```

Erase removes what the journal holds about one member and bans them. It
cannot be undone, so:

- `data_subject` must be a member id (never `""` or a kept note's
  pseudonym; `400`), and **`confirm` must repeat it exactly** -- no case
  folding, no trimming -- or nothing is erased (`400`).
- **Erase is allowed while the member is still on the roster**, and after
  they are gone. The roster in use at the start of the erase decides who
  keeps reading the kept notes (below). Remove the member from the roster
  through gitops afterwards, at any time.
- No roster loaded: `503 unavailable`.

What it does, in one transaction:

1. **Notes belong to their author.** Every note the member wrote, about
   anyone, is deleted with its events.
2. **Others' notes about the member are kept, pseudonymised.** A note
   someone else wrote about the member is kept if anyone could read it at
   erase time. Its readers are frozen: the roster members who could read it
   at that moment, plus its author if on the roster, never the erased
   member. It moves to its own pseudonymous subject (`erased:<random>:<n>`),
   and from then on only those frozen readers and its author read it;
   later roster changes add nobody. A frozen reader still has to be on the
   roster to read anything at all. A note with no reader left is deleted.
3. **Everything else of the subject** -- every snapshot (held ones
   included), record and event -- is deleted.

**Erase is a ban.** From its commit on, every snapshot about the member and
every note about or by them is refused with `410 erased_subject`, whatever
its `observed_at` -- not only data observed before the erase. Every read
naming the member as `reader` is `403 policy`, even while they are still on
the roster. Nothing un-erases. A second erase of the same id is allowed and
keeps the ban.

**Roster ids must never be reused for a different person.** The ban and the
frozen readers are keyed by member id: a new person given an erased
person's id is banned from the start, and one given a frozen reader's id
inherits that reader's access to kept notes about someone else.

**Copies outside the journal are not erased.** What was already delivered
-- events a reader fetched (the openclaw inbox, audiences, agent memory),
files in connector spools -- stays where it is. Erasing those is
glovebox-wekc.18.1. A connector spool purges a subject's files, pending and
rejected, at the first `410 erased_subject` it is answered for that subject
([Connector side: the spool](#connector-side-the-spool)); when nothing more
about the subject is sent, its rejected files stay until `max_age`.

Answer (200):

```json
{"data_subject": "e_child1", "erased_at": "...", "snapshots": 3, "record_current": 40,
 "record_events": 95, "notes_deleted_as_author": 0, "notes_pseudonymised": 2,
 "subject_unknown": false}
```

`record_current` and `record_events` count every row deleted (the subject's
and the notes the member wrote); `notes_deleted_as_author` counts those
notes on their own; `notes_pseudonymised` counts the kept notes, which are
in neither count. **Check `subject_unknown`**: it is `true` when the id was
not on the roster in use and the journal held nothing of it. The erase (a
ban) is recorded anyway, so it covers data that has not arrived yet, and the
core logs a warning -- but a mistyped id gives the same answer, and the
member you meant is then not erased.

[journal-roster.md, section 6](journal-roster.md#6-erase-and-the-roster)
covers erase from the roster's side, including the residual risk of one
admin token; [journal-schema.md](journal-schema.md#what-erase-does-with-household-notes)
covers it table by table. Erase reports no metric.

### POST /v1/admin/rescan

An optional body `{"all": true}`. Re-judges stored text values after a
scanner or rules upgrade, releasing values the scanner now passes and
withholding those it now fails. Without `all`, only values judged by
another scanner version are scanned. The scanner version is
`journal.scannerVersion` + `+rules.` + the first 12 hex digits of the loaded
rules file's SHA-256, so a rules update makes a rescan re-judge everything
by itself; bump `scannerVersion` only for a scanner or binary change.

- **Background.** The answer is `202 {"status": "started"}` at once. The
  rescan runs under the process's lifetime, not the request's: the request
  timeout and a client that goes away do not stop it, and it holds no
  in-flight slot once answered. Progress is in the core's log: `journal
  rescan started`, then `journal rescan finished` (or `failed`) with
  `current_rows`, `event_rows`, `released` and `withheld`.
- **One at a time.** While one runs, another request is `409 conflict` and
  starts nothing.
- **Batches.** Work goes in batches of 100 rows, each committed on its own;
  an interrupted or failed rescan keeps what it committed, and running it
  again continues with what is left.
- **Shutdown order.** The core stops the journal's listener first (no new
  rescan can start), then cancels the journal's context (a running rescan
  stops at its next batch, the background loops return), waits for the
  rescan and the loops, and only then closes the database. If its 10 second
  budget runs out first, the database is left open and the process exits.

Rescan reports no metric.

### Calling the admin endpoints

From a pod admitted by `journal.networkPolicy.from`, running as the
operator ServiceAccount, with a projected token for the journal's audience:

```bash
TOKEN=$(cat /var/run/secrets/journal/token)   # projected, audience glovebox-journal
curl -sS -X POST -H "Authorization: Bearer $TOKEN" \
  http://glovebox-glovebox-journal:9095/v1/admin/held/<snapshot_id>:discard
curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"data_subject": "e_child1", "confirm": "e_child1"}' \
  http://glovebox-glovebox-journal:9095/v1/admin/erase
```

Keep the operator ServiceAccount out of long-running pods: one token of it
is enough to erase and ban any member.

## Audit logs

The core logs, at INFO, through its structured logger:

- **Reads**: one `journal read` line per read that passed the policy step,
  when it is answered: `caller`, `endpoint` (`state`, `events`, `counts`),
  `reader` (or `(not a member id)` for a string that is not shaped like
  one) and `code`. No filter values, no rows.
- **Operator requests**: one `journal admin` line per request past the
  operator check: `caller`, `endpoint` (`admin_held`, `admin_erase`,
  `admin_rescan`), then `snapshot_id` and `action`, or `data_subject` (the
  erased member's id), or `all`, then `code`. A handler that panicked is
  logged at ERROR with `outcome=panic`. A refused non-operator is a WARN
  `journal admin refused` with `caller` and `endpoint` only.
- At WARN: `journal caller not allow-listed` (a `403 forbidden`, with the
  ServiceAccount's namespace and name), `journal authentication
  unavailable`, and `journal erase of an unknown member` (`subject_unknown`).

## Configuration

Chart values (`journal.*` in `charts/glovebox/values.yaml`) render the
core's `journal` config block (`internal/config/journal.go`). The chart
refuses every value the core's validator would refuse, so values that
render are values the core starts with.

| Chart value | Core key | Default | Bounds, as validated |
|---|---|---|---|
| `enabled` | `enabled` | `false` | Off renders nothing (byte-identical chart). |
| `port` | `port` | `9095` | 1-65535, not a port the core already uses. |
| `requestTimeoutSeconds` | `request_timeout_seconds` | `60` | 1-600. |
| `scanTimeoutSeconds` | `scan_timeout_seconds` | `10` | 1-300. |
| `scannerVersion` | `scanner_version` | `core-scan-1` | Non-empty. |
| `database.host` | `database.host` | `""` | Required; one host name (no `,`, `/`, `?`). |
| `database.port` | `database.port` | `5432` | 1-65535. |
| `database.name` | `database.name` | `glovebox_journal` | Non-empty. |
| `database.sslmode` | `database.sslmode` | `require` | The core accepts `require`, `verify-ca`, `verify-full` (the last two need `sslrootcert`); the chart renders only `require` until glovebox-wekc.18.2. Plaintext is refused. |
| `database.existingSecret` / `database.externalSecret.*` | `database.user_file`, `database.password_file` | -- | One of them is required. The files are `/etc/glovebox-journal-db/username` and `password`, each non-empty and at most 4096 bytes; the password is re-read for every new connection. |
| `roster.configMap`, `roster.key` | `roster.path` | `""`, `household.json` | ConfigMap required, in the release's namespace; the file is `/etc/glovebox-roster/<key>`. |
| `roster.format` | `roster.format` | `openclaw-household-v1` | The only format. |
| `roster.reloadSeconds` | `roster.reload_seconds` | `10` | 1-300. |
| `removalGuard.maxFraction` | `removal_guard.max_fraction` | `0.3` | Strictly between 0 and 1. |
| `removalGuard.minKeys` | `removal_guard.min_keys` | `3` | 0-1000. |
| `services` | `services` | `{}` | At least one caller when enabled ([policy](#authorization-the-policy)). |
| `recordTypes` | `record_types` | `[]` | Record type definitions (`name`, `schema_version`, `owner`, `fields`, `renames`, `default_audience`, `fixed_audience`, `allow_no_subject`; spec 4.7). |
| `rules` | `rules` | `[]` | Importance, grouping and notification rules (spec 4.10); unknown keys refused. |
| `staleness[].prefix` | `staleness_prefixes` | `[]` | `source=<v>/` or `source=<v>/surface=<v>/` only (it becomes a metric label), distinct; `maxAgeHours` > 0 is chart-only (the alert's window). |

Chart-only values (`rbac.create`, `networkPolicy.*`,
`database.externalSecret.*`) are described in
[upgrading.md](upgrading.md#record-journal) and in `values.yaml`.

## Metrics and alerts

Exported only when the journal is enabled (a core with it off exports no
`glovebox_journal_` series).

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `glovebox_journal_ready` | gauge | | 1 when the scanner is warm and the roster, schema and record types are in place. |
| `glovebox_journal_snapshots_total` | counter | `record_type`, `status` | Snapshots answered. |
| `glovebox_journal_events_total` | counter | `record_type`, `kind`, `importance` | Events written. |
| `glovebox_journal_scan_withheld_total` | counter | `record_type` | Stored text values whose verdict withholds them. |
| `glovebox_journal_api_requests_total` | counter | `endpoint`, `code` | Requests by route name (`records`, `notes`, `state`, `events`, `counts`, `healthz`, `admin_held`, `admin_erase`, `admin_rescan`, `unrouted`). |
| `glovebox_journal_apply_duration_seconds` | histogram | | Time to apply one snapshot. |
| `glovebox_journal_held_snapshots` | gauge | | Snapshots held by the removal guard (refreshed every minute). |
| `glovebox_journal_roster_info` | gauge | `sha256` | 1, labelled with the SHA-256 of the roster in use. |
| `glovebox_journal_roster_reload_failing` | gauge | | 1 while the latest roster reload failed (the previous roster stays in use), else 0. |
| `glovebox_journal_roster_reload_errors_total` | counter | | Failed roster reloads that were reported. Not a "still failing" signal; use the gauge. |
| `glovebox_journal_visibility_mismatch_total` | counter | | Rows the SQL filter returned that the Go check refused. |
| `glovebox_journal_last_snapshot_timestamp_seconds` | gauge | `prefix` | Newest snapshot under each `journal.staleness` prefix. |

With `metrics.prometheusRule.enabled`, the `<fullname>-journal`
PrometheusRule (aggregated by namespace; each description is its runbook):

| Alert | Fires on | For |
|---|---|---|
| `GloveboxJournalSnapshotHeld` | `glovebox_journal_held_snapshots > 0` | 5m |
| `GloveboxJournalNotReady` | `glovebox_journal_ready == 0`, or absent | 15m |
| `GloveboxJournalScanWithheld` | any increase of `glovebox_journal_scan_withheld_total` within 1h | -- |
| `GloveboxJournalRosterReloadFailing` | `glovebox_journal_roster_reload_failing == 1` | 5m |
| `GloveboxJournalVisibilityMismatch` (critical) | any increase of `glovebox_journal_visibility_mismatch_total`; stays up 24h | -- |
| `GloveboxJournalNoSnapshots` (one per `journal.staleness` entry) | no snapshot under the prefix for `maxAgeHours` | -- |

## Connector side: the spool

A connector with journal rights (Part B; only the PowerSchool connector so
far, [powerschool.md](connectors/powerschool.md#records-journal)) does not
call the journal from its poll. It writes each snapshot to a **spool** on its
own state volume and a separate loop sends them (`connector/records`, spec
4.14). Items are not affected by anything here: a snapshot that cannot be
written, sent or accepted never changes what the connector stages or its
checkpoints.

**Layout.** `<state>/spool/` (the connector's `/state`, its PVC), mode 0700,
holds one file per snapshot named `<observed_at>-<snapshot_id>.json`
(`observed_at` as `20060102T150405.000Z`, UTC, milliseconds), the snapshot's
exact request body. `<state>/spool/rejected/` holds the files the journal
refused permanently. A file is written to a temp file, fsynced, renamed into
place and the directory fsynced before the write returns; the snapshot id
is assigned then, and the file is resent byte for byte, so a resend after a
crash is answered `duplicate` (A9). On start, temp files are removed, a file
found in both directories (a crash during a move) is kept as rejected, and a
file whose name or content is not a snapshot is moved to `rejected/` and
counted (`reason="unreadable"`); a file that cannot be read at all (an I/O
error) is left in place and tried again.

**Emit is synchronous on the poll.** When a kid's turn has verified, the
poll builds that turn's snapshots and writes them to the spool before it
goes on (`Emitter.Emit`): a spool write is on the poll's path, so a slow or
full state volume slows the poll by that much. A write that fails is logged
and counted (`glovebox_journal_spool_write_errors_total`) and the poll
carries on with its items. Nothing is sent from the poll.

**Delivery runs only while the connector's `Watch` runs.** The send loop
(`Emitter.Run`, the one goroutine records add) is started by `Watch` and
stops with it; the framework restarts `Watch` with a new context and the
next loop starts only after the previous one has returned. While no loop
runs (a connector that is stopped, crash-looping, or between a `Watch`
exit and its restart) snapshots stay in the spool; the next start sends
them, oldest first. The loop runs a pass at once, whenever `Emit` writes,
when a backoff ends, and every minute (gauges and `max_age`).

**Sending.** One snapshot at a time (the journal allows 16 requests in
flight for every caller together, A14), oldest first within each scope (the
scope is the record type plus its scope parts: the journal's identity), with
the token re-read from the projected file for every request
(`/var/run/secrets/glovebox-journal/token`, audience `glovebox-journal`). The
action for each answer is the table in
[Answers, and what the connector's spool does](#answers-and-what-the-connectors-spool-does).
A `409 unknown_subject` blocks only that scope, with its own backoff; every
other kept answer stops the pass with a global backoff, at least the
answer's `Retry-After`. Backoff is exponential from `min_backoff_seconds`
(30) to `max_backoff_seconds` (3600) with +/-20% jitter, reset by any
accepted or rejected file, and held in memory only: a restart sends at once,
which is safe because every send is idempotent. A token file that is missing
or empty sends nothing and counts as a deployment fault (code `token`).

**Bounds** (both directories alike):

- `max_age_hours` (default 168, 1-2160): a file older than this (by the
  `observed_at` in its name) is deleted, sent or not -- counted
  `dropped{reason="max_age"}`, or `unknown_subject_expired` when its scope was
  last answered 409.
- `max_bytes` (default 256 MiB, 1 MiB - 4 GiB): on overflow rejected files go
  first, oldest first; then pending files that a newer snapshot of the same
  scope supersedes, oldest first (`dropped{reason="max_bytes"}`). The newest
  pending file of each scope is always kept, even over the limit (logged
  once).

**Erase (`410 erased_subject`).** The file is deleted, and so is every
pending and rejected spool file about the same `data_subject`
(`dropped{reason="erased_subject"}`); for the rest of the process's life a
new snapshot about that subject is not even written. This is the
connector's share of glovebox-wekc.18.1: the journal's erase does not reach
spools by itself, so the first 410 does it. After a restart the next
snapshot about the subject is answered 410 again and purges again.

**Logs** carry the record type, the scope prefix (`source=<v>/surface=<v>/`),
the code and the reason: never a subject id, scope value, key, field value,
file name or the token.

**Metrics** (the connector's `/metrics`, label `connector` on each):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `glovebox_journal_spool_files` | gauge | `dir` (`pending`, `rejected`) | Files in each directory. |
| `glovebox_journal_spool_bytes` | gauge | | Bytes in both directories (bounded by `max_bytes`). |
| `glovebox_journal_spool_oldest_seconds` | gauge | | Age of the oldest pending file (0 when empty). |
| `glovebox_journal_spool_answers_total` | counter | `code`, `reason`, `action` | Journal answers by HTTP code (`0`: transport, `token`: no token), bounded reason (`none`, `other` or a known one) and spool action (`delete`, `erased`, `reject`, `retry_scope`, `retry`, `retry_alert`). |
| `glovebox_journal_spool_rejected_total` | counter | `reason` | Files moved to `rejected/`. |
| `glovebox_journal_spool_dropped_total` | counter | `reason` | Files deleted unsent (`max_age`, `max_bytes`, `unknown_subject_expired`, `erased_subject`). |
| `glovebox_journal_spool_write_errors_total` | counter | | Snapshots that could not be written to the spool. |
| `glovebox_journal_record_values_cut_total` | counter | | `text` values cut to 64 KiB (decision S8). |

With `metrics.prometheusRule.enabled` and records on for an enabled
connector, the `<fullname>-connector-records` PrometheusRule (aggregated by
namespace and connector; each description is its runbook):

| Alert | Fires on | For |
|---|---|---|
| `GloveboxJournalSpoolNotEmpty` | `glovebox_journal_spool_oldest_seconds > 3600` (a persisting 503 or 409 shows here) | 5m |
| `GloveboxJournalSpoolDeploymentFault` | any `answers_total{action="retry_alert"}` within 15m (401, 403 `forbidden`, 400, 404, no token, an unknown answer) | -- |
| `GloveboxJournalSpoolRejected` | any `rejected_total` within 1h | -- |
| `GloveboxJournalSpoolDropped` | any `dropped_total` other than `erased_subject` within 1h | -- |

**Key caps (A4).** The journal caps the keys of one JSON object at the widest
configured record type's field count plus 16. With the PowerSchool types
loaded that is 17 + 16 = 33 (`assignment` is the widest), so no PowerSchool
record, which carries exactly its type's fields, comes near it.

## Known follow-ups

Open beads for the journal core (glovebox-wekc.18):

- **glovebox-wekc.18.1 -- erase fan-out.** Erase does not reach copies
  already delivered outside the journal (the openclaw inbox and audiences,
  agent memory and vaults, connector spools). Connector spools purge a
  subject's files at their first `410 erased_subject` (Part B1); nothing
  tells a connector that has nothing more to send about the subject.
- **glovebox-wekc.18.2 -- `sslmode=verify-full`.** The chart renders only
  `require` (encrypted, server certificate not verified) until it mounts
  the CNPG CA (`ca.crt` only, never `ca.key`).
- **glovebox-wekc.18.3 -- scanner cold start.** Done in substance: the core
  runs one untimed warm-up scan before the journal reports ready (Task 28),
  and the scanner tests warm up before their timed scan. What is left is to
  confirm the bead's acceptance (the first scan after a start well under
  the timeout, also on arm64, and `TestEngineScannerVerdicts` stable under
  `go test -race ./...` load) and close it.
- **glovebox-wekc.18.4 -- per-caller limits.** Only the global in-flight
  limit (16) exists: no per-caller rate limit (429 with `Retry-After`), and
  an authenticated caller sending bodies slowly holds a slot for up to the
  request timeout, so 16 such connections starve other callers. Both want a
  per-caller budget after TokenReview.
- **gitops-kxpb -- blocks enabling.** The openclaw Telegram watchdog's Role
  can exec into and delete every pod in the `openclaw` namespace, the
  glovebox core included, which would expose the journal's database
  credentials and ServiceAccount token. Fix it before `journal.enabled`.
- **The gitops enabling checklist.** Rollout steps 0 and 1 (the database
  and its role, the Vault entry and ExternalSecrets, the operator
  ServiceAccount, the roster admission policy and its tests, the API server
  addresses and the HelmRelease values) are listed in
  `docs/superpowers/plans/2026-10-16-record-journal-core.md`, "Gitops
  checklist for rollout steps 0 and 1", and generically in
  [journal-roster.md, section 7](journal-roster.md#7-enabling-checklist).
  None of it is done yet; the journal stays disabled until it is.
