# Schoology Connector -- Design Specification

**Version 1.0 -- May 2026**

*This document specifies the Schoology connector for the Glovebox content-scanning service. The connector ingests assignment, feed, and inbox-message content from a Schoology parent account on behalf of a household and emits per-kid staging items with `data_subject` and `audience` populated per spec 11 v1.2. Authentication is browser-session-cookie based (spec 06's pattern for unusual auth flows), polling is window-scheduled with splay plus an HTTP trigger, parse failures are preserved as quarantine-bound forensic receipts, and the connector implements the framework's `Connector` + `Watcher` + `Listener` interfaces.*

---

## 1. Purpose

Schoology is a K-12 LMS that schools use for assignments, teacher-to-parent
messages, class feed posts, and attachments. Parent accounts have read access
to their children's data; the open-source [schoology-go](https://github.com/leftathome/schoology-go)
library (v0.1.0) reads this content via browser-session cookies (the only
mechanism Schoology actually exposes to parent accounts).

This connector wraps the library to feed Schoology content into Glovebox's
scanning pipeline. The motivation, per the original kickoff: school content
arriving from a system with hundreds of bored middle schoolers and dozens of
overworked teachers is exactly the kind of content that benefits from
prompt-injection scanning before reaching downstream agents. Concretely: a
homework-helper agent operating on assignment text needs to be defended
against an assignment description that contains adversarial instructions, and
a household digest agent needs to know which teacher messages are for whom.

The connector is also the first to exercise spec 11's `data_subject` +
`audience` plumbing in production, the first to use spec 11 v1.2's `guardians`
+ `caregivers` vocabulary, and the first to depend on a small Glovebox
routing-layer enhancement for tag-based quarantine (see §12). Lessons from
this connector will likely yield a "connector primitive base type" abstraction
that PowerSchool, Walhelm, and future LMS-shaped connectors can inherit from
(§17.4).

## 2. Scope

### 2.1 In Scope

- Reading **assignments** (overdue + upcoming) per kid via
  `schoology.GetOverdueSubmissions(uid)`.
- Reading **faculty feed posts** per kid via `schoology.GetFeed(uid)`.
- Reading **inbox messages** (parent-level, not per-kid) via
  `schoology.GetInbox()`.
- Downloading **attachments** referenced by feed posts and messages via
  `schoology.DownloadAttachment(id)`, capped per-attachment at 25 MB, each
  emitted as its own staging item.
- **Browser-session-cookie authentication** with credentials provisioned via
  K8s Secret + External Secrets Operator + 1Password; session expiry surfaced
  as `PermanentError`.
- **Windowed polling** with random per-day splay (morning + dismissal
  windows) plus an HTTP **trigger endpoint** for on-demand polls.
- **Per-content-type checkpointing** for dedup; highest-ID strategy.
- **Parse-failure forensic preservation**: total failures emit deduped
  "parse failure receipts" to quarantine; partial failures emit "degraded"
  staging items.
- **Quarantine routing** via a small Glovebox routing-layer enhancement
  (§12) that interprets `parse_status` tags.
- **OpenTelemetry traces** for poll/library/staging operations, **Prometheus
  metrics** for poll counts/errors/durations/items, and **structured event
  logs** for human-readable diagnostics.

### 2.2 Out of Scope (Deferred or Never)

- **Grades + grade history.** The library's docs note that on the districts
  it was tested against, Schoology routes grade data to a separate SIS
  (presumably PowerSchool). The grade endpoints are stubbed in the library
  but return empty pages. Will be picked up by the PowerSchool connector
  (spec 13).
- **Calendar / events.** Same reason as grades; library deferred to a
  future contributor.
- **Outbound traffic** (posting messages, marking items read). Library is
  read-only in v0.1.0. The connector's `Listener` HTTP server is designed
  to grow outbound endpoints later (§9.4), but spec 12 v1 does not
  implement any.
- **Headless authentication** for unattended session refresh. The library's
  `auth.Login` is interactive (visible Chromium). `auth.LoginWithPassword`
  works headless ONLY for native-password tenants; SSO/MFA tenants need the
  visible flow. Tracked as a known followup (§17.1).
- **Per-kid schedule differentiation** (older kid polled more aggressively
  than younger). v1 has one schedule for all kids.
- **School-calendar awareness** (snow days, holidays, summer break).
  Connector polls on its weekday schedule regardless; quiet days produce
  empty polls. Trigger endpoint handles deliberate catch-ups.
- **Connector-side proxy extraction** of binary/XML attachment text for
  prompt-injection scanning. The cleaner home for that is a Glovebox-scanner-
  side capability that benefits all connectors uniformly; deferred to a
  separate spec (§17.2).
- **Cross-connector `data_subject` reconciliation.** Spec 11 §10.2 already
  defers this. PowerSchool will use the same `k1`/`k2` opaque labels via
  its own config; consolidation by `data_subject` value works because both
  configs map the same opaque label to that connector's specific upstream
  identifier.

## 3. Architecture Overview

The connector is a single Go binary, deployed as a single K8s Deployment per
household (one container handles all kids). It implements three of the
connector framework's interfaces (per spec 05):

```
                    +-----------------------------+
                    |  Schoology Connector pod    |
                    |                             |
                    |  Connector.Poll()  ◄── runner calls at startup
                    |       │                     |
                    |       ▼                     |
                    |     pollNow() ◄─────────┐   |
                    |       │                 │   |
                    |       ▼                 │   |
                    |     library calls       │   |
                    |     staging.Commit()    │   |
                    |       │                 │   |
                    |  Watcher.Watch() ───────┤   |
                    |  (window scheduler)     │   |
                    |                         │   |
                    |  Listener.Handler()  ───┘   |
                    |  POST /v1/poll              |
                    |                             |
                    +-----------------------------+
```

- **`Connector.Poll()`**: catch-up poll on startup. Drains anything posted
  while the pod was down. Framework's existing semantic.
- **`Watcher.Watch(ctx)`**: long-running scheduler. Goroutine that computes
  today's splay offsets within the configured windows, sleeps until each, and
  calls `pollNow()` at the splayed times.
- **`Listener.Handler()`**: HTTP server exposing `POST /v1/poll`. Bearer-
  token auth, 60-second debounce. Triggers `pollNow()` out-of-band.

All three entry paths converge on a single internal `pollNow()` function so
behavior cannot drift between scheduled, catch-up, and triggered polls. The
function iterates kids and content types, calls the library, dedupes via
checkpoint, stages items per spec 11 v1.2 metadata schema, advances
checkpoint after each successful `Commit()`.

### 3.1 Data flow per poll

```
pollNow()
  ├─ for each kid in config.kids:
  │    library.switchToChild(kid.SchoologyUID)   // mutex-serialized in library
  │    ├─ library.GetOverdueSubmissions(uid)
  │    │    for each assignment with ID > checkpoint:
  │    │      stage(match_key="schoology:<kid.Name>:assignment", ...)
  │    │      checkpoint.Save("assignment:<kid.Name>:last_id", id)
  │    └─ library.GetFeed(uid)
  │         for each feed post with ID > checkpoint:
  │           stage(match_key="schoology:<kid.Name>:feed", ...)
  │           for each attachment in post.Attachments:
  │             library.DownloadAttachment(id)
  │             stage(match_key="schoology:<kid.Name>:attachment", ...)
  │           checkpoint.Save("feed:<kid.Name>:last_id", id)
  └─ library.GetInbox()                          // parent-level; no kid switch
       for each message with ID > checkpoint:
         stage(match_key="schoology:message", ...)
         for each attachment in message.Attachments:
           library.DownloadAttachment(id)
           stage(match_key="schoology:message-attachment", ...)
         checkpoint.Save("message:last_id", id)
```

Each `stage()` is a `StagingWriter.NewItem()` + `WriteContent()` + `Commit()`
triple, with `MatchResult` from the rule matcher providing `Destination`,
`DataSubject`, and `Audience`. `Commit()` validates the metadata against
spec 11 v1.2 rules.

## 4. Configuration

### 4.1 Environment Variables

| Variable | Required | Purpose |
|---|---|---|
| `GLOVEBOX_STAGING_DIR` | yes | Standard framework: path to staging directory (or use `GLOVEBOX_INGEST_URL` for HTTP-staging mode per spec 08). |
| `GLOVEBOX_STATE_DIR` | yes | Standard framework: path to checkpoint state. |
| `GLOVEBOX_CONNECTOR_CONFIG` | no | Path to JSON config file. Default `/etc/connector/config.json`. |
| `SCHOOLOGY_CREDENTIALS_FILE` | yes | Path to the JSON file containing `schoology-go`'s `auth.Credentials` (4 session values). Operator creates via `auth.Login` on workstation, syncs via ESO/1Password. |
| `SCHOOLOGY_HOST` | yes | Schoology tenant host, e.g. `seattleschools.schoology.com`. |
| `SCHOOLOGY_TRIGGER_TOKEN` | yes | Shared secret for the `POST /v1/poll` trigger endpoint. K8s Secret + ESO + 1Password. |
| `SCHOOLOGY_TIMEZONE` | no | Schedule timezone, default `America/Los_Angeles`. |

### 4.2 config.json Shape

```json
{
    "kids": [
        {"name": "k1", "schoology_uid": 12345678},
        {"name": "k2", "schoology_uid": 12345679}
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
        {"match": "schoology:k1:assignment",        "data_subject": "k1", "audience": ["household"],         "destination": "school"},
        {"match": "schoology:k1:feed",              "data_subject": "k1", "audience": ["household"],         "destination": "school"},
        {"match": "schoology:k1:attachment",        "data_subject": "k1", "audience": ["household"],         "destination": "school"},
        {"match": "schoology:k2:assignment",        "data_subject": "k2", "audience": ["household"],         "destination": "school"},
        {"match": "schoology:k2:feed",              "data_subject": "k2", "audience": ["household"],         "destination": "school"},
        {"match": "schoology:k2:attachment",        "data_subject": "k2", "audience": ["household"],         "destination": "school"},
        {"match": "schoology:message",                                    "audience": ["guardians"],         "destination": "school"},
        {"match": "schoology:message-attachment",                         "audience": ["guardians"],         "destination": "school"},
        {"match": "schoology-parse-failure:*",                            "audience": ["guardians"],         "destination": "school"}
    ],
    "identity": {
        "provider": "schoology",
        "auth_method": "session_cookie",
        "tenant": "wagner-home"
    }
}
```

Note `schoology-parse-failure:*` is a catch-all rule for parse-failure
receipts (§11.3); the receipt's `Source` is `schoology-parse-failure`, so
this rule matches it.

### 4.3 Per-Kid Opaque Labels

The `kids[].name` field carries an operator-chosen opaque label per kid
(e.g., `k1`, `k2`). This string becomes the item's `data_subject` value
on disk and in the audit log. Spec 11 §3.3 explicitly allows any
free-form identifier. Operators SHOULD avoid family nicknames or legal
names in this field (PII reduction); the upstream Schoology UID stays
in config only and is not exfiltrated into the data plane. Downstream
agents maintain their own kid-name display registries.

PowerSchool (spec 13) will use the same convention with its own UID
mapping (`{"name": "k1", "powerschool_id": "..."}`) so that
`data_subject: "k1"` matches across connectors without an explicit
reconciliation layer.

## 5. Authentication and Credentials

### 5.1 Auth Flow

The schoology-go library uses browser-session cookies. There is no OAuth,
no API token, no service-account flow — Schoology simply does not expose
those to parent accounts. Cookies expire approximately every 14 days; the
library does not auto-refresh.

**Initial setup (operator action, one-time per session)**:

1. Operator runs `schoology-go auth.Login <tenant-host>` on their
   workstation. A visible Chromium window opens. Operator completes
   whatever login flow the school uses (SSO, MFA, native password).
2. Library captures the session and writes `auth.Credentials` JSON to
   disk (4 values, 0600 file).
3. Operator updates the 1Password item `schoology-session-<household>` with
   the new JSON.
4. ESO syncs the secret to the K8s Secret within ~60s.
5. K8s pod auto-restarts (pod template uses a checksum annotation on the
   Secret) and picks up the new credentials.

**Runtime (connector behavior)**:

- Pod mounts the Secret at `/etc/schoology/credentials.json` read-only,
  mode 0600.
- Connector reads the JSON at startup via the library's
  `auth.LoadCredentials(path)`.
- Connector builds a `*schoology.Client` and starts polling.

**Session expiry (failure mode)**:

- Library detects expired sessions via several signals:
  - `DownloadAttachment` returning `Content-Type: text/html` (login redirect)
  - 401/403 HTTP responses
  - Sentinel error from library functions
- Connector treats any of these as `PermanentError` with the recovery
  message (§11.1).
- K8s reports `CrashLoopBackOff`. External alerting catches it. Operator
  re-runs the initial setup flow.

### 5.2 Identity Block

Every item emitted by this connector carries the following identity per
spec 06 §5:

```json
"identity": {
    "provider": "schoology",
    "auth_method": "session_cookie",
    "account_id": "<parent's Schoology user_id from credentials>",
    "tenant": "<from config.identity.tenant>"
}
```

The `account_id` is the parent's own Schoology UID (the account whose
session is in use). Per spec 11 §3.3, this is distinct from `data_subject`
(which names the kid the item is about). Parent-credential-fetching-child-
data was the motivating use case for the spec 11 schema; this is the first
production use of that distinction.

`auth_method: "session_cookie"` is a new value not in spec 06's initial
enum. Spec 06 §5.2 explicitly states the enum is open and new methods may
be added; this is in-scope.

## 6. Polling Schedule and Trigger

### 6.1 Scheduled Polling

Two daily windows on weekdays (Mon-Fri), one splayed poll per window:

- **Morning**: 07:00 - 09:00 local time
- **Dismissal**: 15:30 - 17:30 local time

**Splay**: each day, when the scheduler decides "today's morning poll
time", it picks a uniform random second within the 7200-second window.
Same for dismissal. Times re-randomize at midnight local-tz. No two
days have the same exact-minute poll cadence; reduces bot-pattern risk
and avoids minute-of-day pileups if Schoology has request rate ceilings.

**Weekend behavior**: scheduled polls skipped. Use the trigger endpoint
(§6.2) for Sunday catch-ups.

**Multi-day downtime**: catch-up poll on next startup drains anything
posted while the pod was down. Checkpoint advances per-item, so partial
catch-up failures don't waste recovered work.

### 6.2 Trigger Endpoint

HTTP server bound on `127.0.0.1:<listen_port>` by default (operator can
expose externally via K8s Service if desired):

```
POST /v1/poll
  Headers:
    Authorization: Bearer <SCHOOLOGY_TRIGGER_TOKEN>
  Responses:
    202 Accepted          { "poll_queued_at": "<timestamp>" }
    401 Unauthorized      missing or wrong bearer token
    429 Too Many Requests Retry-After: <seconds remaining in debounce>
```

**Debounce**: minimum 60 seconds between accepted triggers. Tracked
in-process; debounce state lost on pod restart (intentional — restart is
itself effectively a poll trigger via Connector.Poll).

**Async semantics**: response returns immediately (202 Accepted) on
queue; the poll runs in the background via channel-signal to the Watcher
goroutine.

**Future direction** (out of scope for v1): this same endpoint pattern
is where outbound endpoints will live as the schoology-go library grows
write surfaces — `POST /v1/messages` etc. Spec 12 v1 only implements
`/v1/poll`.

### 6.3 Trigger Interaction with Scheduled Polls

When a trigger fires during a sleeping Watcher (between scheduled
windows or mid-window before the splay time):

- The Watcher's select loop receives the trigger signal and calls
  `pollNow()` immediately.
- The pending-splay sleep is cancelled (replaced by a fresh sleep to the
  *next* window after the trigger completes).
- The window whose splay time was preempted does NOT get a second poll;
  the trigger consumed that window's poll.

This keeps the per-day poll count bounded predictably even with
trigger-heavy operator behavior.

## 7. Content Surfaces and Item Granularity

### 7.1 Assignments

Library call: `schoology.GetOverdueSubmissions(uid)`. Returns the kid's
overdue + upcoming assignments.

Per-item staging shape:

| Field | Value |
|---|---|
| `Source` | `"schoology"` |
| `Sender` | `"<course title> -- <teacher name>"` |
| `Subject` | assignment title |
| `Timestamp` | assignment creation time if exposed by library, else `now()` |
| `DestinationAgent` | `"school"` (from rule) |
| `ContentType` | `"text/plain"` |
| `content.raw` | assignment description + due-date prose (HTML→plaintext) |
| `Tags` | `course: "<course title>"`, `due_date: "<ISO 8601>"`, `status: "overdue"|"upcoming"` |
| `DataSubject` | the kid's opaque label (from rule) |
| `Audience` | `["household"]` (from rule) |

Match key: `schoology:<kid>:assignment`.

### 7.2 Feed Posts

Library call: `schoology.GetFeed(uid)`. Returns faculty posts to the
kid's enrolled course sections.

Per-item staging shape:

| Field | Value |
|---|---|
| `Source` | `"schoology"` |
| `Sender` | poster's name (teacher or staff) |
| `Subject` | post title |
| `Timestamp` | post creation time |
| `DestinationAgent` | `"school"` |
| `ContentType` | `"text/plain"` |
| `content.raw` | post body (HTML→plaintext via library's `content.HTMLToText`) |
| `Tags` | `course: "<course title>"`, `post_type: "<library-supplied>"` |
| `DataSubject` | the kid's opaque label |
| `Audience` | `["household"]` |

Match key: `schoology:<kid>:feed`.

### 7.3 Inbox Messages

Library call: `schoology.GetInbox()` -- parent-level, NOT per-kid. The
library does not tell us which kid a message is about. Cross-referencing
sender against per-kid course lists is feasible but fragile (substitute
teachers, multi-kid coverage); deferred.

Per-item staging shape:

| Field | Value |
|---|---|
| `Source` | `"schoology"` |
| `Sender` | sender's display name |
| `Subject` | message subject line |
| `Timestamp` | message receipt time |
| `DestinationAgent` | `"school"` |
| `ContentType` | `"text/plain"` |
| `content.raw` | message body |
| `Tags` | `thread_id: "<schoology id>"`, optionally `attachment_count: "<N>"` |
| `DataSubject` | (omitted) |
| `Audience` | `["guardians"]` (standalone per spec 11 v1.2 §3.5) |

Match key: `schoology:message`.

The `[guardians]` standalone audience (introduced in spec 11 v1.2) means
"the household's guardians" -- adults responsible for the household, not
specific to one kid. Kids do not see inbox messages by default; this
matches the real-world expectation that teacher-to-parent communications
are between the educator and the responsible adults.

### 7.4 Attachments

Library call: `schoology.DownloadAttachment(id)`. Returns content stream
+ MIME type.

For each attachment referenced by a feed post or message, the connector:

1. Calls `DownloadAttachment`.
2. Checks size against `attachments.max_size_mb` (default 25). If
   exceeded: skip with `slog.Warn`, emit a tag on the parent item
   (`attachment_skipped: "size_exceeded"`), increment
   `schoology_attachments_skipped_total{reason="too_large"}`.
3. Otherwise emits its OWN staging item:

| Field | Value |
|---|---|
| `Source` | `"schoology"` |
| `Sender` | parent post/message sender |
| `Subject` | `"<parent subject> — <attachment filename>"` |
| `Timestamp` | parent's timestamp |
| `DestinationAgent` | `"school"` |
| `ContentType` | library-supplied MIME type |
| `content.raw` | raw attachment bytes |
| `Tags` | `parent_id: "<id>"`, `parent_type: "feed"|"message"`, `filename: "<name>"`, `size_bytes: "<n>"` |
| `DataSubject` | parent's `DataSubject` (kid label) or empty for message attachments |
| `Audience` | parent's audience (`[household]` for feed attachments, `[guardians]` for message attachments) |

Match keys: `schoology:<kid>:attachment` for feed attachments,
`schoology:message-attachment` for message attachments.

The scanner cannot deeply inspect most attachment types in v1 (PDFs,
Word docs, images). A Glovebox-scanner-side string-extraction capability
is deferred (§17.2) and applies uniformly to all connectors when it
lands.

## 8. Checkpoint and Dedup

### 8.1 Checkpoint Keys

Per-content-surface state via the framework's `Checkpoint`:

| Key | Value | Purpose |
|---|---|---|
| `assignment:<kid>:last_id` | string (highest-seen Schoology ID) | Assignment dedup per kid |
| `feed:<kid>:last_id` | string (highest-seen Schoology ID) | Feed dedup per kid |
| `message:last_id` | string (highest-seen Schoology ID) | Message dedup (parent-level) |
| `feed-attachment:<kid>:last_id` | string | Feed attachment dedup per kid |
| `message-attachment:last_id` | string | Message attachment dedup |

Per the framework rule, checkpoint advances only after successful
`Commit()`. Per-(kid, content-type) keys mean a failure in one
iteration does not stall the others.

### 8.2 Dedup Strategy

Highest-ID strategy. On each poll, iterate library response from newest
to oldest; stop when current ID ≤ `last_id`; for IDs > `last_id`, stage
the item and advance the checkpoint.

**Risk**: Schoology IDs *should* be monotonic but might not always be
(backdated edits, etc.). v1 mitigation: log a warning when the library
returns an ID below the threshold; metric
`schoology_items_dropped_total{reason="below_checkpoint"}`. If observed
in practice, upgrade to a hybrid (last_id + recent-set of last N IDs).

## 9. Audience and Rule Configuration

Rules in `config.json` map match keys to destinations and audience tokens.
See §4.2 for the full canonical rule set. Pattern summary:

- **Per-kid surfaces**: `schoology:<kid>:assignment`, `schoology:<kid>:feed`,
  `schoology:<kid>:attachment` → `data_subject: <kid>`, audience as
  configured per content type. Default `[household]`.
- **Parent-level**: `schoology:message`, `schoology:message-attachment` →
  no `data_subject`, audience `[guardians]` (standalone per spec 11 v1.2).
- **Parse-failure receipts**: `schoology-parse-failure:*` → no
  `data_subject`, audience `[guardians]`, content_type set to whatever
  the failing HTTP response was.

Per-kid audience differentiation (e.g., older kid's assignments visible
to siblings but younger kid's not) is supported natively by adding more
rule rows; v1 templates have uniform `[household]` for all kids.

The audience model is intentionally coarse (spec 11 §3.1): "is this for
the kid + their guardians? for the whole family? for external caregivers
too?" Fine-grained authorization (e.g., math tutor only for math content)
lives in downstream agents that maintain their own role registries.

## 10. Identifier Conventions

`data_subject` values are operator-chosen opaque labels (`k1`, `k2`).
Rationale per spec 11 §3.3 and §10 of this spec: family nicknames and
legal names are PII; opaque labels achieve the same downstream-routing
purpose without leaking identifying information into the metadata or
audit log. Spec 11's illustrative examples still use `bee`/`charlie`;
those examples were written when this point was less crisp. A small
spec 11 followup will rewrite those examples (§17.5).

## 11. Error Handling

### 11.1 Session Expiry

Detected via library sentinels (`schoology.ErrSessionExpired` or
equivalent), HTTP 401/403 responses, or `DownloadAttachment` returning
`text/html` (the library already raises this case).

**Response**: `PermanentError` with operator-friendly recovery message:

```
Schoology session expired. To recover:

  1. On your workstation: schoology-go auth.Login <SCHOOLOGY_HOST>
  2. Library writes fresh credentials JSON.
  3. Update 1Password item "schoology-session-<household>" with the new JSON.
  4. ESO syncs the new Secret within ~60s; the pod auto-restarts.
  5. The connector resumes from the last checkpoint.

See docs/AUTH-RECOVERY.md for the full procedure.
```

The connector exits non-zero. K8s reports `CrashLoopBackOff`. External
alerting (if configured) fires.

### 11.2 Transient Errors

Network failures, DNS errors, 5xx responses, connection timeouts. The
connector logs a warning, increments
`connector_errors_total{type="transient"}`, does NOT advance checkpoint,
and returns from `pollNow()` cleanly. The next scheduled or triggered
poll retries.

429 (rate-limited) responses honor `Retry-After` if present, else back
off ~10 minutes.

### 11.3 Parse Failures

The library's parsers can fail when Schoology changes HTML structure or
when a specific item has unexpected shape. Two cases:

**Partial parse failure** (item identified, some fields missing):
emit a degraded staging item. The kid + item ID came through; the body
or due-date or other secondary field did not. Tags carry the failure
context:

```
Tags:
  parse_status:        "degraded"
  parse_error:         "<truncated to 1024 chars>"
  parse_missing_fields: "<comma-separated list>"
  schoology_item_id:    "<numeric>"
  schoology_item_type:  "<feed|message|assignment>"
```

The degraded item flows through staging → scanner → routing. Per §12, the
Q-EARLY routing rule interprets `parse_status: degraded` as
quarantine-bound.

**Total parse failure** (the library couldn't identify the item at all):
emit a **parse failure receipt** to quarantine. The receipt is a synthetic
staging item:

```
Source:           "schoology-parse-failure"
Sender:           "schoology-connector"
Subject:          "[parse-failure] <parser>: <error_summary> (target: <kid|inbox>/<content_type>)"
Timestamp:        <now>
DestinationAgent: "school"
ContentType:      "application/octet-stream"  (or original HTTP response Content-Type)
content.raw:      <the raw HTTP response body that broke the parser>

Tags:
  parse_status:              "failure_receipt"
  parser:                     "<schoology-go library function name>"
  error:                      "<error message, truncated>"
  error_class:                "<sentinel or error type>"
  source_url:                 "<URL the response came from>"
  target_kid:                 "<k1|k2|"" for parent-level>"
  target_content_type:        "<assignment|feed|message>"
  schoology_library_version:  "<from go.mod>"
  trace:                      "<truncated stack/context>"
  affected_count:             "<N if dedup'd>"
  affected_item_ids:          "<comma-separated, truncated>"

DataSubject:  ""
Audience:     ["guardians"]   (operators only)
```

**Receipt deduplication**: one receipt per (parser, error_class) tuple
per pollNow() invocation. If the same error occurs 17 times in one poll
(e.g., schema drift broke all feed posts), exactly one receipt is
emitted with `affected_count: "17"`. Representative bytes attached are
from the first occurrence.

**Subject format**: structured for operator-scannable log lines:

```
[parse-failure] <parser>: <one-line error summary> (target: <kid|inbox>/<content_type>)
```

Examples:
- `[parse-failure] feed_body_extractor: empty string (target: k1/feed)`
- `[parse-failure] message_body_decoder: html parse error (target: inbox/message)`
- `[parse-failure] assignment_due_date: invalid date "TBD" (target: k2/assignment)`

### 11.4 Schema Drift Escalation

If `pollNow()` produces 0 items AND logs parse errors for 10 consecutive
polls, the connector escalates to `PermanentError` with message:
"Schoology library returned 0 items with parse errors for N consecutive
polls; likely upstream schema drift; investigate library version".

The 10-poll threshold ≈ 5 days under the standard schedule (2 polls per
weekday). Configurable via `parse_failure_threshold` in config.json.
Tracked in-process via a `consecutiveEmptyPollsWithErrors` counter that
resets on any successfully emitted item.

## 12. Required Glovebox-Internal Changes

The connector emits items tagged with `parse_status: degraded` or
`parse_status: failure_receipt`. The Glovebox routing layer must interpret
these tags as quarantine-bound. Spec 12 v1 depends on this and CANNOT
ship without it.

**Required routing-layer change** (one tag-based rule in
`internal/routing/`):

- If `metadata.tags["parse_status"]` is either `"degraded"` or
  `"failure_receipt"`, route the item to quarantine (via the existing
  `RouteQuarantine` path) instead of the normal `RoutePass` flow.
- Audit log entry should reflect that quarantine was tag-driven
  (e.g., `audit.AuditEntry.QuarantineReason: "parse_status_tag"`).

This is a small spec/PR cycle that lands before spec 12's implementation
plan executes. Tracked separately so it can be reviewed in isolation.

## 13. Telemetry

### 13.1 Metrics

**Framework-provided** (per the connector-guide):

- `connector_polls_total{connector, status}` (success/error)
- `connector_items_produced_total{connector, destination}`
- `connector_poll_duration_seconds{connector}` (histogram)
- `connector_errors_total{connector, type}` (transient/permanent)
- `connector_items_dropped_total{connector, reason}`
- `connector_checkpoint_age_seconds{connector}`

**Schoology-specific**:

- `schoology_polls_total{trigger_source}` -- `scheduled` / `catch_up` / `triggered`
- `schoology_parse_failures_total{parser, error_class, target_kid, content_type}`
- `schoology_parse_failure_receipts_total{parser, error_class}`
- `schoology_attachments_downloaded_bytes_total{kid, content_type}`
- `schoology_attachments_skipped_total{reason}` (`too_large`, `auth_expired_html`)
- `schoology_trigger_requests_total{outcome}` (`accepted`, `debounced`, `unauthorized`)
- `schoology_view_child_switches_total{kid}`

### 13.2 Traces (OpenTelemetry)

The connector creates its own tracer from the OTel global tracer provider
(the framework does not currently expose one).

Span structure per `pollNow()`:

- `schoology.poll` (root) -- attributes: `trigger_source`, `splay_time`
  (for scheduled), `poll_id` (uuid)
  - `schoology.poll.kid` (per kid) -- attribute: `kid`
    - `schoology.lib.GetOverdueSubmissions` -- attribute: `uid`
    - `schoology.lib.GetFeed` -- attribute: `uid`
    - `schoology.lib.DownloadAttachment` (per attachment) -- attributes:
      `attachment_id`, `bytes`
    - `schoology.staging.commit` (per item) -- attributes: `item_id`,
      `destination`, `data_subject`, `parse_status`
  - `schoology.lib.GetInbox` -- parent-level
- `schoology.trigger.handle` -- root span for trigger HTTP handler;
  attributes: `outcome`, `client_addr`

Exporter: same as Glovebox's existing OTel setup (OTLP to homelab
collector). Exemplars (linking metric increments to traces) deferred.

### 13.3 Event Logs (structured)

Logging library: per Glovebox project convention (verify at implementation
time -- the project may use `slog` or a chosen alternative; the structure
below is library-agnostic).

Representative log entries:

- `info` `schoology poll start` `trigger_source` `splay_time` `kids_count`
- `info` `schoology poll complete` `duration_ms` `items_produced` `errors`
- `warn` `schoology parse failure` `parser` `error_class` `kid` `receipt_emitted` `affected_count`
- `error` `schoology session expired` `details` `recovery_doc`
- `info` `schoology trigger received` `outcome` `remote_addr` `debounce_seconds_remaining`
- `debug` `schoology splay computed` `window` `splay_seconds` `scheduled_for`

Every error log includes `error_class` and the parser/library context for
grep-ability.

## 14. Health Endpoints

Standard framework behavior:

- `/healthz` (port 8080) -- 200 from process start
- `/readyz` (port 8080) -- 503 until first successful poll completes; 200
  thereafter
- `/metrics` (port 8080) -- Prometheus exposition

The trigger endpoint `/v1/poll` is on the Listener port (default 8081),
separate from the health/metrics port per framework conventions.

After a session-expiry `PermanentError`, the process exits; K8s reports
`CrashLoopBackOff`. External alerting catches it (no connector-internal
readiness flip needed).

## 15. Test Strategy

### 15.1 Approach: TS-MOCK

The connector talks to schoology-go via a `SchoologyClient` interface
that wraps the library's surface area. Production code passes the real
client; tests pass a fake. The fake is parameterized for the cases the
connector needs to handle ("normal", "empty", "session expired", "parse
error", "rate limited", etc.).

The library's parsers and HTML extraction are NOT re-tested at the
connector level -- those have their own fixture-backed table-driven
tests in `schoology-go/internal/testdata/`. The connector tests focus
on orchestration:

- **Scheduling**: window math, splay determinism (fixed RNG seed),
  weekday/weekend skip, day-rollover, timezone correctness.
- **Dedup**: highest-ID checkpoint advance, out-of-order item logging.
- **Audience attachment**: rule-matched values populate correctly;
  v0.5.0 enum values pass validation.
- **Trigger endpoint**: HTTP handler accepts authenticated POSTs,
  rejects bad tokens, debounces correctly, kicks `pollNow()`.
- **Error handling**: session expiry → PermanentError with recovery
  message; transient errors don't advance checkpoint; partial parse
  failures emit degraded items; total parse failures emit deduped
  receipts.
- **Quarantine routing tag**: emitted tags match what Q-EARLY routing
  expects.
- **Catch-up + scheduled + triggered convergence**: all three call
  paths go through the same `pollNow()`; verify via single-source-of-
  truth test.

### 15.2 Fixtures

Connector fixtures are minimal: hand-rolled fake responses for the
`SchoologyClient` interface. Library fixtures stay in the library's
own repo. No captured-HTTP-and-replayed at the connector level.

### 15.3 Container Test

Per CLAUDE.md "if an app runs in a container, test it in a container":
a separate CI job builds the connector image, starts it with mocked
SchoologyClient env, hits the trigger endpoint, verifies items appear
on a mounted staging volume. This is heavier than unit tests but
catches packaging/deployment issues. Separate job from unit-test green;
does not block fast unit-test feedback.

### 15.4 Coverage

No strict percentage target. Every error class and every code path
that branches on configuration must be covered. Code-coverage reports
as a smell test, not a gate.

### 15.5 Candidates for Extraction

`TODO: candidate for extraction to connector primitive base type`
comments on:

- The window-scheduler goroutine + splay computation.
- The `SchoologyClient`-style interface-boundary pattern (rename and
  generalize for any LMS).
- The parse-failure-receipt builder.
- The trigger-endpoint HTTP handler + debounce.

When PowerSchool (spec 13) lands and we see the duplication concretely,
extract a connector primitive that hosts these patterns; both connectors
inherit from it.

## 16. Backward Compatibility

This is a new connector. No existing connector is affected. No metadata-
schema changes beyond what spec 11 v1.2 already established.

Container image: `ghcr.io/leftathome/glovebox-schoology` (matches existing
connector naming convention). Helm chart `charts/schoology-connector`
following the pattern of existing connectors. Deployment is a separate
K8s Deployment per household.

## 17. Design Decisions and Known Followups

### 17.1 Headless Authentication

`auth.LoginWithPassword` (headless) works for native-password Schoology
tenants but not for SSO/MFA tenants. The current operator-action-on-
workstation flow is acceptable for v1; investigate headless feasibility
per-tenant as a separate spec when there's an SSO-free deployment to
test against.

### 17.2 Glovebox Scanner Content-Type-Aware Proxy Extraction

Non-plaintext content (PDF, .docx, HTML, XML, opaque binary) currently
flows through scanning unmodified, which means binary types aren't
deeply inspected. A Glovebox-scanner-side capability that extracts a
text proxy from known formats (HTML tag-strip, PDF text extract, .docx
inner-XML strings, `strings`-style fallback for opaque binaries) and
runs prompt-injection patterns against the extracted text would benefit
every connector emitting binary content -- not just schoology. Tracked
as a separate Glovebox-internal spec.

### 17.3 Cross-Connector Subject Reconciliation

When PowerSchool's connector lands (spec 13), it will use the same
opaque-label convention (`k1`, `k2`) for `data_subject` so that items
from both connectors can be grouped per kid without an explicit
reconciliation layer. Spec 11 §10.2 covers the broader picture; spec 13
will reference this design.

### 17.4 Connector Primitive Abstraction

After spec 13 (PowerSchool) lands, extract a "connector primitive base
type" that hosts the windowed scheduler, the client-interface pattern,
the parse-failure-receipt builder, and the trigger-endpoint handler.
Schoology and PowerSchool will both consume it; future LMS-shaped
connectors (Canvas, Google Classroom) inherit cleanly. Tracked as a
separate framework spec.

### 17.5 Spec 11 Examples Use Family Nicknames

Spec 11's illustrative examples still use `bee`/`charlie` as
`data_subject` values. They're technically allowed (the field is
opaque-and-operator-defined) but encourage PII-ish defaults. Small
spec-11 doc-only followup: rewrite those examples to use `k1`/`k2` with
a sentence acknowledging the privacy point.

### 17.6 Medical-Care Audience Tokens

Spec 11 v1.2 §2.2 already defers `spouse`, `medical_providers`,
HIPAA-grade sensitivity escalators until a medical-content connector
("walhelm" or similar) lands with concrete use cases. Spec 12 does not
exercise this gap.

### 17.7 Outbound Endpoints

The Listener pattern is forward-compatible with outbound traffic:
`POST /v1/messages`, `POST /v1/calendar`, etc. Schoology-go is read-only
in v0.1.0; outbound requires upstream library work first. Spec 12 v1
implements only the `/v1/poll` trigger endpoint.

## 18. Acceptance Criteria

A v0.6.0 release that implements this spec must:

1. Build a deployable container `ghcr.io/leftathome/glovebox-schoology`
   with a multi-stage Dockerfile (golang → distroless), following the
   existing connector pattern.
2. Read `config.json` per §4.2 schema; validate `kids[].name` are unique
   opaque labels; validate rules per spec 11 v1.2 audience rules
   (config-load-time per spec 11 §5).
3. Load `auth.Credentials` from `SCHOOLOGY_CREDENTIALS_FILE`; fail
   startup with a helpful message if file missing or unreadable.
4. Implement `Connector.Poll()` (catch-up), `Watcher.Watch(ctx)`
   (windowed scheduler with splay), `Listener.Handler()` (HTTP trigger
   endpoint with bearer-token auth and 60-second debounce).
5. Iterate all configured kids per poll, calling
   `GetOverdueSubmissions`, `GetFeed`, and (parent-level)
   `GetInbox` via the `SchoologyClient` interface (real client in
   prod, fake in tests).
6. For each new item (ID > checkpoint), call `RuleMatcher.Match()` for
   the appropriate match key, build `ItemOptions` with rule-supplied
   `DataSubject` and `Audience`, write content, commit.
7. Download attachments via the library (subject to size cap); emit
   each attachment as its own staging item.
8. On parse failure: total → parse-failure receipt; partial →
   degraded staging item; both with `parse_status` tag.
9. Emit all framework-default metrics plus schoology-specific metrics
   (§13.1).
10. Emit OpenTelemetry traces matching the span structure in §13.2.
11. Emit structured event logs per §13.3.
12. Detect session expiry via library sentinels / HTTP 401/403 /
    `DownloadAttachment` HTML responses; return `PermanentError` with
    the recovery message in §11.1.
13. Pass `go test ./...` clean with full mock-based unit-test coverage
    of orchestration logic; container test passes in a separate CI
    job.
14. Glovebox-internal routing-layer enhancement (§12) lands as a
    separate prerequisite PR; `parse_status` tags route to quarantine.
15. Documentation: a `docs/AUTH-RECOVERY.md` describing the operator
    procedure for session expiry recovery.
