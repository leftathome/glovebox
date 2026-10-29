# Record Journal Part B1 Implementation Plan: the spool and the PowerSchool connector

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rollout step 2 of the record journal (spec `docs/superpowers/specs/2026-10-16-record-journal-design.md`, sections 4.3, 4.9c, 4.14, 5.1, 8, 9), first half: a shared connector-side spool and journal client (`connector/records`), and the PowerSchool connector emitting its seven record types into the journal in shadow, behind `records.enabled` (default false), with the chart changes that give it its own projected ServiceAccount token.

**Architecture:** `connector/records` is a framework package with no knowledge of any source: field and key helpers that produce values the journal accepts, a durable spool directory on the connector's state volume (one file per snapshot, written atomically, sent oldest first per scope), a classifier that maps every journal answer to a spool action by `reason` (spec 4.3 as built), a pinned HTTP client that presents the pod's projected token (audience `glovebox-journal`), a sender loop with per-scope and global backoff, bounds (`max_age`, `max_bytes`), and OTel metrics in the framework's style. `connector/records/recordstest` is a fake journal that implements the whole 4.3 table. The PowerSchool connector gets pure builder functions (pages in, `journalapi.Snapshot`s out) and one emission call at the point in `pollKid` where today's items are staged, after every verification has passed. With `records.enabled` off nothing new is constructed: no spool directory, no metric, no log line, no goroutine.

**Tech Stack:** Go (module `github.com/leftathome/glovebox`, `GOTOOLCHAIN=go1.27.1`), `net/http`, `github.com/google/uuid` (already a dependency), OpenTelemetry metrics through the connector framework's Prometheus exporter, `github.com/leftathome/powerschool-go` v0.4.0 (unchanged), Helm chart `charts/glovebox`, promtool for the alert tests. No new dependency.

**Companion plan:** `docs/superpowers/plans/2026-10-28-record-journal-part-b2-schoology.md` (B2: the Schoology connector on the same package). B2 starts after B1's Tasks 1-11 have merged; it does not need B1's PowerSchool tasks.

---

## How to work this plan

**Bead:** none exists yet for Part B. Before Task 1, file one under the epic (`bd create --parent glovebox-wekc ... "Record journal Part B: school connectors emit records in shadow"`), with two children, B1 and B2, and claim B1. Note in the bead that `glovebox-wekc.15` (a per-child snapshot item) is superseded by this work once step 6 of the rollout lands; do not close it here. Branch: `feat/record-journal-part-b1` off `origin/main` (77c8449 or later). This plan file is the progress record: tick each checkbox as the step is done and commit the tick with the step's commit.

**Feedback loop, every task** (from the repository root):

```bash
export GOTOOLCHAIN=go1.27.1
gofmt -w <the files you created or changed>
go build ./...
go vet ./connector/... ./connectors/powerschool/...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./connector/records/... ./connectors/powerschool/...
go test ./connector/... ./connectors/powerschool/... -count=1 -race
# chart tasks (17-21) also:
scripts/chart-checks.sh
```

No task here writes PostgreSQL, so `scripts/journal-test-db.sh` is not needed; the journal is represented by the fake in `connector/records/recordstest`. Task 23 runs one end-to-end test against the real journal core with a database (`scripts/journal-test-db.sh ./connector/records/...`), because the schema and the record types are what the connector's output must satisfy.

**How far this plan's code was checked:** it was written against `origin/main` 77c8449 by reading the code; the code blocks were **not** compiled. Signatures named from existing code (`journalapi.Snapshot`, `record.ValidateSnapshot`, `record.NewRegistry`, `record.Encode`, `connector.RuleMatcher`, `connector.Metrics.Provider`, `atomicfile.WriteFile`, the PowerSchool `Env`, `courseKey`, `attendanceCourseKey`, `splitGrade`, `textOf`, `flagSet`, `dueDay`, `AttendanceDaysConfig.kindOf`) were checked against the source. Expect to adjust details; the tests define the behaviour. Where the repository and this plan disagree, the repository wins: record the deviation as a NOTE under the task, as the Part A plan did.

**Rules that hold for every file this plan creates:**

- ASCII only, no emoji, in code, comments, test data and docs.
- No real names, schools, courses or ids anywhere: this repository is mirrored publicly. New test data uses `e_child1`, `e_child2`, `e_parent1`, kid labels `k1`, `k2`, course ids like `c1`, and course names like `EXAMPLE SCIENCE 7`. (Existing PowerSchool fixtures use `e_333333` / `e_444444`; new record tests use their own rules helper with `e_child1` / `e_child2`.)
- The connectors do not pre-filter text. `text` fields carry what the library returned (only made valid for JSON and the journal's limits); the journal scans them (spec 4.11). `string` fields are cleaned to one line because the journal refuses control characters in them.
- Records carry `data_subject` = the roster member id. Both connectors already resolve it: a kid label (`k1`) plus the source's student id is configured under `kids`, and the routing rule for `<source>:<kid>:<surface>` carries `data_subject` (`e_...`, the glovebox entity id). The openclaw roster (`household.json`) is keyed by the same entity ids (openclaw `image/internal/household`: "The roster binds opaque glovebox entity ids (the people map keys) to an agent"). So no new mapping is configured: records take `data_subject` and `audience` from the same rule that routes the kid's items. A rule subject that is not on the roster is answered `409 unknown_subject` and stays in the spool; the rollout check in Task 24 compares them before enabling.
- One Kubernetes object per new chart template file.

---

## Decisions for Steve (open; the plan implements the recommendation and marks the spot)

Each is restated in the task that implements it. None is invented spec: where the spec is silent or does not fit the code, the plan says so and recommends.

| # | Question | Where the spec stands | Recommendation (implemented, easy to flip) |
|---|---|---|---|
| S1 | **Settling time** for days with unmarked cells (spec 11, question 2). | "at least three school days old"; asks whether three is right and whether it should be configurable. | Configurable: `records.attendance_day_settle_school_days`, default 3, bounds 1-10. |
| S2 | **What a "school day" is** when counting the settling age. | Not defined. | A grid date on or before today (school time zone) on which at least one cell is not `-`. A day is N school days old when N such dates follow it, up to and including today. |
| S3 | **`attendance_day` `complete_range.to`**. | "the first and last settled date the grid showed". An unsettled day can lie between two settled days (yesterday settled, the day before still has a `.`), so a range up to the last settled date would let the snapshot remove a day it did not judge. | `from` = the first grid date of the school year; `to` = the last date of the longest run of settled grid dates from `from` (every grid date up to `to` is settled). Days are emitted only inside the range. |
| S4 | **`term_comment` scope.** | `(powerschool, term_comments, subject)`, complete. But comments are read one reporting term per page, each page throttled on its own (`teacher_comments.min_interval_hours`), so a snapshot of one term's page is not complete for the subject: it would remove every other term's comments. | Add a `term` part: `(powerschool, term_comments, subject, term)`, complete per term page. |
| S5 | **`term_grade` key.** | `<course_number>:<term>` with `#n` on collision. The connector's item key is `<course_number or name:...>:<completion month>:<attempted>|<school>|<grade level>|<name>` (with `#n` only for rows identical in all of that), so the key does not depend on page order. | Keep the connector's existing key. It equals today's checkpoint id (parity tests and the seed can match on it) and is order-independent. |
| S6 | **`term_comment` key.** | `<course>:<term>`. The connector's item key is `<course_number or name:...>|<expression>:<term>` (`#n` on collision): two sections of one course in one term have different expressions. | Keep the connector's existing key, for the same reasons as S5. |
| S7 | **`course_mark` key fallback.** | "course id; else number-period". The home page grade row has no course number; the connector's `courseKey` falls back to `name:<lowercased name>|<period>`. | Keep `courseKey` (also the `assignment` and `attendance_total` key prefix, and the scope's `course` value). |
| S8 | **Long `text` values.** | `text` at most 64 KiB; a snapshot over a limit is refused whole (413), which would refuse a course's whole assignment list on every poll. | The connector cuts a `text` value at 64 KiB on a rune boundary (counted in `glovebox_journal_record_values_cut_total`), adds no marker and no extra field. The PowerSchool library already caps teacher comments at 16 KiB; descriptions are uncapped. |
| S9 | **`assignment.absent`.** | Field list has `missing`, `late`, `incomplete`, `exempt`, `collected`; the library also reports `absent` and today's items carry it. | Add `absent` bool. (Spec 5.1's table to be amended.) |
| S10 | **`attendance_mark.excused` for `authorized` and `unknown`.** | `excused` bool, no rule. | `true` for `*_excused`, `false` for `*_unexcused`, `null` for `authorized` and `unknown`. |
| S11 | **Custom attendance kinds.** | Kinds are "one of the connector's configured kinds" (the seven names plus `unknown`); `attendance_days.kinds` accepts any kind name today. `attendance_day` counting needs to know which kind is an absence. | With `records.enabled`, the config is refused unless every configured kind is one of the seven names. |
| S12 | **Mailbox login with records.** | 4.9c: a connector with journal rights gets only a projected token. The PowerSchool connector in `loginOnDemand` (mailbox) mode mounts its API token (`automountServiceAccountToken: true`). | The chart refuses `records.enabled` together with `loginOnDemand.enabled`; the login broker mode (projected token for the broker's audience) is fine. |
| S13 | **Records without the journal.** | Silent. | The chart refuses `connectors.powerschool.records.enabled` unless `journal.enabled`, `journal.networkPolicy.writerConnectors` lists `powerschool`, and `journal.services` has the connector's entry with `write_types`. Explicit values, reviewed in gitops; nothing is added implicitly. |
| S14 | **Where the record type definitions live.** | Part A: "the chart ships none". | Defined once in Go in the connector (`RecordTypes()`), written to `charts/glovebox/journal-record-types/powerschool.json` by a generator test, and rendered into the journal config when `journal.connectorRecordTypes` lists `powerschool` (default `[]`). A test fails when the file and the Go definitions drift. |
| S15 | **The seed (rollout step 3).** | Section 7: a one-time Job over retained items. | **Not in this plan or B2.** It reads a different input (items retained since 2026-10-10, parsed from tags and body lines), needs its own operator ServiceAccount and policy entry, and must produce exactly the keys the live connectors produce. B1 and B2 make the key and builder functions exported for it. Write the seed plan after B1 has shipped and an inventory of the retained items exists. |

---

## Where Part A as built differs from what the spec assumed, and how this plan handles it

Taken from the Part A plan's NOTEs (Tasks 15, 23, 24, 25, 26, 29, 31) and `docs/journal-api.md`.

| # | As built | Spec assumed | Handling here |
|---|---|---|---|
| A1 | The 4.3 table gained `404 not_found`, `400 bad_request` for a query string, and `503 unavailable` with `Retry-After: 1` at the in-flight limit (16). | The original table. | The classifier (Task 5) has a row for each; the sender honours `Retry-After`. |
| A2 | A scope whose `source` is not the type's `owner` is `422 schema` at validation, not `403 policy`. | `403 policy`. | Both are permanent (`rejected/`); the classifier keys on `reason`; a test covers both. |
| A3 | Bodies decode strictly: unknown, duplicate and case-variant keys, also inside `fields`, are `422 schema`. | Unknown fields refused. | Builders write exactly the declared field set, every field present (`null` when unknown). A test checks every record against its type, and a test re-decodes payloads through `internal/journal/strictjson`. |
| A4 | Key caps per object = widest record type's field count + 16. | None. | The widest PowerSchool type has 17 fields; the cap follows the configured types, so this only matters if a type grows past 9 + 16. Noted in the docs. |
| A5 | The chart ships no record types. | Types "defined in configuration". | S14. |
| A6 | `string` values with C0 controls are refused (422, permanent); C1, bidi and zero-width characters are replaced by U+FFFD; `text` is kept as received. | Limits only. | `records.Fields.String` reduces values to one line without C0; `Text` only drops NUL and invalid UTF-8 and applies S8. |
| A7 | Keys 1-512 bytes, scope values 1-256 bytes, both without control characters; the scope part order is part of the scope's identity. | Not stated. | `records.SafeKey` / `SafeScopeValue` pass a clean value through and replace any other with `h:` + 32 hex of its SHA-256. Scopes are built by one constructor in a fixed order (`source`, `surface`, `data_subject`, then the rest). |
| A8 | `write_subjects: roster_subjects` is required for source writers; `409 unknown_subject` also when no roster is loaded. | 409 for a subject not in the roster. | 409 blocks only that scope and is retried until `max_age` (then dropped and counted). Task 24's rollout check compares rule subjects with the roster before enabling. |
| A9 | `snapshot_id` must be a UUID; a resend for the same writer, type and scope is answered from the stored outcome (`duplicate` / `held`); the same id elsewhere is `422 snapshot_id_reused`. | Same, less precise. | The id is assigned when the file is written and the file is resent byte for byte, so a resend after a crash is a `duplicate`. |
| A10 | Erase bans **all** later data about the member (`410`), and does not reach connector spools (bead glovebox-wekc.18.1). | 410 for older observations. | A `410` deletes the file and also purges every pending and rejected spool file of that `data_subject` (Task 9). Counted as `dropped{reason="erased_subject"}`. |
| A11 | The removal guard holds only when the scope has **more than** `min_keys` live keys in range and the snapshot removes all of them or more than `max_fraction` (Part A decision 1). | Literal reading held 1 of 1. | No test here expects a hold for a small scope; a `held` answer is a 200 and deletes the file. |
| A12 | `staleness` prefixes are only `source=<v>/` or `source=<v>/surface=<v>/`. | Per scope prefix. | Task 21 documents `source=powerschool/` with `maxAgeHours: 96`. |
| A13 | The chart refuses a `writerConnectors` entry whose ServiceAccount has no `write_types` in `journal.services`. | Not stated. | S13 relies on it and adds the reverse check (records on, connector not a writer). |
| A14 | At most 16 requests in flight; no per-caller rate limit (glovebox-wekc.18.4). | Not stated. | The sender sends one snapshot at a time, sequentially. |
| A15 | Each request is bounded by `journal.requestTimeoutSeconds` (default 60), TokenReview included. | Not stated. | Client timeout default 90 s (`records.timeout_seconds`, 10-600). |
| A16 | `GET /v1/healthz` needs a token and is 503 until ready. | Not stated. | The sender never probes; it sends and acts on the answer. |
| A17 | 4.9c: connectors run as `<fullname>-connector-<name>` with no token mounted (rollout step 0, shipped). | Same. | The projected token volume is added only when `records.enabled` (Task 18). |
| A18 | A 200 body may carry `stats.stale`. | No `stale`. | Ignored; logged at debug. |

---

## File structure

**New framework package `connector/records`** (one responsibility per file):

| File | Responsibility |
|---|---|
| `connector/records/config.go` | `Config` (the connector's `records` block), defaults, validation. |
| `connector/records/values.go` | `Fields` (typed field writers), `SafeKey`, `SafeScopeValue`, `CutText`. |
| `connector/records/snapshot.go` | `Scope` constructor, `NewSnapshot` (UUID, UTC, ordering), `SchoolYear`. |
| `connector/records/spool.go` | The spool directory: open (recovery), put, list, delete, reject, purge. |
| `connector/records/bounds.go` | `max_age` and `max_bytes` enforcement. |
| `connector/records/classify.go` | `(code, reason) -> Action`, spec 4.3 as built. |
| `connector/records/client.go` | Pinned HTTP client, token file, answer parsing, `Retry-After`. |
| `connector/records/sender.go` | One drain pass: per-scope ordering, blocking, backoff. |
| `connector/records/run.go` | `Emitter`: the object a connector holds (spool + sender + loop + metrics). |
| `connector/records/metrics.go` | The spool metrics (OTel, framework provider). |
| `connector/records/recordstest/journal.go` | Fake journal implementing the 4.3 table, optional validation through `record.ValidateSnapshot`. |
| `connector/records/*_test.go` | Tests per file; `crash_test.go` for crash and restart. |

**PowerSchool connector** (`connectors/powerschool/`):

| File | Responsibility |
|---|---|
| `record_types.go` | `RecordTypes()`: the seven `record.Type` definitions; type name constants. |
| `record_binding.go` | `subjectFor(matcher, kid, surface)`: data_subject and audience from the kid's rule; records-time config checks. |
| `records_grades.go` | `BuildCourseMarks`, `BuildAttendanceTotals` (home page rows). |
| `records_assignments.go` | `BuildAssignments` (one snapshot per course read). |
| `records_attendance.go` | `BuildAttendanceMarks` (per school year, `complete_range`). |
| `records_attendance_day.go` | `BuildAttendanceDays`: aggregation, classification, settled days. |
| `records_history.go` | `BuildTermGrades`, `BuildTermComments`. |
| `records_emit.go` | `emitKidRecords`: called from `pollKid`; spool write errors never touch items. |
| `config.go` (modify) | `Records records.Config` field, validation hook. |
| `connector.go` (modify) | hold the emitter; call `emitKidRecords`; start the loop in `Watch`. |
| `cmd/powerschool/main.go` (modify) | build the emitter when enabled. |
| `records_*_test.go`, `records_parity_test.go`, `records_off_test.go` | Tests. |

**Chart** (`charts/glovebox/`):

| File | Change |
|---|---|
| `values.yaml` | `connectors.powerschool.records`, `journal.connectorRecordTypes`. |
| `templates/connector-configmap.yaml` | render the `records` block when enabled. |
| `templates/connector-deployment.yaml` | projected token volume and mount when enabled. |
| `templates/_helpers.tpl` | `glovebox.recordsChecks` (S12, S13), `glovebox.journalRecordTypes`. |
| `templates/configmap.yaml` | append connector record types to `record_types`. |
| `templates/connector-records-prometheusrule.yaml` | new: the spool alerts. |
| `journal-record-types/powerschool.json` | new: generated from `RecordTypes()`. |
| `scripts/test-chart-records.sh`, `scripts/testdata/records-values.yaml`, `scripts/testdata/records-alerts-test.yaml` | new: render and alert tests. |

**Docs:** `docs/journal-api.md` (a "Connector side: the spool" section), `docs/connectors/powerschool.md` (records), `docs/upgrading.md`, `CHANGELOG.md`, and spec 5.1 amendments for S4-S7, S9-S11.

---

## Task 1: `connector/records` configuration

**Files:**
- Create: `connector/records/config.go`
- Test: `connector/records/config_test.go`

The connector's config gains a `records` block. Absent or `enabled: false` means nothing else in it is read or checked (so today's configs keep working byte for byte).

- [ ] **Step 1.1: Write the failing tests.**

```go
package records

import (
	"strings"
	"testing"
	"time"
)

func TestConfigDisabledIsNotChecked(t *testing.T) {
	c := Config{} // enabled false, everything empty
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled config refused: %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	c := Config{Enabled: true, JournalURL: "http://glovebox-glovebox-journal:9095", TokenFile: "/var/run/secrets/glovebox-journal/token"}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 90*time.Second || c.Spool.MaxAge() != 7*24*time.Hour || c.Spool.MaxBytes != 256<<20 {
		t.Fatalf("defaults: %+v", c)
	}
	if c.Spool.MinBackoff() != 30*time.Second || c.Spool.MaxBackoff() != time.Hour {
		t.Fatalf("backoff defaults: %+v", c.Spool)
	}
}

func TestConfigRefusals(t *testing.T) {
	ok := func() Config {
		c := Config{Enabled: true, JournalURL: "http://j:9095", TokenFile: "/t/token"}
		c.ApplyDefaults()
		return c
	}
	cases := map[string]func(*Config){
		"no url":          func(c *Config) { c.JournalURL = "" },
		"url with path":   func(c *Config) { c.JournalURL = "http://j:9095/v1" },
		"url with query":  func(c *Config) { c.JournalURL = "http://j:9095?a=1" },
		"url with user":   func(c *Config) { c.JournalURL = "http://u@j:9095" },
		"ftp url":         func(c *Config) { c.JournalURL = "ftp://j" },
		"relative token":  func(c *Config) { c.TokenFile = "token" },
		"timeout low":     func(c *Config) { c.TimeoutSeconds = 5 },
		"timeout high":    func(c *Config) { c.TimeoutSeconds = 601 },
		"max age zero":    func(c *Config) { c.Spool.MaxAgeHours = -1 },
		"max bytes small": func(c *Config) { c.Spool.MaxBytes = 1 << 10 },
		"backoff order":   func(c *Config) { c.Spool.MinBackoffSeconds, c.Spool.MaxBackoffSeconds = 600, 60 },
	}
	for name, mutate := range cases {
		c := ok()
		mutate(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "records") {
			t.Errorf("%s: err = %v, want a records error", name, err)
		}
	}
}
```

- [ ] **Step 1.2: Run; expect FAIL** (`go test ./connector/records/ -run TestConfig -count=1`: package does not exist).

- [ ] **Step 1.3: Implement.**

```go
// Package records is the connector side of the record journal (design spec
// docs/superpowers/specs/2026-10-16-record-journal-design.md, 4.3, 4.14):
// typed field values the journal accepts, a durable spool of snapshots on
// the connector's state volume, and the sender that delivers them in order
// and acts on every answer by its reason. It knows nothing about any source.
package records

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config is a connector's "records" block. Nothing in it is read unless
// Enabled is true.
type Config struct {
	Enabled bool `json:"enabled"`
	// JournalURL is the journal listener, scheme and host only
	// ("http://glovebox-glovebox-journal:9095"). The chart sets it.
	JournalURL string `json:"journal_url"`
	// TokenFile is the projected ServiceAccount token for the audience
	// glovebox-journal. Re-read for every request; sent to JournalURL only.
	TokenFile string `json:"token_file"`
	// TimeoutSeconds bounds one request (default 90, 10-600). Above the
	// journal's own request timeout (default 60).
	TimeoutSeconds int         `json:"timeout_seconds"`
	Spool          SpoolConfig `json:"spool"`
}

// SpoolConfig bounds the spool (spec 4.14) and its retries.
type SpoolConfig struct {
	MaxAgeHours       int   `json:"max_age_hours"`       // default 168 (7 days)
	MaxBytes          int64 `json:"max_bytes"`           // default 256 MiB
	MinBackoffSeconds int   `json:"min_backoff_seconds"` // default 30
	MaxBackoffSeconds int   `json:"max_backoff_seconds"` // default 3600
}

func (c *Config) ApplyDefaults() {
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = 90
	}
	if c.Spool.MaxAgeHours == 0 {
		c.Spool.MaxAgeHours = 168
	}
	if c.Spool.MaxBytes == 0 {
		c.Spool.MaxBytes = 256 << 20
	}
	if c.Spool.MinBackoffSeconds == 0 {
		c.Spool.MinBackoffSeconds = 30
	}
	if c.Spool.MaxBackoffSeconds == 0 {
		c.Spool.MaxBackoffSeconds = 3600
	}
}

func (c Config) Timeout() time.Duration { return time.Duration(c.TimeoutSeconds) * time.Second }
func (s SpoolConfig) MaxAge() time.Duration { return time.Duration(s.MaxAgeHours) * time.Hour }
func (s SpoolConfig) MinBackoff() time.Duration {
	return time.Duration(s.MinBackoffSeconds) * time.Second
}
func (s SpoolConfig) MaxBackoff() time.Duration {
	return time.Duration(s.MaxBackoffSeconds) * time.Second
}

// Validate checks an enabled block (after ApplyDefaults). A disabled block
// is never checked.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	u, err := url.Parse(strings.TrimSpace(c.JournalURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("records.journal_url must be an http or https URL with a host and nothing else")
	}
	if !strings.HasPrefix(c.TokenFile, "/") || strings.ContainsAny(c.TokenFile, " \t\n") {
		return errors.New("records.token_file must be the absolute path of the token projected for glovebox-journal")
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 600 {
		return errors.New("records.timeout_seconds must be between 10 and 600")
	}
	if c.Spool.MaxAgeHours < 1 || c.Spool.MaxAgeHours > 24*90 {
		return errors.New("records.spool.max_age_hours must be between 1 and 2160")
	}
	if c.Spool.MaxBytes < 1<<20 || c.Spool.MaxBytes > 4<<30 {
		return errors.New("records.spool.max_bytes must be between 1 MiB and 4 GiB")
	}
	if c.Spool.MinBackoffSeconds < 1 || c.Spool.MaxBackoffSeconds < c.Spool.MinBackoffSeconds || c.Spool.MaxBackoffSeconds > 86400 {
		return fmt.Errorf("records.spool: need 1 <= min_backoff_seconds <= max_backoff_seconds <= 86400")
	}
	return nil
}
```

- [ ] **Step 1.4: Run; expect PASS.**
- [ ] **Step 1.5: Commit.**

```bash
git add connector/records
git commit -m "feat(records): connector records config block, off unless enabled"
```

---

## Task 2: Field values, keys and scope values the journal accepts

**Files:**
- Create: `connector/records/values.go`
- Test: `connector/records/values_test.go`

Every field the journal will see goes through `Fields`. Rules (A3, A6, A7, S8): every declared field present, `null` when unknown; `string` one line, no C0 controls, at most 1024 bytes; `text` valid UTF-8 without NUL, at most 64 KiB, otherwise **unchanged** (no scrubbing of format characters, no indentation, no 4 KiB cut: the journal scans it); keys and scope values pass through when clean and short, else become `h:<32 hex>`.

- [ ] **Step 2.1: Write the failing tests.**

```go
package records

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/leftathome/glovebox/internal/journal/record"
)

func TestFieldsStringIsOneCleanLine(t *testing.T) {
	f := NewFields(nil)
	f.String("course", "EXAMPLE  SCIENCE 7\t\n(A)\x00\x07")
	f.String("empty", "  \t ")
	m := f.Map()
	if got := string(m["course"]); got != `"EXAMPLE SCIENCE 7 (A)"` {
		t.Fatalf("course = %s", got)
	}
	if got := string(m["empty"]); got != "null" {
		t.Fatalf("blank string = %s, want null", got)
	}
	f.String("long", strings.Repeat("a", 2000))
	var s string
	_ = json.Unmarshal(f.Map()["long"], &s)
	if len(s) > record.MaxStringBytes {
		t.Fatalf("string of %d bytes", len(s))
	}
}

func TestFieldsTextIsKeptAsReceived(t *testing.T) {
	in := "Line one.\r\n\tLine\u202e two.\u200b\n"
	f := NewFields(nil)
	f.Text("comment", in)
	var out string
	if err := json.Unmarshal(f.Map()["comment"], &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("text changed: %q, want %q (the journal scans it; connectors do not pre-filter)", out, in)
	}
}

func TestFieldsTextDropsOnlyNULAndInvalidUTF8AndCutsAt64KiB(t *testing.T) {
	f := NewFields(nil)
	f.Text("a", "x\x00y\xffz")
	if string(f.Map()["a"]) != `"xyz"` {
		t.Fatalf("a = %s", f.Map()["a"])
	}
	cut := 0
	f2 := NewFields(func() { cut++ })
	f2.Text("b", strings.Repeat("\u00e9", record.MaxTextBytes)) // 2 bytes each
	var s string
	_ = json.Unmarshal(f2.Map()["b"], &s)
	if len(s) > record.MaxTextBytes || !utf8.ValidString(s) || cut != 1 {
		t.Fatalf("len %d valid %v cut %d", len(s), utf8.ValidString(s), cut)
	}
}

func TestFieldsTypedValues(t *testing.T) {
	f := NewFields(nil)
	n := 8.5
	f.Number("score", &n)
	f.Number("none", nil)
	f.Int("absences", 2)
	f.Bool("late", true)
	f.OptBool("excused", nil)
	f.Date("due", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC))
	f.Date("undated", time.Time{})
	f.Timestamp("at", time.Date(2026, 10, 20, 15, 4, 5, 0, time.FixedZone("x", -7*3600)))
	want := map[string]string{"score": "8.5", "none": "null", "absences": "2", "late": "true",
		"excused": "null", "due": `"2026-10-20"`, "undated": "null", "at": `"2026-10-20T22:04:05Z"`}
	for k, v := range want {
		if string(f.Map()[k]) != v {
			t.Errorf("%s = %s, want %s", k, f.Map()[k], v)
		}
	}
}

func TestSafeKeyAndScopeValue(t *testing.T) {
	if SafeKey("c1:98765") != "c1:98765" {
		t.Fatal("clean key changed")
	}
	k := SafeKey("name:example\nscience|1(A)")
	if !strings.HasPrefix(k, "h:") || len(k) != 34 {
		t.Fatalf("key with a newline = %q", k)
	}
	if SafeKey("name:example\nscience|1(A)") != k {
		t.Fatal("not deterministic")
	}
	if got := SafeKey(strings.Repeat("a", 600)); !strings.HasPrefix(got, "h:") {
		t.Fatal("long key kept")
	}
	if got := SafeScopeValue(strings.Repeat("a", 300)); !strings.HasPrefix(got, "h:") {
		t.Fatal("long scope value kept")
	}
	if got := SafeScopeValue(""); !strings.HasPrefix(got, "h:") {
		t.Fatal("empty scope value must not stay empty")
	}
}
```

- [ ] **Step 2.2: Run; expect FAIL.**

- [ ] **Step 2.3: Implement.**

```go
package records

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/leftathome/glovebox/internal/journal/record"
)

// Fields builds one record's fields.
type Fields struct {
	m     map[string]json.RawMessage
	OnCut func()
}

func NewFields(onCut func()) *Fields { return &Fields{m: map[string]json.RawMessage{}, OnCut: onCut} }

// Map returns the fields for journalapi.Record.Fields.
func (f *Fields) Map() map[string]json.RawMessage { return f.m }

var null = json.RawMessage("null")

func (f *Fields) Null(name string) { f.m[name] = null }

// String stores s as one line: every run of whitespace and control
// characters becomes one space, ends trimmed, cut to 1024 bytes on a rune
// boundary. Blank is null. The journal refuses C0 controls in strings
// (422, permanent) and replaces C1 and bidi controls itself.
func (f *Fields) String(name, s string) {
	s = OneLine(s)
	if s == "" {
		f.m[name] = null
		return
	}
	f.m[name] = record.Encode(s)
}

// OneLine is the cleaning String applies, for keys and scope values built
// from page text.
func OneLine(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s, _ = cutRunes(s, record.MaxStringBytes)
	return s
}

// Text stores s as received apart from what JSON and the journal cannot
// carry: invalid UTF-8 and NUL are dropped, and a value over 64 KiB is cut
// on a rune boundary (decision S8). Empty is null.
func (f *Fields) Text(name, s string) {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\x00", "")
	if s == "" {
		f.m[name] = null
		return
	}
	var cut bool
	if s, cut = cutRunes(s, record.MaxTextBytes); cut && f.OnCut != nil {
		f.OnCut()
	}
	f.m[name] = record.Encode(s)
}

func (f *Fields) Int(name string, v int)   { f.m[name] = record.Encode(int64(v)) }
func (f *Fields) Bool(name string, v bool) { f.m[name] = record.Encode(v) }
func (f *Fields) OptBool(name string, v *bool) {
	if v == nil {
		f.m[name] = null
		return
	}
	f.m[name] = record.Encode(*v)
}
func (f *Fields) Number(name string, v *float64) {
	if v == nil {
		f.m[name] = null
		return
	}
	f.m[name] = record.Encode(*v)
}

// Date stores the calendar date of t as written in t's own location; zero
// is null. Callers pass dates already in the school's calendar.
func (f *Fields) Date(name string, t time.Time) {
	if t.IsZero() {
		f.m[name] = null
		return
	}
	f.m[name] = record.Encode(t.Format("2006-01-02"))
}

func (f *Fields) Timestamp(name string, t time.Time) {
	if t.IsZero() {
		f.m[name] = null
		return
	}
	f.m[name] = record.Encode(t.UTC().Format(time.RFC3339))
}

func cutRunes(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	n := max
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

const (
	maxKeyBytes        = 512
	maxScopeValueBytes = 256
)

// SafeKey returns k when the journal accepts it as a record key (1-512
// bytes, valid UTF-8, no control characters), else "h:" + 32 hex digits of
// its SHA-256. Deterministic, so a record keeps its key across polls.
func SafeKey(k string) string { return safe(k, maxKeyBytes) }

// SafeScopeValue is SafeKey for a scope value (1-256 bytes).
func SafeScopeValue(v string) string { return safe(v, maxScopeValueBytes) }

func safe(s string, max int) string {
	if s != "" && len(s) <= max && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl) &&
		!strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cf, r) }) {
		return s
	}
	return "h:" + hash32(s)
}

func hash32(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}
```

Check `record.Encode` handles `int64`, `bool`, `float64`, `string` (it does: `encode` uses `json.Encoder` with HTML escaping off).

- [ ] **Step 2.4: Run; expect PASS.**
- [ ] **Step 2.5: Commit** (`feat(records): field writers, safe keys and scope values`).

---

## Task 3: Scopes and snapshots

**Files:**
- Create: `connector/records/snapshot.go`
- Test: `connector/records/snapshot_test.go`

- [ ] **Step 3.1: Write the failing tests.**

```go
func TestScopeOrderAndSubject(t *testing.T) {
	s := Scope("powerschool", "assignments", "e_child1", Part("course", "c1"))
	want := []journalapi.ScopePart{{"source", "powerschool"}, {"surface", "assignments"},
		{"data_subject", "e_child1"}, {"course", "c1"}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("scope = %v", s)
	}
}

func TestNewSnapshotIsValidForTheJournal(t *testing.T) {
	reg := testRegistry(t) // one type "t1" owner "src" with a string field "a" and a date field "d"
	f := NewFields(nil)
	f.String("a", "x")
	f.Date("d", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 10, 16, 13, 0, 41, 123, time.UTC)
	snap := NewSnapshot("t1", 1, Scope("src", "s", "e_child1"), true,
		&journalapi.CompleteRange{Field: "d", From: "2026-10-01", To: "2026-10-09"}, now,
		[]journalapi.Record{{Key: "k1", DataSubject: "e_child1", Audience: []string{"subject", "guardians"}, Fields: f.Map()}})
	if _, err := uuid.Parse(snap.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if !snap.ObservedAt.Equal(now.Truncate(time.Millisecond)) || snap.ObservedAt.Location() != time.UTC {
		t.Fatalf("observed_at %v", snap.ObservedAt)
	}
	if _, rej := record.ValidateSnapshot(reg, snap, now); rej != nil {
		t.Fatalf("journal would refuse: %v", rej)
	}
}

func TestSchoolYear(t *testing.T) {
	for in, want := range map[string]string{"2026-08-01": "2026-27", "2026-07-31": "2025-26", "2027-01-15": "2026-27", "2099-12-31": "2099-00"} {
		d, _ := time.Parse("2006-01-02", in)
		if got := SchoolYear(d); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}
```

`testRegistry` lives in `connector/records/helpers_test.go` and builds `record.NewRegistry([]record.Type{...})`.

- [ ] **Step 3.2: Run; expect FAIL.**

- [ ] **Step 3.3: Implement.**

```go
package records

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func Part(name, value string) journalapi.ScopePart { return journalapi.ScopePart{name, SafeScopeValue(value)} }

// Scope builds a scope in the one order every connector uses: source,
// surface, data_subject, then extra parts. The order is part of the scope's
// identity in the journal (canonical form), so nothing else builds scopes.
func Scope(source, surface, dataSubject string, extra ...journalapi.ScopePart) []journalapi.ScopePart {
	s := []journalapi.ScopePart{{"source", source}, {"surface", surface}, {"data_subject", dataSubject}}
	return append(s, extra...)
}

// NewSnapshot assembles a snapshot with a fresh UUID. observed_at is UTC,
// truncated to milliseconds (the spool's file name carries it).
func NewSnapshot(recordType string, schemaVersion int, scope []journalapi.ScopePart, complete bool,
	rng *journalapi.CompleteRange, observedAt time.Time, recs []journalapi.Record) journalapi.Snapshot {
	if recs == nil {
		recs = []journalapi.Record{}
	}
	return journalapi.Snapshot{
		SnapshotID: uuid.NewString(), RecordType: recordType, SchemaVersion: schemaVersion,
		Scope: scope, Complete: complete, CompleteRange: rng,
		ObservedAt: observedAt.UTC().Truncate(time.Millisecond), Records: recs,
	}
}

// SchoolYear names the school year of a calendar date: 1 August to 31 July,
// written "2026-27" (spec 5.1).
func SchoolYear(d time.Time) string {
	y := d.Year()
	if d.Month() < time.August {
		y--
	}
	return fmt.Sprintf("%d-%02d", y, (y+1)%100)
}
```

- [ ] **Step 3.4: Run; expect PASS.**
- [ ] **Step 3.5: Commit** (`feat(records): scope constructor, snapshot assembly, school year`).

---

## Task 4: The spool directory: put, list, recover

**Files:**
- Create: `connector/records/spool.go`
- Test: `connector/records/spool_test.go`, `connector/records/crash_test.go`

Layout under the state directory: `spool/` holds pending files, `spool/rejected/` holds permanently refused ones. A file is `<observed_at>-<snapshot_id>.json`, `observed_at` as `20060102T150405.000Z` (UTC, fixed width, so names sort by time). Written with `internal/atomicfile.WriteFile` (temp file, fsync, rename), then the directory is fsynced. On open: stale `*.tmp-*` siblings (a crash during a write) are removed; a file whose name or content cannot be parsed is moved to `rejected/` and counted (`rejected{reason="unreadable"}`), never deleted silently and never sent.

- [ ] **Step 4.1: Write the failing tests.**

```go
func TestPutWritesOneFileNamedByTimeAndID(t *testing.T) {
	dir := t.TempDir()
	sp := openTestSpool(t, dir)
	snap := testSnapshot(t, "e_child1", "c1", at(13, 0))
	if err := sp.Put(snap); err != nil {
		t.Fatal(err)
	}
	names := listNames(t, filepath.Join(dir, "spool"))
	want := "20261016T130000.000Z-" + snap.SnapshotID + ".json"
	if len(names) != 1 || names[0] != want {
		t.Fatalf("files %v, want [%s]", names, want)
	}
	got := readSnapshot(t, filepath.Join(dir, "spool", want))
	if !reflect.DeepEqual(got, snap) {
		t.Fatal("round trip changed the snapshot")
	}
}

func TestPendingIsOldestFirstWithScopeAndSubject(t *testing.T) {
	sp := openTestSpool(t, t.TempDir())
	b := testSnapshot(t, "e_child1", "c1", at(14, 0))
	a := testSnapshot(t, "e_child1", "c2", at(13, 0))
	c := testSnapshot(t, "e_child2", "c1", at(13, 0))
	for _, s := range []journalapi.Snapshot{b, a, c} {
		_ = sp.Put(s)
	}
	es, err := sp.Pending()
	if err != nil {
		t.Fatal(err)
	}
	// Ties on observed_at sort by name (the id), so the order is total.
	if es[0].ObservedAt.After(es[1].ObservedAt) || es[2].ID != b.SnapshotID {
		t.Fatalf("order %v", es)
	}
	if es[2].DataSubject != "e_child1" || es[2].ScopeKey == "" {
		t.Fatalf("entry fields %+v", es[2])
	}
}

func TestOpenRemovesTempFilesAndQuarantinesUnreadable(t *testing.T) {
	dir := t.TempDir()
	sp := openTestSpool(t, dir)
	good := testSnapshot(t, "e_child1", "c1", at(13, 0))
	_ = sp.Put(good)
	spool := filepath.Join(dir, "spool")
	// What a crash mid-write leaves behind, and two kinds of garbage.
	mustWrite(t, filepath.Join(spool, "20261016T130500.000Z-x.json.tmp-1-2"), `{"snapshot_id":`)
	mustWrite(t, filepath.Join(spool, "20261016T131000.000Z-"+uuid.NewString()+".json"), `{not json`)
	mustWrite(t, filepath.Join(spool, "notes.txt"), "x")

	m := newTestMetrics(t)
	sp2, err := OpenSpool(dir, testSpoolConfig(), m, fixedNow(at(15, 0)))
	if err != nil {
		t.Fatal(err)
	}
	es, _ := sp2.Pending()
	if len(es) != 1 || es[0].ID != good.SnapshotID {
		t.Fatalf("pending after recovery: %v", es)
	}
	if n := len(listNames(t, filepath.Join(spool, "rejected"))); n != 2 {
		t.Fatalf("rejected/ has %d files, want 2 (the unparseable json and the stray file)", n)
	}
	if m.counter("glovebox_journal_spool_rejected_total", "reason", "unreadable") != 2 {
		t.Fatal("unreadable files not counted")
	}
	if fileExists(filepath.Join(spool, "20261016T130500.000Z-x.json.tmp-1-2")) {
		t.Fatal("temp file left behind")
	}
}

func TestPutFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	sp := openTestSpool(t, dir)
	_ = os.Chmod(filepath.Join(dir, "spool"), 0o500)
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "spool"), 0o700) })
	if err := sp.Put(testSnapshot(t, "e_child1", "c1", at(13, 0))); err == nil {
		t.Skip("running as root: directory permissions are not enforced")
	}
	if n := len(listNames(t, filepath.Join(dir, "spool"))); n != 0 {
		t.Fatalf("%d files after a failed put", n)
	}
}
```

Helpers (`openTestSpool`, `testSnapshot` builds a one-record snapshot for scope `(src, s, subject, course)`, `at(h, m)` = 2026-10-16 h:m UTC, `listNames` ignores directories, `newTestMetrics` wraps an `sdkmetric.ManualReader`) go in `connector/records/helpers_test.go`.

- [ ] **Step 4.2: Run; expect FAIL.**

- [ ] **Step 4.3: Implement.**

```go
package records

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/leftathome/glovebox/internal/atomicfile"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

const (
	spoolDirName    = "spool"
	rejectedDirName = "rejected"
	nameTimeLayout  = "20060102T150405.000Z"
)

// Entry is one pending or rejected spool file.
type Entry struct {
	Name        string
	Path        string
	ObservedAt  time.Time
	ID          string
	ScopeKey    string // the scope's parts as JSON: a grouping key, not the canonical form
	DataSubject string
	Size        int64
	Rejected    bool
}

// Spool is the directory of snapshots waiting to be sent. Safe for one
// writer (the poll) and one sender at a time; mu serialises them.
type Spool struct {
	dir, rejected string
	cfg           SpoolConfig
	m             *Metrics
	now           func() time.Time
	mu            sync.Mutex
}

// OpenSpool creates spool/ and spool/rejected/ under stateDir (0700) and
// recovers from a crash: temp files are removed; files that cannot be read
// as a snapshot are moved to rejected/ and counted.
func OpenSpool(stateDir string, cfg SpoolConfig, m *Metrics, now func() time.Time) (*Spool, error) {
	s := &Spool{dir: filepath.Join(stateDir, spoolDirName), cfg: cfg, m: m, now: now}
	s.rejected = filepath.Join(s.dir, rejectedDirName)
	if err := os.MkdirAll(s.rejected, 0o700); err != nil {
		return nil, fmt.Errorf("records: create spool: %w", err)
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	s.refreshGauges()
	return s, nil
}

func fileName(snap journalapi.Snapshot) string {
	return snap.ObservedAt.UTC().Format(nameTimeLayout) + "-" + snap.SnapshotID + ".json"
}

// Put writes snap to the spool. Nothing is sent here. observed_at is
// normalised (UTC, milliseconds) so the file name and the content agree.
func (s *Spool) Put(snap journalapi.Snapshot) error {
	snap.ObservedAt = snap.ObservedAt.UTC().Truncate(time.Millisecond)
	body, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("records: encode snapshot: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := atomicfile.WriteFile(filepath.Join(s.dir, fileName(snap)), body, 0o600); err != nil {
		s.m.writeError()
		return err
	}
	syncDir(s.dir)
	s.refreshGaugesLocked()
	return nil
}

// header is what the spool reads back from a file.
type header struct {
	SnapshotID string                 `json:"snapshot_id"`
	Scope      []journalapi.ScopePart `json:"scope"`
	ObservedAt time.Time              `json:"observed_at"`
}

func parseEntry(dir, name string, rejected bool) (Entry, error) {
	if !strings.HasSuffix(name, ".json") || len(name) < len(nameTimeLayout)+1+36+5 {
		return Entry{}, errors.New("not a spool file name")
	}
	ts, err := time.Parse(nameTimeLayout, name[:len(nameTimeLayout)])
	if err != nil {
		return Entry{}, err
	}
	id := strings.TrimSuffix(name[len(nameTimeLayout)+1:], ".json")
	if _, err := uuid.Parse(id); err != nil {
		return Entry{}, err
	}
	path := filepath.Join(dir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		return Entry{}, err
	}
	var h header
	if err := json.Unmarshal(raw, &h); err != nil || h.SnapshotID != id || !h.ObservedAt.Equal(ts) || len(h.Scope) == 0 {
		return Entry{}, errors.New("content does not match the name")
	}
	key, _ := json.Marshal(h.Scope)
	e := Entry{Name: name, Path: path, ObservedAt: ts, ID: id, ScopeKey: string(key), Size: int64(len(raw)), Rejected: rejected}
	for _, p := range h.Scope {
		if p[0] == "data_subject" {
			e.DataSubject = p[1]
		}
	}
	return e, nil
}

// Pending lists spool/ oldest first (observed_at, then name).
func (s *Spool) Pending() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list(s.dir, false)
}

func (s *Spool) list(dir string, rejected bool) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range des {
		if de.IsDir() || strings.Contains(de.Name(), ".tmp-") {
			continue
		}
		if e, err := parseEntry(dir, de.Name(), rejected); err == nil {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ObservedAt.Equal(out[j].ObservedAt) {
			return out[i].ObservedAt.Before(out[j].ObservedAt)
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// Delete removes a file that the journal has accepted or that must not be
// kept.
func (s *Spool) Delete(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(e.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syncDir(filepath.Dir(e.Path))
	s.refreshGaugesLocked()
	return nil
}

// Reject moves a pending file to rejected/ and counts reason.
func (s *Spool) Reject(e Entry, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Rename(e.Path, filepath.Join(s.rejected, e.Name)); err != nil {
		return err
	}
	syncDir(s.dir)
	syncDir(s.rejected)
	s.m.rejectedOne(reason)
	s.refreshGaugesLocked()
	return nil
}

// Body returns the file's bytes, sent verbatim (A9).
func (s *Spool) Body(e Entry) ([]byte, error) { return os.ReadFile(e.Path) }

func (s *Spool) recover() error {
	for _, dir := range []string{s.dir, s.rejected} {
		des, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, de := range des {
			name := de.Name()
			if de.IsDir() {
				continue
			}
			if strings.Contains(name, ".tmp-") {
				_ = os.Remove(filepath.Join(dir, name))
				continue
			}
			if dir == s.dir {
				if _, err := parseEntry(dir, name, false); err != nil {
					if os.Rename(filepath.Join(dir, name), filepath.Join(s.rejected, name)) == nil {
						s.m.rejectedOne("unreadable")
					}
				}
			}
		}
	}
	syncDir(s.dir)
	syncDir(s.rejected)
	return nil
}

func syncDir(dir string) {
	if f, err := os.Open(dir); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}
}
```

`refreshGauges` / `refreshGaugesLocked` (count files in both directories, sum bytes, oldest pending `observed_at`) are written in Task 10; until then make them no-ops so this task compiles, and let `Metrics` be a stub with `writeError()` and `rejectedOne(string)` methods that are nil-safe. Task 10 replaces the stub.

- [ ] **Step 4.4: Run; expect PASS** (`go test ./connector/records/ -run 'TestPut|TestPending|TestOpen' -count=1`).
- [ ] **Step 4.5: Commit** (`feat(records): spool directory with atomic writes and crash recovery`).

---

## Task 5: Classify every journal answer by reason

**Files:**
- Create: `connector/records/classify.go`
- Test: `connector/records/classify_test.go`

The table is spec 4.3 as updated in Part A Task 25 and documented in `docs/journal-api.md` "Answers, and what the connector's spool does". The rule that decides everything else: **only the listed permanent reasons move a file to `rejected/`; an answer the table does not name is kept and alerted on, never dropped.**

- [ ] **Step 5.1: Write the failing test** (every row of the documented table, plus the unknowns).

```go
func TestClassifyMatchesTheJournalTable(t *testing.T) {
	cases := []struct {
		code   int
		status string
		reason string
		want   Action
	}{
		{200, "applied", "", ActDelete},
		{200, "duplicate", "", ActDelete},
		{200, "late", "", ActDelete},
		{200, "held", "", ActDelete},
		{410, "rejected", journalapi.ReasonErasedSubject, ActErased},
		{403, "rejected", journalapi.ReasonPolicy, ActReject},
		{413, "rejected", journalapi.ReasonTooLarge, ActReject},
		{422, "rejected", journalapi.ReasonSchema, ActReject}, // also owner != source (A2)
		{422, "rejected", journalapi.ReasonFutureObservedAt, ActReject},
		{422, "rejected", journalapi.ReasonDuplicateKey, ActReject},
		{422, "rejected", journalapi.ReasonSnapshotIDReused, ActReject},
		{409, "rejected", journalapi.ReasonUnknownSubject, ActRetryScope},
		{400, "rejected", journalapi.ReasonBadRequest, ActRetryAlert},
		{401, "rejected", journalapi.ReasonUnauthenticated, ActRetryAlert},
		{403, "rejected", journalapi.ReasonForbidden, ActRetryAlert},
		{404, "rejected", journalapi.ReasonNotFound, ActRetryAlert},
		{503, "rejected", journalapi.ReasonUnavailable, ActRetry},
		{503, "rejected", journalapi.ReasonAuthUnavailable, ActRetry},
		{500, "rejected", journalapi.ReasonInternal, ActRetry},
		{502, "", "", ActRetry},
		{0, "", "", ActRetry}, // transport error
		// Not in the table: kept and alerted on.
		{200, "rejected", "", ActRetryAlert},
		{200, "something-new", "", ActRetryAlert},
		{422, "rejected", "brand_new_reason", ActRetryAlert},
		{403, "rejected", "", ActRetryAlert},
		{409, "rejected", journalapi.ReasonConflict, ActRetryAlert},
		{429, "", "", ActRetryAlert},
		{410, "rejected", "", ActRetryAlert},
	}
	for _, c := range cases {
		if got := Classify(c.code, c.status, c.reason); got != c.want {
			t.Errorf("%d %s/%s = %v, want %v", c.code, c.status, c.reason, got, c.want)
		}
	}
}

func TestReasonLabelIsBounded(t *testing.T) {
	if ReasonLabel("x\ny") != "other" || ReasonLabel(journalapi.ReasonPolicy) != "policy" {
		t.Fatal("reason label must come from a fixed set")
	}
}
```

- [ ] **Step 5.2: Run; expect FAIL.**

- [ ] **Step 5.3: Implement.**

```go
package records

import (
	"net/http"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// Action is what the spool does with a file after an answer (spec 4.3).
type Action int

const (
	ActDelete     Action = iota // accepted: delete the file
	ActErased                   // 410 erased_subject: delete, and purge the subject's other files (A10)
	ActReject                   // permanent refusal: move to rejected/
	ActRetryScope               // 409 unknown_subject: keep; block this scope only, retry with backoff
	ActRetry                    // the journal is unavailable: keep; stop the pass, retry with backoff
	ActRetryAlert               // a deployment fault or an answer the table does not name: keep, retry, alert
)

var accepted = map[string]bool{
	journalapi.StatusApplied: true, journalapi.StatusDuplicate: true,
	journalapi.StatusLate: true, journalapi.StatusHeld: true,
	// Not answered by /v1/records today; journalapi documents it as delete.
	journalapi.StatusDiscarded: true,
}

var permanent = map[int]map[string]bool{
	http.StatusForbidden:             {journalapi.ReasonPolicy: true},
	http.StatusRequestEntityTooLarge: {journalapi.ReasonTooLarge: true},
	http.StatusUnprocessableEntity: {journalapi.ReasonSchema: true, journalapi.ReasonFutureObservedAt: true,
		journalapi.ReasonDuplicateKey: true, journalapi.ReasonSnapshotIDReused: true},
}

var alerting = map[int]string{
	http.StatusBadRequest:   journalapi.ReasonBadRequest,
	http.StatusUnauthorized: journalapi.ReasonUnauthenticated,
	http.StatusForbidden:    journalapi.ReasonForbidden,
	http.StatusNotFound:     journalapi.ReasonNotFound,
}

// Classify maps an answer to an action. code 0 is a transport error. The
// spool keys on reason: only the reasons the journal documents as
// permanent move a file to rejected/; anything not in the table is kept.
func Classify(code int, status, reason string) Action {
	switch {
	case code == 0:
		return ActRetry
	case code == http.StatusOK:
		if accepted[status] {
			return ActDelete
		}
		return ActRetryAlert
	case code == http.StatusGone && reason == journalapi.ReasonErasedSubject:
		return ActErased
	case permanent[code][reason]:
		return ActReject
	case code == http.StatusConflict && reason == journalapi.ReasonUnknownSubject:
		return ActRetryScope
	case alerting[code] == reason && reason != "":
		return ActRetryAlert
	case code == http.StatusServiceUnavailable && (reason == journalapi.ReasonUnavailable || reason == journalapi.ReasonAuthUnavailable || reason == ""):
		return ActRetry
	case code >= 500 && code != http.StatusServiceUnavailable:
		return ActRetry
	}
	return ActRetryAlert
}

var knownReasons = map[string]bool{}

func init() {
	for _, r := range []string{journalapi.ReasonErasedSubject, journalapi.ReasonPolicy, journalapi.ReasonUnknownSubject,
		journalapi.ReasonTooLarge, journalapi.ReasonSchema, journalapi.ReasonFutureObservedAt, journalapi.ReasonDuplicateKey,
		journalapi.ReasonBadRequest, journalapi.ReasonUnauthenticated, journalapi.ReasonForbidden, journalapi.ReasonAuthUnavailable,
		journalapi.ReasonUnavailable, journalapi.ReasonNotFound, journalapi.ReasonConflict, journalapi.ReasonSnapshotIDReused,
		journalapi.ReasonInternal} {
		knownReasons[r] = true
	}
}

// ReasonLabel bounds a reason for a metric label.
func ReasonLabel(r string) string {
	if knownReasons[r] {
		return r
	}
	if r == "" {
		return "none"
	}
	return "other"
}
```

- [ ] **Step 5.4: Run; expect PASS.**
- [ ] **Step 5.5: Commit** (`feat(records): classify journal answers by reason (spec 4.3 as built)`).

---

## Task 6: The journal client

**Files:**
- Create: `connector/records/client.go`
- Test: `connector/records/client_test.go`

Mirrors `internal/loginbroker/brokerapi.Client`: the transport refuses every host but the journal's, follows no redirect, uses no proxy; the token file is re-read for every request (the kubelet rotates it); the token is never logged. One request: `POST <journal>/v1/records`, `Content-Type: application/json`, the spool file's bytes. The answer is read up to 64 KiB and decoded leniently into `journalapi.Response`; `Retry-After` (seconds) is returned.

- [ ] **Step 6.1: Write the failing tests** against an `httptest.Server`:

- the request is `POST /v1/records`, no query, `Content-Type: application/json`, `Authorization: Bearer <file contents trimmed>`, body byte-identical to the input;
- the token file is re-read (change it between two calls; the server sees both);
- a missing or empty token file is an error before any request (the server sees none), and the error text contains no token;
- a redirect answer (307 to another host) is returned as its code, not followed;
- a request to another host through the same `http.Client` is refused (`ErrHostBlocked`);
- `503` with `Retry-After: 1` gives `RetryAfter == time.Second`; a missing or malformed header gives 0;
- a non-JSON answer body gives `Answer{Code: 502, Status: "", Reason: ""}` (then classified `ActRetry`);
- a hung server is bounded by the configured timeout (use 50 ms in the test).

```go
func TestSendPresentsTheProjectedToken(t *testing.T) {
	var gotAuth, gotType, gotPath, gotQuery string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType, gotPath, gotQuery = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path, r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"applied","stats":{"created":1}}`)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	mustWrite(t, tok, "tok-1\n")
	c, err := NewClient(srv.URL, TokenFile(tok), time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"snapshot_id":"x"}`)
	a, err := c.Send(context.Background(), body)
	if err != nil || a.Code != 200 || a.Status != "applied" {
		t.Fatalf("answer %+v err %v", a, err)
	}
	if gotAuth != "Bearer tok-1" || gotType != "application/json" || gotPath != journalapi.PathRecords || gotQuery != "" || !bytes.Equal(gotBody, body) {
		t.Fatalf("request: %q %q %q %q %q", gotAuth, gotType, gotPath, gotQuery, gotBody)
	}
}
```

Write the other cases in the same style.

- [ ] **Step 6.2: Run; expect FAIL.**

- [ ] **Step 6.3: Implement** `NewClient(baseURL string, token func() (string, error), timeout time.Duration, inner http.RoundTripper) (*Client, error)`, `TokenFile(path string) func() (string, error)`, `type Answer struct { Code int; Status, Reason string; RetryAfter time.Duration }`, `func (c *Client) Send(ctx context.Context, body []byte) (Answer, error)` (an error only for transport failures and a token that cannot be read; the error never contains the token or the body). Copy the `pinned` round tripper from `brokerapi` (do not import `brokerapi`: its errors name the login broker); `CheckRedirect` returns `http.ErrUseLastResponse`; `Proxy: nil`. Answer body read through `io.LimitReader(resp.Body, 64<<10)`; drain and close.

- [ ] **Step 6.4: Run; expect PASS.**
- [ ] **Step 6.5: Commit** (`feat(records): pinned journal client presenting the projected token`).

---

## Task 7: A fake journal implementing the 4.3 table

**Files:**
- Create: `connector/records/recordstest/journal.go`
- Test: `connector/records/recordstest/journal_test.go`

Used by the sender tests here, by the PowerSchool tests and by B2. It is a real HTTP server (`httptest.Server`) and answers exactly as the journal documents, in the journal's order of checks: route (404 `not_found`), query string (400), bearer token equal to the expected one (401 `unauthenticated`), the caller's identity allowed (403 `forbidden`, via `Forbid()`), `Content-Type` (400), body size over 8 MiB (413), strict decode through `internal/journal/strictjson` (422 `schema`), a resend of a stored id (200 `duplicate`, or 422 `snapshot_id_reused` when the scope or type differs), validation through `record.ValidateSnapshot` when a `Registry` is set (its `Rejection` status and reason), erased subjects (410), unknown subjects (409), and otherwise 200 `applied` (or the scripted answer).

- [ ] **Step 7.1: Write the failing tests:** one per row of the table, driven through `records.Client` so the fake and the client agree; plus `Received()` returns the accepted snapshots in arrival order and `Answers()` every (code, reason) sent.

- [ ] **Step 7.2: Run; expect FAIL.**

- [ ] **Step 7.3: Implement.**

```go
// Package recordstest is a fake record journal for connector tests. It
// answers POST /v1/records exactly as docs/journal-api.md documents,
// including every refusal the connector's spool must act on.
package recordstest

type Journal struct {
	*httptest.Server
	Token    string
	Registry *record.Registry // nil: no validation beyond decoding
	Now      func() time.Time

	mu        sync.Mutex
	stored    map[string]stored // snapshot_id -> type and canonical scope
	received  []journalapi.Snapshot
	answers   []Answer
	erased    map[string]bool
	unknown   map[string]bool
	forbidden bool
	script    []Answer // answers to give next, in order, before the normal path
}

type Answer struct {
	Code       int
	Reason     string
	Status     string
	RetryAfter int
}

func New(t testing.TB, token string) *Journal                  // starts the server, t.Cleanup closes it
func (j *Journal) Erase(subject string)
func (j *Journal) Unknown(subject string, unknown bool)
func (j *Journal) Forbid(on bool)
func (j *Journal) Next(a ...Answer)                              // scripted answers, then the normal path
func (j *Journal) Received() []journalapi.Snapshot
func (j *Journal) Answers() []Answer
```

The handler writes `journalapi.Response` bodies (`{"status":"rejected","reason":...,"detail":"..."}`) and `Retry-After` when scripted. For the reuse check compare the record type and the canonical scope, `record.ParseScope(snap.Scope)` then `.Canonical()` (`internal/journal/record/scope.go`), with what was stored for the id.

- [ ] **Step 7.4: Run; expect PASS.**
- [ ] **Step 7.5: Commit** (`test(records): fake journal implementing the 4.3 answer table`).

---

## Task 8: The sender: one drain pass

**Files:**
- Create: `connector/records/sender.go`
- Test: `connector/records/sender_test.go`

One pass over `spool/`: group pending entries by `ScopeKey`; visit scopes in the order of their oldest file; within a scope send strictly oldest first. After each answer:

| Action | File | Scope | Pass |
|---|---|---|---|
| `ActDelete` | delete | continue | continue |
| `ActErased` | delete; purge the subject (Task 9) | stop | continue with other subjects |
| `ActReject` | move to `rejected/` | continue (the next file is independent) | continue |
| `ActRetryScope` | keep | blocked until its own backoff ends | continue with other scopes |
| `ActRetry`, `ActRetryAlert` | keep | -- | **stop**; global backoff (at least `Retry-After`) |

Backoff: exponential from `min_backoff` to `max_backoff`, with +/-20% jitter (`rand` injected), reset on any `ActDelete`/`ActReject`. Global and per-scope backoff live in memory only: a restart sends at once, which is safe because every send is idempotent (A9).

- [ ] **Step 8.1: Write the failing tests** with `recordstest.Journal` and a fake clock:

```go
func TestDrainSendsOldestFirstPerScope(t *testing.T)            // 3 files, 2 scopes: arrival order at the journal is per-scope oldest first
func TestDrainAcceptedFilesAreDeleted(t *testing.T)              // applied, duplicate, late, held -> spool empty
func TestDrainRejectsOnlyPermanentReasons(t *testing.T)          // policy/too_large/schema/... -> rejected/; counted by reason
func TestDrainUnknownSubjectBlocksOnlyThatScope(t *testing.T)    // e_child2 409 -> its two files stay, in order; e_child1 delivered
func TestDrainUnknownSubjectKeepsOrderWithinTheScope(t *testing.T) // the second file of the blocked scope is not sent
func TestDrainUnavailableStopsThePass(t *testing.T)              // 503 on the first file: nothing else sent; NextAttempt >= now+min_backoff
func TestDrainHonoursRetryAfter(t *testing.T)                    // 503 Retry-After: 120 with min_backoff 30 -> next attempt at +120s
func TestDrainDeploymentFaultsAreKeptAndCounted(t *testing.T)    // 401, 403 forbidden, 400, 404, unknown reason: kept; answers_total{reason} counted
func TestDrainBackoffGrowsAndResets(t *testing.T)                // 30s, 60s, 120s ... capped; reset after an accepted file
func TestDrainTransportErrorIsRetried(t *testing.T)              // server closed: kept, ActRetry
func TestDrainResendAfterLostAnswerIsDuplicate(t *testing.T)     // journal applies but the answer is lost (script a connection reset after apply): next pass gets duplicate and deletes
```

- [ ] **Step 8.2: Run; expect FAIL.**

- [ ] **Step 8.3: Implement.**

```go
type Sender struct {
	spool  *Spool
	client *Client
	m      *Metrics
	cfg    SpoolConfig
	now    func() time.Time
	jitter func() float64 // in [-0.2, 0.2]; tests pass func() float64 { return 0 }
	log    *slog.Logger

	globalUntil time.Time
	globalStep  int
	scopes      map[string]scopeBackoff
}

type scopeBackoff struct {
	until time.Time
	step  int
}

// PassResult says when the next pass is due (zero: whenever there is work).
type PassResult struct {
	Sent      int
	NextDue   time.Time
	Remaining int
}

func (s *Sender) Drain(ctx context.Context) PassResult {
	now := s.now()
	if now.Before(s.globalUntil) {
		return PassResult{NextDue: s.globalUntil}
	}
	_ = s.spool.Enforce() // Task 9
	pending, err := s.spool.Pending()
	if err != nil {
		s.log.Warn("records spool could not be listed", "error", err)
		return PassResult{NextDue: now.Add(s.cfg.MinBackoff())}
	}
	order, groups := groupByScope(pending)
	res := PassResult{}
	erased := map[string]bool{}
	for _, key := range order {
		if b, ok := s.scopes[key]; ok && now.Before(b.until) {
			res.NextDue = earliest(res.NextDue, b.until)
			continue
		}
		for _, e := range groups[key] {
			if erased[e.DataSubject] {
				break
			}
			body, err := s.spool.Body(e)
			if err != nil {
				continue // the file went away (bounds); nothing to do
			}
			a, err := s.client.Send(ctx, body)
			if err != nil {
				a = Answer{} // code 0: transport
			}
			act := Classify(a.Code, a.Status, a.Reason)
			s.m.answer(a.Code, a.Reason, act)
			switch act {
			case ActDelete:
				_ = s.spool.Delete(e)
				res.Sent++
				s.globalStep = 0
				delete(s.scopes, key)
				continue
			case ActReject:
				_ = s.spool.Reject(e, ReasonLabel(a.Reason))
				s.log.Error("records snapshot refused by the journal; moved to rejected/",
					"reason", ReasonLabel(a.Reason), "file", e.Name)
				continue
			case ActErased:
				_ = s.spool.Delete(e)
				n, _ := s.spool.PurgeSubject(e.DataSubject)
				erased[e.DataSubject] = true
				s.log.Warn("records subject erased in the journal; its spooled snapshots were deleted", "files", n+1)
			case ActRetryScope:
				b := s.scopes[key]
				b.step++
				b.until = now.Add(s.backoff(b.step, 0))
				s.scopes[key] = b
				res.NextDue = earliest(res.NextDue, b.until)
			case ActRetry, ActRetryAlert:
				s.globalStep++
				s.globalUntil = now.Add(s.backoff(s.globalStep, a.RetryAfter))
				if act == ActRetryAlert {
					s.log.Error("records journal refused the connector for a reason that is not the data's; keeping the spool",
						"code", a.Code, "reason", ReasonLabel(a.Reason))
				}
				res.NextDue = s.globalUntil
				res.Remaining = s.remaining()
				return res
			}
			break // ActErased, ActRetryScope: leave this scope
		}
	}
	res.Remaining = s.remaining()
	return res
}

func (s *Sender) backoff(step int, retryAfter time.Duration) time.Duration {
	d := s.cfg.MinBackoff() << min(step-1, 20)
	if d > s.cfg.MaxBackoff() || d <= 0 {
		d = s.cfg.MaxBackoff()
	}
	d = time.Duration(float64(d) * (1 + s.jitter()))
	if retryAfter > d {
		d = retryAfter
	}
	return d
}
```

Log lines carry the file name (time and UUID) and reasons; never a scope value, subject id or field value (the subject id is a roster id, but log lines go to Loki; keep them out).

- [ ] **Step 8.4: Run; expect PASS.**
- [ ] **Step 8.5: Commit** (`feat(records): sender drains the spool in order and acts on every answer`).

---

## Task 9: Bounds: `max_age`, `max_bytes`, subject purge

**Files:**
- Create: `connector/records/bounds.go`
- Test: `connector/records/bounds_test.go`

Spec 4.14: `max_age` (default 7 days) and `max_bytes` (default 256 MiB) apply to `spool/` and `spool/rejected/` alike; `max_age` applies to every file, newest included; a file still answered `409` when it reaches `max_age` is deleted and counted; on overflow the oldest **intermediate** snapshots of a scope are dropped, the newest of each scope is always kept. Rejected files count toward `max_bytes` and are dropped first (they will never be sent).

`Enforce` runs at the start of every drain pass and after every `Put`.

- [ ] **Step 9.1: Write the failing tests.**

```go
func TestMaxAgeDropsEveryOldFileNewestIncluded(t *testing.T)     // one scope, one file, 8 days old -> gone; dropped_total{reason="max_age"} 1
func TestMaxAgeCountsUnknownSubjectSeparately(t *testing.T)     // a file last answered 409 -> dropped_total{reason="unknown_subject_expired"}
func TestMaxAgeAppliesToRejected(t *testing.T)                  // rejected/ file 8 days old -> gone
func TestMaxBytesDropsRejectedFirstThenIntermediates(t *testing.T) {
	// scope A: a1 < a2 < a3 ; scope B: b1 ; one rejected file r1.
	// Limit fits 3 files: r1 dropped, then a1 (oldest intermediate); a2? the
	// limit decides; a3 and b1 (newest of each scope) always stay.
}
func TestMaxBytesNeverDropsTheNewestOfAScope(t *testing.T)      // limit smaller than the newest files alone: all newest kept; logged once
func TestPurgeSubjectRemovesPendingAndRejected(t *testing.T)    // e_child1 files in both dirs gone, e_child2 untouched; dropped_total{reason="erased_subject"}
```

The 409 case needs memory of the last answer: the sender records, per `ScopeKey`, that its head was answered `unknown_subject` (`Spool.MarkUnknown(key)`); `Enforce` uses it for the label only. The label is the only thing that changes, so losing it at restart is harmless.

- [ ] **Step 9.2: Run; expect FAIL.**
- [ ] **Step 9.3: Implement** (files in `rejected/` that `parseEntry` cannot read -- the ones `recover()` quarantined as unreadable -- are aged by their mtime and counted toward `max_bytes` too, so `max_age` really applies to every file; add `TestMaxAgeAppliesToUnreadableRejectedFiles`) `func (s *Spool) Enforce() error`, `func (s *Spool) PurgeSubject(subject string) (int, error)`, `func (s *Spool) MarkUnknown(scopeKey string)`. Age is `now - ObservedAt` (the file name), so a clock that jumps forward expires files early rather than keeping data at rest longer; note that in the doc comment.
- [ ] **Step 9.4: Run; expect PASS.**
- [ ] **Step 9.5: Commit** (`feat(records): spool bounds and subject purge on erase`).

---

## Task 10: Spool metrics

**Files:**
- Create: `connector/records/metrics.go` (replacing the Task 4 stub)
- Test: `connector/records/metrics_test.go`

Same style as `connector/metrics.go`: OTel instruments on the framework's provider (`cc.Metrics.Provider().Meter("glovebox.records")`), a `connector` attribute on every series, nil-safe methods. Names (spec 4.14, plus one counter for the 4.3 "alert" rows):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `glovebox_journal_spool_files` | gauge | `connector`, `dir` (`pending`, `rejected`) | Files in the spool. |
| `glovebox_journal_spool_bytes` | gauge | `connector` | Bytes in both directories (for `max_bytes`). |
| `glovebox_journal_spool_oldest_seconds` | gauge | `connector` | Age of the oldest pending file (0 when empty). Refreshed by every spool operation and by the loop every minute. |
| `glovebox_journal_spool_dropped_total` | counter | `connector`, `reason` (`max_age`, `max_bytes`, `unknown_subject_expired`, `erased_subject`) | Files deleted unsent. |
| `glovebox_journal_spool_rejected_total` | counter | `connector`, `reason` (a `ReasonLabel`, or `unreadable`) | Files moved to `rejected/`. |
| `glovebox_journal_spool_answers_total` | counter | `connector`, `code`, `reason` (a `ReasonLabel`), `action` (`delete`, `erased`, `reject`, `retry_scope`, `retry`, `retry_alert`) | Every journal answer (code `0` for transport). |
| `glovebox_journal_spool_write_errors_total` | counter | `connector` | Snapshots that could not be written to the spool (items are unaffected). |
| `glovebox_journal_record_values_cut_total` | counter | `connector` | `text` values cut to 64 KiB (S8). |

- [ ] **Step 10.1: Write the failing tests:** scrape through `sdkmetric.ManualReader`; after a scripted sequence (put 3, one applied, one rejected `schema`, one 409, one transport error) every series has the expected value; labels are only from the fixed sets (a reason `"x\ny"` appears as `other`); `oldest_seconds` is 0 for an empty spool and the age of the oldest file otherwise; a nil `*Metrics` is safe everywhere.
- [ ] **Step 10.2: Run; expect FAIL.**
- [ ] **Step 10.3: Implement** `NewMetrics(meter metric.Meter, connectorName string) (*Metrics, error)`, the recording methods used in Tasks 4-9, and `refreshGaugesLocked` in `spool.go` (one `ReadDir` of each directory).
- [ ] **Step 10.4: Run; expect PASS.**
- [ ] **Step 10.5: Commit** (`feat(records): spool metrics`).

---

## Task 11: The emitter and its loop; crash and restart

**Files:**
- Create: `connector/records/run.go`
- Test: `connector/records/run_test.go`, `connector/records/crash_test.go` (extend)

`Emitter` is what a connector holds:

```go
type Emitter struct {
	spool  *Spool
	sender *Sender
	m      *Metrics
	kick   chan struct{}
	log    *slog.Logger
}

// New builds the emitter for an enabled config: opens (and recovers) the
// spool under stateDir (an empty stateDir is refused: the spool must be on
// the state volume, never a relative path), builds the client and metrics.
// It starts nothing.
func New(cfg Config, stateDir, connectorName string, meter metric.Meter, now func() time.Time) (*Emitter, error)

// Emit writes snapshots to the spool and wakes the loop. An error is a
// spool write failure: logged and counted; the caller carries on (items
// are never affected by records).
func (e *Emitter) Emit(snaps []journalapi.Snapshot) error

// Run drains at once, then whenever Emit wakes it, when a backoff ends,
// and every minute (gauges, max_age). It returns when ctx ends.
func (e *Emitter) Run(ctx context.Context)

// OnCut is the Fields callback that counts cut text values.
func (e *Emitter) OnCut()
```

`Run` is the only goroutine the records feature adds. A panic in a drain pass is recovered, logged and counted as a transport-style retry (the loop keeps running).

- [ ] **Step 11.1: Write the failing tests.**

```go
func TestRunDeliversWhatEmitWrites(t *testing.T)                 // Emit 2 -> fake journal receives 2 within 1s; spool empty
func TestRunRetriesAfterBackoff(t *testing.T)                    // first answer 503, then applied: delivered after the fake clock passes min_backoff
func TestRunStopsWithContext(t *testing.T)                       // cancel -> Run returns; no goroutine left (goleak-style check with runtime.NumGoroutine)
func TestEmitFailureIsReportedNotFatal(t *testing.T)            // read-only spool dir: Emit returns an error, write_errors_total 1, Run still fine
```

`crash_test.go` (spec 8 "crash mid-write, replay order"):

```go
// A process killed between the temp write and the rename leaves a temp
// file; a restart removes it and sends nothing for it.
func TestCrashDuringPutLeavesNoSnapshot(t *testing.T)

// A process killed after the journal applied a snapshot but before the
// file was deleted resends it after the restart; the journal answers
// duplicate and the file is deleted. The journal applied it once.
func TestCrashAfterSendBeforeDeleteIsADuplicate(t *testing.T)

// Files of one scope written by a process that died before sending are
// delivered by the next process in observed_at order, after files of an
// older scope.
func TestRestartReplaysInOrder(t *testing.T)

// A restart forgets backoff: the first pass sends at once.
func TestRestartSendsAtOnce(t *testing.T)
```

Simulate a crash by building a second `Emitter` on the same state directory without closing the first (no shared state other than the directory), and for the temp-file case by writing the temp file by hand with `atomicfile`'s naming (`<name>.tmp-<pid>-<nanos>`). For "applied but answer lost", script the fake journal to apply and then hijack and close the connection.

- [ ] **Step 11.2: Run; expect FAIL.**
- [ ] **Step 11.3: Implement.**
- [ ] **Step 11.4: Run; expect PASS** (`go test ./connector/records/... -count=1 -race`; also `-count=20` for the loop tests once, to shake out timing).
- [ ] **Step 11.5: Commit** (`feat(records): emitter loop; crash and restart tests`).

**B2 can start after this commit merges.**

---

## Task 12: PowerSchool record types and the subject binding

**Files:**
- Create: `connectors/powerschool/record_types.go`, `connectors/powerschool/record_binding.go`
- Modify: `connectors/powerschool/config.go`
- Test: `connectors/powerschool/record_types_test.go`, `connectors/powerschool/record_binding_test.go`, `connectors/powerschool/records_testkit_test.go`

`RecordTypes()` returns the seven types (owner `powerschool`, schema version 1). Field sets (spec 5.1, with S9):

| Type | Fields | default_audience |
|---|---|---|
| `course_mark` | `course`, `period`, `teacher` string; `q1_letter`, `q2_letter`, `s1_letter`, `q3_letter`, `q4_letter`, `s2_letter` string; `q1_percent` ... `s2_percent` number | `[subject, guardians]` |
| `assignment` | `course`, `category`, `title`, `letter` string; `due` date; `score`, `max_score`, `percent` number; `missing`, `late`, `incomplete`, `exempt`, `collected`, `absent` bool; `teacher_comment`, `description`, `category_description` text | `[subject, guardians]` |
| `attendance_total` | `course` string; `absences`, `tardies` int | `[subject, guardians]` |
| `attendance_mark` | `date` date; `course`, `period`, `code`, `code_description`, `kind` string; `excused` bool | `[subject, guardians]` |
| `attendance_day` | `date` date; `periods_met`, `periods_unmarked`, `absent_unexcused`, `absent_excused`, `late`, `left_early`, `authorized`, `unknown_codes` int; `any_unexcused` bool; `classification` string | `[subject, guardians]` |
| `term_grade` | `course`, `term`, `grade` string; `earned`, `attempted` number | `[subject, guardians]` |
| `term_comment` | `course`, `term` string; `comment` text | `[guardians]` |

No field is `required` (every field nullable; a future version may add fields only as optional, Part A 4.7).

Binding: `subjectFor(m *connector.RuleMatcher, kid Kid, surface string) (Binding, bool)` returns the matching rule's `DataSubject` and a copy of its `Audience`. `ValidateConfig` already guarantees every enabled surface of a kid resolves to one rule with a `data_subject`, one subject per kid, no subject shared by two kids, and teacher comments guardians-only. With `records.enabled` add (S11): every key of `attendance_days.kinds` is one of the seven names.

- [ ] **Step 12.1: Write the failing tests.**

```go
func TestRecordTypesAreValidForTheJournal(t *testing.T) {
	reg, err := record.NewRegistry(RecordTypes())
	if err != nil {
		t.Fatal(err)
	}
	for _, ty := range RecordTypes() {
		if ty.Owner != "powerschool" || ty.SchemaVersion != 1 {
			t.Errorf("%s: owner %q version %d", ty.Name, ty.Owner, ty.SchemaVersion)
		}
	}
	_ = reg
}

func TestTermCommentDefaultAudienceIsGuardiansOnly(t *testing.T) {
	for _, ty := range RecordTypes() {
		if ty.Name == TypeTermComment && !reflect.DeepEqual(ty.DefaultAudience, []string{"guardians"}) {
			t.Fatalf("term_comment default audience %v", ty.DefaultAudience)
		}
	}
}

func TestRecordsConfigRefusesCustomAttendanceKinds(t *testing.T) {
	cfg := validRecordsConfig(t)
	cfg.AttendanceDays.Kinds = map[string][]string{"absence_unexcused": {"UA"}, "tardy_custom": {"T"}}
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "records") {
		t.Fatalf("err = %v", err)
	}
	cfg.Records.Enabled = false
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("without records the custom kind is allowed as today: %v", err)
	}
}

func TestSubjectForUsesTheKidsRule(t *testing.T) {
	m := connector.NewRuleMatcher(recordRules())
	b, ok := subjectFor(m, Kid{Name: "k2"}, teacherCommentSurface)
	if !ok || b.DataSubject != "e_child2" || !reflect.DeepEqual(b.Audience, []string{"guardians"}) {
		t.Fatalf("binding %+v %v", b, ok)
	}
}
```

`records_testkit_test.go` holds `recordRules()` (the `testRules()` set with `e_child1` / `e_child2` instead of the existing ids), `recordsRegistry(t)`, `validateAll(t, snaps)` (runs `record.ValidateSnapshot` on each and re-decodes `json.Marshal(snap)` through `strictjson.DecodeLimits` with Part A's caps), `fieldsOf(t, snap, key)`, and `wantFields(t, ty, rec)` (the record's field names equal the type's declared set exactly).

- [ ] **Step 12.2: Run; expect FAIL.**
- [ ] **Step 12.3: Implement.** In `config.go`: `Records PowerSchoolRecords \`json:"records"\`` on `Config` (the struct below); `ApplyDefaults` calls `c.Records.ApplyDefaults()` only when `c.Records.Enabled` (so a disabled block stays zero); `ValidateConfig` calls `c.Records.Validate()` and the kinds check. Add `AttendanceDaySettleSchoolDays int \`json:"attendance_day_settle_school_days"\`` to the records block **in the PowerSchool config, not the shared one** -- a PowerSchool-only setting (S1): put it in a `PowerSchoolRecords` struct embedding `records.Config`:

```go
type PowerSchoolRecords struct {
	records.Config
	// AttendanceDaySettleSchoolDays: a day with unmarked cells becomes an
	// attendance_day record once this many school days have followed it
	// (decision S1; default 3, 1-10).
	AttendanceDaySettleSchoolDays int `json:"attendance_day_settle_school_days"`
}
```

`ApplyDefaults` sets `AttendanceDaySettleSchoolDays` to 3 when zero (records enabled only); `ValidateConfig` refuses values outside 1-10 when records are enabled. Code elsewhere uses `cfg.Records.Config` for the shared part. Check that `encoding/json` flattens the embedded struct's fields (it does for an anonymous struct field without a tag).

- [ ] **Step 12.4: Run; expect PASS.**
- [ ] **Step 12.5: Commit** (`feat(powerschool): record type definitions and records config`).

---

## Task 13: `course_mark` and `attendance_total`

**Files:**
- Create: `connectors/powerschool/records_grades.go`
- Test: `connectors/powerschool/records_grades_test.go`

Both come from the home page grade rows (`page.Grades`), one record per course, one scope per course (`(powerschool, grades|attendance_totals, subject, course=<courseKey>)`, complete). Unlike today's items, every row becomes a record, including a course with no grade posted and a course with 0 absences and 0 tardies: records describe state, not change. (These scopes have one record each, so `complete` can never remove anything; tests do not expect a removal -- spec 5.1.)

- [ ] **Step 13.1: Write the failing tests.**

```go
func TestCourseMarksOneScopePerCourse(t *testing.T) {
	g1 := sampleGrade("c1", "EXAMPLE SCIENCE 7", "B+88")
	g1.Q2Grade = "A-91"
	g2 := sampleGrade("", "EXAMPLE  ART\t", "") // no frn: name|period key; no grade posted
	b := Binding{DataSubject: "e_child1", Audience: []string{"subject", "guardians"}}
	snaps := BuildCourseMarks(b, []*pslib.Grade{g1, nil, g2}, obsAt)
	validateAll(t, snaps)
	if len(snaps) != 2 {
		t.Fatalf("%d snapshots", len(snaps))
	}
	s := snaps[0]
	if !s.Complete || s.CompleteRange != nil || len(s.Records) != 1 || s.Records[0].Key != "c1" {
		t.Fatalf("snapshot %+v", s)
	}
	wantScope(t, s, "powerschool", "grades", "e_child1", "course", "c1")
	f := fieldsOf(t, s, "c1")
	wantJSON(t, f, map[string]string{"q1_letter": `"B+"`, "q1_percent": "88", "q2_letter": `"A-"`, "q2_percent": "91", "s1_letter": "null", "course": `"EXAMPLE SCIENCE 7"`, "period": `"1(A)"`, "teacher": `"Teacher, Example"`})
	if snaps[1].Records[0].Key != courseKey(g2) {
		t.Fatal("fallback key must be courseKey (decision S7)")
	}
}

func TestAttendanceTotalsEveryCourseIncludingZero(t *testing.T) {
	g := sampleGrade("c1", "EXAMPLE SCIENCE 7", "B+88")
	snaps := BuildAttendanceTotals(bind1, []*pslib.Grade{g}, obsAt)
	validateAll(t, snaps)
	wantJSON(t, fieldsOf(t, snaps[0], "c1"), map[string]string{"absences": "0", "tardies": "0", "course": `"EXAMPLE SCIENCE 7"`})
	wantScope(t, snaps[0], "powerschool", "attendance_totals", "e_child1", "course", "c1")
}

func TestCourseKeyWithControlCharactersIsHashedConsistently(t *testing.T) {
	g := sampleGrade("", "EXAMPLE\nSCIENCE", "B")
	a := BuildCourseMarks(bind1, []*pslib.Grade{g}, obsAt)
	b := BuildAttendanceTotals(bind1, []*pslib.Grade{g}, obsAt)
	validateAll(t, append(a, b...))
	if a[0].Records[0].Key != b[0].Records[0].Key || !strings.HasPrefix(a[0].Records[0].Key, "h:") {
		t.Fatal("same course, same safe key on both surfaces")
	}
}
```

- [ ] **Step 13.2: Run; expect FAIL.**
- [ ] **Step 13.3: Implement.** `recordCourseKey(g) = records.SafeKey(courseKey(g))` (export as `CourseRecordKey` for the seed, S15); percent through `splitGrade` then `strconv.ParseFloat` (an unparsable percent is null, the letter keeps the raw cell, as `splitGrade` does); `Fields.String` for every string. A nil row is skipped. Two rows with the same course key on one page (should not happen; `frn` is per enrolment): keep the first, count `powerschool_record_rows_skipped_total{reason="duplicate_key"}` -- add the counter to `telemetry.go`, nil-safe like the others.
- [ ] **Step 13.4: Run; expect PASS.**
- [ ] **Step 13.5: Commit** (`feat(powerschool): course_mark and attendance_total record builders`).

---

## Task 14: `assignment`

**Files:**
- Create: `connectors/powerschool/records_assignments.go`
- Test: `connectors/powerschool/records_assignments_test.go`

One snapshot per course **read** in this turn (`reads []courseRead`, built by `readAssignments` after every scores page passed the student binding): scope `(powerschool, assignments, subject, course=<courseKey>)`, complete, every assignment of the course (the lookback filter does not apply, spec 5.1). A course whose read failed is not in `reads` and gets no snapshot (no removal from a partial view). Key: `<courseKey>:<assignment id>`, with `#n` for an id repeated within the course, exactly as `stageAssignments` computes it, then `SafeKey`.

Fields: `course` (`g.CourseName`), `category`, `title` (`a.Title`), `letter` (`a.LetterGrade`) string; `due` from `dueDay` (the calendar date; undated is null); `score` (`a.Score`; but an entered zero with `Score == nil` and `Status == graded` is `0` -- the same rule as `isScored`), `max_score` (`a.MaxScore`, null when 0 and unscored), `percent` (`a.Percentage`) number; the six flags from `flagSet`; `teacher_comment` (`a.TeacherComment` when `a.HasTeacherComment`), `description` (`a.Description`), `category_description` (`a.CategoryDescription`, null when equal to `description`, as `textOf` does) text -- through `Fields.Text`, i.e. **not** `boundText`: no 4 KiB cut, no cleaning.

- [ ] **Step 14.1: Write the failing tests:** keys equal the checkpoint ids `stageAssignments` writes for the same reads (run `stageAssignments` on a test env with the same reads, collect `CheckpointKey(assignmentSurface, "k1", key)` ids, compare); an assignment far outside the lookback window is present; a shared-section id repeated gives `id`, `id#2`; the teacher comment fixture from `assignment_text_test.go` with line breaks, tabs and a bidi control arrives unchanged in `teacher_comment`; a 70 KiB description is cut to <= 64 KiB and `OnCut` fires once; a snapshot whose encoded size exceeds 8 MiB is not emitted but logged and counted (`powerschool_record_rows_skipped_total{reason="snapshot_too_large"}`) instead of being refused 413 on every poll; an entered zero is `score: 0`; two courses give two snapshots; zero reads give zero snapshots; every record has exactly the type's field set; `validateAll` passes.
- [ ] **Step 14.2: Run; expect FAIL.**
- [ ] **Step 14.3: Implement** `BuildAssignments(b Binding, reads []courseRead, tz *time.Location, observedAt time.Time, onCut func()) []journalapi.Snapshot` and `AssignmentRecordKey(g *pslib.Grade, id string, n int) string` (exported for the seed).
- [ ] **Step 14.4: Run; expect PASS.**
- [ ] **Step 14.5: Commit** (`feat(powerschool): assignment record builder`).

---

## Task 15: `attendance_mark`

**Files:**
- Create: `connectors/powerschool/records_attendance.go`
- Test: `connectors/powerschool/records_attendance_test.go`

From the attendance page (`extras.attendance`). One record per coded cell (`Kind == CellCode && Code != ""`), key `<attendanceCourseKey(course)>:<date>` (`SafeKey`), the same id `ProcessAttendanceDays` checkpoints. Grouped by `SchoolYear(date)`: one snapshot per school year present in the grid, scope `(powerschool, attendance_marks, subject, school_year=<y>)`, `complete: true` with `complete_range {field: date, from: <first grid date of that year>, to: <last grid date of that year>}`, where "grid dates" are the dates of every cell of every row, whatever the cell holds. A page with no dates at all gives no snapshot (spec 4.3: a page that parsed with no dates is sent as `complete=false`; with no dates there are also no records, so nothing is sent). A page with dates and no codes gives a legitimate empty snapshot with its range (every code corrected away).

Fields: `date`; `course` (`CourseName`), `period` (`Expression`), `code`, `code_description` (legend description) string; `kind` from `cfg.kindOf(entry.Category)`; `excused` per S10.

- [ ] **Step 15.1: Write the failing tests.**

```go
func TestAttendanceMarksPerSchoolYearWithRange(t *testing.T)     // dates 2026-10-02..10-08 -> one snapshot, school_year 2026-27, range 10-02..10-08, records for VU and L only
func TestAttendanceMarksSplitAtTheSchoolYear(t *testing.T)       // a grid spanning 07-30..08-03 -> two snapshots, each with its own range
func TestAttendanceMarksEmptyButDatedIsAnEmptyCompleteSnapshot(t *testing.T)
func TestAttendanceMarksNoDatesNoSnapshot(t *testing.T)
func TestAttendanceMarksKeysEqualTheCheckpointIDs(t *testing.T)  // same page through ProcessAttendanceDays (lookback large): the checkpoint ids == record keys
func TestAttendanceMarksExcused(t *testing.T)                    // EA -> true, UA -> false, FT (AUTH) -> null, a code not in the legend -> kind unknown, excused null
func TestAttendanceMarksReEnrolmentIsADifferentRecord(t *testing.T) // two rows, same name and period, different EnrollDate -> two keys
func TestAttendanceMarksTermChangeDoesNotRemoveLastTerm(t *testing.T) {
	// Spec 8: term rollover with complete_range. Feed the fake journal (with
	// a registry) last term's page, then this term's page; assert through
	// the fake's simple state model (Task 7 keeps none) -- instead assert
	// that the second snapshot's range starts after every date of the first,
	// so the journal's removal step cannot touch last term's records.
}
```

The last test checks the property the journal relies on (range disjointness). The end-to-end version against the real journal is in Task 23.

- [ ] **Step 15.2: Run; expect FAIL.**
- [ ] **Step 15.3: Implement** `BuildAttendanceMarks(b Binding, page *pslib.AttendancePage, cfg AttendanceDaysConfig, observedAt time.Time) []journalapi.Snapshot` and `AttendanceMarkRecordKey(course *pslib.AttendanceCourse, date time.Time) string`. Dates: `day.Date` is midnight UTC of the school day; format with `Format("2006-01-02")` in UTC (as `ProcessAttendanceDays` does).
- [ ] **Step 15.4: Run; expect PASS.**
- [ ] **Step 15.5: Commit** (`feat(powerschool): attendance_mark record builder with complete_range per school year`).

---

## Task 16: `attendance_day`: aggregation and classification

**Files:**
- Create: `connectors/powerschool/records_attendance_day.go`
- Test: `connectors/powerschool/records_attendance_day_test.go`

Per date, across all course rows of the page (spec 5.1):

- `periods_met`: cells that are not `-` (`CellNoMeeting`); `periods_unmarked`: `.` cells; a code cell with an empty code counts as met and present.
- per code cell, its kind: `absence_unexcused` -> `absent_unexcused`; `absence_excused` -> `absent_excused`; `late_*` -> `late`; `left_early_*` -> `left_early`; `authorized` -> `authorized`; `unknown` -> `unknown_codes`.
- `any_unexcused`: any `*_unexcused` kind on the day.
- `classification`, over marked periods `marked = periods_met - periods_unmarked`, `absences = absent_unexcused + absent_excused`:
  - `full_day`: `absences >= 1 && absences == marked`;
  - `partial_day`: `absences >= 1 && absences < marked`;
  - `late_or_left_early`: `absences == 0 && late + left_early >= 1`;
  - `other`: otherwise (only `authorized` / `unknown` codes).
- Only days with at least one code are candidates; a fully present day is not a record.

This task builds the pure aggregation and classification (`dayTally`, `classify`); Task 17 adds settling and the snapshot.

- [ ] **Step 16.1: Write the failing table test** with the cases spec 8 names: full (every met period `UA` or `EA`), partial (one of four `UA`), late-or-left-early (`L` and `LE`, no absence), no-meeting (`-` cells do not count: two met periods both `UA` plus two `-` is `full_day`), unmarked (one `.` among three met, two `UA` -> marked 2, `full_day`), authorized only (`FT` -> `other`, `any_unexcused` false), unknown code (`ZZ` not in the legend -> `unknown_codes` 1, `other`), mixed excused and unexcused absence (`full_day`, `any_unexcused` true), a late unexcused plus an excused absence (partial with `any_unexcused` true), and a day with no codes (no candidate).
- [ ] **Step 16.2: Run; expect FAIL.**
- [ ] **Step 16.3: Implement.**

```go
type dayTally struct {
	date                                                    time.Time
	met, unmarked, absUnex, absEx, late, leftEarly, auth, unknown int
	anyUnex, coded                                         bool
}

func tallyDays(page *pslib.AttendancePage, cfg AttendanceDaysConfig) map[string]*dayTally {
	out := map[string]*dayTally{}
	for _, course := range page.Courses {
		if course == nil {
			continue
		}
		for _, cell := range course.Days {
			k := cell.Date.UTC().Format("2006-01-02")
			t := out[k]
			if t == nil {
				t = &dayTally{date: cell.Date.UTC()}
				out[k] = t
			}
			switch cell.Kind {
			case pslib.CellNoMeeting:
				continue
			case pslib.CellUnmarked:
				t.met++
				t.unmarked++
				continue
			}
			t.met++
			if cell.Kind != pslib.CellCode || cell.Code == "" {
				continue
			}
			entry, _ := page.Describe(cell.Code)
			t.coded = true
			switch kind := cfg.kindOf(entry.Category); kind {
			case "absence_unexcused":
				t.absUnex++
				t.anyUnex = true
			case "absence_excused":
				t.absEx++
			case "late_unexcused", "late_excused":
				t.late++
				t.anyUnex = t.anyUnex || kind == "late_unexcused"
			case "left_early_unexcused", "left_early_excused":
				t.leftEarly++
				t.anyUnex = t.anyUnex || kind == "left_early_unexcused"
			case "authorized":
				t.auth++
			default:
				t.unknown++
			}
		}
	}
	return out
}

func (t *dayTally) classification() string {
	marked, abs := t.met-t.unmarked, t.absUnex+t.absEx
	switch {
	case abs >= 1 && abs == marked:
		return "full_day"
	case abs >= 1:
		return "partial_day"
	case t.late+t.leftEarly >= 1:
		return "late_or_left_early"
	}
	return "other"
}
```

(`abs > marked` cannot happen: every absence cell is a met, marked cell.)

- [ ] **Step 16.4: Run; expect PASS.**
- [ ] **Step 16.5: Commit** (`feat(powerschool): attendance_day tally and classification`).

---

## Task 17: `attendance_day`: settled days and the snapshot

**Files:**
- Modify: `connectors/powerschool/records_attendance_day.go`
- Test: `connectors/powerschool/records_attendance_day_test.go`

Settled (spec 5.1 with S1, S2): a date is settled when it is before today in the school's time zone (`Env.tz()`) and either it has no `.` cell or at least `settle` school days have followed it, where a school day is a grid date on or before today with `met > 0`, and "followed" counts those strictly after the date up to and including today.

Snapshot per school year (S3): `from` = the first grid date of the year; `to` = the last date of the longest prefix (from `from`, in date order over grid dates of that year that are before today) in which every date is settled; records = coded days within `[from, to]`; scope `(powerschool, attendance_days, subject, school_year=<y>)`, complete, `complete_range {date, from, to}`. If no date of the year is settled, no snapshot for that year. Key: the date.

- [ ] **Step 17.1: Write the failing tests** (spec 8: "unmarked-then-settled and corrected days"):

```go
func TestDayWithUnmarkedCellIsNotSettledUntilThreeSchoolDays(t *testing.T) {
	// Mon has UA in period 1 and "." in period 2. Today Tue: Mon not settled
	// (one school day followed). Today Thu (Tue, Wed, Thu followed): settled,
	// classification over marked periods only -> full_day.
}
func TestDayWithoutUnmarkedCellsSettlesTheNextDay(t *testing.T)    // yesterday all marked -> settled today; today itself never
func TestNoMeetingDaysAreNotSchoolDays(t *testing.T)                // a holiday column of "-" cells does not count toward the three
func TestSettleIsConfigurable(t *testing.T)                         // settle 1 -> settled after one school day
func TestRangeStopsBeforeTheFirstUnsettledDay(t *testing.T) {
	// Grid Mon..Fri, today next Mon. Thu has a "." and only Fri+Mon follow
	// (2 school days): Thu unsettled; Fri settled. to = Wed (S3), Fri not
	// emitted, Thu not emitted.
}
func TestCorrectedDayDropsOut(t *testing.T) {
	// First page: Tue UA. Second page: Tue blank. The second snapshot's
	// range covers Tue and has no Tue record: the journal records removed.
}
func TestFullyPresentDayIsNotARecord(t *testing.T)
func TestAttendanceDaysSplitAtTheSchoolYear(t *testing.T)
func TestAttendanceDayFieldsAreTheSpecSet(t *testing.T)            // wantFields + validateAll
```

- [ ] **Step 17.2: Run; expect FAIL.**
- [ ] **Step 17.3: Implement** `BuildAttendanceDays(b Binding, page *pslib.AttendancePage, cfg AttendanceDaysConfig, settle int, now time.Time, tz *time.Location, observedAt time.Time) []journalapi.Snapshot`. "Today" is `now.In(tz)`'s calendar date compared with the grid's UTC-midnight dates by `YYYY-MM-DD` string.
- [ ] **Step 17.4: Run; expect PASS.**
- [ ] **Step 17.5: Commit** (`feat(powerschool): attendance_day settled days and complete_range`).

---

## Task 18: `term_grade` and `term_comment`

**Files:**
- Create: `connectors/powerschool/records_history.go`
- Test: `connectors/powerschool/records_history_test.go`

`term_grade`: from the grade history page when it was read this turn (`extras.history`; throttled by `term_grades.min_interval_hours`, so most polls send none). Scope `(powerschool, term_grades, subject)`, complete. One record per row with a letter grade; key = the connector's existing key (S5): the same expression `ProcessTermGrades` builds (`<course>:<term>:<attempted>|<school>|<grade level>|<name>`, `#n` for identical rows), through `SafeKey`. Factor that expression out of `ProcessTermGrades` into `termGradeKey(g, seen)` and use it from both (the golden test `TestSurfaceFingerprints_Golden` pins that the item path's key does not change). Fields: `course` (`CourseName`), `term` (completion month `YYYY-MM`, else the raw text, else `undated`, as today), `grade` (`LetterGrade`) string; `earned` (`CreditEarned`), `attempted` (`CreditAttempted`) number.

`term_comment`: one snapshot per comments page accepted this turn (`extras.comments`, each a confirmed term; a page without a confirmed term is never used, as today). Scope `(powerschool, term_comments, subject, term=<TERM>)` (S4), complete. One record per comment with visible text; key = the connector's existing key (S6) `<course_number or name:...>|<expression>:<term>` (`#n`), factored into `teacherCommentKey(cm, term, seen)` and used by both paths. Fields: `course` (`CourseName`), `term` string; `comment` text (`cm.Comment` through `Fields.Text`, not `boundText`). The teacher's name is not a field (today it goes in the body only; spec 5.1 has no teacher field for this type). Audience: the binding's audience, which `ValidateConfig` guarantees is exactly `[guardians]`; the builder refuses (returns no snapshot, counts `powerschool_record_rows_skipped_total{reason="audience_not_guardians_only"}`) if it is not, the same defence in depth as `stage`.

- [ ] **Step 18.1: Write the failing tests:** keys equal the item checkpoint ids for the existing fixtures (`historyFixture`, `commentsFixture`); the golden test still passes unchanged; a comment with CRLF, tabs and doubled spaces arrives unchanged; an empty comment is not a record; an unconfirmed page gives no snapshot; two term pages give two snapshots with different `term` parts; a binding with `[subject, guardians]` gives no `term_comment` snapshot; `validateAll`.
- [ ] **Step 18.2: Run; expect FAIL.**
- [ ] **Step 18.3: Implement** `BuildTermGrades(b, page, observedAt)` and `BuildTermComments(b, pages, observedAt, onCut)`, plus the two key functions (exported as `TermGradeRecordKey` / `TermCommentRecordKey` wrappers for the seed).
- [ ] **Step 18.4: Run; expect PASS** (including `go test ./connectors/powerschool/ -run TestSurfaceFingerprints_Golden`).
- [ ] **Step 18.5: Commit** (`feat(powerschool): term_grade and term_comment record builders`).

---

## Task 19: Emitting from the poll; off means byte-for-byte as today

**Files:**
- Create: `connectors/powerschool/records_emit.go`
- Modify: `connectors/powerschool/connector.go` (`PowerSchoolConnector`, `pollKid`, `Watch`), `connectors/powerschool/cmd/powerschool/main.go`
- Test: `connectors/powerschool/records_emit_test.go`, `connectors/powerschool/records_off_test.go`, `connectors/powerschool/records_parity_test.go`

`pollKid` stages at its end, after the selection was confirmed before and after the reads and the student binding passed. Records are emitted right after that, from the same in-memory pages, and only then:

```go
	if c.cfg.GradesEnabled() { ... }           // unchanged
	...
	res.add(c.stageExtraPages(ctx, env, kid, extras))
	if c.records != nil {
		c.emitKidRecords(env, kid, readAt, grades, reads, extras)
	}
	return res, true, ""
```

`readAt` is `env.now()` taken at the top of `pollKid`, before `SelectStudent` (the start of the kid's read). `emitKidRecords` builds every enabled surface's snapshots (surface switches decide, exactly as for items: `GradesEnabled` -> `course_mark`; `AttendanceEnabled` -> `attendance_total`; `AssignmentsEnabled` -> `assignment`; `AttendanceDaysEnabled` -> `attendance_mark` and `attendance_day`; `TermGradesEnabled` -> `term_grade`; `TeacherCommentsEnabled` -> `term_comment`), resolves each surface's binding with `subjectFor` (no rule: no records for that surface, as for items), and calls `c.records.Emit(snaps)`. A builder panic is recovered and logged; an `Emit` error is logged and counted. Neither changes `res`, the checkpoint or anything staged.

`c.records` is an interface so the tests can use the real emitter or a recorder:

```go
type recordSink interface {
	Emit([]journalapi.Snapshot) error
	OnCut()
	Run(context.Context)
}
```

`Watch` starts `go c.records.Run(ctx)` when `c.records != nil`. `main.go` builds the emitter only when `cfg.Records.Enabled` (`records.New(cfg.Records.Config, stateDir, "powerschool", provider.Meter("glovebox.records"), time.Now)`); the meter comes from the framework's `connector.Metrics` in `Wire` -- so construct the emitter in `Wire` from a factory set by `main.go` (`c.NewRecords = func(meter metric.Meter) (recordSink, error)`), keeping `main.go` free of metric plumbing. A failure to open the spool is fatal at start (a config that asked for records and cannot have them fails loudly, like the login broker's misconfiguration).

- [ ] **Step 19.1: Write the failing tests.**

`records_off_test.go`:

```go
// With records off nothing new exists: no spool directory, no metric, no
// log line, and the staged items and the checkpoint are what they were.
func TestRecordsOffChangesNothing(t *testing.T) {
	stateDir := t.TempDir()
	c, staging := pollOnceWithFixtures(t, stateDir, false) // full fixture: grades, assignments, attendance page, history, comments
	if fileExists(filepath.Join(stateDir, "spool")) {
		t.Fatal("spool created with records off")
	}
	if strings.Contains(scrape(t, c), "glovebox_journal_") {
		t.Fatal("records metrics exported with records off")
	}
	if strings.Contains(c.logs.String(), "records") {
		t.Fatal("records log line with records off")
	}
	_ = staging
}

// The items staged with records on are identical to those with records
// off: shadow emission changes no item and no checkpoint.
func TestRecordsOnStagesTheSameItems(t *testing.T) {
	_, off := pollOnceWithFixtures(t, t.TempDir(), false)
	_, on := pollOnceWithFixtures(t, t.TempDir(), true)
	if diff := diffItems(readItems(t, off), readItems(t, on)); diff != "" {
		t.Fatalf("records changed the items: %s", diff)
	}
	// and state.json equal, ignoring meta:last_poll
}
```

`pollOnceWithFixtures` swaps `slog.Default()` for a logger writing to a buffer (the connector logs through the global `slog`) and restores it in `t.Cleanup`. `diffItems` compares metadata (minus item id and timestamps) and bodies; `pollOnceWithFixtures` runs `pollNow` once through the fake client with a fixed clock and, when on, a real `records.Emitter` against a `recordstest.Journal` with the PowerSchool registry.

`records_emit_test.go`: records are emitted only for a kid whose turn verified (a selection mismatch after the reads: items none, records none); a surface switched off emits no records for it; a surface whose rule is missing emits none for it; a spool write failure (read-only state dir after start) leaves items staged and counts `write_errors_total`; the observed_at of every snapshot equals `readAt`; two kids -> every snapshot's `data_subject` is that kid's (`e_child1` / `e_child2`) and no snapshot of k1's turn carries k2's subject; teacher comments carry `[guardians]` only.

`records_parity_test.go` (spec "parity tests showing that records and today's items agree"), on the same fixtures:

- for each surface, the set of record keys whose data an item was staged for equals the set of checkpoint ids the item path wrote in this poll, for records that items also cover (course rows with a grade; courses with nonzero attendance; assignments within lookback or flagged; coded days within lookback; term grades within lookback; comments with text). Records cover a superset (zero attendance, old assignments, grade-less courses); assert the superset relation and the exact equality on the covered subset;
- for each staged item, the record with its key carries the same facts as the item's tags (grade letter and percent, absences and tardies, code and kind, score and flags, term grade and credits): a field-to-tag table per surface, compared after `OneLine` on both sides;
- `data_subject` and `audience` of every record equal the staged item's metadata for the same kid and surface.

- [ ] **Step 19.2: Run; expect FAIL.**
- [ ] **Step 19.3: Implement.**
- [ ] **Step 19.4: Run; expect PASS**, and the whole existing suite unchanged: `go test ./connectors/powerschool/... -count=1 -race`.
- [ ] **Step 19.5: Commit** (`feat(powerschool): emit record snapshots in shadow behind records.enabled`).

---

## Task 20: Chart: the PowerSchool records block and its projected token

**Files:**
- Modify: `charts/glovebox/values.yaml`, `charts/glovebox/templates/connector-configmap.yaml`, `charts/glovebox/templates/connector-deployment.yaml`, `charts/glovebox/templates/_helpers.tpl`
- Create: `scripts/test-chart-records.sh`, `scripts/testdata/records-values.yaml`
- Test: `scripts/test-chart-records.sh` (picked up by `scripts/chart-checks.sh`'s glob)

Values (under `connectors.powerschool`):

```yaml
    # Record journal snapshots (rollout step 2, in shadow: items are staged
    # unchanged). Needs journal.enabled, this connector in
    # journal.networkPolicy.writerConnectors and its ServiceAccount in
    # journal.services with write_types. Renders a projected token for the
    # audience glovebox-journal; refused together with loginOnDemand (that
    # mode mounts the API token). The spool lives under /state/spool.
    records:
      enabled: false
      tokenExpirationSeconds: 600
      timeoutSeconds: 90
      attendanceDaySettleSchoolDays: 3
      spool:
        maxAgeHours: 168
        maxBytes: 268435456
```

Rendering, only when `records.enabled`:

- `connector-configmap.yaml`: `$config = mustMergeOverwrite (deepCopy $config) (dict "records" (dict "enabled" true "journal_url" (printf "http://%s-journal:%d" (include "glovebox.fullname" $) (int $.Values.journal.port)) "token_file" "/var/run/secrets/glovebox-journal/token" "timeout_seconds" ... "attendance_day_settle_school_days" ... "spool" (dict "max_age_hours" ... "max_bytes" ...)))`. Merge after the existing login merges (the template reassigns `$config` in two places; merge into the final value).
- `connector-deployment.yaml`: a volume

```yaml
        # A token for the record journal and for nothing else: audience
        # glovebox-journal, so neither the Kubernetes API nor the login
        # broker accepts it. Short-lived; the kubelet rotates it and the
        # connector re-reads the file for every request.
        - name: journal-token
          projected:
            defaultMode: 0440
            sources:
              - serviceAccountToken:
                  audience: glovebox-journal
                  expirationSeconds: {{ $records.tokenExpirationSeconds | default 600 | int }}
                  path: token
```

  and a mount `/var/run/secrets/glovebox-journal` read-only. Nothing else in the pod changes (`automountServiceAccountToken` stays false). Generalise the condition with a helper `glovebox.recordsEnabled` (`dict "name" $name "connector" $connector`) so B2 and later connectors reuse it; for now only `powerschool` may enable it (`fail` for any other name: no other connector has a builder).
- `_helpers.tpl` `glovebox.recordsChecks` (called from the connector configmap when enabled; S12, S13): fail unless `journal.enabled`; fail if `loginOnDemand.enabled`; fail unless `journal.networkPolicy.writerConnectors` contains the name; fail unless `journal.services["<ns>/<connector SA>"].write_types` is non-empty (the existing reverse check stays); fail if `tokenExpirationSeconds` < 600 (the kubelet's minimum) or > 3600; `attendanceDaySettleSchoolDays` 1-10.

- [ ] **Step 20.1: Write the failing render tests** in `scripts/test-chart-records.sh` (same harness shape as `test-chart-journal.sh`: `check NAME PYEXPR args`, `expect_fail NAME FRAGMENT args`):
  - default values render byte-identically to `BASE_REF` (`origin/main`) -- the off-by-default guarantee;
  - `journal.enabled` + records off: powerschool pod unchanged from the same render on `BASE_REF`;
  - records on (with `scripts/testdata/records-values.yaml`: journal enabled, writer connector, services entry for `openclaw/glovebox-glovebox-connector-powerschool` with the seven `write_types`, `write_scopes: {source: [powerschool]}`, `write_subjects: roster_subjects`): config.json has the `records` block with the URL `http://glovebox-glovebox-journal:9095` and the token path; the pod has exactly one projected token with audience `glovebox-journal`, `automountServiceAccountToken: false`, no other token source; the RSS connector's pod (enabled in the values) has no journal token and its SA no services entry;
  - refusals: records without `journal.enabled`; with `loginOnDemand.enabled`; without the writer connector entry; without the services entry; `tokenExpirationSeconds: 300`; records on another connector (`connectors.rss.records.enabled`).
- [ ] **Step 20.2: Run; expect FAIL** (`scripts/test-chart-records.sh`).
- [ ] **Step 20.3: Implement.**
- [ ] **Step 20.4: Run; expect PASS**, then `REQUIRE_BASE_REF=1 scripts/chart-checks.sh`.
- [ ] **Step 20.5: Commit** (`feat(chart): powerschool records block and projected journal token`).

---

## Task 21: Chart: record types, staleness and rules values

**Files:**
- Create: `charts/glovebox/journal-record-types/powerschool.json`, `connectors/powerschool/record_types_file_test.go`
- Modify: `charts/glovebox/values.yaml` (`journal.connectorRecordTypes`), `charts/glovebox/templates/configmap.yaml`, `charts/glovebox/templates/_helpers.tpl`, `scripts/test-chart-records.sh`

S14: `journal.connectorRecordTypes: []`; listing `powerschool` appends the file's types to `record_types` in the core's config (`.Files.Get (printf "journal-record-types/%s.json" .)` decoded with `fromJsonArray`, `concat` with `.recordTypes`). A name without a file fails the render. A name in `connectorRecordTypes` whose types also appear in `recordTypes` fails (two definitions of one type version).

- [ ] **Step 21.1: Write the failing tests.** Go: `TestRecordTypesFileIsUpToDate` reads `../../charts/glovebox/journal-record-types/powerschool.json` and compares it with `json.MarshalIndent(RecordTypes(), "", "  ")`; with `-update` (a test flag) it writes the file. Chart: `connectorRecordTypes: [powerschool]` renders seven types into the core config; an unknown name fails; a duplicate fails; default renders unchanged.
- [ ] **Step 21.2: Run; expect FAIL.**
- [ ] **Step 21.3: Implement**; generate the file with `go test ./connectors/powerschool/ -run TestRecordTypesFileIsUpToDate -update`.
- [ ] **Step 21.4: Run; expect PASS.**
- [ ] **Step 21.5:** Document in `values.yaml` comments, next to `journal.staleness` and `journal.rules`, the values Part B needs in gitops (no defaults change):

```yaml
  #   staleness:
  #     - prefix: "source=powerschool/"
  #       maxAgeHours: 96
  #   rules (spec 4.10, conjunction form, Part A Task 10 amendment):
  #     - name: full_day_unexcused
  #       record_type: attendance_day
  #       field_becomes: {classification: full_day, any_unexcused: true}
  #       ...
```

  Copy the rule shapes from `internal/journal/rules` tests (the as-built schema), not from the spec's 4.10 example.
- [ ] **Step 21.6: Commit** (`feat(chart): connector record types from the connector's own definitions`).

---

## Task 22: Spool alerts

**Files:**
- Create: `charts/glovebox/templates/connector-records-prometheusrule.yaml`, `scripts/testdata/records-alerts-test.yaml`
- Modify: `scripts/test-chart-records.sh`

Rendered when `metrics.prometheusRule.enabled` and at least one connector has `records.enabled`. Aggregated by `namespace, connector` (the connector label from the metrics). Each description is its runbook (what it means, what to check, how to clear), in the style of `journal-prometheusrule.yaml`.

| Alert | Expression | For | Spec |
|---|---|---|---|
| `GloveboxJournalSpoolNotEmpty` | `max by (namespace, connector) (glovebox_journal_spool_oldest_seconds) > 3600` | 5m | 4.14, 4.15 |
| `GloveboxJournalSpoolDeploymentFault` | `sum by (namespace, connector, reason) (increase(glovebox_journal_spool_answers_total{reason=~"unauthenticated\|forbidden\|bad_request\|not_found\|other\|none", action="retry_alert"}[15m])) > 0` | -- | 4.3 "alert" rows |
| `GloveboxJournalSpoolRejected` | `sum by (namespace, connector, reason) (increase(glovebox_journal_spool_rejected_total[1h])) > 0` | -- | a permanent refusal is a connector bug |
| `GloveboxJournalSpoolDropped` | `sum by (namespace, connector, reason) (increase(glovebox_journal_spool_dropped_total{reason!="erased_subject"}[1h])) > 0` | -- | 4.14 "the drop counter makes that visible" |

The `NotEmpty` runbook text says that a persisting `503` (spec 4.3: retried and alerted) surfaces through this alert after an hour, and points at `glovebox_journal_spool_answers_total{reason="unavailable"}` and the core's `GloveboxJournalNotReady`.

- [ ] **Step 22.1: Write the failing promtool unit tests** (`promtool test rules`, skipped with a note when promtool is absent, as `test-chart-journal.sh` does): each alert fires on its series and not on a healthy one; `NotEmpty` does not fire for a spool that empties within the hour; `DeploymentFault` does not fire for 409 or 503.
- [ ] **Step 22.2: Run; expect FAIL.**
- [ ] **Step 22.3: Implement.**
- [ ] **Step 22.4: Run; expect PASS.**
- [ ] **Step 22.5: Commit** (`feat(chart): spool alerts for record-writing connectors`).

---

## Task 23: End to end against the real journal core

**Files:**
- Create: `connectors/powerschool/records_journal_db_test.go`

One test, behind `JOURNAL_TEST_DATABASE_URL` (skips without it, fails with `JOURNAL_TEST_REQUIRE_DB=1`, like Part A's DB tests): start the journal's HTTP API in-process with `internal/journal/journaltest` (a fresh database), the PowerSchool record types, a roster fixture with `e_child1` / `e_child2` / `e_parent1`, a policy entry for a test ServiceAccount, and `satoken/satokentest`'s fake TokenReview; run two polls of the connector with records on against it (the second poll: one code corrected away, one assignment scored, a new term's attendance page); then read `/v1/state` and `/v1/events` as `e_parent1`.

Assert: every snapshot was `applied`; the first poll created the expected records; the second gives `updated` for the scored assignment, `removed` for the corrected attendance mark and attendance day, and **no** removal of last term's marks (term rollover with `complete_range`, spec 8); a resend of a spooled file gives `duplicate`; `term_comment` rows are invisible to `e_child1`'s reader; nothing of `e_child2`'s turn is in `e_child1`'s rows.

- [ ] **Step 23.1: Write the test** (look at `internal/journal/httpapi/rig_test.go` for how the API is assembled in tests; reuse `journaltest` and export any helper the test needs from there rather than duplicating it).
- [ ] **Step 23.2: Run** `scripts/journal-test-db.sh ./connectors/powerschool/ -run TestRecordsAgainstTheJournal` (on this WSL host use `JOURNAL_PG_HOST_NETWORK=1 ... -p 1`, see the Part A Task 31 note); expect FAIL until the wiring is right, then PASS.
- [ ] **Step 23.3:** Add `./connectors/powerschool/` to the `test-journal-db` GitLab job's package list and to `.github/workflows/ci.yml`'s database step.
- [ ] **Step 23.4: Commit** (`test(powerschool): records end to end against the journal core`).

---

## Task 24: Docs, container build, final verification

**Files:**
- Modify: `docs/journal-api.md` (new section "Connector side: the spool"), `docs/connectors/powerschool.md` (new section "Records (journal)"), `docs/upgrading.md`, `CHANGELOG.md`, `docs/superpowers/specs/2026-10-16-record-journal-design.md` (5.1 amendments for S4-S7, S9-S11, marked "amended by Part B1 plan, decision Sn")

- [ ] **Step 24.1: Write the docs.** `journal-api.md`: spool layout, file name, what each answer does (link to the table), bounds, metrics and alerts, crash behaviour, the subject purge on 410 (and that it is the connector's share of glovebox-wekc.18.1). `powerschool.md`: the seven types (fields, keys, scopes), how `data_subject` and audience are taken from the kid's rules, what is emitted when, the settling rule, the config keys. `upgrading.md`: enabling checklist for gitops (below). `CHANGELOG.md`: an Unreleased entry.

- [ ] **Step 24.2: The gitops enabling checklist** (in `upgrading.md`; not done by this plan):
  1. The journal is enabled and ready (Part A checklist done).
  2. **Subjects:** every `data_subject` in the PowerSchool rules is a roster member: `kubectl -n openclaw get cm openclaw-household -o jsonpath='{.data.household\.json}' | jq -r '.people|keys[]'` contains each rule's `data_subject` (otherwise those snapshots wait in the spool with 409 until `max_age`).
  3. HelmRelease values: `journal.connectorRecordTypes: [powerschool]`; `journal.services."openclaw/glovebox-glovebox-connector-powerschool": {write_types: [course_mark, assignment, attendance_mark, attendance_day, attendance_total, term_grade, term_comment], write_scopes: {source: [powerschool]}, write_subjects: roster_subjects}`; `journal.networkPolicy.writerConnectors: [powerschool]`; `journal.staleness: [{prefix: "source=powerschool/", maxAgeHours: 96}]`; `connectors.powerschool.records.enabled: true`.
  4. After the next poll window: `glovebox_journal_spool_files{dir="pending"}` returns to 0; `glovebox_journal_snapshots_total{status="applied"}` grows on the core; no `GloveboxJournalSpool*` alert; the connector's items are unchanged (the openclaw patrol sees nothing different).
  5. Rollback: `records.enabled: false`. The spool directory stays on the PVC until deleted by hand (log it in gitops' change description); nothing in the item path depends on it.

- [ ] **Step 24.3: Container.** `podman build -f connectors/powerschool/Dockerfile -t glovebox-powerschool:records-test .` succeeds; run it once with a records-enabled config against nothing reachable (`journal_url: http://127.0.0.1:1`) and a read-only root plus a writable `/state` and check that it starts, creates `/state/spool`, and logs no token (`podman run --read-only -v <tmp>:/state ...`, then stop it).

- [ ] **Step 24.4: Final verification** (fresh, `GOTOOLCHAIN=go1.27.1`): `go build ./...`; `go vet ./...`; staticcheck v0.8.1 over `./connector/... ./connectors/powerschool/...`; `gofmt -l` over the files the branch touches; `go test ./... -count=1 -race`; `scripts/journal-test-db.sh ./connectors/powerschool/ ./internal/journal/...`; `REQUIRE_BASE_REF=1 scripts/chart-checks.sh`; the ASCII grep over the branch's files (`git diff --name-only origin/main | xargs grep -nP '[^\x00-\x7F]'` finds nothing); the placeholder check (no id other than `e_child*`, `e_parent*`, existing fixture ids in new files). Record the results as a NOTE under this task.

- [ ] **Step 24.5: Commit** (`docs(records): connector spool, powerschool records, enabling checklist`), push the branch, open the MR. Do not enable anything in gitops from this plan.

---

## Out of this plan

- The Schoology connector: B2 (`docs/superpowers/plans/2026-10-28-record-journal-part-b2-schoology.md`).
- The seed Job (rollout step 3): S15; its own plan.
- Openclaw's shadow comparison (rollout step 4) and everything after.
- Stopping items for record surfaces (rollout step 6): until then items are staged unchanged.
