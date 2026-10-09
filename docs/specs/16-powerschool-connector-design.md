# PowerSchool Connector -- Design Specification

**Version 0.2 (DRAFT) -- October 2026**

*This document specifies the PowerSchool connector for Glovebox. It reads a
parent-portal account's grades, gradebook assignments and attendance counts
per child and stages them as per-kid items with `data_subject` and `audience`
set per spec 11. It is modelled on the Schoology connector (spec 12) and
differs from it in three places that matter: the read path cannot be a pure
GET path (two POSTs are allowlisted, section 5), the portal shows one child
per session so the children are read one at a time behind a switch that is
verified before and after (section 6), and records are mutable so dedup is
by fingerprint rather than highest id. The connector ships disabled and
depends on an unreleased powerschool-go; section 11 lists what is missing.*

*The page structure this design relies on -- the explicit `li.selected`
marker, the switcher form, server-rendered grades -- was observed on a live
instance (structure only). The connector and the library code have been
tested offline against fakes and synthetic fixtures built to that shape; no
request has been made to a real PowerSchool instance by this code. Claims
about upstream behaviour are marked as observed, read from the library
source, or unverified.*

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

## 3. What powerschool-go does

Read from the library source on branch `feat/readonly-session-client`
(based on `v0.1.2`; unreleased -- section 11.1).

| Concern | Finding |
|---|---|
| Login | `Authenticate` starts a **local** headless Chrome via chromedp's exec allocator, types the username and password into the `/public/` form, clicks submit and copies the browser's cookies. There is no plain-HTTP login and no remote-browser option (nothing like schoology-go's `auth.WithBrowserURL`). |
| Session reuse | `ExportSession` / `ImportSession` / `WithSession` carry the cookies plus an expiry. A client built with a session and no credentials cannot log in: `ensureAuthenticated` returns `ErrSessionExpired`. The expiry the library records at login is a one-year placeholder, not the portal's session lifetime. |
| Page reads | By default each page read starts a headless Chrome. With `WithHTTPPageFetch` every post-login read is a plain GET through the injected `http.Client`, with each session cookie attached to the request explicitly (no cookie jar: observed live, a jar dropped the cookies and the portal answered 302). Pages are server-rendered; `home.html` contains `table#tblgrades` without JavaScript (observed live). |
| Non-GET requests | Two from Go code, both through the injected client: `POST /ws/xte/assignment/lookup` (JSON filter: section ids, student ids, date range; `Referer` required) and `POST /guardian/home.html` with the single form field `selected_student_id` (the student switch). The login form submit happens inside Chrome, in the login workload only. |
| Student selection | The portal shows one student per session. The home page marks that student explicitly (`#students-list li.selected`); `Student.Selected` is set from that marker and from nothing else, and the page-level details are attached to that student only. `GetHomePage` returns the students and the grades table from one fetch, each grade stamped with the selected student's id. `SelectStudent(id)` posts the switcher form when another student is selected, re-reads the page and fails (`ErrSwitchNotConfirmed`) unless the marker moved; with no marker it posts nothing (`ErrSelectionUnknown`). |
| Dead session | A redirect to `/public/`, a 401/403, or a 200 whose body is the sign-in form is a session-expired `*Error`, on page reads and on the lookup. |
| Errors and logs | Errors carry no response body, query string or student id; an unexpected status is an `*HTTPStatusError` (operation, status, body length). INFO lines carry no ids. |
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
- The connector receives that JSON as a file (`POWERSCHOOL_SESSION_FILE`)
  or in `POWERSCHOOL_SESSION`, and the portal location as
  `POWERSCHOOL_HOST`. It builds the library client with `WithSession` and no
  credentials.
- A missing or malformed session, or one past its `expires_at`, is a startup
  error; nothing is requested upstream.
- **A session the portal refuses is not presented twice.** When a poll gets
  an auth-class error the connector writes a marker in its state directory
  (`rejected-session`, a SHA-256 over the session's cookies -- never the
  cookies) and returns a `PermanentError`; the framework exits the process.
  The restarted process loads the same session, finds its fingerprint in
  the marker, and waits -- re-reading the session source once a minute and
  making no upstream request -- until the source holds a different session.
  The health endpoints are not up while it waits, so the pod is not ready;
  that is the operator signal. The mechanism is `connector.RejectedSession`,
  shared with the Schoology connector. A session delivered in the
  `POWERSCHOOL_SESSION` environment variable cannot change under a running
  process; there the wait lasts until the pod is replaced. Prefer the file
  form.

The operator's existing Vault secret has `powerschool_username`,
`powerschool_password` and `powerschool_hostname`. Only the hostname is
mapped into the connector. The chart comments say so.

## 5. Read-only guarantee

"Read-only" here means: the connector cannot create, edit, submit or delete
anything in the parent's account, whatever a library upgrade or a future
code path asks for. It does not mean "GET only" -- the portal does not allow
that -- so the rule is an allowlist, deny by default.

### 5.1 The transport

Every request the library makes goes through the injected client, and
through `powerschool.Transport` (`connectors/powerschool/transport.go`),
which applies, in order:

1. **Origin pin.** Only requests whose scheme is `https` and whose URL host
   is *exactly* the configured host are forwarded: no suffix or
   case-insensitive match (`evil-ps.example.org` is not `ps.example.org`),
   no userinfo, no opaque URL, no `Host`-header override. The library
   attaches the session cookie to each request itself, so this is what
   stops a redirect or a stray code path from carrying it elsewhere.
2. **No method override.** A request carrying `X-HTTP-Method-Override` (or
   `X-HTTP-Method`, `X-Method-Override`, `X-Original-Method`,
   `X-Original-URL`, `X-Rewrite-URL`) is refused, GETs included.
3. **Method gate with an allowlist.** GET and HEAD are forwarded. Any other
   request is refused by `connector.ReadOnlyTransport` unless it is one of
   the two entries in section 5.2.

The `http.Client` has no cookie jar and a `CheckRedirect` that follows a
redirect only when the next request is a GET or HEAD. A 302/303 after a
form post is therefore followed (as a GET, through the transport again); a
307/308, which would replay a POST and its body at a location the server
chose, is not.

### 5.2 The allowlist: two POSTs

The owner's rule: a POST that does not modify or mutate data may be
approved, one request at a time, by name. Two have been.

**(a) Assignment lookup** -- `POST /ws/xte/assignment/lookup`.
*Why it is read-only:* it is the query the portal's own scores page issues;
the JSON body is a filter (section ids, student ids, a date range) and the
response is the list of assignments and scores. It has no GET form.
*What is allowed:* method exactly `POST`; `URL.EscapedPath()` equal to the
literal path with `RawPath` empty (so a trailing slash, a percent-encoded
slash or letter, a doubled slash, dot segments and any change of case are
all refused, although several of those decode to the right `URL.Path`); no
fragment; a query of at most the library's numeric cache-buster `_`. Only
while `surfaces.assignments` is enabled; with it off this entry does not
exist.

**(b) Student switch** -- `POST /guardian/home.html`, body
`selected_student_id=<id>`.
*Why it is read-only:* it is the portal's own switcher --
`switchStudent(id)` sets that one hidden field of `#switch_student_form` and
submits it (observed live). It changes which child the session *displays*.
It is state on the portal's side of the session, and that is the whole
effect: no record of either child is created, changed or submitted.
*What is allowed:* the same path, method and origin hardening as (a), with
no query at all; `Content-Type` exactly
`application/x-www-form-urlencoded`, once; a body of known length that is
byte-for-byte `selected_student_id=` followed by a student id **configured
for one of the connector's kids**. The check is on the raw bytes, not on a
parsed form, so an extra field, a repeated field, an encoded separator, a
multipart body, an empty or over-long body and an id that is not configured
are all refused. The transport forwards a copy of the bytes it checked.
With no kids configured the entry does not exist.

`home.html` is also where the login form posts. A login body
(`account=...&pw=...`) is not `selected_student_id=<configured id>` and is
refused like any other POST.

**Growing this list is a decision, not an edit.** Each entry is a literal
path plus a complete description of the request; nothing is matched by
prefix, pattern or "looks like a query". The literals live in the connector
(`AssignmentLookupPath`, `SwitchStudentPath`, `SwitchStudentField`), not in
the library; at startup `CheckLibrary` refuses to run if the linked library
posts anywhere else, and a test pins the strings.

### 5.3 No browser in the read path

The library's default page reads go through a headless Chrome that the
transport never sees. `NewProductionClient` switches the library to HTTP
page fetch and refuses (`ErrBrowserReads`) a library that does not honour
the switch. In that mode nothing but the injected client talks to the
portal.

### 5.4 What the guarantee does not cover

- The login workload. It must POST credentials; it is a different process
  with a different credential set and its own review.
- Whether a GET on the portal has side effects (last-access timestamps,
  notification read-state). Unverified.
- The switch does have one observable effect outside the connector if the
  portal keeps the selection per *account* rather than per session: a
  parent using the portal at the same moment would see the other child
  selected. Unverified (section 11.2); it is also the case the end-of-turn
  check in section 6.1 exists for.

## 6. Kids, match keys and per-child visibility

Kids are opaque labels with a portal student id, as in spec 12 section 10:

```json
"kids": [{"name": "k1", "student_id": "111111"},
         {"name": "k2", "student_id": "111112", "student_number": "9000002"}]
```

`student_id` is the number in the portal's student switcher
(`javascript:switchStudent(<id>)`). The optional `student_number` is the
school-issued number the page shows as "Student ID #"; when set it is a
second, independent check (below). It identifies a real child: set it in
the deployment's private values, not in a committed file. Match keys:

- `powerschool:<kid>:grade`
- `powerschool:<kid>:assignment`
- `powerschool:<kid>:attendance`

Config validation: at startup every enabled surface of every kid must
resolve to a rule that sets `data_subject`; one kid's surfaces must agree
on it; no two kids may share it, a `student_id` or a `student_number`. A
`*` rule cannot stand in. There is no fallback destination at runtime
either: no rule, no item. Checkpoint keys include the kid label.

### 6.1 One child at a time

The portal returns the *selected* student's data whatever it is asked for.
Attaching a child's label to a page is therefore only ever justified by
that page's own explicit marker. The connector never infers the selection
from anything else -- not from which student is listed first, and not from
which student carries page details. (An earlier library version filled the
details in for the first student whenever the marker was missing, and an
earlier connector read "has details" as "is selected": on markup without
the marker, the second child's records were staged under the first child's
label. Both are fixed; this section is the rule that replaced them.)

A poll takes each configured kid in turn, in config order. Polls are
serialised, so two polls never interleave their kids. One kid's turn:

1. **Select.** `SelectStudent(kid.student_id)`: read the home page; if it
   marks another student, post the switcher form (section 5.2 b) and read
   the page again. The library fails unless the page read last marks the
   requested student.
2. **Verify.** On the page that step 1 returned, the connector checks for
   itself: exactly one student is marked, it is this kid's; every row of
   the grades table is stamped with that same student id (parsed from the
   same response); and, if `student_number` is configured, the number on
   the page equals it.
3. **Read.** That page's grades table is the kid's grades and attendance.
   If the assignments surface is on, one scores page and one lookup per
   course follow. Each scores page names its student (`studentFRN`); every
   one read in this turn must name the same student, and one that names
   none counts as a mismatch.
4. **Verify again.** Read the home page once more; it must still mark this
   kid's student. The selection lives on the portal's side, and if it moved
   while step 3 was running, part of what step 3 read may be the other
   child's. The connector does not switch back and carry on.
5. **Stage.** Only now are items written and checkpoints advanced.

Any failure in steps 1-4 discards everything read for that kid in this
poll: nothing is staged, no checkpoint moves, and the next clean poll stages
it all as if this one had not happened.

What each failure does to the rest of the poll:

| Failure | `error_class` / skip reason | This kid | Remaining kids |
|---|---|---|---|
| Page carries no selection marker (or two) | `selection_unknown` | skipped | skipped unread; nothing is posted |
| Home page unreadable (network, parse, HTTP status) | `network`, `parse`, `http_status`, ... | skipped | skipped unread |
| Session refused | `auth` | skipped | poll ends; section 4 |
| Student id not in the account's switcher | `not_found` / `unknown_student` | skipped | continue |
| Switch posted but not confirmed | `switch_not_confirmed` | skipped | continue |
| Marker, grade rows or scores pages disagree | `selection_mismatch` | discarded | continue |
| Configured `student_number` is not the page's | `student_number_mismatch` | skipped | continue |

Requests per poll, for two kids with *n* courses each and assignments on:
between 5 and 6 home-page GETs, at most 2 switch POSTs (each followed by
one redirect GET), and 2*n* scores GETs and 2*n* lookup POSTs.

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

Windows are local `HH:MM` ranges in `POWERSCHOOL_TIMEZONE` (default
`America/Los_Angeles`), optionally weekdays only. The default is a single
late-afternoon window, i.e. one poll a day. There is no trigger endpoint.

**Exactly one scheduled poll per window.** `Watch` picks one random time
inside the next window, sleeps until then, and polls. It then schedules the
following poll from the *end* of the window it just polled, not from the
moment the poll fired; scheduling from the fire time re-rolls inside the
same window and polls it again most of the time. The end of the last polled
window (`notBefore`) is kept on the connector, so it survives the framework
restarting `Watch`.

**The framework's own polling is neutralised.** The framework calls `Poll`
at startup and again on `PollInterval`, and restarts `Watch` each time.
`main.go` sets `PollInterval` to 30 days, and `Poll` is a no-op after its
first call in a process.

**A restart is not a reason to poll.** Every poll records its start time in
the checkpoint (`meta:last_poll`) before its first request. On the first
`Poll` of a process:

- no recorded poll (first ever start): poll now;
- a window has opened since the recorded poll (the process was down through
  it, or has just started inside a window that has not been polled yet):
  poll now, once -- this is the only poll that can happen outside a window;
- otherwise: do not poll; wait for the schedule. If the recorded poll was in
  a window that is still open, that window is treated as done.

So the number of portal polls is the number of windows that open while the
deployment exists, plus at most one late catch-up per window missed --
independent of how often the pod restarts. A simulated week under many
seeds, with and without repeated restarts, asserts exactly one poll per
window and three per weekday for a three-window config
(`scheduler_window_test.go`).

A failed poll is not retried inside its window; it is retried at the next
one. Within a poll, per-course requests are spaced by
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

- Auth-class errors (session expired or rejected) at any step stop the poll,
  mark the session rejected and return a `PermanentError`; the framework
  exits the process and the restart waits for a new session (section 4).
- Anything else is logged, counted and retried at the next window. A course
  whose lookup fails is skipped; the rest of the kid's turn continues.
  Selection failures are in the table in section 6.1.
- An item the staging backend refuses is not checkpointed; it is staged
  again, still as `new`, at the next poll.
- A poll that stages from no kid at all is a failed poll.
  `failure_threshold` (default 5) consecutive failed polls is a
  `PermanentError`, so a silently useless deployment becomes visible. The
  restart does not poll (section 8), so this is not a request loop.
- A request refused by the transport is classified `write_blocked` or
  `host_blocked` and logged at ERROR by the transport (method, host and
  path; never the query).

**What is logged about an upstream error.** Never its text. A library or
transport error can carry the request URL -- whose query names a student's
enrolment and school (`frn`, `schoolid`) -- or quote the response, which is
a child's record; the pod log is read by people and agents who are not
entitled to that record. Log lines and the `PermanentError` handed to the
framework carry `error_class` (a fixed vocabulary) and `error_detail`, the
error chain rendered as kinds only: library error codes, HTTP status and
body length, known sentinel names, Go type names
(`powerschool-go:NETWORK_ERROR > url:Get > http:503(1832b)`). Kids are
named by their opaque label; student ids and student numbers are not
logged. The library applies the same rule to its own errors and INFO lines,
and the connector runs it at WARN.

Metrics, on top of the framework's: `powerschool_polls_total{trigger_source}`,
`powerschool_items_total{surface,kid,change}`,
`powerschool_surface_errors_total{surface,error_class,kid}`,
`powerschool_kids_skipped_total{kid,reason}`. They are registered on the
framework's own meter provider, the one behind `/metrics`. Spans:
`powerschool.poll`, `powerschool.poll.kid`, `powerschool.lib.SelectStudent`,
`powerschool.lib.GetHomePage`, `powerschool.lib.GetAssignments`,
`powerschool.staging.commit`.

## 11. Library gaps and open questions

### 11.1 Required before the connector can run

1. **A powerschool-go release** containing branch
   `feat/readonly-session-client` (HTTP page fetch, explicit selection,
   `GetHomePage`, `SelectStudent`, typed login-page and status errors).
   Until then `go.mod` replaces the module with a pseudo-version served
   from the private GitLab, which CI and the image build cannot fetch.
   After the release: drop the `replace`, require the tag.
2. **A login workload** that produces the session (section 4). It needs
   Chrome in its own image; a remote-browser option in the library would
   let it reuse the cluster's Browserless.
3. **One supervised live run** (section 11.2) before the connector is
   enabled against the real account.

### 11.2 Needs a live login to settle

Observed live already (structure only): the `li.selected` marker and the
`<li >` shape of unselected items; the switcher form and its script;
`tblgrades` in the raw `home.html`; that cookies must be attached per
request. Not yet exercised or observed:

- **The switch, end to end.** The form post's status and `Location`;
  whether it sets or rotates a cookie (the library ignores `Set-Cookie`, so
  a rotated session cookie would read as an expired session on the next
  request); that the page read afterwards carries the marker on the
  requested student.
- **Whether the selection is per session or per account**, and which
  student a fresh session starts on. If per account, a parent using the
  portal can move the connector's selection mid-turn (handled: section 6.1
  step 4) and the connector moves theirs (not avoidable while switching).
- **The scores page.** That `data-sectionid` and `studentFRN` are in the raw
  `scores.html` response; that `studentFRN` differs between the two
  children (the per-turn invariant depends on it); whether the page also
  carries the `li.selected` marker.
- **Single-child accounts.** Whether the page has a `#students-list` and
  marks its one item. Without a marker the connector reads nothing.
- **A dead session.** 302 to `/public/`, a 200 sign-in page at the same URL
  or a 401 -- all three are handled, none has been seen. The sign-in page
  is recognised by its form fields on a page with no switcher; a
  `scores.html` request with incomplete parameters gets the same answer and
  cannot be told apart from a dead session, so a library bug there would
  read as a rejected session.
- **Real session lifetime.** The recorded expiry is a placeholder; expiry
  is only discovered when a read is refused.
- What a grade cell contains beyond `letter<br>percent` on this district's
  instance (standards-based marks, `--`, `[ i ]`).
- Whether the lookup response distinguishes "may still be turned in" from
  "closed", and whether there are score comments.
- Whether the district blocks or rate-limits a non-browser client.
- Whether the absence and tardy columns sit where the library expects (it
  reads fixed column positions). `/guardian/attendance.html`,
  `/guardian/termgrades.html` and `/guardian/teachercomments.html` exist
  (observed) and are not read; dated attendance would come from the first.

## 12. Chart

`connectors.powerschool` in the generic map: Deployment, ConfigMap, state
PVC and, when `externalSecret.enabled`, the generic ExternalSecret mapping
`POWERSCHOOL_HOST` and `POWERSCHOOL_SESSION`. Remote keys and properties are
values; the username and password are not mapped. Disabled by default.
