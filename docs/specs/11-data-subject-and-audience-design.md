# Data Subject and Audience Metadata -- Design Specification

**Version 1.0 -- April 2026**

*This document extends the metadata schema (spec 04 §5.2) and the identity/provenance model (spec 06 §5) with two additive concepts: `subject` (the person an item is about, distinct from the identity that authenticated) and `audience` (a set of role-relative tokens describing who the item is intended to be seen by). The extension is driven by connectors where parent credentials retrieve data about children (Schoology, PowerSchool) and where per-item visibility rules differ by content type (grades vs flyers vs submitted work). V1 is metadata-only: Glovebox validates and stamps these fields but does not filter or route on them. The schema is designed so future audience-aware routing or enforcement can be layered on without breaking callers.*

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

- Adding `subject` (string) and `audience` (array of enum tokens) fields to
  `metadata.json`.
- Validation rules for both fields, including cross-field constraints.
- Plumbing through `BaseConfig`, `Rule`/`MatchResult`, and `ItemOptions` so the
  connector library can populate these fields.
- Merge semantics across the three value sources (config default, rule,
  per-item).
- Audit log extension to record subject and audience per item.
- Backward-compatibility guarantees for existing connectors.

### 2.2 Out of Scope (Deferred)

- Audience-aware routing (rules that branch on audience value).
- Enforcement gates (quarantine or drop items on policy violation).
- Named audience shorthands (e.g., `"student-private"` as an alias for
  `["subject", "parents"]`).
- Multi-subject items (subject as an array). V1 requires emitting separate
  items if a single upstream record legitimately concerns multiple subjects.
- Extended-family tokens (`extended`, `grandparents`, `caregivers`).
- Cross-connector subject reconciliation (e.g., Schoology's "bee" and
  PowerSchool's "bee" being recognized as the same person). Deferred to a
  later identity-normalization spec.
- Populating these fields retroactively in existing connectors (RSS, IMAP,
  Gmail, Outlook, GitHub, GitLab, LinkedIn, Meta, Bluesky, X). They continue
  to emit items without `subject`/`audience`.

## 3. Schema Additions

### 3.1 metadata.json

Two new **top-level** fields alongside the existing `identity` block. They are
not nested inside `identity` because they describe the item, not the
authenticator.

```json
{
    "source": "schoology",
    "sender": "Mr. Rodriguez",
    "subject_line": "Math quiz retakes",
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

    "subject": "bee",
    "audience": ["subject", "parents"],

    "tags": {
        "course": "pre-algebra"
    }
}
```

Note: the existing metadata field currently called `subject` (the email-style
subject line) is referenced as `subject_line` in the example above for
readability. The actual field name in the implementation today is `subject`.
This name collision is resolved in §10.

### 3.2 `subject` Field

| Property     | Value                                                           |
|--------------|-----------------------------------------------------------------|
| JSON key     | `subject` (pending rename -- see §10)                           |
| Type         | string                                                          |
| Required     | No                                                              |
| Max length   | 256 characters                                                  |
| Constraints  | No control characters                                           |
| Semantics    | Identifier for the person or entity the item is *about*         |

The value is a free-form identifier chosen by the connector or its rule
config. Glovebox performs no semantic interpretation -- it treats the string
as opaque. Matching subjects across connectors (e.g., correlating Schoology's
and PowerSchool's "bee") is deferred to a later spec.

Omission is valid and means "item has no specific subject" -- e.g., a
school-wide flyer.

### 3.3 `audience` Field

| Property     | Value                                                           |
|--------------|-----------------------------------------------------------------|
| JSON key     | `audience`                                                      |
| Type         | array of strings                                                |
| Required     | No                                                              |
| Max entries  | 16                                                              |
| Constraints  | Each element drawn from the enum below; no duplicates           |

Enum tokens:

| Token       | Meaning                                                           |
|-------------|-------------------------------------------------------------------|
| `subject`   | the person named in `subject`                                     |
| `parents`   | the `subject`'s parents/guardians                                 |
| `siblings`  | the `subject`'s siblings                                          |
| `household` | everyone in the household (effectively subject + parents + siblings) |
| `public`    | no access restriction; may be shared outside the household        |

Semantics are **role-relative**: tokens like `subject`, `parents`, `siblings`
are interpreted relative to whatever identifier is in the `subject` field.
Resolution ("is Steve one of Bee's parents?") happens at the consuming
boundary, which is each downstream agent, using whatever household model that
agent maintains. Glovebox does not hold a family roster.

### 3.4 Cross-Field Validation

- If `subject` is empty/omitted, `audience` may not contain any of `subject`,
  `parents`, `siblings` (these tokens are meaningless without a subject).
  `household` and `public` remain valid.
- `public` must appear alone. Any array that contains `public` plus any other
  token is rejected. Rationale: `public` broader than any other; narrower
  tokens alongside it add no meaning and confuse downstream logic.
- `household` must appear alone when combined with role-relative tokens.
  `["household", "parents"]` is rejected: `household` already includes
  parents. `["household"]` and `["subject", "parents"]` are both valid.
- Empty array (`"audience": []`) is rejected -- it is ambiguous. Consumers
  should omit the field entirely to signal "no explicit audience."

### 3.5 Default When Omitted

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
`identity` and `tags` model:

### 4.1 Config-Level Defaults

Extend `BaseConfig`:

```go
type BaseConfig struct {
    Rules          []Rule          `json:"rules"`
    Routes         []Rule          `json:"routes"` // deprecated fallback
    ConfigIdentity *ConfigIdentity `json:"identity,omitempty"`
    SubjectDefault  string         `json:"subject_default,omitempty"`
    AudienceDefault []string       `json:"audience_default,omitempty"`
}
```

Useful primarily for single-subject connector instances (e.g., one Schoology
container per kid, each with its own config declaring its subject).

### 4.2 Rule-Matched

Extend `Rule` and `MatchResult`:

```go
type Rule struct {
    Match       string            `json:"match"`
    Destination string            `json:"destination"`
    Tags        map[string]string `json:"tags,omitempty"`
    Subject     string            `json:"subject,omitempty"`
    Audience    []string          `json:"audience,omitempty"`
}

type MatchResult struct {
    Destination string
    Tags        map[string]string
    Subject     string
    Audience    []string
}
```

This is the **primary pathway** for Schoology and PowerSchool: a rule like
`"schoology:bee:grade" -> {destination: school, subject: bee, audience:
[subject, parents]}` encodes the visibility policy declaratively per content
type.

Example (schoology):

```json
{
    "rules": [
        {"match": "schoology:bee:grade",       "destination": "school", "subject": "bee",     "audience": ["subject", "parents"]},
        {"match": "schoology:bee:submitted",   "destination": "school", "subject": "bee",     "audience": ["subject", "parents"]},
        {"match": "schoology:bee:assignment",  "destination": "school", "subject": "bee",     "audience": ["household"]},
        {"match": "schoology:charlie:grade",   "destination": "school", "subject": "charlie", "audience": ["subject", "parents"]},
        {"match": "schoology:*:flyer",         "destination": "school",                       "audience": ["public"]},
        {"match": "*",                         "destination": "school",                       "audience": ["household"]}
    ]
}
```

### 4.3 Per-Item

Extend `ItemOptions`:

```go
type ItemOptions struct {
    // ... existing fields ...
    Identity *Identity
    Tags     map[string]string
    RuleTags map[string]string
    Subject  string
    Audience []string
}
```

Use when subject/audience must be decided at runtime in connector code rather
than declaratively via rules (e.g., PowerSchool distinguishing "final grade"
from "interim grade" based on data that's not part of the match key).

## 5. Merge Semantics

Applied by `StagingWriter.Commit()`, for both `subject` and `audience`
independently:

1. If per-item (`ItemOptions.Subject` / `ItemOptions.Audience`) is non-empty,
   use it. Stop.
2. Else if rule-matched (`MatchResult.Subject` / `MatchResult.Audience`) is
   non-empty, use it. Stop.
3. Else if config default (`BaseConfig.SubjectDefault` /
   `BaseConfig.AudienceDefault`) is non-empty, use it. Stop.
4. Else the field is omitted from `metadata.json`.

**Per-item wins outright; no union.** This is the same rule tags and identity
use today. Rationale: predictable semantics. A union/merge model makes it
too easy to accidentally broaden an audience by having two independent
contributions combine (the original mistake-with-privacy-consequences scenario
this spec exists to prevent).

Empty-vs-present:

- `Subject`: empty string = "not set at this layer."
- `Audience`: nil or length-0 slice = "not set at this layer." A validated
  non-empty slice replaces outright.

Config defaults are themselves subject to the same validation rules at
load time -- a config with `audience_default: []` is rejected at startup.

## 6. Validation

Additions to `internal/staging/validate.go`:

- `subject`: if present, ≤256 chars, no control characters.
- `audience`: if present, each element must be one of the enum tokens
  (§3.3); no duplicates; ≤16 entries; non-empty array.
- Cross-field rule from §3.4:
  - Empty `subject` + `audience` containing `subject`/`parents`/`siblings`
    → reject.
  - `public` combined with any other token → reject.
  - `household` combined with `subject`/`parents`/`siblings` → reject.

All violations are treated as permanent errors on the item (matching the
existing behavior for malformed metadata per spec 04 §5.4). The connector's
`Commit()` returns an error; the temp directory is cleaned up; the checkpoint
is not advanced.

## 7. Audit Log

Extend `AuditEntry` in `internal/audit/logger.go`:

```go
type AuditEntry struct {
    // ... existing fields ...
    Identity *staging.Identity  `json:"identity,omitempty"`
    Tags     map[string]string  `json:"tags,omitempty"`
    Subject  string             `json:"subject,omitempty"`
    Audience []string           `json:"audience,omitempty"`
}
```

Both fields use `omitempty` for the same reason as `identity`/`tags`:
unauthenticated or subjectless items have nothing to record.

Purpose: forensic trail that captures the producer's declared intent at
ingest time. If audience policy evolves (e.g., a named shorthand is redefined
later), the audit log preserves the actual audience tokens that applied to
each item, decoupling policy drift from data provenance.

## 8. Backward Compatibility and Migration

### 8.1 Additive Changes Only

- All new fields are optional with `omitempty`.
- Existing `metadata.json` files and existing connectors continue to validate
  and pass through without modification.
- No rename of existing fields in v1 (the field-name collision in §10 is
  flagged as a followup, not addressed here).

### 8.2 Version Bump

v0.3.0 -- minor, additive under 0.x semver. Public Go API gains:

- `BaseConfig.SubjectDefault`, `BaseConfig.AudienceDefault`
- `Rule.Subject`, `Rule.Audience`
- `MatchResult.Subject`, `MatchResult.Audience`
- `ItemOptions.Subject`, `ItemOptions.Audience`

No removals or renames. Existing callers compile unchanged.

### 8.3 Existing Connectors

No code changes required for RSS, IMAP, Gmail, Outlook, GitHub, GitLab,
LinkedIn, Meta, Bluesky, X, Jira, Trello, Notion, gdrive, onedrive, teams,
gcalendar, arxiv, hackernews, steam, youtube, semantic-scholar. They continue
to emit items with no subject or audience. Reader-side default (§3.5) treats
these as household-visible, matching current behavior.

### 8.4 Rules-Config Schema

Rule JSON objects may now carry optional `subject` and `audience` fields.
Existing config files without them continue to load.

## 9. Usage Examples

### 9.1 Single-Subject Connector (Powerschool-per-Kid)

Deploy one PowerSchool container per kid, each with:

```json
{
    "subject_default": "bee",
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

Every item gets `subject: "bee"`, `audience: ["subject", "parents"]` unless a
specific rule overrides. Connector code typically needs no per-item override
for subject or audience.

### 9.2 Multi-Subject Connector (Schoology-All-Kids)

Single Schoology container covering both kids. The connector queries each
child's endpoint in turn; rule match keys embed the child name so rules can
fan out per-kid:

```json
{
    "rules": [
        {"match": "schoology:bee:grade",         "subject": "bee",     "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:bee:submitted",     "subject": "bee",     "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:bee:assignment",    "subject": "bee",     "audience": ["household"],          "destination": "school"},
        {"match": "schoology:bee:message",       "subject": "bee",     "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:charlie:grade",     "subject": "charlie", "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:charlie:submitted", "subject": "charlie", "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:charlie:assignment","subject": "charlie", "audience": ["household"],          "destination": "school"},
        {"match": "schoology:charlie:message",   "subject": "charlie", "audience": ["subject", "parents"], "destination": "school"},
        {"match": "schoology:*:flyer",                                  "audience": ["public"],             "destination": "school"},
        {"match": "*",                                                  "audience": ["household"],          "destination": "school"}
    ],
    "identity": {
        "provider":    "schoology",
        "auth_method": "oauth",
        "tenant":      "wagner-home"
    }
}
```

Connector code emits items with match keys like `schoology:bee:assignment`
for Bee's assignments, and the rule matcher fills in subject and audience.

### 9.3 Existing Connector (IMAP, Unchanged)

No config changes. Items flow as before with no subject/audience. Consumers
read the missing audience as `["household"]` per §3.5.

## 10. Known Followups

### 10.1 Field-Name Collision

`metadata.json` already has a field called `subject` (the email-style subject
line, per spec 04 §5.2). Adding a new `subject` field for "data subject" is a
direct name collision.

V1 resolution: the existing field keeps its name; new "data subject" field
also uses `subject` only if the rename happens first. Two options to resolve
before v0.3.0 ships:

- **Option A** -- rename the existing email-style subject line to
  `subject_line` and claim `subject` for the new field. Requires updating
  all existing connectors and reader code. Breaking at the JSON level.
- **Option B** -- name the new field something else: `data_subject` or
  `about` or `subject_id`. Non-breaking, but less clean.

Recommendation: **Option B, `data_subject`**. Minimizes churn, preserves
existing connector output, and the longer name makes the concept-vs-email-
subject distinction explicit at the metadata level.

This spec assumes Option B is adopted. If the reviewer or operator prefers
Option A, the rename must be coordinated with all connectors in the same
release.

### 10.2 Cross-Connector Subject Reconciliation

Once Schoology and PowerSchool both populate `subject: "bee"` for the same
person, downstream consumers can group items by subject. But if the two
connectors diverge (e.g., one uses the Schoology user ID as the subject,
the other uses a PowerSchool student ID), correlation breaks. A later spec
should define a normalization convention or a separate identity-reconciliation
layer.

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

1. Accept `subject` and `audience` (or `data_subject` per §10.1) as optional
   top-level fields on `metadata.json`, validated per §6.
2. Accept the same fields on `Rule` and on `BaseConfig` (as defaults).
3. Accept the same fields on `ItemOptions` with per-item-wins merge
   semantics per §5.
4. Include the fields on `AuditEntry` with `omitempty`.
5. Produce clean `go vet` and pass existing tests, with new tests covering:
   - Each valid enum token
   - Each rejected combination from §3.4
   - Merge precedence (per-item > rule > config default > omitted)
   - Reader-side default = `["household"]` for items that omit the field
6. Leave all existing connectors unchanged in behavior and output.
