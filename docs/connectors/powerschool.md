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

Shipped **disabled, and not runnable yet**. Two gaps in the pinned
`powerschool-go` (v0.1.0) block it:

1. Its page reads go through a headless browser that the connector's
   read-only transport cannot see. The binary detects this and **exits at
   startup** rather than run without the guarantee. A fix is proposed in
   `powerschool-go` branch `fix/http-page-fetch`.
2. It cannot switch students. On a two-child account only the child the
   session opens on is read; the other is skipped every poll.

There is also no login workload yet to produce the session. See spec 16
section 11 for the full list, including what still needs checking against a
live portal.

## Authentication

The connector never sees the account's username or password and never logs
in. At startup `cmd/powerschool/main.go` reads:

- `POWERSCHOOL_HOST` -- the portal location. A bare host or a full `https://`
  URL; the binary normalises it (`powerschool.NormalizeBaseURL`) and always
  uses https.
- `POWERSCHOOL_SESSION` -- a session document, or `POWERSCHOOL_SESSION_FILE`,
  a path to one (the file wins if both are set). It is the JSON form of
  `powerschool-go`'s `SessionExport`: `{"cookies":[...],"expires_at":"..."}`.

The session is produced by a separate login workload that holds the
credentials, drives the browser login and calls `Client.ExportSession`. A
missing, malformed or expired session is a startup error. If the session
expires while running, the connector exits and the restarted container reads
whatever the Secret then holds.

Optional: `POWERSCHOOL_TIMEZONE` (scheduler and due-date timezone, default
`America/Los_Angeles`).

## Read-only guarantee

The session belongs to a real parent account. Every request goes through
`powerschool.Transport`, which:

- forwards only `https` requests to the configured host, so the session
  cookie cannot be carried anywhere else; and
- hands everything to `connector.ReadOnlyTransport`, which refuses any method
  but GET/HEAD before it leaves the process -- with **one exception**: when
  the assignments surface is enabled, a POST to exactly
  `/ws/xte/assignment/lookup` is forwarded. That endpoint is the query the
  portal's own scores page uses to list a section's assignments; there is no
  GET form of it. Set `surfaces.assignments: false` and the transport is
  strictly GET/HEAD.

The library client is built with a session and no credentials, so it cannot
re-authenticate. Agents never receive the session -- glovebox hands them
scanned files only.

## Configuration

Sample: [`connectors/powerschool/config.json`](../../connectors/powerschool/config.json).

Framework fields (from `connector.BaseConfig`) are as for every connector:
`rules`, `identity`, `fetch_limits`, `data_subject_default`,
`audience_default`.

Connector-specific fields:

| field | type | required | description |
|-------|------|----------|-------------|
| `kids` | `[]{name,student_id}` | yes | children to poll. `name` is an opaque label (`k1`, `k2`) -- never a name -- with no `:` or whitespace. `student_id` is the numeric id from the portal's student switcher, as a string; `"0"` fails validation. Names and ids must be unique |
| `poll_schedule` | object | yes | `weekdays_only` (bool) + `windows` (`[]{start,end}`), `HH:MM` local times. One poll at a random time inside each window |
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

Unit tests use a hand-rolled fake client (`helpers_test.go`). The read-only
wiring is tested by driving the real `powerschool-go` client through the
transport against a local TLS test server (`transport_test.go`). All ids,
names and fixtures are synthetic.
