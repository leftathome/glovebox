# PowerSchool Connector -- Design Specification

**Version 0.1 (DRAFT) -- October 2026**

*This document specifies the PowerSchool connector for Glovebox. It reads a
parent-portal account's grades, gradebook assignments and attendance counts
per child and stages them as per-kid items with `data_subject` and `audience`
set per spec 11. It is modelled on the Schoology connector (spec 12) and
differs from it in three places that matter: the read path cannot be a pure
GET path, the library cannot yet select a child, and records are mutable so
dedup is by fingerprint rather than highest id. The connector ships disabled
and does not run against the currently pinned library; section 11 lists what
is missing.*

*Everything here was designed and tested offline against fakes and synthetic
fixtures. No request has been made to a real PowerSchool instance. Claims
about upstream behaviour are marked as read from the library source or as
unverified.*

---

## 1. Purpose

A school district's PowerSchool parent portal is where grades, the teacher's
gradebook (each assignment's due date, score, and missing / late / incomplete
flags) and attendance live. The household's agents should be able to answer
"what is due tomorrow", "what is missing", "what changed in the gradebook"
and "was there an absence" for each child, with each child's records visible
only to that child and their guardians.

The credential is a parent's own portal account. That account can do things
(update contact details, submit forms) that an unattended process must never
do, so the connector is read-only by construction (section 5).

## 2. Scope

In scope: three per-kid surfaces -- `grade`, `assignment`, `attendance` --
from one parent account; a daily windowed poll; per-record change detection;
a Helm values block (generic `connectors:` map), disabled by default.

Out of scope: logging in (section 4); any write; teacher comments, GPA,
grade history, progress-report PDFs (stubs or heuristics in the library,
section 3); dated attendance records (the library has none); parse-failure
receipts and an HTTP trigger endpoint as in spec 12 (not ported; failures are
logs and metrics here).

## 3. What powerschool-go v0.1.0 does

Read from the library source at tag `v0.1.0` (`go build`, `go vet` and
`go test ./...` pass there).

| Concern | Finding |
|---|---|
| Login | `Authenticate` starts a **local** headless Chrome via chromedp's exec allocator, types the username and password into the `/public/` form, clicks submit and copies the browser's cookies. There is no plain-HTTP login and no remote-browser option (nothing like schoology-go's `auth.WithBrowserURL`). |
| Session reuse | `ExportSession` / `ImportSession` / `WithSession` carry the cookies plus an expiry. A client built with a session and no credentials cannot log in: `ensureAuthenticated` returns `ErrSessionExpired`. |
| Client injection | `WithHTTPClient` exists, but only **one** call uses the injected client: the assignment lookup. |
| Page reads | `GetStudents`, `GetGrades`, `GetGPA`, `GetGradeHistory`, `FetchScoresMetadata` (called by `GetAssignments`), `GetProgressReports` and `SwitchStudent` each start a fresh headless Chrome, set the cookies and read the rendered DOM. None of that traffic passes through the injected client. |
| Non-GET requests | Exactly one from Go code: `POST /ws/xte/assignment/lookup` (JSON filter: section ids, student ids, date range; `Referer` required). The login form submit happens inside Chrome. Chrome also issues whatever the portal's own JavaScript issues on each page it renders. |
| Student selection | `GetGrades(ctx, studentID)` ignores `studentID`; it returns the rows for whichever student the session's home page shows. `SwitchStudent` loads the home page and returns -- the switch itself is a TODO. |
| Real surfaces | Students (ids and names; details for the selected one), the home-page grades table (per-term grades, per-course absence and tardy totals, scores-page URL), assignments (title, due date, category, points, score, percent, letter, flags). |
| Stubs / heuristics | GPA (text heuristic), grade history (placeholder column guess), progress reports (placeholder; hard-coded `psplugin.example.org` host), `DownloadProgressReport` (returns "not yet implemented"). Attendance and calendar are declared as types only. |

Two details of the assignment mapping the connector has to live with: a
score of zero leaves `Assignment.Score` nil (the library tests `> 0`), and a
late assignment keeps status `pending` or `graded` with only the flag set.

## 4. Authentication: where login happens

The connector never logs in and never holds the username or password.

- A separate **login workload** (not part of this change; the analogue of
  `schoology-auth-refresher`) holds the credentials, runs the browser login,
  calls `ExportSession` and stores the resulting JSON
  (`{"cookies":[...],"expires_at":"..."}`) where ESO can sync it.
- The connector receives that JSON as `POWERSCHOOL_SESSION` (or a file path
  in `POWERSCHOOL_SESSION_FILE`) and the portal location as
  `POWERSCHOOL_HOST`. It builds the library client with `WithSession` and no
  credentials.
- A missing, malformed or expired session is a startup error. A session that
  expires mid-run surfaces as an auth-class error and the process exits
  (`PermanentError`); the restarted container picks up whatever session the
  Secret then holds.

The operator's existing Vault secret has `powerschool_username`,
`powerschool_password` and `powerschool_hostname`. Only the hostname is
mapped into the connector. The chart comments say so.

## 5. Read-only guarantee

### 5.1 The transport

Every request the library makes through the injected client goes through
`powerschool.Transport` (`connectors/powerschool/transport.go`):

1. **Host pin.** Only `https` requests to the configured host are forwarded.
   The library attaches the session cookie to each request itself, so this
   is what stops a redirect or a stray code path from carrying it elsewhere.
2. **Method gate.** Everything else is delegated to
   `connector.ReadOnlyTransport`, which refuses any method but GET/HEAD
   before the request leaves the process.

### 5.2 The one exception

PowerSchool serves assignment scores only from
`POST /ws/xte/assignment/lookup`. It is a query -- the body is a filter and
nothing upstream changes -- but it is a POST, and a strict GET/HEAD gate
would make the assignments surface impossible.

The transport therefore forwards a POST whose path is **exactly** that
string, on the pinned host, and only when the assignments surface is enabled
in config. Any other POST, the same path with a prefix or suffix, and
PUT/PATCH/DELETE to that path are refused. With `surfaces.assignments:
false` the transport is strictly GET/HEAD.

This is a deliberate narrowing of "refuses non-GET/HEAD", not an equivalent
of it. What makes it acceptable is that the allowlist is one literal path
with read semantics; what would make it unacceptable is growing it. In
particular the student switch (section 11) must not be added here without a
decision: it is a POST that changes server-side session state.

### 5.3 Page reads bypass the transport in v0.1.0

Per section 3, v0.1.0 reads pages through Chrome, which the transport never
sees. A connector built on it would have an injected read-only client that
guards one request and a browser that guards none. That is not a read-only
guarantee, so the connector refuses it: `NewProductionClient` probes the
library for an HTTP page-fetch mode and returns `ErrBrowserReads` when it is
absent. Against v0.1.0 the binary logs that error and exits at startup.

The mode is proposed in powerschool-go branch `fix/http-page-fetch`
(`WithHTTPPageFetch`, `SetHTTPPageFetch`, `UsesHTTPPageFetch`): page reads
become plain GETs through the injected client, browser-only calls return an
error instead of starting Chrome, and a redirect to `/public/` is reported
as `ErrSessionExpired` instead of being parsed as an empty page. The probe
is an interface assertion so this repository builds against v0.1.0 today and
starts working when `go.mod` is bumped to a release containing the branch.

### 5.4 What the guarantee does not cover

- The login workload. It must POST; it is a different process with a
  different credential set and its own review.
- Whether a GET on the portal has side effects (last-access timestamps,
  notification read-state). Unverified.

## 6. Kids, match keys and per-child visibility

Kids are opaque labels with a portal student id, as in spec 12 section 10:

```json
"kids": [{"name": "k1", "student_id": "111111"}, {"name": "k2", "student_id": "111112"}]
```

`student_id` is the number in the portal's student switcher
(`javascript:switchStudent(<id>)`). Match keys:

- `powerschool:<kid>:grade`
- `powerschool:<kid>:assignment`
- `powerschool:<kid>:attendance`

Three mechanisms keep one child's records from carrying another child's
label:

1. **Config validation.** At startup every enabled surface of every kid must
   resolve to a rule that sets `data_subject`; one kid's surfaces must agree
   on it; no two kids may share it. A `*` rule cannot stand in. There is no
   fallback destination at runtime either: no rule, no item.
2. **Selection check.** Because the library returns the *selected* student's
   data whatever id it is asked for, each poll first lists the students and
   determines which one the session is showing (the library fills in
   `StudentNumber` for exactly that one). A kid is read only if its
   `student_id` equals that id. Zero or several candidates means unknown,
   and the poll stages nothing.
3. **Per-kid checkpoint scope.** Checkpoint keys include the kid label.

Consequence, until the library can switch students: a two-child account
polls only the child the session opens on. The other is skipped every poll
with reason `not_selected`. This is the correct failure -- no data rather
than mislabelled data -- and it is the main reason the connector is not yet
useful for a two-child household.

## 7. Surfaces and item shape

Common to all items: `source: powerschool`, `sender` = course name,
`content_type: text/plain`, tier `personal`, and tags `kid`, `surface`,
`change` (`new` | `updated`), `course`, `period`. Facts that are unknown are
absent keys, never empty strings. The body repeats the same facts as
readable lines; the tags are the contract for a deterministic summariser.

### 7.1 `grade` -- one item per course

| tag | meaning |
|---|---|
| `term` | the latest term with a posted grade (`Q4`, `S2`, `Q3`, `Q2`, `S1`, `Q1` in that priority) |
| `grade` | that term's cell as the library returns it (e.g. `B+88`) |
| `grade_letter`, `grade_percent` | the cell split, when it has that shape |
| `grade_q1` ... `grade_s2` | every posted term |
| `previous_grade` | the current grade at the last emission, on `updated` |

A course with no posted grade stages nothing.

### 7.2 `assignment` -- one item per gradebook assignment

| tag | meaning |
|---|---|
| `assignment_id`, `category` | from the gradebook |
| `due_day` | calendar date, `2006-01-02` |
| `due_date` | RFC 3339, start of that day in the connector timezone |
| `status` | the library's status: `pending`, `graded`, `missing`, `incomplete`, `collected`, `absent`, `exempt` |
| `missing`, `late`, `incomplete`, `exempt`, `collected`, `absent` | each always present as `true` / `false` |
| `flags` | the true flags, comma-joined; absent when none |
| `scored` | a score has been entered |
| `outstanding` | still wants work: missing or incomplete, or neither scored nor collected; never when exempt |
| `score`, `max_score`, `percent`, `letter_grade` | when present |

Counting without parsing prose: *missing* is `missing=true`; *due tomorrow*
is `outstanding=true` and `due_day` = tomorrow; *past due* is
`outstanding=true` and `due_day` < today. "Past due" is not a tag because it
changes with the calendar, not with the record, and would never be
re-emitted.

Two limits a consumer must know. PowerSchool is a gradebook, not a
submission system: work handed in but not yet marked `collected` or scored
reads as `outstanding`. And nothing in the lookup response says whether a
missing assignment may still be made up or re-submitted; `missing` and
`incomplete` are the nearest signals.

**First-sight backlog.** An assignment seen for the first time that was due
more than `assignments.lookback_days` (default 30) ago and carries none of
missing / late / incomplete is checkpointed without being staged. If it
later changes, it stages. This keeps a mid-year enable from staging the
whole year.

### 7.3 `attendance` -- one item per course whose counts moved

| tag | meaning |
|---|---|
| `absences`, `tardies` | year-to-date totals for the course |
| `absences_delta`, `tardies_delta` | change since the last emission (may be negative: a correction) |

A course never seen is compared against zero, so a clean record stages
nothing. The library exposes no dated records, so this surface says "the
count moved", not "absent on the 3rd".

## 8. Poll schedule

The windowed scheduler is the Schoology one (spec 12 section 6.1): local
`HH:MM` windows in `POWERSCHOOL_TIMEZONE` (default `America/Los_Angeles`),
one random time per window, optional weekdays-only. The default is a single
late-afternoon window, i.e. one poll a day. There is no trigger endpoint.

The framework also polls once at startup and on its own re-poll interval;
`main.go` sets that interval to 24 hours so the portal is not read every
five minutes (the framework default).

Within a poll, per-course requests are spaced by
`assignments.course_delay_ms` (default 500).

## 9. Change detection and checkpointing

Schoology items are append-only and dedup on a highest-seen id. PowerSchool
records are edited in place, so each record has its own checkpoint entry,
`<surface>:<kid>:<record>`, holding a fingerprint:

- grade: every term's cell, readable (`Q1=B+88|Q2=|...`), so the previous
  grade can be reported;
- attendance: `absences=N|tardies=M`;
- assignment: a hash over title, due date, category, status, flags, score,
  points possible, percent and letter. The description is not included.

An unchanged fingerprint stages nothing; a new or different one stages one
item and is saved after `Commit()`. A failed save is logged with
`duplicate_risk` and re-stages next poll. Entries are never pruned; they
accumulate across school years (hundreds of short strings per child per
year).

State must persist across restarts (a PVC in the chart): losing it re-stages
every current grade and everything inside the lookback window.

## 10. Errors, escalation, telemetry

- Auth-class errors (session expired or rejected) at any step stop the poll
  and return a `PermanentError`; the framework exits the process.
- Anything else is logged, counted and retried at the next window. A course
  whose lookup fails is skipped; the rest of the poll continues.
- A poll that reads no kid at all (listing failed, selection unknown, no
  configured kid selected, grades page failed) is a failed poll.
  `failure_threshold` (default 5) consecutive failed polls is a
  `PermanentError`, so a silently useless deployment becomes a visible
  crash loop.
- A request refused by the transport is classified `write_blocked` or
  `host_blocked` and logged at ERROR by the transport.

Metrics, on top of the framework's: `powerschool_polls_total{trigger_source}`,
`powerschool_items_total{surface,kid,change}`,
`powerschool_surface_errors_total{surface,error_class,kid}`,
`powerschool_kids_skipped_total{kid,reason}`. Spans: `powerschool.poll`,
`powerschool.poll.kid`, `powerschool.lib.GetAssignments`,
`powerschool.staging.commit`.

## 11. Library gaps and open questions

### 11.1 Required before the connector can run

1. **HTTP page fetch** (section 5.3). Proposed in powerschool-go
   `fix/http-page-fetch`; needs review, a live check and a release, then a
   `go.mod` bump here.
2. **Student switching.** Without it only the session's default child is
   read (section 6). Needs a live look at what `switchStudent(id)` does on
   the wire, then a decision on how that fits section 5.2.
3. **A login workload** that produces the session (section 4). With v0.1.0
   it would need Chrome in its own image; a remote-browser option in the
   library would let it reuse the cluster's Browserless.

### 11.2 Needs a live login to answer

- Is `table#tblgrades` present in the raw `home.html` response, or built by
  JavaScript? If the latter, HTTP page fetch returns no courses and grades
  need a different source.
- Are `data-sectionid` and `studentFRN` in the raw `scores.html` response?
  (A comment in the library says yes; not re-checked.)
- What does `switchStudent(id)` send -- a form POST to `home.html`, a GET
  with a parameter, something else -- and is the selection per session or
  per account?
- How long does a session really last? The library takes the latest cookie
  expiry, or four hours when it finds none; its own notes say 24 hours.
- Does an expired session answer with a 302 to `/public/` (what the
  proposed patch detects), a 200 login page at the same URL, or a 401?
- What does a grade cell contain beyond `letter<br>percent` on this
  district's instance (standards-based marks, `--`, `[ i ]`)?
- Does the lookup response carry anything that distinguishes "may still be
  turned in" from "closed"? Are there score comments?
- Does the district block or rate-limit a non-browser client, and does the
  login page sit behind SSO or a CAPTCHA?
- Is there a dated attendance page worth scraping for absences by day?
- Do the absence and tardy columns sit where the library expects on this
  instance (it reads fixed column positions)?

## 12. Chart

`connectors.powerschool` in the generic map: Deployment, ConfigMap, state
PVC and, when `externalSecret.enabled`, the generic ExternalSecret mapping
`POWERSCHOOL_HOST` and `POWERSCHOOL_SESSION`. Remote keys and properties are
values; the username and password are not mapped. Disabled by default.
