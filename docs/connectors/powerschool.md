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

Shipped **disabled, and not deployable yet**:

1. It depends on an unreleased `powerschool-go` (branch
   `feat/readonly-session-client`). `go.mod` currently replaces the module
   with a pseudo-version on a private host, which CI and the image build
   cannot fetch. A release is needed first.
2. There is no login workload yet to produce the session.
3. The student switch and the dead-session handling have only been run
   against synthetic fixtures built to the live page's shape. One supervised
   run against the real portal is needed before enabling.

See spec 16 section 11 for the full list.

## Authentication

The connector never sees the account's username or password and never logs
in. At startup `cmd/powerschool/main.go` reads:

- `POWERSCHOOL_HOST` -- the portal location. A bare host or a full `https://`
  URL; the binary normalises it (`powerschool.NormalizeBaseURL`) and always
  uses https.
- `POWERSCHOOL_SESSION_FILE`, a path to a session document, or
  `POWERSCHOOL_SESSION`, the document itself (the file wins if both are
  set). It is the JSON form of `powerschool-go`'s `SessionExport`:
  `{"cookies":[...],"expires_at":"..."}`.

The session is produced by a separate login workload that holds the
credentials, drives the browser login and calls `Client.ExportSession`. A
missing, malformed or expired session is a startup error.

If the portal refuses the session while the connector is running, the
connector records a fingerprint of it (a hash, in `rejected-session` under
`GLOVEBOX_STATE_DIR`) and exits. The restarted process does **not** present
that session again: it waits, not ready and making no upstream request,
until the session source holds a different one. With the file form that
happens when the mounted Secret is updated; with the environment form the
pod has to be replaced.

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
explicit selection marker, read, check the marker again, and only then
stage. If the page carries no marker, nothing is posted and no kid is read.
If anything read for a kid disagrees about which student it belongs to,
everything read for that kid in that poll is discarded. Spec 16 section 6.1.

## Schedule

One poll per configured window, at a random time inside it. The framework's
periodic re-poll is disabled, and a restart polls only if a window opened
while the process was down. Spec 16 section 8.

## Configuration

Sample: [`connectors/powerschool/config.json`](../../connectors/powerschool/config.json).

Framework fields (from `connector.BaseConfig`) are as for every connector:
`rules`, `identity`, `fetch_limits`, `data_subject_default`,
`audience_default`.

Connector-specific fields:

| field | type | required | description |
|-------|------|----------|-------------|
| `kids` | `[]{name,student_id,student_number?}` | yes | children to poll. `name` is an opaque label (`k1`, `k2`) -- never a name -- with no `:` or whitespace. `student_id` is the numeric id from the portal's student switcher, as a string; `"0"` fails validation. Optional `student_number` is the school-issued number the portal page shows; when set, the selected student's number on the page must equal it or that kid is skipped. It identifies a real child: set it in private values only. Names, ids and numbers must be unique |
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
        POWERSCHOOL_HOST:    { key: eso/glovebox-powerschool, property: powerschool_hostname }
        POWERSCHOOL_SESSION: { key: eso/glovebox-powerschool-session, property: session }
```

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

Until `powerschool-go` is released, building needs access to the private
module host: `GOPRIVATE=gitlab.orac.local GOFLAGS=-mod=mod`.
