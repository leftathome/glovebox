# Data Subject and Audience Metadata -- Design Specification

**Version 1.1 -- April 2026**

*This document extends the metadata schema (spec 04 §5.2) and the identity/provenance model (spec 06 §5) with two additive concepts: `data_subject` (the person an item is about, distinct from the identity that authenticated) and `audience` (a set of role-relative tokens describing who the item is intended to be seen by). The extension is driven by connectors where parent credentials retrieve data about children (Schoology, PowerSchool) and where per-item visibility rules differ by content type (grades vs flyers vs submitted work). V1 is metadata-only: Glovebox validates and stamps these fields but does not filter or route on them. The schema is designed so future audience-aware routing or enforcement can be layered on without breaking callers.*

---

## 1. Purpose

Existing connectors assume the authenticated identity is also the data subject:
RSS has no subject (unauthenticated feed); IMAP's `account_id` is both the
authenticator and the mailbox owner; GitHub/GitLab items are produced by the
user whose token was used. The `Identity` schema in spec 06 captures this
assumption explicitly -- `account_id` is "who authenticated."

Two upcoming connectors break that assumption:

- **Schoology**: the household's parent authenticates with parent credentials
  and the API returns data about the children on that account (assignments,
  messages, flyers, feed posts).
- **PowerSchool**: same pattern (parent credentials, child data) plus grades.

Additionally, within a single connector, different item types carry different
visibility expectations:

| Content                          | Intended audience                       |
|----------------------------------|-----------------------------------------|
| Grades                           | student + parents                       |
| Submitted work                   | student + parents                       |
| Fresh/uncompleted assignments    | whole household                         |
| Teacher messages about one kid   | student + parents                       |
| School flyers / PTA bulletins    | no restriction (already public content) |

The current schema has no way to express either "who this item is about" or
"who should be able to see it" as first-class, validated data. The only
available mechanism is free-form `tags`, which provides no typo protection --
a misspelled `audiance: [parnets]` would silently pass validation and later
manifest as a privacy leak or silent drop. That risk is unacceptable for
policy-relevant information.

This spec adds the minimum schema to describe these concepts correctly, with
enumerated validation, and defers all enforcement/routing work to later specs.

## 2. Scope

### 2.1 In Scope

- Adding `data_subject` (string) and `audience` (array of enum tokens) fields
  to `metadata.json`.
- Validation rules for both fields, including cross-field constraints.
- Plumbing through `BaseConfig`, `Rule`/`MatchResult`, and `ItemOptions` so
  the connector library can populate these fields.
- Merge semantics across the three value sources (config default, rule,
  per-item).
- Audit log extension to record `data_subject` and audience per item.
- Backward-compatibility guarantees for existing connectors.

### 2.2 Out of Scope (Deferred)

- Audience-aware routing (rules that branch on audience value).
- Enforcement gates (quarantine or drop items on policy violation).
- Named audience shorthands (e.g., `"student-private"` as an alias for
  `["subject", "parents"]`).
- Multi-subject items (`data_subject` as an array). V1 requires emitting
  separate items if a single upstream record legitimately concerns multiple
  data subjects.
- Extended-family tokens (`extended`, `grandparents`, `caregivers`).
- Cross-connector data-subject reconciliation (e.g., Schoology's "bee" and
  PowerSchool's "bee" being recognized as the same person). Deferred to a
  later identity-normalization spec.
- Populating these fields retroactively in existing connectors (RSS, IMAP,
  Gmail, Outlook, GitHub, GitLab, LinkedIn, Meta, Bluesky, X). They continue
  to emit items without `data_subject`/`audience`.

## 3. Schema Additions

### 3.1 Terminology -- "Subject" Disambiguation

The word "subject" appears in three distinct contexts in Glovebox. Implementers
and reviewers MUST keep them separate.

| Name (where it appears)                     | Meaning                                                           | Notes |
|---------------------------------------------|-------------------------------------------------------------------|-------|
| `subject` -- top-level field on `metadata.json` (existing, per spec 04 §5.2) | **Email-style subject line** (title of the message or item).   | Unchanged by this spec. No rename. |
| `data_subject` -- top-level field on `metadata.json` (new, this spec)        | **Person or entity the item is about** (identifier string).     | Introduced by this spec. |
| `subject` -- token inside the `audience` array (new, this spec)              | **Role:** the person named in `data_subject`.                   | Lives only inside the `audience` enum. Never appears standalone. |

Throughout this document:

- "the `subject` field" always means the existing email-style subject line.
- "`data_subject`" (always the full word) means the new who-is-this-about
  identifier.
- "the `subject` audience token" or "`"subject"` in `audience`" means the role
  enum value; it is never a field.

Downstream code and documentation SHOULD prefer the full names
(`data_subject`, `subject` field, `subject` audience token) in any context
where the shorter form is ambiguous.

### 3.2 metadata.json -- Shape After This Spec

Two new **top-level** fields alongside the existing `identity` block. They are
not nested inside `identity` because they describe the item, not the
authenticator.

```json
{
    "source": "schoology",
    "sender": "Mr. Rodriguez",
    "subject": "Math quiz retakes",
    "timestamp": "2026-04-21T12:00:00Z",
    "destination_agent": "school",
    "content_type": "text/plain",
    "ordered": false,
    "auth_failure": false,

    "identity": {
        "account_id": "steve@schoology",
        "provider": "schoology",
        "auth_method": "oauth",
        "tenant": "wagner-home"
    },

    "data_subject": "bee",
    "audience": ["subject", "parents"],

    "tags": {
        "course": "pre-algebra"
    }
}
```

Note the three distinct uses per §3.1:

- `"subject": "Math quiz retakes"` -- email-style subject line of the message.
- `"data_subject": "bee"` -- the kid this message is about.
- `"audience": ["subject", "parents"]` -- the `"subject"` token here is the
  enum role meaning "the person named in `data_subject`" (Bee), together with
  Bee's parents.

### 3.3 `data_subject` Field

| Property     | Value                                                           |
|--------------|-----------------------------------------------------------------|
| JSON key     | `data_subject`                                                  |
| Type         | string                                                          |
| Required     | No                                                              |
| Max length   | 256 characters                                                  |
| Constraints  | No control characters                                           |
| Semantics    | Identifier for the person or entity the item is *about*         |

The value is a free-form identifier chosen by the connector or its rule
config. Glovebox performs no semantic interpretation -- it treats the string
as opaque. Matching `data_subject` values across connectors (e.g.,
correlating Schoology's and PowerSchool's "bee") is deferred to a later spec.

Omission is valid and means "item has no specific data subject" -- e.g., a
school-wide flyer.

### 3.4 `audience` Field

| Property     | Value                                                           |
|--------------|-----------------------------------------------------------------|
| JSON key     | `audience`                                                      |
| Type         | array of strings                                                |
| Required     | No                                                              |
| Max entries  | 16                                                              |
| Constraints  | Each element drawn from the enum below; no duplicates           |

Enum tokens (**these are audience roles, NOT references to any `metadata.json`
field**):

| Token       | Meaning                                                                       |
|-------------|-------------------------------------------------------------------------------|
| `subject`   | the person named in `data_subject` (see §3.1 for disambiguation)              |
| `parents`   | the `data_subject`'s parents/guardians                                        |
| `siblings`  | the `data_subject`'s siblings                                                 |
| `household` | everyone in the household (effectively subject + parents + siblings combined) |
| `public`    | no access restriction; may be shared outside the household                    |

Semantics are **role-relative**: tokens like `subject`, `parents`, `siblings`
are interpreted relative to whatever identifier is in the `data_subject`
field. Resolution ("is Steve one of Bee's parents?") happens at the consuming
boundary, which is each downstream agent, using whatever household model that
agent maintains. Glovebox does not hold a family roster.

### 3.5 Cross-Field Validation

- If `data_subject` is empty/omitted, `audience` may not contain any of
  `subject`, `parents`, `siblings` (these tokens are meaningless without a
  data subject). `household` and `public` remain valid.
- `public` must appear alone. Any array that contains `public` plus any other
  token is rejected. Rationale: `public` is broader than any other; narrower
  tokens alongside it add no meaning and confuse downstream logic.
- `household` must appear alone when combined with role-relative tokens.
  `["household", "parents"]` is rejected: `household` already includes
  parents. `["household"]` and `["subject", "parents"]` are both valid.
- Empty array (`"audience": []`) is rejected -- it is ambiguous. Producers
  should omit the field entirely to signal "no explicit audience."

### 3.6 Default When Omitted

If `audience` is absent from `metadata.json`, consumers MUST treat the item
as if `audience` were `["household"]`.

Glovebox does **not** materialize this default into the file at write time.
Reading code applies the default. This keeps the on-disk schema honest about
what the producer actually declared vs what the framework inferred.

Rationale for `household` (rather than a stricter deny default) in v1:

- Current Glovebox deployments are single-household homelabs; the operator
  and all consumers live in the household.
- Existing connectors (pre-spec-11) emit items with no audience; treating
  them as household-visible matches current behavior with no regressions.
- A stricter default can be introduced in a later spec without a schema
  change, by flipping the reader-side default.

## 4. Plumbing

Values can reach a staged item from three sources, mirroring the existing
`identity` and `tags` model. All new Go fields follow Go's CamelCase
convention; JSON tags use snake_case matching the field's JSON key.

### 4.1 Config-Level Defaults

Extend `BaseConfig`:

```go
type BaseConfig struct {
    Rules              []Rule          `json:"rules"`
    Routes             []Rule          `json:"routes"` // deprecated fallback
    ConfigIdentity     *ConfigIdentity `json:"identity,omitempty"`
    DataSubjectDefault string          `json:"data_subject_default,omitempty"`
    AudienceDefault    []string        `json:"audience_default,omitempty"`
}
```

Useful primarily for single-data-subject connector instances (e.g., one
Schoology container per kid, each with its own config declaring its
`data_subject`).

### 4.2 Rule-Matched

Extend `Rule` and `MatchResult`:

```go
type Rule struct {
    Match       string            `json:"match"`
    Destination string            `json:"destination"`
    Tags        map[string]string `json:"tags,omitempty"`
    DataSubject string            `json:"data_subject,omitempty"`
    Audience    []string          `json:"audience,omitempty"`
}

type MatchResult struct {
    Destination string
    Tags        map[string]string
    DataSubject string
    Audience    []string
}
```

This is the **primary pathway** for Schoology and PowerSchool: a rule like
`"schoology:bee:grade" -> {destination: "school", data_subject: "bee",
audience: ["subject", "parents"]}` encodes the visibility policy declaratively
per content type.

Example (schoology) -- note `data_subject` on the rule object vs the
`"subject"` token inside `audience`:

```json
{
    "rules": [
        {"match": "schoology:bee:grade",       "destination": "school", "data_subject": "bee",     "audience": ["subject", "parents"]},
        {"match": "schoology:bee:submitted",   "destination": "school", "data_subject": "bee",     "audience": ["subject", "parents"]},
        {"match": "schoology:bee:assignment",  "destination": "school", "data_subject": "bee",     "audience": ["household"]},
        {"match": "schoology:charlie:grade",   "destination": "school", "data_subject": "charlie", "audience": ["subject", "parents"]},
        {"match": "schoology:*:flyer",         "destination": "school",                            "audience": ["public"]},
        {"match": "*",                         "destination": "school",                            "audience": ["household"]}
    ]
}
```

### 4.3 Per-Item

Extend `ItemOptions`:

```go
type ItemOptions struct {
    // ... existing fields ...
    Identity    *Identity
    Tags        map[string]string
    RuleTags    map[string]string
    DataSubject string
    Audience    []string
}
```

Use when `data_subject` or audience must be decided at runtime in connector
code rather than declaratively via rules (e.g., PowerSchool distinguishing
"final grade" from "interim grade" based on data that's not part of the match
key).

## 5. Merge Semantics

Applied by `StagingWriter.Commit()`, for both `data_subject` and `audience`
independently:

1. If per-item (`ItemOptions.DataSubject` / `ItemOptions.Audience`) is
   non-empty, use it. Stop.
2. Else if rule-matched (`MatchResult.DataSubject` / `MatchResult.Audience`)
   is non-empty, use it. Stop.
3. Else if config default (`BaseConfig.DataSubjectDefault` /
   `BaseConfig.AudienceDefault`) is non-empty, use it. Stop.
4. Else the field is omitted from `metadata.json`.

**Per-item wins outright; no union.** This is the same rule tags and identity
use today. Rationale: predictable semantics. A union/merge model makes it
too easy to accidentally broaden an audience by having two independent
contributions combine (the original mistake-with-privacy-consequences
scenario this spec exists to prevent).

### 5.1 Empty-Value Semantics at Each Layer

- **Go call sites** (`ItemOptions`, `Rule`, `BaseConfig`):
  - `DataSubject == ""` = "not set at this layer."
  - `Audience` as `nil` or length-0 slice = "not set at this layer."
  - A validated non-empty value at a layer replaces all lower-priority layers
    outright.

- **Serialized `metadata.json`**:
  - `data_subject` field: omitted = "no data subject declared." Present-but-
    empty-string is rejected by the validator (§6).
  - `audience` field: omitted = reader applies default `["household"]`
    (§3.6). Present-but-empty-array (`"audience": []`) is rejected (§3.5).

Config defaults themselves go through the same validation at **config load
time** -- a config with `audience_default: []` or `audience_default: ["public",
"household"]` is rejected at startup, before any items are produced.

## 6. Validation

Validation lives in `internal/staging/validate.go` and runs as part of
`StagingWriter.Commit()` -- the same point at which identity, tags, and the
existing metadata fields are validated (per spec 06 §8.2). Do not add a
separate validation pass; extend the existing one.

Rules:

- `data_subject`: if present, ≤256 chars, no control characters. The Go
  zero value (empty string) is treated as omission -- the library cannot
  distinguish "explicit empty string" from "field absent" without adopting
  `*string` everywhere, which adds friction for no behavioral difference
  once merged.
- `audience`: if present, each element must be one of the enum tokens
  (§3.4); no duplicates; ≤16 entries; non-empty array.
- Cross-field rules from §3.5:
  - Empty `data_subject` + `audience` containing `subject`/`parents`/
    `siblings` → reject.
  - `public` combined with any other token → reject.
  - `household` combined with `subject`/`parents`/`siblings` → reject.
- Config-load-time validation applies the same rules to
  `DataSubjectDefault` and `AudienceDefault`. A malformed default fails
  startup, not first-item-commit.

All violations are treated as permanent errors on the item (matching the
existing behavior for malformed metadata per spec 04 §5.4). The connector's
`Commit()` returns an error; the temp directory is cleaned up; the
checkpoint is not advanced.

## 7. Audit Log

Extend `AuditEntry` in `internal/audit/logger.go`:

```go
type AuditEntry struct {
    // ... existing fields ...
    Identity    *staging.Identity  `json:"identity,omitempty"`
    Tags        map[string]string  `json:"tags,omitempty"`
    DataSubject string             `json:"data_subject,omitempty"`
    Audience    []string           `json:"audience,omitempty"`
}
```

Both fields use `omitempty` for the same reason as `identity`/`tags`:
unauthenticated or data-subject-less items have nothing to record.

Purpose: forensic trail that captures the producer's declared intent at
ingest time. If audience policy evolves (e.g., a named shorthand is redefined
later), the audit log preserves the actual audience tokens that applied to
each item, decoupling policy drift from data provenance.

## 8. Backward Compatibility and Migration

### 8.1 Additive Changes Only

- All new fields are optional with `omitempty`.
- Existing `metadata.json` files and existing connectors continue to validate
  and pass through without modification.
- The pre-existing `subject` field (email-style subject line, per spec 04
  §5.2) is **not renamed**. The new concept takes the longer name
  `data_subject` to avoid collision; see §10 for why this choice was made.

### 8.2 Version Bump

v0.3.0 -- minor, additive under 0.x semver. Public Go API gains:

- `BaseConfig.DataSubjectDefault`, `BaseConfig.AudienceDefault`
- `Rule.DataSubject`, `Rule.Audience`
- `MatchResult.DataSubject`, `MatchResult.Audience`
- `ItemOptions.DataSubject`, `ItemOptions.Audience`

No removals or renames. Existing callers compile unchanged.

### 8.3 Existing Connectors

No code changes required for RSS, IMAP, Gmail, Outlook, GitHub, GitLab,
LinkedIn, Meta, Bluesky, X, Jira, Trello, Notion, gdrive, onedrive, teams,
gcalendar, arxiv, hackernews, steam, youtube, semantic-scholar. They continue
to emit items with no `data_subject` or `audience`. Reader-side default
(§3.6) treats these as household-visible, matching current behavior.

### 8.4 Rules-Config Schema

Rule JSON objects may now carry optional `data_subject` and `audience`
fields. Existing config files without them continue to load.

## 9. Usage Examples

### 9.1 Single-Data-Subject Connector (PowerSchool-per-Kid)

Deploy one PowerSchool container per kid, each with:

```json
{
    "data_subject_default": "bee",
    "audience_default": ["subject", "parents"],
    "rules": [
        {"match": "grade",              "destination": "school"},
        {"match": "progress_report",    "destination": "school"},
        {"match": "*",                  "destination": "school"}
    ],
    "identity": {
        "provider":    "powerschool",
        "auth_method": "oauth",
        "tenant":      "wagner-home"
    }
}
```

Every item gets `data_subject: "bee"` and `audience: ["subject", "parents"]`
unless a specific rule overrides. Connector code typically needs no per-item
override.

### 9.2 Multi-Data-Subject Connector (Schoology-All-Kids)

Single Schoology container covering both kids. The connector queries each
child's endpoint in turn; rule match keys embed the child name so rules can
fan out per-kid. In the configuration below, the `data_subject` key on each
rule object sets the new field; the `"subject"` string inside `audience`
arrays is the role-enum token meaning "the person named in `data_subject`"
per §3.4.

```json
{
    "rules": [
        {"match": "schoology:bee:grade",         "data_subject": "bee",     "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:bee:submitted",     "data_subject": "bee",     "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:bee:assignment",    "data_subject": "bee",     "audience": ["household"],          "destination": "school"},
        {"match": "schoology:bee:message",       "data_subject": "bee",     "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:charlie:grade",     "data_subject": "charlie", "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:charlie:submitted", "data_subject": "charlie", "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:charlie:assignment","data_subject": "charlie", "audience": ["household"],          "destination": "school"},
        {"match": "schoology:charlie:message",   "data_subject": "charlie", "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:*:flyer",                                      "audience": ["public"],             "destination": "school"},
        {"match": "*",                                                      "audience": ["household"],          "destination": "school"}
    ],
    "identity": {
        "provider":    "schoology",
        "auth_method": "oauth",
        "tenant":      "wagner-home"
    }
}
```

Connector code emits items with match keys like `schoology:bee:assignment`
for Bee's assignments, and the rule matcher fills in `data_subject` and
`audience`.

The `schoology:*:flyer` and `*` rules deliberately omit `data_subject`:
flyers are genuinely subjectless (they concern the school or class, not a
specific kid), and the catch-all safety net intentionally drops to the
default `household` audience without forcing a specific subject.

### 9.3 Existing Connector (IMAP, Unchanged)

No config changes. Items flow as before with no `data_subject`/`audience`.
Consumers read the missing audience as `["household"]` per §3.6.

## 10. Design Decisions and Deferred Work

### 10.1 Field Naming: Why `data_subject` (Not `subject`)

`metadata.json` already has a top-level field `subject`, which carries the
email-style subject line of an item (per spec 04 §5.2). The obvious name for
the new "who is this item about" field would also be `subject`, but that
would collide.

Two possible resolutions were considered:

- **Option A** -- rename the existing email-style subject line to
  `subject_line` and claim `subject` for the new concept. Cleaner semantics
  at rest, but a breaking change to `metadata.json` that ripples through all
  ~20 existing connectors, the staging validator, the audit log schema, and
  every reader.
- **Option B** -- name the new field `data_subject` and leave `subject` as
  the email-style subject line.

**Option B was adopted.** Rationale:

1. Non-breaking. No existing connector's output changes.
2. The longer name `data_subject` makes the concept-vs-email-subject-line
   distinction explicit every time the field is referenced, in code or in
   JSON.
3. The asymmetry is acceptable: the email-style `subject` is a classic
   message concept; the new `data_subject` names a distinct, newer concept.
4. §3.1 documents the disambiguation once, prominently, so downstream readers
   are not left to infer it.

Note the unavoidable residual collision: the `audience` enum uses `subject`
as one of its role tokens. This is deliberately scoped to the enum's own
namespace (it never appears standalone as a field) and is documented in §3.1
and §3.4.

### 10.2 Cross-Connector Data-Subject Reconciliation

Once Schoology and PowerSchool both populate `data_subject: "bee"` for the
same person, downstream consumers can group items by data subject. But if
the two connectors diverge (e.g., one uses the Schoology user ID as the
value, the other uses a PowerSchool student ID), correlation breaks. A later
spec should define a normalization convention or a separate identity-
reconciliation layer.

### 10.3 Audience-Aware Routing

Once usage patterns are established, rules could evolve to branch on
audience as part of the match key, or Glovebox could split items into
multiple staging lanes based on audience. Design punted to a later spec.

### 10.4 Enforcement Gates

If agents diverge on how they honor audience, Glovebox could grow a hard
gate that refuses to release items whose declared audience violates a
destination-agent policy. Again, later spec.

## 11. Acceptance Criteria

A v0.3.0 release that implements this spec must:

1. Accept `data_subject` (string) and `audience` (array of enum tokens) as
   optional top-level fields on `metadata.json`, validated per §6.
2. Accept `data_subject` and `audience` on `Rule` and the corresponding
   defaults on `BaseConfig` (`DataSubjectDefault`, `AudienceDefault`).
3. Accept `DataSubject` and `Audience` on `ItemOptions` with per-item-wins
   merge semantics per §5.
4. Include `DataSubject` and `Audience` on `AuditEntry` with `omitempty`.
5. Produce clean `go vet` and `staticcheck`, pass existing tests, and add
   new tests covering:
   - Each valid enum token in `audience`.
   - Each rejected combination from §3.5 (empty `data_subject` + role token,
     `public` with extras, `household` with role tokens, empty array).
   - Merge precedence (per-item > rule > config default > omitted) for both
     fields independently.
   - Reader-side default `audience = ["household"]` for items that omit the
     field.
   - Config-load-time rejection of malformed `DataSubjectDefault` /
     `AudienceDefault`.
6. Leave all existing connectors unchanged in behavior and output. The
   pre-existing `subject` field (email-style subject line) must remain
   byte-identical in produced `metadata.json` files.
