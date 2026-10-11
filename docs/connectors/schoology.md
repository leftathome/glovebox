# schoology connector

Polls the Schoology LMS for each configured child's assignments, feed, and
messages (with attachments) and stages new items for scanning. This is the
reference **credentialed** connector: it reads the operator's real household
account read-only, driven from a saved browser session.

| | |
|---|---|
| Image | `ghcr.io/leftathome/glovebox-schoology` |
| Credential class | `real-readonly` (household Schoology session) — see [integration-credentials.md](integration-credentials.md) |
| Enricher runtime | no (distroless) |
| Live integration test | `connectors/schoology/live_integration_test.go` (`//go:build integration`, reference credentialed live test) |

## Authentication

Schoology has no first-class API key, so the connector authenticates with a
saved browser **session**. At startup `cmd/schoology/main.go` reads:

- `SCHOOLOGY_HOST` — the tenant subdomain (e.g. `yourschool.schoology.com`).
- `SCHOOLOGY_CREDENTIALS_FILE` — path to a JSON credentials file written by
  `schoology-go`'s `auth.SaveCredentials`, holding the session fields
  `SessID` / `CSRFToken` / `CSRFKey` / `UID`.

These are loaded via `schoologyauth.LoadCredentials` and passed to
`schoologylib.NewClient(host, WithSession(...))`. Any missing/invalid value is
a permanent startup error that exits non-zero and points at
`docs/AUTH-RECOVERY.md`.

The session JSON is **produced by the separate `schoology-auth-refresher`**
(an auth helper, not a source connector), which drives Browserless to log in
and refresh the cookie, writes the 5-field session to Vault, and lets ESO sync
it into the connector pod. The connector's own read path does **not** use
Browserless — it only consumes the materialized session. The secret is
provisioned in Vault and reaches the connector via an ESO `ExternalSecret`; no
secret values live in this repo. Vault path: TBD (private ci-templates).

Optional environment: `SCHOOLOGY_TIMEZONE` (timezone of the poll schedule and
of the wall-clock due/posted times Schoology prints on an assignment page,
default `America/Los_Angeles`) and `SCHOOLOGY_TRIGGER_TOKEN` (bearer token for the
`POST /v1/poll` trigger endpoint). The live test additionally reads
`SCHOOLOGY_KID_UID`, a real child UID kept in env (not committed) since it is
account-specific PII.

## Read-only guarantee

The session belongs to a real parent account, which upstream can post,
message and submit. The connector cannot: every request it makes goes through
`connector.ReadOnlyTransport`, which refuses any HTTP method other than
GET/HEAD before it leaves the process (`connector/readonly.go`). Agents never
receive the session or the credentials -- glovebox hands them scanned files
only. Any future write capability (replying to a teacher, say) must be a
separate, explicitly labelled workload that acts visibly as the account owner;
it must not loosen this connector.

`SCHOOLOGY_HOST` may be a bare host or a full `https://` URL; both binaries
normalise it (`schoology.NormalizeHost`).

## Configuration

Sample:
[`connectors/schoology/config.json`](../../connectors/schoology/config.json).
This is the effective config the integration test drives (through the
connector's normal `ApplyDefaults` + `ValidateConfig` path).

Framework fields (from `connector.BaseConfig`, shared by every connector):

| field | type | required | description |
|-------|------|----------|-------------|
| `rules` | `[]Rule` | yes | routing rules; each has `match`, `destination`, optional `tags`, `data_subject`, `audience` |
| `identity` | object | no | `provider` / `auth_method` / `tenant` identity stamped on staged items |
| `fetch_limits` | object | no | `per_source` / `per_poll` caps (0 = unlimited) |
| `data_subject_default` | string | no | default data subject when a rule sets none |
| `audience_default` | `[]string` | no | default audience when a rule sets none |

Connector-specific fields:

| field | type | required | description |
|-------|------|----------|-------------|
| `kids` | `[]{name,schoology_uid}` | yes | children to poll. `name` is an operator-chosen opaque label (`k1`, `k2`) — never a legal name, to keep PII out of metadata/audit logs — and is referenced by `schoology:<name>:*` rules. `schoology_uid` is the child's numeric Schoology UID (a `0` UID fails `ValidateConfig`). At least one kid required; names must be unique |
| `poll_schedule` | object | yes | `weekdays_only` (bool) + `windows` (`[]{start,end}`) daily polling windows. Times are `HH:MM` local-time strings in the connector timezone. At least one window required; each window's `end` must be strictly after its `start` |
| `trigger` | object | no | HTTP trigger endpoint. `debounce_seconds` (default 60) + `listen_port` (default 8081) |
| `attachments` | object | no | `max_size_mb` caps per-attachment download size (default 25) |
| `parse_failure_threshold` | int | no | consecutive parse failures tolerated before schema-drift escalation (default 10) |
| `assignment_detail` | object | no | the assignment detail surface, off by default; see [Assignment detail](#assignment-detail). `kids` (`[]string`, labels from `kids`; empty = off), `max_per_poll` (default 20, max 200), `refresh_hours` (default 20), `request_spacing_ms` (default 1500, 1000-60000) |

```json
{
    "kids": [
        {"name": "k1", "schoology_uid": 11111111},
        {"name": "k2", "schoology_uid": 22222222}
    ],
    "poll_schedule": {
        "weekdays_only": true,
        "windows": [
            {"start": "07:00", "end": "09:00"},
            {"start": "15:30", "end": "17:30"}
        ]
    },
    "trigger": {
        "debounce_seconds": 60,
        "listen_port": 8081
    },
    "attachments": {
        "max_size_mb": 25
    },
    "parse_failure_threshold": 10,
    "rules": [
        {"match": "schoology:k1:assignment",        "data_subject": "e_333333", "audience": ["household"],            "destination": "school"},
        {"match": "schoology:k1:feed",              "data_subject": "e_333333", "audience": ["subject", "guardians"], "destination": "school"},
        {"match": "schoology:k1:attachment",        "data_subject": "e_333333", "audience": ["subject", "guardians"], "destination": "school"},
        {"match": "schoology:k2:assignment",        "data_subject": "e_444444", "audience": ["household"],            "destination": "school"},
        {"match": "schoology:k2:feed",              "data_subject": "e_444444", "audience": ["subject", "guardians"], "destination": "school"},
        {"match": "schoology:k2:attachment",        "data_subject": "e_444444", "audience": ["subject", "guardians"], "destination": "school"},
        {"match": "schoology:message",                                          "audience": ["guardians"],           "destination": "school"},
        {"match": "schoology:message-attachment",                               "audience": ["guardians"],           "destination": "school"},
        {"match": "schoology-parse-failure:*",                                  "audience": ["guardians"],           "destination": "school"}
    ],
    "identity": {
        "provider": "schoology",
        "auth_method": "session_cookie",
        "tenant": "example-home"
    }
}
```

## Routing

Match keys emitted by this connector:

- `schoology:<kid>:assignment` — an assignment for the named kid.
- `schoology:<kid>:assignment_detail` -- what the assignment's own page says
  (only when the surface is enabled for that kid; see
  [Assignment detail](#assignment-detail)).
- `schoology:<kid>:feed` — a feed/course-update item for the named kid.
- `schoology:<kid>:attachment` — an attachment attached to the kid's content.
- `schoology:message` — an inbox message (account-level, not per kid).
- `schoology:message-attachment` — an attachment on an inbox message.
- `schoology-parse-failure:*` — a parse-failure receipt (schema-drift signal).

Each matched rule sets the `destination` agent and, optionally,
`data_subject` / `audience` for privacy-aware routing. Per-kid rules carry
`data_subject` so each child's school data routes under its own subject.

## Assignment detail

The `assignment` item is one list row: title, course, due date, status. The
`assignment_detail` surface reads each listed assignment's own page and
stages what a parent can see there as a second item, linked to the first by
the `assignment_id` tag. It is **off by default** and enabled per kid.

Schoology does not show a parent account scores, grades or submission state
on these pages, so the item has none. Grades come from the PowerSchool
connector.

### Enabling

List the kid under `assignment_detail.kids` and give it its own rule, placed
ahead of any `"*"` rule:

```json
"assignment_detail": {"kids": ["k1"], "max_per_poll": 20},
"rules": [
    {"match": "schoology:k1:assignment_detail", "data_subject": "e_333333", "audience": ["household"], "destination": "school"}
]
```

`ValidateConfig` refuses to start when an enabled kid has no rule that names
its key exactly. A wildcard rule would route the item under the config-wide
default subject, not the child's.

### Item shape

| field | value |
|-------|-------|
| `source` | `schoology` |
| `sender` | the course title (the detail page's, which names the section; the list row's when the page shows none) |
| `subject` | the assignment title |
| `content_type` | `text/plain` |
| `data_subject`, `audience`, `destination_agent` | from the kid's `schoology:<kid>:assignment_detail` rule |

Tags. Those marked optional are left out when the page does not show the
field; they are never staged empty. Every value is reduced to one line and
to the staging limit of 1024 bytes; a value that had to be cut ends in
` [truncated]` and the item carries `metadata_truncated=true`. The same goes
for the subject and the sender.

| tag | value |
|-----|-------|
| `kind` | `assignment_detail` |
| `assignment_id` | the Schoology assignment id (the same id the `assignment` item is for) |
| `assignment_kind` | `assignment`, `assessment` or `unknown` |
| `comments_state` | `populated`, `empty`, `unavailable` or `unknown` |
| `comment_count` | number of top-level entries in the comment block (`0` unless populated) |
| `has_description` | `true` or `false` |
| `course` | optional; the course title |
| `category` | optional; grading category, e.g. `Homework` |
| `grading_period` | optional; as printed, usually a name and a date range |
| `due_date` | optional; RFC 3339 UTC, only when the printed date was parsed |
| `due_raw` | optional; the due date exactly as printed |
| `posted` | optional; RFC 3339 UTC when parsed, otherwise the text as printed |
| `assessment_status` | optional; an assessment's status line |
| `course_unverified` | `true` only when the list names a course and the page shows none, so the two could not be compared (the item is then also `parse_status=degraded`) |
| `metadata_truncated` | `true` only when a value above was cut to fit |
| `parse_status`, `parse_error`, ... | only on an item whose page had content the library could not understand (`parse_status=degraded`) |

Content is a plain-text projection (invented example):

```
Assignment: Chapter 3 questions
Kind: assignment
Course: Example Course A: Section 1
Folder: Unit One
Category: Homework
Period: Quarter 1: 2026-09-01 - 2026-11-06
Due: Friday, October 16, 2026 at 11:59 pm (2026-10-16T23:59:00-07:00)
Posted: Wed Sep 16, 2026 at 9:15 am (2026-09-16T09:15:00-07:00)
Link: /assignment/9100000001

Description:
----- BEGIN DESCRIPTION: untrusted teacher-authored text, each line prefixed "| " -----
| Read pages 40-52.
| Answer the four questions on the handout.
----- END DESCRIPTION -----

Teacher comments: 1 present (text not captured yet)
```

Lines for fields the page does not show are left out, except the due date:
an item with none says `Due: no due date listed`. `Status:` appears for
assessments. Nothing is attached.

**Teacher-authored text cannot pass for the connector's own lines.** Every
line that starts at column zero is written by the connector. The single-line
values (title, course, folder, category, period, status) have line breaks and
control characters removed. The description, which is the library's
plain-text rendering (its HTML is never staged), sits between the BEGIN and
END lines with every line prefixed `| `. A description that contains
`Status: graded`, `Teacher comments:` or the END line itself therefore shows
up as `| Status: graded` and so on.

**Comments are a count, not text, for now.** No page with a real comment had
been seen when this was written, so the library cannot tell a comment's
author, time and body apart (it reports the layout as unmapped). Until it
can, the item says only that comments exist and how many: `comments_state`,
`comment_count` and the one fixed line above. The text is neither staged nor
part of the item's identity, so a comment appearing or the count changing
stages a new item, and run-together text such as "2 minutes ago" turning into
"1 day ago" does not. When the library maps the layout the text is staged in
a framed block like the description's (`commentTextMapped` in
`assignment_detail.go` is the switch).

**Size.** The content is capped at 96 KiB and comment text at 16 KiB, on top
of the library's 64 KiB cap on the description. What is cut is the
description first; a marker line says so.

### New items and changes

Consumers that notice new work by item id get a **new item whenever the
staged content changes** -- an edited description, a moved due date, a new
status, a first or further teacher comment -- and nothing when it does not.
The connector hashes everything it stages for the item (sender, subject,
tags, content; not the staging time) and remembers, per kid and assignment,
the hash it last staged, under a checkpoint key of the form
`<kid>|detail|<assignment_id>|<hash>`. A page that changes and then changes
back is staged a third time.

### Attribution

The assignment page is the same whichever child the session is viewing, so
the page cannot say whose assignment it is. The kid comes only from the list
the id was found in:

- only ids from a kid's own overdue/upcoming list are ever requested;
- an id in two kids' lists is requested once and staged once per kid, each
  under that kid's rule;
- the page must agree with the list row on two things, or nothing is staged
  and a receipt says why:
  - the **course** (`course_mismatch`). The list prints a course as its name
    and the page as name plus section (`Example Course A` /
    `Example Course A: Section 1`), so one title may be the other followed
    by a `:` suffix; comparison ignores case and spacing.
  - the **title** (`title_mismatch`). The two pages print it identically, so
    only case and spacing are ignored.
- if the list names a course and the page shows none, the comparison cannot
  be made. The item is staged, tagged `course_unverified=true` and
  `parse_status=degraded`, with a `course_unverified` receipt, so a change
  to the page's breadcrumb is visible rather than silently waved through.

### Request budget

A detail page is read only when:

- the assignment's list row is new or changed since the last poll, or
- it is still listed and has not been read for `refresh_hours` (default 20).
  This is how a comment added later is noticed. The default is 20 rather
  than 24 so that it means "once a day": each poll lands at a random time
  inside its window, and with 24 a poll a few minutes earlier in the day
  than yesterday's would find the row not yet due and skip a day.

Reads are spaced `request_spacing_ms` apart (default 1500) and capped at
`max_per_poll` assignments per poll across all kids (default 20). A regular
assignment costs one request. An assessment costs two, because its comments
are on a separate page; a "not available" answer there is normal and is
staged as `comments_state=unavailable`.

Who goes first when the cap bites:

- new and changed rows before refreshes;
- among those, the kids take turns, one row each, and the kid who starts
  rotates from poll to poll, so one child's long list cannot use the cap up;
- within a kid, a row the cap deferred earlier goes ahead of rows that
  appeared later, wherever Schoology lists them;
- a refresh three intervals overdue is treated as new.

No request is started once the poll's context is cancelled, or when it could
not finish before the end of the poll window the poll is in (45 s is allowed
per request). What is left waits for the next poll.

An assignment that disappears from the list is remembered for three more
listings before it is forgotten, so one short or empty answer from Schoology
does not cause everything to be read again.

### Failures

| what happened | result |
|---------------|--------|
| auth answer for one assignment (the page or its comments page) | the session is checked with a request that involves no assignment. Refused too: the surface stops and the poll ends on the same rejected-session exit as the other surfaces ([AUTH-RECOVERY.md](../AUTH-RECOVERY.md)). Session fine: `unexpected_page` receipt for that assignment, retried after `refresh_hours`. Check could not be made: the surface stops for this poll and the session is not called dead |
| permission denied or not found for one assignment | receipt for that assignment (`error_class=permission` / `not_found`); the rest of the poll carries on; retried after `refresh_hours` |
| course or title does not match, or the page is for another id | receipt (`course_mismatch` / `title_mismatch` / `id_mismatch`); not staged; retried after `refresh_hours` |
| the id leads to some other kind of page, or to a body that is not HTML | receipt (`unexpected_page` / `parse`) for that assignment; the rest carry on; retried after `refresh_hours` |
| page content present but not understood (e.g. an unrecognised date format) | item staged with `parse_status=degraded`, plus a `row_parse` receipt. An absent field is not an error |
| comments page not understood | item staged with `comments_state=unknown`, plus a `comments_parse` receipt |
| network error, rate limit or HTTP 5xx | receipt; the surface stops for this poll and tries again next poll |
| staging unreachable or failing (ingest down, 5xx) | the surface stops at once without spending the rest of its requests; nothing is marked seen; everything is tried again next poll |
| staging rejects the item for good (400/413, metadata validation) | `stage_rejected` receipt; retried after `refresh_hours`, not every poll |
| every detail handled in a poll (three or more) was rejected or did not parse | one `surface_degraded` receipt and the poll counts toward the schema-drift threshold: this is what a change to Schoology's pages looks like |
| poll cancelled mid-request | nothing recorded: no receipt, no failure |

The schoology-go library reports an auth error for any answer that ends on
the site's front, home, parent or user page, because that is where an
expired session is sent. One withdrawn assignment that redirects there looks
the same, which is why an auth answer is confirmed before it is believed:
acting on it unconfirmed would mark a live session as rejected and exit the
process.

Receipts use the existing parse-failure receipt
(`schoology-parse-failure:assignment_detail`, falling back to
`schoology-parse-failure:*`) with `parser=assignment_detail`, one per kid and
error class per poll.

Metric: `schoology_assignment_detail_fetches_total{kid,outcome}` with outcome
`staged`, `unchanged`, `rejected`, `error` or `deferred`. Spans:
`schoology.poll.assignment_detail`, `schoology.lib.GetAssignment`,
`schoology.lib.GetAssignmentComments`, `schoology.lib.ValidateSession`.

## Enabling in the Helm chart

Schoology is **not** wired through the generic `connectors:` map. It has
dedicated templates driven by two top-level values keys: `schoologyConnector:`
(the Deployment + Service + ConfigMap) and `schoologyAuthRefresher:` (the
session-refresh CronJob + ServiceAccount + ESO `ExternalSecret`s). In a real
install both are enabled, because the connector consumes the session Secret the
refresher produces.

```yaml
schoologyConnector:
  enabled: true
  image:
    repository: ghcr.io/leftathome/glovebox-schoology
    tag: ""            # defaults to .Chart.AppVersion when empty
  # host: ""           # empty => read SCHOOLOGY_HOST from the refresher Secret
  timezone: America/Los_Angeles
  # Off while kids is empty. Each kid listed here also needs a
  # "schoology:<kid>:assignment_detail" rule below, ahead of the "*" rule.
  assignmentDetail:
    kids: []           # e.g. [k1, k2]
    maxPerPoll: 20
    refreshHours: 20
    requestSpacingMs: 1500
  config:
    rules:
      - { match: "schoology:k1:assignment", destination: school, data_subject: k1, audience: [operator] }
      - { match: "schoology:k1:feed",       destination: school, data_subject: k1, audience: [operator] }
      - { match: "schoology:message",       destination: school, data_subject: household, audience: [operator] }
      - { match: "*",                       destination: school, audience: [operator] }
    data_subject_default: household
    audience_default: [operator]
    kids:
      # REPLACE placeholder UIDs with each child's real Schoology UID before
      # enabling -- a 0 UID fails ValidateConfig at startup.
      - { name: k1, schoology_uid: 0 }
      - { name: k2, schoology_uid: 0 }
    poll_schedule:
      weekdays_only: true
      windows:
        - { start: "07:00", end: "19:00" }
    trigger: { debounce_seconds: 60, listen_port: 8081 }
    attachments: { max_size_mb: 25 }

# The session credentials this connector consumes are produced separately:
schoologyAuthRefresher:
  enabled: true
  schedule: "0 6 */12 * *"   # ~48h headroom before the 14-day cookie expiry
  image:
    repository: ghcr.io/leftathome/glovebox-schoology-auth-refresher
  vault:
    credentialsPath: "glovebox/schoology/default/credentials"  # {host,username,password}
    sessionPath: "glovebox/schoology/default/session"          # 5-field session JSON
  browserless:
    url: ""   # e.g. ws://browserless.browserless.svc.cluster.local:3000
```

The refresher drives Browserless, writes the session JSON to Vault, and ESO
syncs it into the connector pod as the `-schoology-session` Secret
(`SCHOOLOGY_CREDENTIALS_FILE` + `SCHOOLOGY_HOST`). Browserless belongs to the
refresher, not to the connector's read path.

## Integration test

Two tests cover this connector:

- `connectors/schoology/integration_test.go` — an offline, fake-driven
  integration test that exercises the production routing rules and stage
  round-trip against a fake Schoology client. Runs anywhere, no credentials.
- `connectors/schoology/live_integration_test.go` (`//go:build integration`) —
  the reference credentialed live test. It wires the real `schoology-go`
  client exactly as `cmd/schoology/main.go` does, from `SCHOOLOGY_HOST` /
  `SCHOOLOGY_CREDENTIALS_FILE` / `SCHOOLOGY_KID_UID`, and asserts a full stage
  round-trip. The session credentials only exist in-cluster (ESO), so it runs
  green only in the nightly/manual GitLab `integration` stage under
  `GLOVEBOX_INTEGRATION=1` and SKIPs cleanly everywhere else. Its CI job is
  `allow_failure: true` temporarily (a child's Schoology surfaces can be
  legitimately empty, and the session path is still being brought up); drop
  `allow_failure` once the ESO secret is wired and a min-content source is
  agreed.
