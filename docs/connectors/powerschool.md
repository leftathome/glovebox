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

Shipped **disabled**. It builds against the released
`github.com/leftathome/powerschool-go v0.2.0`, and the portal behaviour it
depends on has been verified live with the library. What is missing before
it can be enabled:

1. The **login job** that produces the session (below). Not built yet;
   tracked separately.
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

The session is produced by a separate, scheduled **login job** that holds
the credentials, drives the browser login and calls `Client.ExportSession`.
A missing, malformed or expired session is a startup error.

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
- A session rejected mid-day costs data only until the next off-hours
  login; the first poll on the new session stages everything that changed
  in between. It is not an incident and must not trigger a login.

### When the session is rejected

The connector records a fingerprint of the session (a hash, in
`rejected-session` under `GLOVEBOX_STATE_DIR`) and exits. The restarted
process does **not** present that session again. It waits, making no
upstream request at all, and re-reads the session file once a minute; when
the login job replaces the Secret the kubelet updates the mounted file in
place and the connector starts polling again without a pod restart.

While it waits the pod is **Running and NotReady** (`/healthz` 200,
`/readyz` 503), not crash-looping. `kubectl get pods` showing 0/1 Ready with
no restarts, and the log line `this session was already rejected upstream;
waiting for a new one`, mean "waiting for the next login", nothing more.

To learn how long sessions really last (the expiry in the session document
is a placeholder), the connector reports each rejected session's age: the
gauge `powerschool_rejected_session_age_seconds` and one INFO log line with
`session_age_seconds`. Use it to set the login job's schedule.

Optional: `POWERSCHOOL_TIMEZONE` (scheduler and due-date timezone, default
`America/Los_Angeles`).

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
| `surfaces` | object | no | `grades`, `assignments`, `attendance` booleans; each defaults to `true`. At least one must be on |
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
  or changed (new score, flag set or cleared, due date moved).
- `powerschool:<kid>:attendance` -- a course's absence/tardy totals, when
  they move.

There is no fallback: an item with no matching rule is dropped.

Tags are the contract for downstream summarisers; see spec 16 section 7 for
the full tables. The ones that answer the usual questions:

| question | tags |
|---|---|
| what is missing | `surface=assignment`, `missing=true` |
| what is due tomorrow | `outstanding=true`, `due_day=<tomorrow>` |
| what is past due | `outstanding=true`, `due_day` before today |
| what was just graded | `surface=assignment`, `change=updated`, `scored=true` |
| did a grade move | `surface=grade`, `change=updated`, `grade`, `previous_grade` |
| new absence or tardy | `surface=attendance`, `absences_delta`, `tardies_delta` |

`outstanding` cannot tell "not handed in" from "handed in, not yet marked":
PowerSchool is a gradebook and only knows what the teacher entered.
Attendance is per-course year-to-date counts, not dated records.

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
        # ... the same three for k2 with its own data_subject
    externalSecret:
      enabled: true
      secretStoreRef: { kind: ClusterSecretStore, name: vault-default }
      data:
        POWERSCHOOL_HOST: { key: eso/glovebox-powerschool, property: powerschool_hostname }
      files:                      # mounted, not env: updated in place
        mountPath: /etc/powerschool
        data:
          session.json: { key: eso/glovebox-powerschool-session, property: session }
```

`POWERSCHOOL_SESSION_FILE=/etc/powerschool/session.json` is set by the
chart's default `env`.

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


