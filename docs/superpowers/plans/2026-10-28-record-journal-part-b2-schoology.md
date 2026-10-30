# Record Journal Part B2 Implementation Plan: the Schoology connector

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rollout step 2 of the record journal, second half: the Schoology connector emits `lms_assignment` and `lms_assignment_detail` snapshots into the journal in shadow (spec `docs/superpowers/specs/2026-10-16-record-journal-design.md` 5.2), through the shared spool built in B1, behind `records.enabled` (default false).

**Architecture:** Two pure builders (the overdue/upcoming listing in, one windowed snapshot out; the verified detail reads in, one windowed snapshot out), one emission call per kid in `pollNow` after the listing and one after the detail surface, and the chart's Schoology templates gaining the same records block and projected token as PowerSchool's. Everything about spooling, sending, answers, bounds and metrics is `connector/records` from B1 and is not repeated here.

**Tech Stack:** as B1; `github.com/leftathome/schoology-go` v0.3.0 (unchanged).

**Prerequisite:** B1 (`docs/superpowers/plans/2026-10-28-record-journal-part-b.md`) Tasks 1-11 merged (the `connector/records` package and `recordstest`), and B1 Tasks 20-22 merged for the chart helpers this plan extends. Read B1's "How to work", "Rules", "Decisions" and "Part A deviations" sections first: they hold here too. Bead: the B2 child filed in B1's "How to work". Branch `feat/record-journal-part-b2` off `origin/main` after those merges. This file is the progress record.

NOTE (B2 implementation): B1 Tasks 12-24 were still in open MRs (!119, !121) when B2 started, so the branch is based on `origin/feat/journal-part-b1-chart` (d687390), which carries all of B1, not on `origin/main`. Rebase onto `origin/main` once B1 merges. The whole-module `go.sum` gap in `internal/sanitizeapi` is fixed on main but not on this base; it is left alone here.

**Feedback loop, every task:** B1's, with `./connectors/schoology/...` in place of `./connectors/powerschool/...`.

---

## What this does and does not capture (spec 5.2, unchanged)

- A change while an assignment is in the overdue/upcoming window (status, due date, title) becomes an `updated` event. Today the connector's seen-set suppresses it.
- An assignment that leaves the window produces no event: the scope is not complete, so nothing is removed; its row keeps its last values with `in_latest_snapshot=false`.
- PowerSchool's `assignment` (a complete scope) is the authoritative record of submitted and scored work.
- Posts and messages stay items.

---

## Decisions for Steve (accepted at the recommended defaults)

NOTE (B2 implementation, 2026-10-30): T1-T6 were accepted at the recommended defaults below; the plan implements them as written.

| # | Question | Where the spec stands | Recommendation |
|---|---|---|---|
| T1 | **`lms_assignment_detail.teacher`.** | Listed as a `string` field. `schoology-go` v0.3.0's `AssignmentDetail` has no teacher (the parent's page does not show one; the library's docs say so). | Leave it out of schema version 1. When the library exposes a teacher, add it as an optional field in version 2 (Part A 4.7 allows that without a migration). Spec 5.2 to be amended. |
| T2 | **Degraded detail reads.** | Silent. Today a detail with parse errors, an unreadable comments page or a course that could not be checked against the list (`course_unverified`) is still staged, tagged as degraded. | Emit a detail record only for a clean read: the id, course and title checks passed, no parse error, comments read. A degraded read would write empty or partial `description`/`comments` and produce an `updated` event that blanks good values. Items keep today's behaviour. |
| T3 | **`comments` text.** | One `text` field. The library returns a list (`LayoutUnmapped` today: each entry is author, time and body run together). | Entries joined with a blank line, in page order, each entry as the library returned it; `null` when the block is empty; never the "count only" placeholder the item body uses. |
| T4 | **`status` and `link` values.** | `status`, `link` strings. | `status` as the library gives it (the item's `status` tag, `Overdue` / `Upcoming`); `link` the path-only URL the library gives (the item's `Link:` line). No host is invented. |
| T5 | **Per-kid rule strictness.** | 4.9c and 5.2 assume each record has the child's subject. Today only the detail surface requires an exact per-kid rule; the assignment surface may be routed by `*`, which carries one default subject for every child. | With `records.enabled`, every kid needs an exact `schoology:<kid>:assignment` rule ahead of any `*` with a `data_subject`, distinct across kids, and the detail rule (when the surface is on for the kid) must name the same subject. Without records, unchanged. |
| T6 | **State volume.** | 4.14: the spool is on the connector's state volume. The Schoology connector's state is an `emptyDir` unless `schoologyConnector.state.persistent` (or `existingClaim`) is set. | The chart refuses `schoologyConnector.records.enabled` with an `emptyDir` state: a spool that a pod restart wipes is not durable. |

Part A deviations: B1's table applies unchanged. Specific to Schoology: both scopes are `complete: false`, so the removal guard (A11) and `complete_range` never apply, and a `held` answer cannot occur.

---

## File structure

| File | Responsibility |
|---|---|
| `connectors/schoology/record_types.go` | `RecordTypes()`: `lms_assignment`, `lms_assignment_detail` (owner `schoology`, v1). |
| `connectors/schoology/record_binding.go` | `subjectFor(matcher, kid, surface)`; the T5 checks. |
| `connectors/schoology/records_assignments.go` | `BuildAssignmentListing(b, kid, rows, observedAt)`. |
| `connectors/schoology/records_detail.go` | `VerifiedDetail`, `BuildAssignmentDetails(b, reads, observedAt, onCut)`. |
| `connectors/schoology/records_emit.go` | emission from `pollNow`; spool errors never touch items. |
| `connectors/schoology/config.go` (modify) | `Records records.Config`. |
| `connectors/schoology/assignment_detail.go` (modify) | `DetailDeps.OnVerified` callback at the point a clean read passes its checks. |
| `connectors/schoology/connector.go`, `cmd/schoology/main.go` (modify) | hold the emitter, emit, start the loop. |
| `charts/glovebox/values.yaml`, `templates/schoology-connector-configmap.yaml`, `templates/schoology-connector-deployment.yaml`, `templates/_helpers.tpl`, `templates/connector-records-prometheusrule.yaml` (modify) | records block, token, checks, alerts condition. |
| `charts/glovebox/journal-record-types/schoology.json` | generated from `RecordTypes()`. |
| `scripts/test-chart-records.sh`, `scripts/testdata/records-values.yaml` (modify) | Schoology cases. |

---

## Task 1: Records config and the per-kid rule checks

**Files:**
- Modify: `connectors/schoology/config.go`
- Create: `connectors/schoology/record_binding.go`
- Test: `connectors/schoology/record_binding_test.go`, `connectors/schoology/records_testkit_test.go`

- [x] **Step 1.1: Write the failing tests.**

```go
func TestRecordsNeedAnExactAssignmentRulePerKid(t *testing.T) {
	cfg := recordsConfig(t) // kids k1, k2; exact assignment rules e_child1, e_child2; records enabled
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Rules = append([]connector.Rule{{Match: "*", Destination: "school", DataSubject: "e_child1"}}, cfg.Rules...)
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "records") {
		t.Fatalf("a * rule ahead of the kid's rule: err = %v", err)
	}
}

func TestRecordsRefuseOneSubjectForTwoKids(t *testing.T)            // both kids' rules name e_child1
func TestRecordsRefuseARuleWithoutSubject(t *testing.T)
func TestRecordsRefuseDetailRuleWithAnotherSubject(t *testing.T)    // assignment e_child1, assignment_detail e_child2 for k1
func TestWithoutRecordsTheRulesAreAsLiberalAsToday(t *testing.T)   // the * case above passes with records off
```

`records_testkit_test.go`: `recordRules()` (exact rules for `schoology:k1:assignment`, `schoology:k1:assignment_detail`, the same for `k2`, subjects `e_child1` / `e_child2`, audience `[subject, guardians]`), `recordsRegistry(t)`, `validateAll(t, snaps)` (as in B1).

- [x] **Step 1.2: Run; expect FAIL.**
- [x] **Step 1.3: Implement** `Records records.Config` on `Config`; `ApplyDefaults` and `Validate` of the block as in B1 Task 12; `validateKidRecordRules(c)` using `firstMatchIsExact` (already in `config.go`) and `connector.NewRuleMatcher`.
- [x] **Step 1.4: Run; expect PASS.**
- [x] **Step 1.5: Commit** (`feat(schoology): records config and per-kid rule checks`).

NOTE (B2 implementation): `Records records.Config` on `Config` (`json:"records"`, no Schoology-only settings, so no wrapper struct). `ApplyDefaults` calls `applyRecordsDefaults` and `ValidateConfig` calls `validateRecords` (both in `record_binding.go`, no-ops when disabled); `validateRecords` runs before `validateAssignmentDetail` so the records refusal names records. A kid without any exact `schoology:<kid>:assignment` rule is refused with records on, even with the detail surface off (extra test). The red run was seen behaviourally: the field and a stub `subjectFor` were added first, and seven tests failed on their assertions before the implementation.

---

## Task 2: Record types

**Files:**
- Create: `connectors/schoology/record_types.go`, `charts/glovebox/journal-record-types/schoology.json`
- Test: `connectors/schoology/record_types_test.go`, `connectors/schoology/record_types_file_test.go`

| Type | Fields | default_audience |
|---|---|---|
| `lms_assignment` | `title`, `course`, `status`, `link` string; `due` timestamp | `[subject, guardians]` |
| `lms_assignment_detail` | `description`, `comments` text (T1: no `teacher` in v1) | `[subject, guardians]` |

- [x] **Step 2.1: Write the failing tests:** the registry accepts the types; owner `schoology`; `TestRecordTypesFileIsUpToDate` (with `-update`), as in B1 Task 21.
- [x] **Step 2.2: Run; expect FAIL.**
- [x] **Step 2.3: Implement**; generate the JSON file.
- [x] **Step 2.4: Run; expect PASS.**
- [x] **Step 2.5: Commit** (`feat(schoology): record type definitions`).

NOTE (B2 implementation): `RecordTypes()` in `record_types.go` (owner `schoology`, version 1, both `[subject, guardians]`, no field required, T1: no `teacher`); `TestRecordTypeFieldSets` pins the field names and types. The file was generated with `go test ./connectors/schoology/ -run TestRecordTypesFileIsUpToDate -update`. Red: with a stub returning no types, `TestRecordTypesAreValidForTheJournal` and `TestRecordTypesFileIsUpToDate` failed on their assertions.

---

## Task 3: `lms_assignment` from the listing

**Files:**
- Create: `connectors/schoology/records_assignments.go`
- Test: `connectors/schoology/records_assignments_test.go`

One snapshot per kid per listing that answered (`processAssignments`'s `listed == true`): scope `(schoology, assignments, subject)`, `complete: false`, one record per row (nil rows skipped; rows the library failed to parse are not in `rows` and are simply absent, which a windowed scope tolerates). Key: `strconv.FormatInt(a.ID, 10)`; a row with `ID == 0` is skipped and counted (`schoology_record_rows_skipped_total{reason="no_id"}`, nil-safe, in `telemetry.go`); a repeated id keeps the first. Fields: `title` (`a.Title`), `course` (`a.CourseTitle`), `status` (`string(a.Status)`), `link` (`a.URL`) through `Fields.String`; `due` (`a.DueAt`, zero is null) through `Fields.Timestamp`. An empty listing that answered gives an empty snapshot (harmless with `complete: false`; it confirms nothing and removes nothing) -- send it anyway so the journal's staleness metric sees the scope.

- [x] **Step 3.1: Write the failing tests:** fields and key for the fixture rows from `assignments_test.go`; every record has exactly the type's fields; a status change between two listings gives two snapshots whose records differ only in `status` (the journal turns that into `updated`; the item path stages nothing the second time -- assert that too, it is the gap 5.2 describes); `complete` false; `validateAll`; the record keys equal the ids `SaveLastSeenID` writes for the same rows.
- [x] **Step 3.2: Run; expect FAIL.**
- [x] **Step 3.3: Implement** `BuildAssignmentListing(b Binding, rows []*schoologylib.Assignment, observedAt time.Time) journalapi.Snapshot` and `AssignmentRecordKey(id int64) string` (for the seed).
- [x] **Step 3.4: Run; expect PASS.**
- [x] **Step 3.5: Commit** (`feat(schoology): lms_assignment record builder`).

NOTE (B2 implementation): the library's `AssignmentStatus` values are lowercase (`overdue` / `upcoming`), as is the item's `status` tag; T4's "Overdue / Upcoming" is read as "the library's value", which is what the record carries. A repeated id keeps the first row and is counted `duplicate_key` besides `no_id` (the journal refuses a repeated key), both through `Binding.OnSkip` into `schoology_record_rows_skipped_total` (`Telemetry.RecordRecordRowSkipped`, nil-safe). Records are in listing order. `Binding.scope` / `Binding.record` live in `record_binding.go`. Red: against a stub builder all three tests failed (snapshot_id not a UUID); the first green run then failed on the status casing, fixed in the test as above.

---

## Task 4: `lms_assignment_detail` from verified detail reads

**Files:**
- Modify: `connectors/schoology/assignment_detail.go` (`DetailDeps`, `handle`)
- Create: `connectors/schoology/records_detail.go`
- Test: `connectors/schoology/records_detail_test.go`

In `detailRun.handle`, the read is attributed to the kid only after the id, course and title checks. Add, immediately after those checks and after the parse-error and comments-error bookkeeping, and **before** the unchanged-hash early return (a record is emitted for every clean read, changed or not; the journal skips unchanged content):

```go
	if deps.OnVerified != nil && !parseFailed && !courseUnverified {
		deps.OnVerified(VerifiedDetail{Kid: kid, Rule: ks.rule, Row: w.row, Detail: d, Comments: f.comments, ReadAt: run.now})
	}
```

`DetailDeps.OnVerified func(VerifiedDetail)` is nil when records are off, so the item path is untouched (the golden and review tests of `assignment_detail_*_test.go` must pass unchanged). Place the hook after the line that computes `courseUnverified` and before the unchanged-hash early return.

Builder: one snapshot per kid per poll with at least one verified read: scope `(schoology, assignment_detail, subject)`, `complete: false`, key = the assignment id. Fields: `description` (`d.DescriptionText`) and `comments` (T3: populated -> entries' `Text` joined by `"\n\n"`; otherwise null) through `Fields.Text` -- not `cleanText`, `writeUntrusted` or the item's framing.

- [ ] **Step 4.1: Write the failing tests:** a clean read gives a record with the description unchanged (fixture with line breaks, a link rendered `text (href)`, a bidi control); an unchanged re-read (seen hash equal) still gives a record and stages no item; a read with a parse error, one with a comments error, and one with `course_unverified` give no record but still stage their item as today; an id or title mismatch gives neither; comments: populated (two entries -> joined), empty -> null; `OnVerified == nil` changes nothing (compare staged items and checkpoint with and without the hook set); `validateAll`.
- [ ] **Step 4.2: Run; expect FAIL.**
- [ ] **Step 4.3: Implement** `VerifiedDetail`, `BuildAssignmentDetails(b Binding, reads []VerifiedDetail, observedAt time.Time, onCut func()) []journalapi.Snapshot`.
- [ ] **Step 4.4: Run; expect PASS** (whole `./connectors/schoology/...` suite).
- [ ] **Step 4.5: Commit** (`feat(schoology): lms_assignment_detail records from verified detail reads`).

---

## Task 5: Emitting from the poll; off means byte-for-byte as today

**Files:**
- Create: `connectors/schoology/records_emit.go`
- Modify: `connectors/schoology/connector.go`, `connectors/schoology/cmd/schoology/main.go`
- Test: `connectors/schoology/records_emit_test.go`, `connectors/schoology/records_off_test.go`, `connectors/schoology/records_parity_test.go`

In `pollNow`, per kid: after `processAssignments` returns `listed == true`, build the listing snapshot with the kid's assignment binding and `Emit` it (`observed_at` = the time taken just before `processAssignments`). Detail: set `DetailDeps.OnVerified` (when records are on) to collect reads per kid; after `ProcessAssignmentDetails` returns, build and emit one detail snapshot per kid that has reads. The same `recordSink` interface and factory-in-`Wire` arrangement as B1 Task 19; `Watch` starts `Run`.

- [ ] **Step 5.1: Write the failing tests:**
  - off: no `spool` directory, no `glovebox_journal_` series, no records log line; staged items and the seen sets equal to a run without the change (run the existing integration-style fixtures);
  - on: the same items; snapshots per kid with that kid's subject only; a listing error (GetOverdueSubmissions fails) emits nothing for that kid; a session refused (details skipped) emits only listings; a spool write failure leaves items staged and counts `write_errors_total`;
  - parity: for each item staged from the listing, a record with the same id carries the same `course`, `status`, due and link as the item's tags and body; the record set is a superset (rows already in the seen set are records too); for each detail item staged from a clean read, a record with its id exists; `data_subject` and `audience` equal the item's.
- [ ] **Step 5.2: Run; expect FAIL.**
- [ ] **Step 5.3: Implement.**
- [ ] **Step 5.4: Run; expect PASS.**
- [ ] **Step 5.5: Commit** (`feat(schoology): emit record snapshots in shadow behind records.enabled`).

---

## Task 6: Chart: the Schoology records block and token

**Files:**
- Modify: `charts/glovebox/values.yaml` (`schoologyConnector.records`, same shape as B1's minus `attendanceDaySettleSchoolDays`), `templates/schoology-connector-configmap.yaml`, `templates/schoology-connector-deployment.yaml`, `templates/_helpers.tpl` (`glovebox.recordsChecks` accepts `schoology`; T6), `templates/connector-records-prometheusrule.yaml` (condition includes `schoologyConnector.records.enabled`), `scripts/test-chart-records.sh`, `scripts/testdata/records-values.yaml`

The Schoology configmap merges the `records` block into `$cfg` before `toJson` (after the `assignment_detail` merge). The deployment adds the same `journal-token` projected volume and mount as B1 Task 20, and keeps `automountServiceAccountToken: false`.

- [ ] **Step 6.1: Write the failing render tests:** default values byte-identical to `BASE_REF`; records on (journal enabled, writer `schoology`, services entry `openclaw/glovebox-glovebox-connector-schoology` with `write_types: [lms_assignment, lms_assignment_detail]`, `write_scopes: {source: [schoology]}`, `write_subjects: roster_subjects`, `state.persistent: true`): the block and token render, no other token; refusals: `state.persistent: false` without `existingClaim` (T6), no writer entry, no services entry, journal disabled; the PrometheusRule renders with only Schoology's records on.
- [ ] **Step 6.2: Run; expect FAIL.**
- [ ] **Step 6.3: Implement.**
- [ ] **Step 6.4: Run; expect PASS** (`REQUIRE_BASE_REF=1 scripts/chart-checks.sh`).
- [ ] **Step 6.5: Commit** (`feat(chart): schoology records block and projected journal token`).

---

## Task 7: Chart: Schoology record types

**Files:**
- Modify: `scripts/test-chart-records.sh`, `charts/glovebox/values.yaml` (comment)

- [ ] **Step 7.1: Write the failing test:** `journal.connectorRecordTypes: [powerschool, schoology]` renders nine types into the core config; `[schoology]` alone renders two.
- [ ] **Step 7.2: Run; expect FAIL** (if B1's helper reads files by name it may already pass: then the step is the test only; note it).
- [ ] **Step 7.3:** Document `journal.staleness` `source=schoology/` (`maxAgeHours: 96`) in the values comment.
- [ ] **Step 7.4: Run; expect PASS.**
- [ ] **Step 7.5: Commit** (`feat(chart): schoology record types`).

---

## Task 8: End to end against the real journal core

**Files:**
- Create: `connectors/schoology/records_journal_db_test.go`

As B1 Task 23, with the Schoology fake client: poll 1 lists A (Upcoming) and B (Overdue) and reads A's detail; poll 2 lists A with status changed and a new due date, B no longer listed, C new; poll 3 re-reads A's detail with a new comment.

Assert through `/v1/state` and `/v1/events` as `e_parent1`: A has an `updated` event with `status` and `due` changed; B is not removed and its row has `in_latest_snapshot: false`; C is `created`; A's detail has an `updated` event on `comments`; the detail's text values carry scan verdicts (not withheld for plain text); nothing of `e_child2` appears under `e_child1`.

- [ ] **Step 8.1: Write the test.**
- [ ] **Step 8.2: Run** `scripts/journal-test-db.sh ./connectors/schoology/ -run TestRecordsAgainstTheJournal`; PASS.
- [ ] **Step 8.3:** Add `./connectors/schoology/` to the CI database jobs.
- [ ] **Step 8.4: Commit** (`test(schoology): records end to end against the journal core`).

---

## Task 9: Docs, container build, final verification

**Files:**
- Modify: `docs/connectors/schoology.md`, `docs/upgrading.md`, `CHANGELOG.md`, spec 5.2 (T1 amendment)

- [ ] **Step 9.1: Docs:** the two types, the windowed-scope semantics stated plainly (the 5.2 list above), T2's rule, config keys, and the gitops enabling checklist: (1) `schoologyConnector.state.persistent: true`; (2) every rule subject is a roster member (B1's `jq` check); (3) values `journal.connectorRecordTypes` gains `schoology`, the services entry, `writerConnectors` gains `schoology`, `staleness` gains `source=schoology/`, `schoologyConnector.records.enabled: true`; (4) after a poll window the spool is empty and no `GloveboxJournalSpool*` alert fires; (5) rollback is `records.enabled: false`.
- [ ] **Step 9.2: Container:** `podman build -f connectors/schoology/Dockerfile -t glovebox-schoology:records-test .`; start it with a records-enabled config and a writable `/state`, check `/state/spool` appears and no token is logged.
- [ ] **Step 9.3: Final verification:** B1 Task 24's list with the Schoology packages; record results as a NOTE.
- [ ] **Step 9.4: Commit** (`docs(records): schoology records and enabling checklist`), push, open the MR. Nothing is enabled in gitops from this plan.
