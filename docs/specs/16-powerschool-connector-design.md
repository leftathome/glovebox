# PowerSchool Connector -- Design Specification

**Version 0.3 (DRAFT) -- October 2026**

*This document specifies the PowerSchool connector for Glovebox. It reads a
parent-portal account's grades, gradebook assignments and attendance counts
per child and stages them as per-kid items with `data_subject` and `audience`
set per spec 11. It is modelled on the Schoology connector (spec 12) and
differs from it in three places that matter: the read path cannot be a pure
GET path (two POSTs are allowlisted, section 5), the portal shows one child
per session so the children are read one at a time behind a switch that is
verified before and after (section 6), and records are mutable so dedup is
by fingerprint rather than highest id. The connector ships disabled;
section 11 lists what is missing before it can be enabled.*

*The upstream behaviour this design relies on -- the explicit
`li.selected` marker, the switcher form and that it works, server-rendered
pages, per-session selection, the scores page following the session's
selection, a second login ending the first session -- was verified against
a live instance with the library (section 11.2). The connector itself has
only been run offline, against fakes and synthetic fixtures built to that
shape. Claims about upstream behaviour are marked as observed, read from
the library source, or unverified.*

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

In scope: six per-kid surfaces from one parent account -- `grade`,
`assignment` and `attendance` (on by default), and `attendance_day`,
`term_grade` and `teacher_comment` (each from a further portal page, off by
default; section 7.4); a windowed poll; per-record change detection; a Helm
values block (generic `connectors:` map), disabled by default.

Out of scope: logging in (section 4); any write; GPA and progress-report
PDFs (a heuristic and an unimplemented stub in the library, section 3);
parse-failure receipts and an HTTP trigger endpoint as in spec 12 (not
ported; failures are logs and metrics here).

## 3. What powerschool-go does

Read from the library source on branch `feat/attendance-termgrades-comments`
(`v0.2.0` plus the three page parsers; unreleased -- section 11.1).

| Concern | Finding |
|---|---|
| Login | `Authenticate` starts a **local** headless Chrome via chromedp's exec allocator, types the username and password into the `/public/` form, clicks submit and copies the browser's cookies. There is no plain-HTTP login and no remote-browser option (nothing like schoology-go's `auth.WithBrowserURL`). |
| Session reuse | `ExportSession` / `ImportSession` / `WithSession` carry the cookies plus an expiry. A client built with a session and no credentials cannot log in: `ensureAuthenticated` returns `ErrSessionExpired`. The expiry the library records at login is a one-year placeholder, not the portal's session lifetime. |
| Page reads | By default each page read starts a headless Chrome. With `WithHTTPPageFetch` every post-login read is a plain GET through the injected `http.Client`, with each session cookie attached to the request explicitly (no cookie jar: observed live, a jar dropped the cookies and the portal answered 302). Pages are server-rendered; `home.html` contains `table#tblgrades` without JavaScript (observed live). |
| Non-GET requests | Two from Go code, both through the injected client: `POST /ws/xte/assignment/lookup` (JSON filter: section ids, student ids, date range; `Referer` required) and `POST /guardian/home.html` with the single form field `selected_student_id` (the student switch). The login form submit happens inside Chrome, in the login workload only. |
| Student selection | The portal shows one student per session. The home page marks that student explicitly (`#students-list li.selected`); `Student.Selected` is set from that marker and from nothing else, and the page-level details are attached to that student only. `GetHomePage` returns the students and the grades table from one fetch, each grade stamped with the selected student's id. `SelectStudent(id)` posts the switcher form when another student is selected, re-reads the page and fails (`ErrSwitchNotConfirmed`) unless the marker moved; with no marker it posts nothing (`ErrSelectionUnknown`). |
| Dead session | A redirect whose `Location` is under `/public/`, a 401/403, or a 200 whose body is the sign-in form is a session-expired `*Error`, on page reads and on the lookup. Any other redirect is not. |
| Errors and logs | Errors carry no response body, query string or student id; an unexpected status is an `*HTTPStatusError` (operation, status, body length). INFO lines carry no ids. |
| Real surfaces | Students (ids and names; details for the selected one), the home-page grades table (per-term grades, per-course absence and tardy totals, scores-page URL), assignments (title, due date, category, points, score, percent, letter, flags). |
| More pages | `GetAttendance` (the per-course, per-day grid of `attendance.html`, with dates, codes and the legend as `Code` / `Category` / `Description`), `GetGradeHistoryPage` (`termgrades.html`: stored grades and credit), `GetTeacherComments` (`teachercomments.html?fg=<term>`; the page must mark the requested term). Each returns `PageStudent` with the id that page's own switcher marks, and fails rather than return a page that marks nobody. Unreleased (section 11.1); structure verified live, code run on fixtures. |
| Stubs / heuristics | GPA (text heuristic). Progress reports return `ErrNotImplemented`. Calendar is declared as types only. |

Two details of the assignment mapping the connector has to live with: a
score of zero leaves `Assignment.Score` nil (the library tests `> 0`), and a
late assignment keeps status `pending` or `graded` with only the flag set.

## 4. Session lifecycle: where login happens, and what ends a session

**The connector never logs in.** It never holds the username or password,
has no code path that can authenticate, and its transport refuses a
credentialed POST (section 5).

- A separate, scheduled **login job** (`powerschool-auth-refresher`, the
  analogue of `schoology-auth-refresher`; see section 4.4) holds the
  credentials, runs the browser login, calls `ExportSession` and stores the
  resulting JSON (`{"cookies":[...],"expires_at":"..."}`) where ESO can sync
  it.
- The connector receives that JSON as a mounted file
  (`POWERSCHOOL_SESSION_FILE`, preferred; this is what the chart sets up)
  or in `POWERSCHOOL_SESSION`, and the portal location as
  `POWERSCHOOL_HOST`. It builds the library client with `WithSession` and no
  credentials.
- A missing or malformed session, or one past its `expires_at`, is a startup
  error; nothing is requested upstream.

### 4.1 One account, one session

The connector uses the owner's own parent account; there is no separate
guardian account for it. Observed live (once): **a second login to the
account ended the first session** -- the first session's next request got
the sign-in page. The design treats "one live session per account" as the
rule. Consequences:

- **The owner signing in ends the connector's session.** This is expected,
  not a fault. The owner signs in rarely, and when he does it is between
  16:00 and 21:00 Pacific.
- **A login by the job signs the owner out.** The login job must therefore
  **never run between 16:00 and 21:00 Pacific**. It runs off-hours, and no
  more often than the session's real lifetime requires (measured below).
- **The connector stays out of the way.** The default poll windows are three
  weekday windows outside that span -- 06:00-06:30, 15:00-15:30,
  21:30-22:00 `America/Los_Angeles` -- so a poll does not find a session the
  owner's evening login has just ended, and does not run while he is using
  the portal.
- **A session rejected mid-day costs data only until the next off-hours
  login.** Nothing is lost: records are fingerprinted, and the first poll on
  the new session stages whatever changed in between. It does not page
  anyone and must not trigger a login.

Selection, by contrast, is per session (observed live): the connector
switching students does not change what the owner sees in his own session.

### 4.2 A rejected session

When a poll gets an auth-class error (a redirect to the sign-in area, a
401/403, or the sign-in page served with a 200):

1. The poll stops. Nothing read for the kid in progress is staged.
2. The session's age is recorded (4.3), and a marker is written to the
   state directory (`rejected-session`: a SHA-256 over the session's
   cookies, never the cookies).
3. The poll returns a `PermanentError`; the framework exits the process.
4. The restarted process loads the same session, finds its fingerprint in
   the marker, and **waits, making zero upstream requests**: it re-reads the
   session file once a minute until it holds a different session. Because
   the session is a mounted Secret, the kubelet updates the file in place
   when the login job writes a new one, and the wait ends **without a pod
   restart**.
5. While it waits, the binary serves `/healthz` (200) and `/readyz` (503)
   on the health port itself -- the framework's health server only starts
   with `connector.Run` -- so the pod sits **Running, NotReady** rather
   than being killed by its liveness probe into `CrashLoopBackOff`. When
   the wait ends that listener is closed before the framework binds the
   port.

The mechanism (`connector.RejectedSession`) is shared with the Schoology
connector. A session delivered in the `POWERSCHOOL_SESSION` environment
variable cannot change under a running process; there the wait lasts until
the pod is replaced.

### 4.3 Measuring the real session lifetime

The expiry the library records at login is a one-year placeholder, so the
portal's real session lifetime is unknown. The connector measures it: it
keeps, in the state directory, when it first loaded the current session
(`session-first-seen`, keyed by the session fingerprint), and at the moment
that session is first rejected it emits the age

- in the gauge `powerschool_rejected_session_age_seconds`, and
- in one INFO line, `powerschool session rejected: age since this connector
  first loaded it`, with `session_age_seconds` and `session_age_hours` and
  no identifier of any kind.

The process exits right after a rejection, usually before a scrape, so the
age is also saved (`session-rejected-age`) and the next process exports the
same gauge at startup. The figure is time since the connector first loaded
the session -- slightly less than time since login -- and it conflates the
two ways a session ends (idle or absolute timeout, and the owner signing
in); a run of values clustered at one duration is the timeout.

The operator's existing Vault secret has `powerschool_username`,
`powerschool_password` and `powerschool_hostname`. Only the hostname is
mapped into the connector. The chart comments say so.

### 4.4 The login job

`connectors/powerschool-auth-refresher` (bead `glovebox-wekc.7`), deployed
by the chart's `powerschoolAuthRefresher` block. Operator runbook:
`docs/connectors/powerschool-auth-refresher.md`. It follows
`schoology-auth-refresher` (spec 06 section 12) in layout, Vault wiring
(Kubernetes auth, KV v2) and exit codes 0-4. What section 4.1 adds:

- **Blackout in the binary.** No login between 16:00 and 21:00
  `America/Los_Angeles`, evaluated against the real clock in that zone at
  start and again immediately before each login; such a run exits 0 having
  contacted nothing. A login may take up to 90s, so one is started only when
  the next 150s are clear of the window. The window is a constant:
  configuration can widen it, never shorten it or change its zone. The
  chart additionally refuses to render a schedule that can fire inside the
  window or within `startingDeadlineSeconds` plus three minutes before it.
  The schedule is a convenience; the binary is the guarantee.
- **A latch against lockout.** A sibling KV path, `.../login-state`, holds
  the job's own record. A login the portal rejects sets `login_blocked`
  there with a salted SHA-256 fingerprint of the credentials used; every
  later run refuses to start a browser (exit 6) until the fingerprint of
  the configured credentials differs or an operator deletes the record. The
  record is written before each login, so a rejection can always be
  recorded; if that write fails there is no login.
- **Unconfirmed logins.** The library reports success whenever the portal
  shows no error notice, so a CAPTCHA, a second factor or an expired
  password looks like a login. Such a login -- returned, but its session
  does not open the home page -- is `login_unconfirmed` (exit 5), is never
  written, and is counted in the state record; two in a row set the latch.
  Only a confirmed login resets the count.
- **Sign-in page check.** Before the browser starts, one GET of the sign-in
  page through section 5's transport must show a password form served by,
  and posting to, the configured host over https; otherwise exit 7 and no
  browser. The browser fetches the page again, so this is not atomic;
  closing that belongs in `powerschool-go`.
- **No needless login.** The job reads the stored session first. If it is
  younger than a minimum age and one GET of the home page with it returns
  the signed-in page, the job exits 0. A young session whose state cannot be
  established (network error) is also left alone.
- **One login per run.** One attempt; one more only when the first failed
  before the credentials were submitted (browser start, DNS, connect).
  `powerschool-go` returns a single undifferentiated error for a failed
  login, so "before the submit" is an allowlist over its text and everything
  else is not retried. The Job has `backoffLimit: 0`.
- **Read-only after login.** The new session is confirmed with one GET of
  the home page through section 5's transport with neither POST exception
  enabled. An unconfirmed session is not written (exit 5), so a login the
  library wrongly reported as successful cannot replace a working session.
- **Schedules.** 05:00 Monday-Friday (minimum age 10h: logs in daily) and
  21:05 Sunday-Thursday (minimum age 30h: logs in only when the day's
  session is gone; after the blackout, before the 21:30 poll), as two
  CronJobs in `America/Los_Angeles`.
- **`expires_at`.** The library's value is the latest cookie expiry or a
  four-hour guess, neither of which describes the portal's session. The job
  writes login time + 36h; section 4.3's measurement is what will replace
  that number.
- **Vault secret.** `secret/glovebox/powerschool/default/session`, fields
  `session` (the JSON above, as a string; the property the connector's
  `externalSecret.files` reads), `logged_in_at`, `expires_at`,
  `selected_student_id`, written check-and-set. Policy: `create`, `read`,
  `update` on that path and on the state path -- `read` because of the
  young-session check and the latch. The token is revoked at exit.
- **Browser.** `powerschool-go` v0.4.0 can only start a local Chromium, so
  the image bundles one (`debian:bookworm-slim` pinned by digest, uid 65532,
  `--no-sandbox`). The pod has a read-only root and memory-backed `/tmp`,
  `$HOME` and `/dev/shm`; the profile directory is removed after each login.

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
- Anything outside the connector's own session. Selection is per session
  (observed live), so the switch does not change what the owner sees in
  his. Signing in is a different matter -- it ends the other session
  (section 4.1) -- which is one more reason the connector cannot log in.

## 6. Kids, match keys and per-child visibility

Kids are opaque labels with a portal student id, as in spec 12 section 10:

```json
"kids": [{"name": "k1", "student_id": "111111", "student_api_id": "999001"},
         {"name": "k2", "student_id": "111112", "student_api_id": "999002",
          "student_number": "9000002"}]
```

| field | what it is | used for |
|---|---|---|
| `student_id` | the number in the portal's student switcher (`javascript:switchStudent(<id>)`) | selecting the child; the home-page marker |
| `student_api_id` (optional) | the student id the portal embeds in that child's scores pages (`studentFRN`, the library's `StudentAPIID`); not the same number as `student_id` | the binding in 6.2 |
| `student_number` (optional) | the school-issued number the home page shows as "Student ID #" | a further check on the home page |

The two optional fields identify a real child: set them in the deployment's
private values, not in a committed file. Match keys:

- `powerschool:<kid>:grade`
- `powerschool:<kid>:assignment`
- `powerschool:<kid>:attendance`

Config validation: at startup every enabled surface of every kid must
resolve to a rule that sets `data_subject`; one kid's surfaces must agree
on it; no two kids may share it, a `student_id`, a `student_api_id` or a
`student_number`. A `*` rule cannot stand in. There is no fallback
destination at runtime either: no rule, no item. Checkpoint keys include
the kid label.

### 6.1 One child at a time

The portal returns the *selected* student's data whatever it is asked for.
Attaching a child's label to a page is therefore only ever justified by
evidence on that page. The connector never infers the selection from
anything else -- not from which student is listed first, and not from which
student carries page details. (An earlier library version filled the
details in for the first student whenever the marker was missing, and an
earlier connector read "has details" as "is selected": on markup without
the marker, the second child's records were staged under the first child's
label.)

A poll takes each configured kid in turn, in config order. Polls are
serialised, so two polls never interleave their kids. One kid's turn:

1. **Select.** `SelectStudent(kid.student_id)`: read the home page; if it
   marks another student, post the switcher form (section 5.2 b) and read
   the page again. The library fails unless the page read last marks the
   requested student. If the page marks nobody, nothing is posted.
2. **Verify the home page.** On the page step 1 returned, the connector
   checks for itself: exactly one student is marked, it is this kid's;
   every row of the grades table is stamped with that same student id
   (parsed from the same response); and, if `student_number` is configured,
   the number on the page equals it. That page's grades table is the kid's
   grades and attendance.
3. **Read the assignment pages** -- only if the assignments surface is
   enabled. For each course: the scores page, the check in 6.2 on that
   page, and only then the lookup POST, sent for exactly the section and
   student ids the checked page carried.
4. **Verify the home page again** -- only if step 3 ran. Read it once more;
   it must still mark this kid's student. The connector does not switch
   back and carry on. With the assignments surface off, nothing is read
   after the page verified in step 2, so there is nothing to re-check and
   this step does not run.
5. **Learn** (6.2), if this kid has no bound id yet.
6. **Stage.** Only now are items written and checkpoints advanced.

Any failure in steps 1-5 discards everything read for that kid in this
poll: nothing is staged, no checkpoint moves, and the next clean poll stages
it all as if this one had not happened.

### 6.2 The scores-page binding

**The fact it rests on (observed live).** A course's `scores.html` is
rendered for the student the *session* has selected, not for the course in
the URL: fetching child B's scores URL while child A is selected returns a
page carrying child A's student id. That id is distinct per child. So the
id on a scores page is direct evidence of which child the session had
selected at the moment that page was served.

**Why the marker checks are not enough.** Steps 2 and 4 look at the
selection before and after the assignment reads. A selection that changes
to the other child after step 2 and changes back before step 4 passes both,
and every scores page served in between is the other child's. Comparing the
scores pages only with each other does not catch it either: they agree, on
the wrong child.

**The rule.** Each kid is bound to one scores-page student id, and every
scores page read for that kid must carry exactly that id.

- The bound id is `student_api_id` from config when set.
- Otherwise it is **learned**: on the first poll in which the kid's turn
  passes step 2, every scores page of the turn carries one and the same id,
  that id is not bound to another kid, and step 4 passes, the id is saved
  in the checkpoint (`meta:student_api_id:<kid>`) and required on every
  later poll, across restarts.
- Two kids never share an id: config validation rejects it, a page carrying
  an id bound to *another* kid is refused whether or not this kid is bound
  yet, and an id cannot be learned for a second kid.
- A scores page that carries no id is refused like a wrong one.

The first page that fails ends the kid's turn at once: the lookup for that
course is not sent, the remaining courses are not read, nothing is staged
for the kid (grades and attendance included), no checkpoint of the kid
advances, and the error class is `selection_changed`.

**What it does not cover.**

- *The learning poll.* Until a kid is bound, a flip that lasts for the whole
  of that kid's assignment reads is not detectable (if the other kid is
  already bound it is: the page carries that kid's id). A wrong id learned
  this way fails closed afterwards -- the kid's real pages no longer match,
  and the other kid is refused its own id -- but that one poll stages the
  wrong child's assignment data. **Set `student_api_id` in config** to close
  it; the learned values can be read from the checkpoint file after a
  supervised first run.
- *Grades and attendance with the assignments surface off.* They come from
  the one home page whose marker step 2 verified, in the same response; no
  scores page is read, so there is no binding evidence, and none is needed
  for a single response.

What each failure does to the rest of the poll:

| Failure | `error_class` / skip reason | This kid | Remaining kids |
|---|---|---|---|
| Page carries no selection marker (or two) | `selection_unknown` | skipped | skipped unread; nothing is posted |
| Home page unreadable (network, parse, HTTP status) | `network`, `parse`, `http_status`, ... | skipped | skipped unread |
| Session refused | `auth` | skipped | poll ends; section 4.2 |
| Student id not in the account's switcher | `not_found` / `unknown_student` | skipped | continue |
| Switch posted but not confirmed | `switch_not_confirmed` | skipped | continue |
| Marker or grade rows disagree; marker moved by step 4 | `selection_mismatch` | discarded | continue |
| A scores page is not this kid's (6.2), or another page's own switcher marks another student (6.3) | `selection_changed` | discarded | continue |
| A page's switcher marks nobody (6.3) | `selection_unknown` | discarded | continue |
| Learned id already bound to another kid | `student_api_id_conflict` | discarded | continue |
| Configured `student_number` is not the page's | `student_number_mismatch` | skipped | continue |

Requests per poll, for two kids with *n* courses each and assignments on:
between 5 and 6 home-page GETs, at most 2 switch POSTs (each followed by
one redirect GET), and 2*n* scores GETs and 2*n* lookup POSTs.

### 6.3 Pages other than the home page and the scores pages

The three page surfaces (section 7.4) read `attendance.html`,
`termgrades.html` and `teachercomments.html`. Each describes the session's
selected student, like everything else -- and each carries the portal's
full student switcher, exactly as the home page does: `ul#students-list`
with exactly one `li.selected` (verified live on all four pages).

So every page says, itself, whose it is, and the rule is the same for each:

1. The library reads the selected id from that page's own switcher and
   returns it with the page (`PageStudent.SelectedID`). It refuses
   (`ErrSelectionUnknown`) to return a page that marks nobody or more than
   one student.
2. The connector compares that id with the `student_id` of the kid whose
   turn it is, for **every** page it reads (`checkPageStudent`). Another id
   is `selection_changed`; no id is `selection_unknown`. Either discards
   the kid's whole turn (section 6.1): nothing is staged for the kid, no
   checkpoint moves, and the throttled surfaces' last-read times do not
   advance.
3. The comparison is on the switcher id only. The pages' headings end with
   the student's name (`Teacher Comments: Last, First`); the library
   reports it for people to read, and nothing compares it.

Because each page carries its own evidence, no home-page read is needed
around any of them. The turn's end-of-turn check (section 6.1 step 4) still
runs whenever any page was read after the home page that opened the turn.
A turn with everything on, for one kid:

```
home (select, verify)  ->  termgrades  ->  attendance  ->
teachercomments x terms  ->  [scores -> lookup] x courses  ->  home
```

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
| `has_teacher_comment` | always present, `true` / `false`: the teacher left a note with text on this student's score |
| `teacher_comment_truncated` | `true` when the comment in the body is not the whole comment; absent otherwise |
| `changed` | on `change=updated`: the parts that moved, comma-joined, from `details`, `due_date`, `status`, `flags`, `score`, `teacher_comment`, `description` (section 9); `record` for an old-format checkpoint that cannot say |

**Free text.** After the fact lines the body carries up to three labelled
blocks, each only when non-empty: `Teacher comment:` (the note on this
student's score, `Assignment.TeacherComment`, from the same lookup response
as the score -- powerschool-go v0.4.0), `Assignment description:` and
`Category description:` (omitted when it equals the assignment
description).

**Untrusted text and layout.** Every string from the portal is untrusted,
and the item's layout must be the connector's own:

- a fact -- on every surface each `Label: value` line of the body (grade
  cells, attendance codes and legend text included), each tag value, the
  subject and the sender -- is reduced to ONE line (`oneLine`): all line breaks and runs of whitespace
  become one space, bounded at 256 bytes;
- a text block -- the three above, and the body of the term-level
  `teacher_comment` item under a `Comment:` label -- is plain text only
  (the library's `TeacherCommentHTML` is never read), cleaned per line (`cleanText`), every line indented two
  spaces, bounded at 4 KiB with a `[truncated]` line after a cut block,
  never in a tag;
- in both, LF, CR, VT, FF, NEL (U+0085) and the Unicode line and paragraph
  separators (Zl, Zp) all count as line breaks; format characters (Cf:
  bidirectional controls, zero-width characters, BOM) and the remaining
  C0/C1 controls are dropped. Nothing is decoded or interpreted.

The only lines of a body that start in column one are therefore the
connector's own labels, each at most once. Cleaning applies to what is
rendered; the keys and fingerprints of the other five surfaces (section 9)
are still computed over the text as read, so it re-stages nothing.

**Audience (owner decision, 2026-10-11).** The per-assignment comment is
not a surface of its own. It rides on the assignment's item and so has that
item's `data_subject` and `audience` (section 6): per-assignment teacher
feedback goes to the child who did the work and to the guardians, i.e. the
`assignment` rule's audience. The guardians-only rule of section 7.4
applies to the term-level `teacher_comment` surface and is unchanged.

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

### 7.4 The three page surfaces

Three further surfaces each read one more portal page. Each has its own
flag under `surfaces` and each is **off unless set true** (the first three
are on unless set false): they cost extra requests per child per poll. The
page structure they rely on has been checked against the live portal; the
connector code has run against synthetic fixtures only (section 11.2).

They are read inside the kid's turn (section 6.3), never outside it.

#### `attendance_day` -- one item per (course, day) with a code

From `/guardian/attendance.html`: a grid with one row per course and one
cell per school day. A cell is blank (present), an attendance code, `-` or
`.`. Only codes stage.

The page's legend is `CODE=CATEGORY-Description` (for example
`ME=EA-Medical/Sick`, `VU=UA-Unexcused Travel`, `FT=AUTH-Field Trip`). It is
district policy vocabulary and is **parsed from the page on every read,
never hardcoded**.

| tag | meaning |
|---|---|
| `date` | the day, `2006-01-02` |
| `code` | the attendance code as printed |
| `category` | the code's legend category; absent when the legend lacks the code |
| `code_description` | its legend description; absent likewise |
| `kind` | a coarse class derived from `category` (below) |
| `previous_code` | on `updated`: the code it replaced |

`kind` comes from the category through `attendance_days.kinds`, a map of
kind to categories. The default:

| kind | categories |
|---|---|
| `absence_excused` | `EA` |
| `absence_unexcused` | `UA`, `A` |
| `late_excused` | `EL` |
| `late_unexcused` | `UL`, `L` |
| `left_early_excused` | `LE` |
| `left_early_unexcused` | `LU` |
| `authorized` | `AUTH` |

`authorized` is an authorized activity (a field trip, the student elsewhere
in the building): it is staged, and tagged so a summariser does not count
it as an absence. A category the map does not list -- or a code the legend
does not list -- gets `kind=unknown` and **is still staged**, so a new code
is seen rather than dropped. A category may appear under one kind only.

`-` and `.` are not in the legend. They are non-events (no class for the
student that day; nothing marked) and are never staged. They are counted in
`powerschool_attendance_non_event_cells_total{kid,cell}` (`cell` = `dash` /
`dot`) so that if one of them turned out to carry a meaning, the change in
how often it appears would be noticed.

The header prints month/day without a year; the library takes the year from
each row's enrolment dates and checks every date against its weekday
column. A course is identified by name, period and enrolment start date, so
a section that was dropped and re-enrolled is two courses and its two rows
do not overwrite each other. Fingerprint: the code. A day is staged again
when its code changes (unexcused becomes excused). A code that is removed
stages nothing: the cell is then blank, like a day never marked. A coded day
first seen more than `attendance_days.lookback_days` (default 30) after it
happened is recorded without being staged.

The existing `attendance` surface (per-course totals from the home page) is
unchanged; the two can be on together.

#### `term_grade` -- one item per stored grade

From `/guardian/termgrades.html`, which on this portal is the **grade
history**: the stored final grade and credit of each completed course, one
row per course and completion month (columns `Date Compl`, `Grade Level`,
`School`, `Course #`, `Course`, `Credit Earned`, `Credit Attempted`,
`Grade`). It is not a per-quarter view (the in-progress Q1..S2 grades are
the `grade` surface), it has no percentages, and it names no reporting
term.

| tag | meaning |
|---|---|
| `term` | the completion month, `2006-01` |
| `date_completed` | as printed, `MM/YYYY` |
| `grade` | the stored letter grade |
| `course_number`, `school`, `grade_level` | as printed |
| `credit_earned`, `credit_attempted` | as printed |
| `included_in_gpa` | `false` when the page marks the grade with `*` |
| `previous_grade` | on `updated`, when the letter changed |

A stored grade is identified by course number, completion month, credit
attempted, school, grade level and course name -- none of which changes
when a grade is corrected -- so reordering the page stages nothing; only
rows identical in all of those fall back to their position. Fingerprint:
letter, credits, GPA flag. First-sight backlog:
`term_grades.lookback_days` (default 365). Read at most once per
`term_grades.min_interval_hours` (default 24) per kid.

#### `teacher_comment` -- one item per (course, term) with a comment

From `/guardian/teachercomments.html`, one GET per reporting term:
`teachercomments.html?fg=<term>`. (The parameter is `fg`; the page's
selector is *named* `storecode`, but that name has no effect as a
parameter.) `teacher_comments.terms` lists the terms to read; empty means
the bare page -- which shows the current term -- and then every other term
its selector offers. Read at most once per
`teacher_comments.min_interval_hours` (default 24) per kid. A row with an
empty comment stages nothing.

| tag | meaning |
|---|---|
| `term` | the reporting term (`Q1`, `S1`, ...) |
| `course_number` | as printed |
| `teacher_present` | `true` when the row names a teacher |

The teacher's name is in the body only, never in a tag, the subject or the
sender. Fingerprint: a hash of the comment text and teacher; an edited
comment stages again.

**Teacher comments are for the parents only** (owner decision). This is
enforced as an **allowlist**, in three places:

1. `ValidateConfig`, at startup, whether or not the surface is enabled:
   every rule that *can* match a kid's `powerschool:<kid>:teacher_comment`
   key -- the exact rule, and any wildcard (`*`; also `powerschool:*` and
   `powerschool:<kid>:*`, which the framework's matcher does not match
   today and which are treated as if it did) -- must set `audience`, and
   every token in it must be exactly `guardians`. `subject`, `household`,
   `public`, `siblings`, `caregivers`, a token in another case or an
   unknown one is a config error naming the rule, alone or combined with
   `guardians`. An unset audience is refused: it would fall back to a
   default this connector does not control.
2. Rule order. The framework's `RuleMatcher` is first-match-wins, so a
   broader rule placed before a kid's exact `teacher_comment` rule would
   win over it. That is a config error too, even when the broader rule is
   itself guardians-only (the exact rule's `data_subject` would be dead).
3. At staging time, as defence in depth: a `teacher_comment` item whose
   resolved audience is not exactly `["guardians"]` is not staged and not
   checkpointed. It is counted (`connector_items_dropped_total{reason=
   "teacher_comment_audience"}`, `powerschool_surface_errors_total{
   error_class="audience_not_guardians_only"}`) and logged with the kid
   label and the number of audience tokens -- no content.

**Ships disabled.** `surfaces.teacher_comments` is `false` in the sample
config, the chart defaults and the built-in defaults, and must stay off
until a real, non-empty teacher comment has been parsed from a live page:
none was posted in any term when the page was verified, so reading a
comment's text has only ever run against fixtures.

**Bounded reads.** One request per reporting term. At most
`teacher_comments.max_terms` (default 12) terms are read per kid per poll,
whatever the selector offers; the cap is logged once per process when it
applies. Each term is throttled on its own: a term whose page was read and
accepted is not read again for `min_interval_hours`; a term whose page was
refused (another term's page, an HTTP error) is asked for again at the
next poll. The list of terms the bare page offered is kept in the
checkpoint so that retry does not need the bare page again.

**Term confirmation.** The term is half of a comment's identity, and the
page states it: the selector option of the term being shown carries
`selected` (verified live: the bare page marks the current term, `?fg=Q1`
marks Q1). The library reads the available terms from the options' URLs --
the first option is the "show dropped classes" entry and is never a term,
even though its URL carries the requested term -- and, when a term was
requested, refuses a page that marks a different one (`term_mismatch`; that
term is skipped for the poll). `teacher_comments.require_term_confirmation`
(default true) additionally refuses to stage a page that marks no term at
all (`term_unconfirmed`).

## 8. Poll schedule

Windows are local `HH:MM` ranges in `POWERSCHOOL_TIMEZONE` (default
`America/Los_Angeles`), optionally weekdays only. The shipped default is
three weekday windows, 06:00-06:30, 15:00-15:30 and 21:30-22:00, chosen to
stay outside 16:00-21:00 when the account's owner may be signed in (section
4.1). There is no trigger endpoint.

**Exactly one scheduled poll per window.** `Watch` picks one random time
inside the next window, sleeps until then, and polls. It then schedules the
following poll from the *end* of the window it just polled, not from the
moment the poll fired; scheduling from the fire time re-rolls inside the
same window and polls it again most of the time. The end of the last polled
window (`notBefore`) is kept on the connector, so it survives the framework
restarting `Watch`. A window's start is inclusive and its end exclusive:
when two windows touch and the second one's random time falls on its first
second, that time equals the instant scheduling resumes from, and it is
taken, not skipped.

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
- assignment: `a3:` and one short hash per part -- `details` (title,
  category name), `due_date`, `status`, `flags`, `score` (score, points
  possible, percent, letter), `teacher_comment`, `description` -- so that a
  changed record can say which parts moved (the `changed` tag). Each part
  is hashed over what the item shows: facts as their cleaned line, the two
  texts as cleaned and cut for the body, plus whether the block ends in
  the truncation marker. An edit that changes nothing in the staged item
  stages nothing. The category description is not included: it belongs to
  the category, and one edit would stage every assignment in it.

A staged item's id is minted per item, so for a consumer that notices new
items by id the fingerprint is the record's identity: an unchanged record
yields no item, a changed one at most one. Two changes are recorded without
an item, both for an assignment outside the lookback window with no
missing / late / incomplete flag: first sight (section 7.2), and a change
to nothing but the description. A teacher-comment change always stages.

Assignment fingerprints in the first release's format (one hash over the
raw gradebook facts, no text) are recognised by recomputing that hash,
which a test holds to literal values: such a record is re-baselined without
staging, unless it carries a teacher comment, which stages once
(`changed=teacher_comment`). If the old hash no longer matches, the record
stages with `changed=record`.

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
  exits the process and the restart waits for a new session (section 4.2).
  This is routine (section 4.1), not an alert condition.
- Anything else is logged, counted and retried at the next window. A course
  whose lookup fails is skipped; the rest of the kid's turn continues.
  Selection failures are in the table in section 6.2.
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
named by their opaque label; student ids, scores-page student ids and
student numbers are not logged. The library applies the same rule to its own errors and INFO lines,
and the connector runs it at WARN.

Metrics, on top of the framework's: `powerschool_polls_total{trigger_source}`,
`powerschool_items_total{surface,kid,change}`,
`powerschool_surface_errors_total{surface,error_class,kid}`,
`powerschool_kids_skipped_total{kid,reason}`,
`powerschool_rejected_session_age_seconds` (section 4.3),
`powerschool_attendance_non_event_cells_total{kid,cell}` (section 7.4). They are
registered on the framework's own meter provider, the one behind
`/metrics`. Spans: `powerschool.poll`, `powerschool.poll.kid`,
`powerschool.lib.SelectStudent`, `powerschool.lib.GetHomePage`,
`powerschool.lib.FetchScoresMetadata`, `powerschool.lib.GetAssignments`,
`powerschool.lib.attendance_day`, `powerschool.lib.term_grade`,
`powerschool.lib.teacher_comment`, `powerschool.staging.commit`.

## 11. Open items

### 11.1 Required before the connector is enabled

1. **The login job** (section 4.4): scheduled, off-hours only (never
   16:00-21:00 Pacific), writing the session where the chart's
   `externalSecret.files` picks it up. Built (`glovebox-wekc.7`), with
   Chromium in its own image; not yet run against the live portal. A
   remote-browser option in the library would let it reuse the cluster's
   Browserless and drop the bundled browser.
2. **A supervised first run** with `student_api_id` either configured or
   read back from the checkpoint and then configured (section 6.2).

3. **A powerschool-go release** containing the three page parsers. Done:
   `go.mod` requires the released v0.4.0 (v0.3.0 brought the page parsers,
   v0.4.0 the per-assignment teacher comment and category description).
   The binary refuses to start on any other version
   (`reviewedLibraryVersion`, checked by `CheckLibrary` against the build
   information; a replaced module or missing build information is refused
   too).
4. **A supervised first run of the three page surfaces** (section 11.2)
   before any of them is left switched on.

### 11.2 Verified live, and still unverified

Verified against a live instance (structure and behaviour; no real value is
recorded in this repository):

- HTTP page fetch through the library works; pages are server-rendered and
  `home.html` contains the grades table.
- The `li.selected` marker, the `<li >` shape of unselected items, the
  switcher form and its script.
- `SelectStudent` works in both directions and the home page confirms the
  selection each time. The switch does not rotate cookies.
- Selection is per session: an independent login kept its own default
  child.
- `scores.html` reflects the session's selected student, not the course
  link, and its student id is distinct per child (section 6.2).
- Observed once: a second login invalidated the first session (section
  4.1).

Verified live for the three page surfaces (structure only):

- All four pages -- home, attendance, grade history, teacher comments --
  carry the full student switcher with exactly one `li.selected`.
- The attendance legend's text and its `CODE=CATEGORY-Description` form;
  the cell values that occur in the grid (blank, `-`, `.`, and codes).
- The grade history's column headers.
- Teacher comments: the term parameter is `fg` (`storecode` has no effect);
  the option values are URLs; the page marks the shown term with
  `selected`; the first option's URL takes on the requested term.

Not yet verified, for the three page surfaces:

- **A non-empty teacher comment.** None was posted in any term when the
  page was checked, so reading a comment's text is fixture-tested only.
- **The connector's own run.** The library parsers and the connector's
  three surfaces have been exercised against synthetic fixtures built to
  the verified shapes, not against the portal.
- **Attendance:** that the teacher line ends with the room; that the weekly
  columns are always Monday to Friday; the year logic across a real
  new-year week. `-` and `.` are treated as non-events on the owner's
  reading; the debug counter exists in case that is wrong.
- **Grade history:** what the bare URL returns for a student with several
  school years -- everything, or one `termid` (the site map links it as
  `termgrades.html?schoolid=..&termid=..`). Observed: one school year, two
  completion months.

Not yet verified, generally:

- **The real session lifetime**, and whether it is an idle or an absolute
  timeout. Measured from now on (section 4.3).
- **That "one session per account" always holds**, rather than having been
  a coincidence of the one observation. The design assumes it does; if it
  does not, nothing breaks.
- **Single-child accounts.** Whether the page has a `#students-list` and
  marks its one item. Without a marker the connector reads nothing.
- **Which dead-session shape the portal uses** in each case (302 to
  `/public/`, a 200 sign-in page, 401). All are handled. A `scores.html`
  request with incomplete parameters also gets the sign-in page and cannot
  be told apart from a dead session, so a library bug there would read as a
  rejected session.
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
PVC and, when `externalSecret.enabled`, two ExternalSecrets. One maps
`POWERSCHOOL_HOST` into the environment. The other
(`externalSecret.files`, a generic addition to the connector templates)
materialises the session as `session.json` in a Secret that the Deployment
mounts read-only at `/etc/powerschool`, with
`POWERSCHOOL_SESSION_FILE=/etc/powerschool/session.json`; the mount has no
`subPath`, so the kubelet updates the file in place when the Secret
changes. The pod template carries `checksum/secret`, a hash of the
ExternalSecret template, next to `checksum/config`. Remote keys and
properties are values; the username and password are not mapped. Disabled
by default.
