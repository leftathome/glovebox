# powerschool connector

Polls a PowerSchool parent portal for each configured child's course grades,
gradebook assignments and attendance counts, and stages what is new or
changed. It reads the operator's real parent account read-only, from a saved
session. Design: [spec 16](../specs/16-powerschool-connector-design.md).

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-powerschool` |
| Credential class | `real-readonly` (parent portal session) -- see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | none yet -- everything is tested offline with a fake client and synthetic fixtures |

## Status

Shipped **disabled**. The portal behaviour the first three surfaces depend
on has been verified live with the library. It builds against the released
`powerschool-go` v0.4.0 (see "The library version" below). What is missing
before it can be enabled:

1. The **login job** that produces the session
   ([powerschool-auth-refresher.md](powerschool-auth-refresher.md)) enabled
   alongside it. Built; not yet run against the live portal.
2. A supervised first run, to bind each kid to its scores-page student id
   (see "One child at a time").

See spec 16 section 11.

## Authentication and the session

**The connector never logs in.** It never sees the account's username or
password and has no way to authenticate. At startup
`cmd/powerschool/main.go` reads:

- `POWERSCHOOL_HOST` -- the portal location. A bare host or a full `https://`
  URL; the binary normalises it (`powerschool.NormalizeBaseURL`) and always
  uses https.
- `POWERSCHOOL_SESSION_FILE`, a path to a session document (what the chart
  sets up: a mounted Secret), or `POWERSCHOOL_SESSION`, the document itself.
  The file wins if both are set. It is the JSON form of `powerschool-go`'s
  `SessionExport`: `{"cookies":[...],"expires_at":"..."}`.

The session is produced by a separate, scheduled **login job**,
`powerschool-auth-refresher`, that holds the credentials, drives the browser
login, calls `Client.ExportSession` and writes the result to Vault; ESO syncs
it into the mounted Secret. Its runbook, with the Vault policy and the
schedules, is [powerschool-auth-refresher.md](powerschool-auth-refresher.md).
Without login on demand a missing, malformed or expired session is a
startup error; with it, the connector starts and asks for one at its first
poll.

### One account, one session

The connector uses the owner's own parent account, and the portal appears
to keep **one live session per account**: a second login ends the first
(observed once). So:

- When the owner signs in to the portal, the connector's session ends. That
  is expected. He does so rarely, and between 16:00 and 21:00 Pacific.
- A login by the job would sign the owner out. **The login job must never
  run between 16:00 and 21:00 Pacific.**
- The default poll windows (06:00-06:30, 15:00-15:30, 21:30-22:00, weekdays)
  stay outside that span. Keep any you add outside it too.
- A session ended mid-day costs data only until the next login; the first
  poll on the new session stages everything that changed in between. It is
  not an incident. It does not trigger a login during 16:00-21:00 Pacific
  under any configuration.

### When the session is rejected (without login on demand)

This is what happens with `login_on_demand` off; with it on, see "Idle
sessions, the pre-poll check and login on demand" below.

The connector records a fingerprint of the session (a hash, in
`rejected-session` under `GLOVEBOX_STATE_DIR`) and exits. The restarted
process does **not** present that session again. It waits, making no
upstream request at all, and re-reads the session file once a minute; when
the login job replaces the Secret the kubelet updates the mounted file in
place and the connector starts polling again without a pod restart.

While it waits the pod is **Running and Ready** (`/healthz` 200, `/readyz`
200), neither crash-looping nor holding up a rollout: waiting is what it
should be doing, and readiness gates every Helm upgrade and rollback of the
release (up to 0.11.0 it answered `/readyz` 503 here, which made a values
change applied during a wait time out, and Flux's rollback with it --
glovebox-wekc.17). What it is doing is reported, not hidden in readiness:

- `/readyz` body: `ok (degraded: session=waiting_for_new_session)`;
- `/statusz`: JSON with the `session` condition, its state and `since`
  (when the session was refused -- the time of the `rejected-session`
  marker, so it survives restarts);
- `powerschool_session_state{state="unknown|usable|waiting_for_new_session"}`
  (1 for the current state) and
  `powerschool_session_state_since_timestamp_seconds{state=...}`;
- the log line `this session was already rejected upstream; waiting for a
  new one`.

With `metrics.prometheusRule.enabled` the chart alerts
(`GloveboxPowerSchoolWaitingForSession`) when the wait passes
`metrics.prometheusRule.sessionWaitingHours` (60 by default: logins and
polls are weekday-only, so the longest routine wait is Friday's 21:26 login
to Monday's 05:56 one, about 56.5h). When the wait began is kept in
`session-state` in the state directory, so a restart does not reset it, in
any login mode.

The same state is reported with `login_on_demand` on, from each poll's
session check: `usable`, `unknown` (the portal's answer was inconclusive),
or `waiting_for_new_session` (dead, and no usable replacement yet --
deferred, latched, backed off). The connector is Ready from start-up in
every mode: the first poll, which may wait minutes for a login, no longer
decides readiness.

To learn how long sessions really last (the expiry in the session document
is a placeholder), the connector reports each rejected session's age: the
gauge `powerschool_rejected_session_age_seconds` and one INFO log line with
`session_age_seconds`. Use it to set the login job's schedule.

Optional: `POWERSCHOOL_TIMEZONE` (scheduler and due-date timezone, default
`America/Los_Angeles`).

## Idle sessions, the pre-poll check and login on demand

**Finding (2026-10-11).** The portal ends an *idle* session in well under an
hour. A session stored at 19:23:51Z and never used was refused at 20:27Z by
the connector and at 20:28Z by the login job; measured since: an idle
session is dead at 30 minutes and at 45, and alive at 4. A session that is
being used works: a catch-up poll on a 4-minute-old session read both
children in 24 s (238 items, no failures). So with three polls a day, every
poll finds the previous poll's session dead, and a login scheduled an hour
before a poll is an hour too early. `sessionLifetime` / `expires_at` (36h)
is an upper bound on how long a session is presented, not a statement about
the portal.

**There is no keepalive, by decision.** Requesting pages nobody needs, to
look like a person who is still there, is not something this system does
(owner decision). That rules out two things, not one: the connector does
not touch the portal between polls, and the login workload does not check
the stored session unless somebody is waiting for the answer. A session is
obtained when a poll needs one.

### The check at the start of every poll

Before any child is read, scheduled poll or catch-up, the connector makes
one GET of the home page through its pinned read-only transport and
classifies the answer by its status, where the redirects ended and what
kind of page came back. The status is looked at first.

| State | When | What happens |
|-------|------|--------------|
| live | 200, and the page has the student switcher | the poll runs |
| dead | 200 under `/public/` **with a password form** (the portal's own sign-in page, positively identified); or 200 at the access gateway's `/my.logout.php3` (reached with the session's cookies attached) | see below |
| unknown | everything else: a network error; any status but 200, 401 and 403 included; the gateway's `/my.policy`; a page under `/public/` without a password form; any other page | the poll is skipped; nothing is changed and no login is asked for |

Dead is a strong claim -- it leads to a login request -- so only a
positive identification earns it.

A dead session is logged as `powerschool session is no longer accepted`
(INFO, with `reason` and `session_age_seconds` since the connector first
loaded that session) -- not as a kid that could not be selected with
`error_class=parse`, which is what 0.10.3 logged. That happened because the
access gateway answers a lapsed cookie with a redirect through
`/my.policy` and a new cookie, the connector's HTTP client dropped the new
cookie, and the read ended on the gateway's error page, which the library
could only call unparseable. The connector's client now carries a cookie
the gateway renews (`internal/psportal`, shared with the login job), so
the portal itself gets to answer. That overlay sits inside the pinned
transport, can add or replace a cookie but never remove one, and is emptied
at the start of every poll.

### Login on demand (`login_on_demand`, chart: `loginOnDemand`)

Off by default. With it on, a dead session (or no session file yet, or one
past its `expires_at`) makes the connector **ask** for a login and wait.

```
connector (poll starts)            login workload (every 3 min in its windows)
----------------------             -------------------------------------------
GET home page -> dead
PATCH ConfigMap  request =
  "<time> <fingerprint prefix>" -> run starts: blackout? no
re-read session file every 5 s     read ConfigMap: a request, fresh, not served
  (no portal traffic)              Vault: latch clear, interval respected
                                   stored session = the dead one -> no check
                                   sign-in page check, ONE login, confirm
                                   write session + "served" to Vault
                         ESO syncs the Secret; the kubelet updates the file
new session in the file
GET home page -> live
poll (once)
```

Runs of the login workload that find no pending request -- nearly all of
them -- exit at their first step having sent nothing to the portal.

**What the connector can and cannot do.** It parses pages from the portal
and is treated as the component most likely to be taken over. Its entire
reach towards a login is one `PATCH` of one key of one ConfigMap,
`<fullname>-powerschool-login-request`, holding a time and sixteen hex
digits. It cannot create a Job, a pod or any other object, cannot read a
Secret, has no username, password or browser, and its portal transport
refuses the sign-in POST. The login workload validates the field's size and
shape, never echoes it, and acts on it only under its own rules. **A
connector under an attacker's control can therefore cause at most:**

- logins only inside the login workload's on-request windows (a chart
  value, checked against the blackout at render time) and never between
  15:57:30 and 21:00 Pacific -- the blackout is compiled into the login
  binary;
- at most one login per 20 minutes (enforced in the login binary from its
  own record in Vault, which the connector cannot write) -- three an hour;
- no login at all while the latch is set, and none with credentials it
  could choose: it supplies no input to the login beyond "now";
- no read of, and no influence over, what is done with the session.

Each such login would sign the account's owner out if he were signed in at
the time, outside the hours he uses the portal. That is the whole of the
damage, and it is the damage an honest connector does three times a day.

Limits the connector also keeps on itself (they make an honest connector
quiet; they are not what a compromised one is held by):

- at most one request per poll, and a poll happens once per window: there
  is never a second request in the same window, also not after a restart;
- at most one per 20 minutes overall (`min_interval_minutes`; the time of
  the last request is in the checkpoint, and a recorded time in the future
  counts as "just now");
- never between 15:57:30 and 21:00 Pacific;
- never when the session file is less than 2 minutes old;
- **backoff:** after 2 requests in a row that did not lead to a session
  the portal accepts, it stops asking and raises its permanent-failure
  path (below), until the session file changes or an operator resets it.

**Waiting.** After asking, the connector re-reads the session file every
5 s for up to `wait_seconds` (default 600) and contacts the portal for
nothing. Worst case from request to session-in-pod, added up: the login
workload's next run (up to 3 min, plus ~20 s for the pod), the login (up to
~100 s), ESO (1 min), the kubelet's update of the mounted Secret (up to
~90 s) -- about 7.5 minutes. When a different session appears it is checked
once; if live, the poll runs then, even if the window's nominal end has
passed. The wait bounds how late that one attempt can be.

What an operator sees:

| Log line / `skip_reason` | Meaning | Look at |
|--------------------------|---------|---------|
| `asked for a login; waiting for the new session` then `a new session is in place and accepted; polling` | the normal case | nothing |
| `session_state_unknown:<reason>` | the check could not tell; nothing was asked for; not a failure | the portal, if it repeats |
| `blackout` | the session was dead between 15:57:30 and 21:00 Pacific; nothing was asked for; not a failure | nothing; the 21:30 poll asks |
| `login_requested_recently`, `session_file_fresh` | a request or a login was just made; not asking again; not a failure | the earlier lines |
| `no_new_session` (ERROR, `poll failed`) | asked, and no new session reached the pod within the wait | the login workload's runs in that window: `no_login_requested` means it did not see the request (RBAC, the ConfigMap name); `login_request_too_soon`, `login_blocked`, `signin_page_unrecognised`, `blackout_refused` say why it declined; none at all means no run was due (its windows vs. the poll windows). Then the ExternalSecret's status |
| `new_session_not_accepted` (ERROR) | a new session arrived and the portal refuses it too | the login workload's log; sign in by hand to see the portal |
| `login_request_failed` (ERROR) | the request could not be written (403: the Role or its binding; 404: the ConfigMap; a timeout: a NetworkPolicy) | the error in the line; RBAC and network below |
| `login_backoff`, and the process exits with `N login requests in a row produced no session the portal accepts` | the connector has stopped asking | below |

The three ERROR rows are failed polls: they count toward
`failure_threshold` and in `powerschool_polls_skipped_total{reason}`.

**When the connector stops asking.** Two unanswered requests in a row mean
something is wrong that another login will not fix, and every login risks
signing the owner out. The connector exits with a permanent error (the
restart is the alert); every later poll does the same without asking. It
resumes by itself when the session file changes -- so the usual fix is to
find the cause in the login workload's log, then run the login once by
hand ([runbook](powerschool-auth-refresher.md#running-it-once-by-hand)).
To make it ask again without that, set the connector's environment
variable `POWERSCHOOL_LOGIN_BACKOFF_RESET` to any value it has not seen
before (`connectors.powerschool.env`); it is honoured once.

RBAC (rendered by the chart with `loginOnDemand.enabled`): the connector's
own ServiceAccount (`<fullname>-connector-powerschool`, which it has in every
mode) gets its token mounted, and a Role bound to it alone grants
exactly `get`, `update` and `patch` on the one ConfigMap
`<fullname>-powerschool-login-request`. Nothing on Jobs, CronJobs, pods or
Secrets, and no create or delete of anything.

Network: the connector must reach the Kubernetes API server. The chart's
own connector NetworkPolicy allows all egress. In a namespace whose policies
are managed elsewhere and deny by default, add (the API server is usually
on the control-plane nodes' port 6443; policy is evaluated after the
`kubernetes` Service's address is translated, so the Service address alone
is not enough):

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: powerschool-to-apiserver
  namespace: openclaw
spec:
  podSelector:
    matchExpressions:
      # the connector, and the login workload's on-request runs
      - key: app.kubernetes.io/component
        operator: In
        values: [connector, powerschool-auth-refresher]
  policyTypes: [Egress]
  egress:
    - to:
        # kubectl get endpoints kubernetes -n default
        - ipBlock: { cidr: <control-plane-node-1>/32 }
        - ipBlock: { cidr: <control-plane-node-2>/32 }
        - ipBlock: { cidr: <control-plane-node-3>/32 }
      ports:
        - { protocol: TCP, port: 6443 }
    - to:
        # kubectl get svc kubernetes -n default  (for CNIs that match
        # before translation)
        - ipBlock: { cidr: <kubernetes-service-cluster-ip>/32 }
      ports:
        - { protocol: TCP, port: 443 }
```

(Narrow the selector to `glovebox.io/connector: powerschool` if other
connectors must not reach the API.) With Cilium the equivalent is a
CiliumNetworkPolicy with `toEntities: [kube-apiserver]`. The login
workload's own chart NetworkPolicy needs the same addresses in
`powerschoolAuthRefresher.networkPolicy.kubernetesAPICIDRs`; the chart
refuses to render without them.

Two HTTP clients exist in the connector and neither can reach the other's
host: the portal client (session cookie; portal host; read-only) and the
Kubernetes client (ServiceAccount token; API server host; get and patch of
ConfigMaps and nothing else -- `internal/kubecm`). A test proves both
directions.

### The login broker (`login_on_demand.broker`, chart: `loginBroker`)

The other way of asking, and an alternative to the request above: instead
of leaving a note in a ConfigMap for a CronJob that looks every three
minutes, the connector calls a small always-on service that holds the
credentials and gets the outcome -- and, when there is one, the session --
in the answer. Design, policy and threat model:
[`internal/loginbroker/DESIGN.md`](../../internal/loginbroker/DESIGN.md).

```json
"login_on_demand": {
  "enabled": true,
  "broker": {
    "url": "http://glovebox-glovebox-login-broker-powerschool.glovebox.svc:8080",
    "token_file": "/var/run/secrets/login-broker/token",
    "timeout_seconds": 420
  }
}
```

The chart writes this block from `connectors.powerschool.loginBroker.enabled`
and `loginBrokers.powerschool`; `configmap_name` and `broker.url` together
are refused, by the binary and by the chart. What differs from the mailbox
on the connector's side:

- Nothing is waited for. The session in the answer is put into use at once
  and checked once; the mounted session file receives the same session
  through Vault and ESO a little later, and until then does not displace it.
- The connector applies no blackout and no interval of its own: the broker
  enforces both and says `deferred`, or `budget_exhausted` (a skipped poll,
  not a failed one). The connector then does not ask again before the time
  the broker named.
- The process never exits over a login that cannot be had -- the broker's
  latch, a deferral, an unreachable broker. An exit is a restart, and a
  restart loop would put a request to the portal on a timer. It stays up,
  logs an ERROR at every poll, and asks the broker at most once per poll.
- A session found dead is remembered as dead (in the checkpoint, across
  restarts): the portal is not asked about it again.
- A session that dies in the middle of a poll is replaced and the poll is
  run again on the new one, once.
- A session whose state cannot be established for three polls in a row is
  reported as a failed poll (ERROR line, skip metric).
- Two sessions in a row that were delivered and then refused by the portal
  stop the asking. The count is in the checkpoint and survives a restart;
  a session the portal accepts, or `POWERSCHOOL_LOGIN_BACKOFF_RESET`,
  clears it.
- The pod needs no access to the Kubernetes API. It presents a token of
  its own ServiceAccount (`<fullname>-connector-powerschool`, the broker's
  allowed caller) projected for the broker's audience, to the broker's
  address only.

## Read-only guarantee

The session belongs to a real parent account. Every request goes through
`powerschool.Transport`, which:

- forwards only `https` requests to exactly the configured host, so the
  session cookie cannot be carried anywhere else;
- refuses any request carrying a method-override header; and
- hands everything to `connector.ReadOnlyTransport`, which refuses any
  method but GET/HEAD before it leaves the process -- with an explicit
  allowlist of **two** POSTs, neither of which changes any record:
  1. `POST /ws/xte/assignment/lookup`, only while the assignments surface is
     enabled. It is the query the portal's own scores page uses to list a
     section's assignments; there is no GET form of it.
  2. `POST /guardian/home.html` whose body is exactly
     `selected_student_id=<id>` for a student id configured under `kids`.
     It is the portal's own student switcher and changes which child the
     session displays, nothing else.

Everything about those two requests is matched literally -- path as sent on
the wire, method, content type, and for the switch every byte of the body.
Spec 16 section 5 has the rule and the reasoning.

The library client is built with a session and no credentials, so it cannot
re-authenticate. Agents never receive the session -- glovebox hands them
scanned files only.

## One child at a time

The portal shows one child per session. Each poll takes the configured kids
in turn: select the kid's student (switching if needed), check the page's
explicit selection marker, read, and only then stage. If the page carries no
marker, nothing is posted and no kid is read.

With the assignments surface on, every scores page read for a kid must
carry that kid's own student id -- the portal renders a scores page for
whichever student the session has selected, so this id is proof of whose
page it is -- and the home-page marker is checked again after the reads. If
any page is another child's, everything read for that kid in that poll is
discarded (`error_class=selection_changed`) and none of the kid's
checkpoints move.

The id comes from `student_api_id` in the kid's config, or is learned on the
kid's first clean poll and kept in the checkpoint
(`meta:student_api_id:<kid>`). Setting it in config also protects that
first poll: after a supervised first run, read the learned values out of
the checkpoint and put them in your private values. Spec 16 sections 6.1
and 6.2.

## Schedule

One poll per configured window, at a random time inside it; by default
three weekday windows outside 16:00-21:00 local (see "One account, one
session"). The framework's periodic re-poll is disabled, and a restart
polls only if a window opened while the process was down. Spec 16 section 8.

## Configuration

Sample: [`connectors/powerschool/config.json`](../../connectors/powerschool/config.json).

Framework fields (from `connector.BaseConfig`) are as for every connector:
`rules`, `identity`, `fetch_limits`, `data_subject_default`,
`audience_default`.

Connector-specific fields:

| field | type | required | description |
|-------|------|----------|-------------|
| `kids` | `[]{name,student_id,student_api_id?,student_number?}` | yes | children to poll. `name` is an opaque label (`k1`, `k2`) -- never a name -- with no `:` or whitespace. `student_id` is the numeric id from the portal's student switcher, as a string; `"0"` fails validation. Optional `student_number` is the school-issued number the portal page shows; when set, the selected student's number on the page must equal it or that kid is skipped. Optional `student_api_id` is the student id the portal embeds in that child's scores pages; every scores page read for the kid must carry it (learned on the first clean poll when unset). Both identify a real child: set them in private values only. Names, ids, api ids and numbers must be unique |
| `poll_schedule` | object | yes | `weekdays_only` (bool) + `windows` (`[]{start,end}`), `HH:MM` local times. Exactly one poll at a random time inside each window |
| `surfaces` | object | no | `grades`, `assignments`, `attendance` booleans, each default `true`; `attendance_days`, `term_grades`, `teacher_comments` booleans, each default `false`. At least one must be on |
| `attendance_days.lookback_days` | int | no | coded days first seen more than this many days late are recorded without being staged (default 30) |
| `attendance_days.kinds` | `map[kind][]category` | no | which attendance legend categories each `kind` tag covers (default in "The three page surfaces"); a category may appear under one kind only |
| `term_grades.lookback_days` | int | no | stored grades first seen more than this many days after completion are recorded without being staged (default 365) |
| `term_grades.min_interval_hours` | int | no | least time between reads of the grade history page per kid (default 24) |
| `teacher_comments.terms` | `[]string` | no | reporting terms to read (`Q1`, `S1`, ...); empty = the current term and every other term the page offers |
| `teacher_comments.min_interval_hours` | int | no | least time between reads of each term's comment page per kid (default 24); a term whose page was refused is retried at the next poll |
| `teacher_comments.max_terms` | int | no | most reporting terms read per kid per poll (default 12) |
| `teacher_comments.require_term_confirmation` | bool | no | stage comments only from a page that itself marks its reporting term (default `true`) |
| `assignments.lookback_days` | int | no | assignments first seen with a due date older than this and no missing/late/incomplete flag are recorded without being staged (default 30) |
| `assignments.course_delay_ms` | int | no | pause between per-course requests (default 500) |
| `failure_threshold` | int | no | consecutive polls that read no student before the connector exits (default 5) |

Startup also enforces per-child visibility on `rules`: every enabled surface
of every kid must match a rule that sets `data_subject`, a kid's surfaces
must agree on it, and no two kids may share it. A `*` rule cannot substitute
for the per-kid rules.

## Routing

Match keys emitted by this connector:

- `powerschool:<kid>:grade` -- a course's grades, when first seen or changed.
- `powerschool:<kid>:assignment` -- a gradebook assignment, when first seen
  or changed (new score, flag set or cleared, due date moved, or a teacher's
  comment or the assignment description added, edited or removed). The
  `changed` tag says which.
- `powerschool:<kid>:attendance` -- a course's absence/tardy totals, when
  they move.
- `powerschool:<kid>:attendance_day` -- one (course, day) on which the
  student was marked with an attendance code; again if the code changes.
  Off by default.
- `powerschool:<kid>:term_grade` -- one stored final grade (course and
  completion month) from the grade history page; again if it is corrected.
  Off by default.
- `powerschool:<kid>:teacher_comment` -- a teacher's comment for a course
  and reporting term, when non-empty; again if it is edited. Off by
  default. **For the parents only** -- see below.

There is no fallback: an item with no matching rule is dropped.

Tags are the contract for downstream summarisers; see spec 16 section 7 for
the full tables. The ones that answer the usual questions:

| question | tags |
|---|---|
| what is missing | `surface=assignment`, `missing=true` |
| what is due tomorrow | `outstanding=true`, `due_day=<tomorrow>` |
| what is past due | `outstanding=true`, `due_day` before today |
| what was just graded | `surface=assignment`, `change=updated`, `scored=true` (`changed` contains `score`) |
| did a grade move | `surface=grade`, `change=updated`, `grade`, `previous_grade` |
| new absence or tardy | `surface=attendance`, `absences_delta`, `tardies_delta` |
| which day, which class | `surface=attendance_day`, `date`, `course`, `code`, `kind` (`absence_unexcused`, `late_excused`, `authorized`, ...) |
| final grade posted | `surface=term_grade`, `term` (completion month), `grade`, `credit_earned` |
| a teacher commented on one assignment | `surface=assignment`, `has_teacher_comment=true`; newly written or edited when `changed` contains `teacher_comment` (text in the body, under `Teacher comment:`) |
| a teacher wrote a term comment | `surface=teacher_comment`, `term`, `course` (text in the body) |

`outstanding` cannot tell "not handed in" from "handed in, not yet marked":
PowerSchool is a gradebook and only knows what the teacher entered.
`attendance` is per-course year-to-date counts; the dated records are
`attendance_day`. `term_grade` is the portal's grade history (stored final
grades with credit, no percentages); the in-progress quarter grades are
`grade`.

### Free text on an assignment item

Besides the gradebook facts, an `assignment` item's body carries up to
three blocks of teacher-authored text, each only when there is text for it:

```
Assignment: Density lab report
Course: Example Science 7
Category: Labs
Due: 2026-10-08
Status: graded
Score: 8/10
Percent: 80
Grade: B-

Teacher comment:
  Good work. Show the units next time.

Assignment description:
  Write up the density lab.
  Two pages.

Category description:
  Labs are 40% of the grade.
```

- **Teacher comment** is the note a teacher left on this student's score
  for this assignment (the notes icon of the portal's class score page). It
  comes from the same assignment-lookup response as the score: no extra
  request.
- **Assignment description** and **Category description** are the other two
  tabs of the portal's assignment detail dialog. The category description
  is left out when it says the same as the assignment description.

Tags: `has_teacher_comment` is always present (`true` / `false`);
`teacher_comment_truncated=true` is present only when the comment in the
body is not the whole comment; `changed` is present on an item staged for a
change (below). The text itself is never in a tag.

**Untrusted text.** Everything the portal returns was typed by a third
party. Nothing is decoded, expanded or acted on, and the layout is the
connector's, not the text's:

- A *fact* -- on every surface: each `Label: value` line of the body
  (titles, course, period and teacher names, grade cells, attendance codes
  and their legend text), every tag value, the subject and the sender -- is
  reduced to one line: every
  kind of line break becomes a space, runs of whitespace become one space,
  and the value is cut at 256 bytes. A title containing a line break
  followed by `Grade: A+` stays on the `Assignment:` line.
- A *text block* (the three above, and the term-level teacher comment,
  which is under a `Comment:` label) is the library's plain-text form --
  the comment's HTML form is never read -- with each line trimmed, runs of
  whitespace inside a line made one space, and blank lines at the ends
  dropped. Every line is indented by two spaces, and the connector's own
  labels are the only lines that start in column one, so text a teacher
  typed cannot pass for one of the connector's lines. A block is cut at
  4 KiB (4096 bytes, on a character boundary) and a cut block is followed
  by a line reading `[truncated]`.
- "Line break" means all of them: LF, CR, VT, FF, NEL (U+0085) and the
  Unicode line and paragraph separators (U+2028, U+2029). Format characters
  (bidirectional controls, zero-width spaces and joiners, the byte order
  mark) and all other control characters are removed.

**A new item per change.** A staged item gets a fresh id every time, and a
consumer that notices new items by id sees a new item only when the
assignment's fingerprint changes. The fingerprint has one part per row
below, and an item staged for a change names the parts that moved in its
`changed` tag (comma-joined, in this order):

| `changed` | covers |
|---|---|
| `details` | title, category name |
| `due_date` | due date |
| `status` | the status |
| `flags` | missing / late / incomplete / exempt / collected / absent |
| `score` | score, points possible, percent, letter |
| `teacher_comment` | the teacher's comment: added, edited or removed |
| `description` | the assignment description |
| `record` | only after an upgrade: the gradebook facts differ from an old-format checkpoint that cannot say which |

- Each part is taken over what the item **shows**: an edit that changes
  nothing visible -- invisible characters, trailing space, markup of the
  comment, text beyond the 4 KiB cut -- does not stage an identical item. A
  moved line break is a change.
- The **category description is not part of it.** It belongs to the
  category, and one edit would otherwise stage every assignment in that
  category again. The current text is shown whenever an assignment is
  staged for a reason of its own.
- **Old assignments.** An assignment outside `assignments.lookback_days`
  with no missing / late / incomplete flag is not staged on first sight,
  and is not staged again when nothing but its description is edited. Any
  other change stages it however old it is -- a teacher comment always
  does.

**Who can read it.** The comment is part of the assignment's own item, so
it has exactly that item's `data_subject` and `audience` -- those of the
`powerschool:<kid>:assignment` rule. With the sample rules
(`audience: [subject, guardians]`) that is the child who did the work and
the guardians. **Owner decision, 2026-10-11:** per-assignment teacher
feedback goes to the child and the guardians; term-level teacher comments
(the `teacher_comment` surface below) stay guardians-only. There is no
separate switch for the per-assignment comment; it follows the kid's
`assignment` rule.

This cleaning is applied to what is rendered only. The checkpoint keys
and fingerprints of the `grade`, `attendance`, `attendance_day`,
`term_grade` and `teacher_comment` surfaces are still taken over the text
as read, exactly as before, so the change stages nothing that was already
delivered.

**Upgrading.** Checkpoints in the first release's format (one hash, no
text) are recognised: on the first poll after the upgrade an assignment
whose gradebook facts have not moved is re-baselined without being staged,
unless it already carries a teacher comment, which is staged once
(`change=updated`, `changed=teacher_comment`) because it has never been
delivered.

### The library version

The connector runs on exactly one `powerschool-go` version, the one whose
source was read for requests, POST paths and form fields: `v0.4.0`, held in
`reviewedLibraryVersion` (`transport.go`). At startup `CheckLibrary` reads
the version linked into the binary from its build information and refuses
to start unless it is that version. It fails closed:

- another version, a pseudo-version or `(devel)`: refused;
- the module replaced by a `replace` directive, to a directory or to
  anything else, whatever version the replacement claims: refused;
- a binary with no build information, or one that does not list the
  library: refused. `go build`, `go run` and `go test` in this module all
  record it, so an ordinary build is never in this case. There is no flag
  or environment variable that skips the check.

To change the library: read the diff between the two versions for any new
request, POST path or form field; then raise the `go.mod` requirement and
`reviewedLibraryVersion` in the same commit and say what was checked.
`TestReviewedLibraryVersion` fails CI when the two disagree. Independently,
`CheckLibrary` also refuses to start if the library's two POST endpoints
are not the ones the transport allowlists, and the transport refuses
anything else at run time whatever the library does.

v0.3.0 to v0.4.0 was reviewed for this connector: it adds response decoding
only (the score comment and the category description from the lookup
response the library already requested) -- no request, no POST path, no
form field.

### Term-level teacher comments are for the parents only

Route `powerschool:<kid>:teacher_comment` with `data_subject` = that kid and
`audience: ["guardians"]`. The sample config and the chart defaults do.

This is an allowlist. Startup **refuses** any rule that can match a
`teacher_comment` key -- the exact rule or a `*` rule -- unless its audience
is set and every token is exactly `guardians`: `subject`, `household`,
`public`, `siblings`, `caregivers` or anything else is an error, alone or
next to `guardians`. A broader rule may not come before the exact one
(rules are first-match-wins). This holds even when the surface is off. As a
last line, an item whose resolved audience is not exactly `["guardians"]`
is not staged. The teacher's name appears in the item body, never in a
tag.

**The surface ships disabled** in the sample config and the chart, and must
stay off until a real, non-empty teacher comment has been parsed from a
live page -- none was posted when the page was verified.

### The three page surfaces

`attendance_day`, `term_grade` and `teacher_comment` each read one more
portal page per child (`attendance.html`, `termgrades.html`,
`teachercomments.html?fg=<term>` -- the last once per reporting term). They
are off unless switched on under `surfaces`, because they add requests.
The page structure they rely on has been verified against the live portal;
the connector code has so far run against synthetic fixtures only.

They are read inside the same verified per-child turn as everything else.
Every one of those pages carries the portal's student switcher, so every
page says itself whose it is: the connector requires each page's selected
student id to equal the child being read, and a page that marks the other
child (or nobody) discards that child's whole poll. Spec 16 sections 6.3
and 7.4.

`attendance_day` tags each day with the portal's own `code`, `category` and
`code_description` (the legend is read from the page) and a coarse `kind`
derived from the category:

| kind | legend categories (default) |
|---|---|
| `absence_excused` | `EA` |
| `absence_unexcused` | `UA`, `A` |
| `late_excused` | `EL` |
| `late_unexcused` | `UL`, `L` |
| `left_early_excused` | `LE` |
| `left_early_unexcused` | `LU` |
| `authorized` | `AUTH` (field trip, elsewhere in the building: not an absence) |

Override with `attendance_days.kinds`. A category in no kind is staged with
`kind=unknown`. The grid's `-` and `.` cells are not attendance codes and
are never staged; `powerschool_attendance_non_event_cells_total` counts
them.

A comments page is staged only when the page itself marks the reporting
term it shows (`teacher_comments.require_term_confirmation`, default
`true`); the portal does mark it. No teacher comment was posted in any term
when this was checked, so until one appears the surface stages nothing.

## Enabling in the Helm chart

`powerschool` uses the generic `connectors:` map.

```yaml
connectors:
  powerschool:
    enabled: true
    config:
      kids:
        - { name: k1, student_id: "111111" }
        - { name: k2, student_id: "111112" }
      rules:
        - { match: "powerschool:k1:grade",      destination: school, data_subject: e_333333, audience: [subject, guardians] }
        - { match: "powerschool:k1:assignment", destination: school, data_subject: e_333333, audience: [subject, guardians] }
        - { match: "powerschool:k1:attendance", destination: school, data_subject: e_333333, audience: [subject, guardians] }
        # the page surfaces, if switched on:
        - { match: "powerschool:k1:attendance_day",  destination: school, data_subject: e_333333, audience: [subject, guardians] }
        - { match: "powerschool:k1:term_grade",      destination: school, data_subject: e_333333, audience: [subject, guardians] }
        - { match: "powerschool:k1:teacher_comment", destination: school, data_subject: e_333333, audience: [guardians] }  # parents only
        # ... the same six for k2 with its own data_subject
    externalSecret:
      enabled: true
      secretStoreRef: { kind: ClusterSecretStore, name: vault-default }
      data:
        POWERSCHOOL_HOST: { key: eso/glovebox-powerschool, property: powerschool_hostname }
      files:                      # mounted, not env: updated in place
        mountPath: /etc/powerschool
        data:
          session.json: { key: glovebox/powerschool/default/session, property: session }
```

`POWERSCHOOL_SESSION_FILE=/etc/powerschool/session.json` is set by the
chart's default `env`.

The `session.json` reference is the chart's default and must be the path
the login job writes (`powerschoolAuthRefresher.vault.sessionPath`, property
`session`); with both enabled the chart refuses to render if they differ.
Enable the login job in the same change:
[powerschool-auth-refresher.md](powerschool-auth-refresher.md#values).

The remote keys and property names are yours to change. Do **not** map the
account's username or password into this connector. The rule destinations
must be in `scanner.agentAllowlist`. State is a PVC: the checkpoints are
per-record fingerprints, and losing them re-stages every current grade and
recent assignment.

## Testing

```
go test -race ./connectors/powerschool/...
```

Unit tests use a hand-rolled fake client with the portal's selection state
(`helpers_test.go`). The read-only wiring, the student switch and whole
polls are also tested by driving the real `powerschool-go` client through
the production transport against a stateful loopback TLS server
(`transport_test.go`, `transport_switch_test.go`). All ids, names and
fixtures are synthetic.


