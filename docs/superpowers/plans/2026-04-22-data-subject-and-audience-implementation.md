# Spec 11 -- Data Subject and Audience Metadata Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `data_subject` and `audience` fields to Glovebox's item metadata, plus validation, plumbing, and audit-log support, per `docs/specs/11-data-subject-and-audience-design.md`. Metadata-only v1; no routing/enforcement.

**Architecture:** Additive-only. New audience-validation primitive in `internal/staging/audience.go` (pure function, no deps). Struct extensions in `internal/staging/types.go`, `internal/audit/logger.go`, `connector/rule.go`, `connector/runner.go`, `connector/staging.go`. Merge semantics (per-item > rule > config default > omitted) mirror the existing `MergeIdentity` pattern. Validation runs at `StagingWriter.Commit()` for items; at config load for defaults.

**Tech Stack:** Go 1.26, standard library only. `go test ./...` and `go vet ./...`. No new dependencies.

**Target version:** v0.3.0 (minor, additive under 0.x semver).

**Spec:** `docs/specs/11-data-subject-and-audience-design.md` (v1.1).

**Tracking:** All tasks have beads issues. Root: `glovebox-m2b9` (spec) → `glovebox-o3sh, 4ahf, hcm2, 82kv, q8m0, u1sv, ibzt, 2rdq` (implementation).

---

## File Structure

| File | Status | Responsibility |
|------|--------|----------------|
| `internal/staging/audience.go` | **new** | Audience enum constants, `ValidateAudience()` pure function, cross-field rules |
| `internal/staging/audience_test.go` | **new** | Table-driven tests for every valid token, every rejected combination |
| `internal/staging/types.go` | modify | Add `DataSubject`, `Audience` fields to `ItemMetadata` |
| `internal/staging/metadata.go` | modify | Extend `Validate()` to call `ValidateAudience()` + validate `DataSubject` length/control-chars |
| `internal/staging/metadata_test.go` | modify | Cases for the new validation rules |
| `internal/audit/logger.go` | modify | Add `DataSubject`, `Audience` fields to `AuditEntry` |
| `internal/audit/logger_test.go` | modify | JSON roundtrip for new fields |
| `connector/rule.go` | modify | Add `DataSubject`, `Audience` to `Rule` and `MatchResult`; propagate in `RuleMatcher.Match()` |
| `connector/rule_test.go` | modify | Match result propagation tests |
| `connector/runner.go` | modify | Add `DataSubjectDefault`, `AudienceDefault` to `BaseConfig`; validate at load |
| `connector/runner_test.go` | modify | Config-load-time rejection of malformed defaults |
| `connector/staging.go` | modify | Add `DataSubject`, `Audience` to `ItemOptions`; implement merge in `buildMetadata()` |
| `connector/staging_test.go` | modify | Merge precedence tests (per-item > rule > config default > omitted) |
| `connector/integration_test.go` | modify | End-to-end: connector emits item → metadata.json has new fields → audit log captures them |
| `CHANGELOG.md` | modify | v0.3.0 entry |

Each file has **one clear responsibility**. Tasks are ordered so each produces a compilable, testable commit; dependencies are explicit (by beads ID) below.

---

## Dependency Graph

```
1 (o3sh: audience primitive) ────┐
                                 ├── 3 (hcm2: commit validation) ──┐
2 (4ahf: ItemMetadata fields) ───┤                                  │
                                 └── 7 (ibzt: AuditEntry) ──┐       │
                                                            │       │
4 (82kv: Rule extension) ──────────────────────────────────┼───────┤
                                                            │       │
1 ──→ 5 (q8m0: BaseConfig defaults) ───────────────────────┼───────┤
                                                            │       │
                                             6 (u1sv: merge)┴───────┤
                                                                    │
                                             8 (2rdq: integration)──┘
```

Ready-to-start in parallel at T=0: Tasks 1, 2, 4. After those land, Tasks 3, 5, 7 unblock. Task 6 gates on 2+3+4+5. Task 8 is final.

---

## Task 1: Audience Enum + Validator Primitive

**Beads:** `glovebox-o3sh`
**Depends on:** (none — foundation)
**Blocks:** 3, 5

**Files:**
- Create: `internal/staging/audience.go`
- Create: `internal/staging/audience_test.go`

### Step 1.1 -- Claim the bead

- [ ] Run: `bd update glovebox-o3sh --claim`

### Step 1.2 -- Write the failing tests

- [ ] Create `internal/staging/audience_test.go`:

```go
package staging

import (
	"strings"
	"testing"
)

func TestValidateAudience_ValidCombinations(t *testing.T) {
	cases := []struct {
		name           string
		audience       []string
		hasDataSubject bool
	}{
		{"subject-and-parents", []string{"subject", "parents"}, true},
		{"subject-and-parents-and-siblings", []string{"subject", "parents", "siblings"}, true},
		{"subject-only", []string{"subject"}, true},
		{"parents-only", []string{"parents"}, true},
		{"siblings-only", []string{"siblings"}, true},
		{"household-with-subject", []string{"household"}, true},
		{"household-without-subject", []string{"household"}, false},
		{"public-with-subject", []string{"public"}, true},
		{"public-without-subject", []string{"public"}, false},
		{"nil", nil, true},
		{"nil-without-subject", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAudience(tc.audience, tc.hasDataSubject); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

func TestValidateAudience_RejectedCombinations(t *testing.T) {
	cases := []struct {
		name           string
		audience       []string
		hasDataSubject bool
		wantSubstr     string
	}{
		{"unknown-token", []string{"grandparents"}, true, "unknown audience token"},
		{"empty-array", []string{}, true, "empty"},
		{"duplicates", []string{"subject", "subject"}, true, "duplicate"},
		{"too-many", make([]string, 17), true, "too many"},
		{"public-with-subject-token", []string{"public", "subject"}, true, "public must appear alone"},
		{"public-with-household", []string{"public", "household"}, true, "public must appear alone"},
		{"household-with-parents", []string{"household", "parents"}, true, "household must appear alone"},
		{"household-with-subject-token", []string{"household", "subject"}, true, "household must appear alone"},
		{"subject-token-without-data-subject", []string{"subject"}, false, "requires data_subject"},
		{"parents-without-data-subject", []string{"parents"}, false, "requires data_subject"},
		{"siblings-without-data-subject", []string{"siblings", "household"}, false, "requires data_subject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// for too-many, seed with valid tokens so only count triggers failure
			if tc.name == "too-many" {
				for i := range tc.audience {
					tc.audience[i] = "household"
				}
			}
			err := ValidateAudience(tc.audience, tc.hasDataSubject)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q did not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}
```

### Step 1.3 -- Run tests to confirm they fail

- [ ] Run: `go test ./internal/staging/ -run TestValidateAudience -v`
- [ ] Expected: compile error -- `undefined: ValidateAudience`

### Step 1.4 -- Write the minimal implementation

- [ ] Create `internal/staging/audience.go`:

```go
package staging

import "fmt"

// Audience role tokens per spec 11 §3.4.
const (
	AudienceSubject   = "subject"
	AudienceParents   = "parents"
	AudienceSiblings  = "siblings"
	AudienceHousehold = "household"
	AudiencePublic    = "public"
)

const maxAudienceEntries = 16

var validAudienceTokens = map[string]bool{
	AudienceSubject:   true,
	AudienceParents:   true,
	AudienceSiblings:  true,
	AudienceHousehold: true,
	AudiencePublic:    true,
}

// roleRelativeTokens are the tokens that require a data_subject to be meaningful.
var roleRelativeTokens = map[string]bool{
	AudienceSubject:  true,
	AudienceParents:  true,
	AudienceSiblings: true,
}

// ValidateAudience enforces the spec 11 §3.5 cross-field rules on an audience
// slice. A nil slice is treated as "not set" and returns nil. An empty but
// non-nil slice is rejected.
func ValidateAudience(audience []string, hasDataSubject bool) error {
	if audience == nil {
		return nil
	}
	if len(audience) == 0 {
		return fmt.Errorf("audience must be omitted entirely, not empty")
	}
	if len(audience) > maxAudienceEntries {
		return fmt.Errorf("audience has too many entries (max %d)", maxAudienceEntries)
	}

	seen := make(map[string]bool, len(audience))
	hasPublic := false
	hasHousehold := false
	hasRoleRelative := false

	for _, tok := range audience {
		if !validAudienceTokens[tok] {
			return fmt.Errorf("unknown audience token %q", tok)
		}
		if seen[tok] {
			return fmt.Errorf("duplicate audience token %q", tok)
		}
		seen[tok] = true
		switch tok {
		case AudiencePublic:
			hasPublic = true
		case AudienceHousehold:
			hasHousehold = true
		}
		if roleRelativeTokens[tok] {
			hasRoleRelative = true
		}
	}

	if hasPublic && len(audience) > 1 {
		return fmt.Errorf("public must appear alone in audience")
	}
	if hasHousehold && hasRoleRelative {
		return fmt.Errorf("household must appear alone; it already includes subject/parents/siblings")
	}
	if !hasDataSubject && hasRoleRelative {
		return fmt.Errorf("audience token requires data_subject to be set")
	}

	return nil
}
```

### Step 1.5 -- Run tests to confirm they pass

- [ ] Run: `go test ./internal/staging/ -run TestValidateAudience -v`
- [ ] Expected: all cases PASS

### Step 1.6 -- Vet

- [ ] Run: `go vet ./internal/staging/...`
- [ ] Expected: no output

### Step 1.7 -- Commit

- [ ] Run:

```bash
git add internal/staging/audience.go internal/staging/audience_test.go
git commit -m "$(cat <<'EOF'
staging: audience token enum + ValidateAudience primitive (glovebox-o3sh)

Pure-function validator for the audience role-token set defined in spec 11
§3.4. Enforces cross-field rules from §3.5: unknown-token rejection,
duplicate rejection, 16-entry cap, public-must-be-alone, household-must-
be-alone, role-tokens-require-data_subject.

Foundation for commit-time validation (glovebox-hcm2) and config-load
validation (glovebox-q8m0).
EOF
)"
```

### Step 1.8 -- Close bead

- [ ] Run: `bd close glovebox-o3sh`

**Exit criteria:** new file committed, all 21 test cases pass, `go vet` clean.

---

## Task 2: ItemMetadata DataSubject + Audience Fields

**Beads:** `glovebox-4ahf`
**Depends on:** (none — can run parallel with Tasks 1 and 4)
**Blocks:** 3, 6, 7

**Files:**
- Modify: `internal/staging/types.go` (existing lines 19-30)
- Modify: `internal/staging/types_test.go` (or create if absent)

### Step 2.1 -- Claim the bead

- [ ] Run: `bd update glovebox-4ahf --claim`

### Step 2.2 -- Write the failing test

- [ ] Check if `internal/staging/types_test.go` exists: `ls internal/staging/types_test.go`
- [ ] If it doesn't exist, create with package header; otherwise append.
- [ ] Add:

```go
func TestItemMetadata_DataSubjectAndAudienceRoundtrip(t *testing.T) {
	original := ItemMetadata{
		Source:           "schoology",
		Sender:           "Mr. Rodriguez",
		Subject:          "Math quiz retakes",
		Timestamp:        time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC),
		DestinationAgent: "school",
		ContentType:      "text/plain",
		DataSubject:      "bee",
		Audience:         []string{"subject", "parents"},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"data_subject":"bee"`) {
		t.Errorf("marshaled JSON missing data_subject: %s", data)
	}
	if !strings.Contains(string(data), `"audience":["subject","parents"]`) {
		t.Errorf("marshaled JSON missing audience: %s", data)
	}

	var roundtripped ItemMetadata
	if err := json.Unmarshal(data, &roundtripped); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if roundtripped.DataSubject != "bee" {
		t.Errorf("data_subject lost in roundtrip: got %q", roundtripped.DataSubject)
	}
	if len(roundtripped.Audience) != 2 || roundtripped.Audience[0] != "subject" || roundtripped.Audience[1] != "parents" {
		t.Errorf("audience lost in roundtrip: got %v", roundtripped.Audience)
	}
}

func TestItemMetadata_DataSubjectAndAudienceOmitempty(t *testing.T) {
	m := ItemMetadata{
		Source:           "rss",
		Sender:           "feed",
		Subject:          "title",
		Timestamp:        time.Unix(0, 0).UTC(),
		DestinationAgent: "general",
		ContentType:      "text/plain",
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "data_subject") {
		t.Errorf("expected data_subject to be omitted: %s", data)
	}
	if strings.Contains(string(data), "audience") {
		t.Errorf("expected audience to be omitted: %s", data)
	}
}
```

- [ ] If creating the file fresh, add imports:
```go
package staging

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)
```

### Step 2.3 -- Run tests to confirm failure

- [ ] Run: `go test ./internal/staging/ -run TestItemMetadata_DataSubject -v`
- [ ] Expected: compile errors -- `unknown field DataSubject` and `unknown field Audience` in struct literal

### Step 2.4 -- Add fields to ItemMetadata

- [ ] Edit `internal/staging/types.go`, extend `ItemMetadata` struct:

```go
type ItemMetadata struct {
	Source           string            `json:"source"`
	Sender           string            `json:"sender"`
	Subject          string            `json:"subject"`
	Timestamp        time.Time         `json:"timestamp"`
	DestinationAgent string            `json:"destination_agent"`
	ContentType      string            `json:"content_type"`
	Ordered          bool              `json:"ordered"`
	AuthFailure      bool              `json:"auth_failure"`
	Identity         *ItemIdentity     `json:"identity,omitempty"`
	Tags             map[string]string `json:"tags,omitempty"`
	DataSubject      string            `json:"data_subject,omitempty"`
	Audience         []string          `json:"audience,omitempty"`
}
```

### Step 2.5 -- Run tests to confirm pass

- [ ] Run: `go test ./internal/staging/ -run TestItemMetadata_DataSubject -v`
- [ ] Expected: both PASS

### Step 2.6 -- Run full package tests and vet

- [ ] Run: `go test ./internal/staging/...`
- [ ] Expected: all PASS (no regressions)
- [ ] Run: `go vet ./internal/staging/...`
- [ ] Expected: no output

### Step 2.7 -- Commit

- [ ] Run:

```bash
git add internal/staging/types.go internal/staging/types_test.go
git commit -m "$(cat <<'EOF'
staging: add DataSubject and Audience fields to ItemMetadata (glovebox-4ahf)

Additive schema extension per spec 11 §3.3 and §3.4. JSON tags use
omitempty so existing connectors continue to emit unchanged metadata.json
files.

Prerequisite for commit-time validation (glovebox-hcm2), audit-log
extension (glovebox-ibzt), and the merge pipeline in staging.go
(glovebox-u1sv).
EOF
)"
```

### Step 2.8 -- Close bead

- [ ] Run: `bd close glovebox-4ahf`

**Exit criteria:** two new tests pass, all pre-existing `internal/staging/` tests pass, `go vet` clean.

---

## Task 3: Commit-Time Validation

**Beads:** `glovebox-hcm2`
**Depends on:** `glovebox-o3sh`, `glovebox-4ahf`
**Blocks:** 6

**Files:**
- Modify: `internal/staging/metadata.go` (Validate function at line 56)
- Modify: `internal/staging/metadata_test.go`

### Step 3.1 -- Claim the bead

- [ ] Run: `bd update glovebox-hcm2 --claim`

### Step 3.2 -- Write the failing tests

- [ ] Append to `internal/staging/metadata_test.go`:

```go
func TestValidate_DataSubjectLength(t *testing.T) {
	m := validBaseMetadata()
	m.DataSubject = strings.Repeat("a", 257)
	if err := Validate(&m); err == nil {
		t.Fatal("expected error for data_subject > 256 chars")
	} else if !strings.Contains(err.Error(), "data_subject") {
		t.Errorf("error should mention data_subject: %v", err)
	}
}

func TestValidate_DataSubjectControlChars(t *testing.T) {
	m := validBaseMetadata()
	m.DataSubject = "bee\x00charlie"
	if err := Validate(&m); err == nil {
		t.Fatal("expected error for control chars in data_subject")
	}
}

func TestValidate_DataSubjectEmptyStringRejected(t *testing.T) {
	m := validBaseMetadata()
	m.DataSubject = ""
	m.Audience = nil // ensure we aren't validating via audience
	// empty string should be treated as "omitted" and skip validation
	if err := Validate(&m); err != nil {
		t.Errorf("empty data_subject should be treated as omitted, got: %v", err)
	}
}

func TestValidate_AudienceValid(t *testing.T) {
	cases := [][]string{
		{"subject", "parents"},
		{"household"},
		{"public"},
	}
	for _, aud := range cases {
		m := validBaseMetadata()
		m.DataSubject = "bee"
		m.Audience = aud
		if err := Validate(&m); err != nil {
			t.Errorf("audience %v should validate, got: %v", aud, err)
		}
	}
}

func TestValidate_AudienceInvalid(t *testing.T) {
	cases := []struct {
		name        string
		dataSubject string
		audience    []string
		wantSubstr  string
	}{
		{"unknown-token", "bee", []string{"grandparents"}, "unknown audience"},
		{"public-not-alone", "bee", []string{"public", "subject"}, "public must appear alone"},
		{"household-not-alone", "bee", []string{"household", "parents"}, "household must appear alone"},
		{"role-token-without-subject", "", []string{"subject"}, "requires data_subject"},
		{"empty-array", "bee", []string{}, "audience must be omitted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validBaseMetadata()
			m.DataSubject = tc.dataSubject
			m.Audience = tc.audience
			err := Validate(&m)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}
```

- [ ] Check if `validBaseMetadata()` helper already exists in the test file. If not, add at the end:

```go
func validBaseMetadata() ItemMetadata {
	return ItemMetadata{
		Source:           "test",
		Sender:           "test-sender",
		Subject:          "subject line",
		Timestamp:        time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC),
		DestinationAgent: "test-agent",
		ContentType:      "text/plain",
	}
}
```

(If `validBaseMetadata` or a close equivalent already exists, reuse it instead.)

### Step 3.3 -- Run tests to confirm failure

- [ ] Run: `go test ./internal/staging/ -run "TestValidate_(DataSubject|Audience)" -v`
- [ ] Expected: tests FAIL (validation accepts invalid input or compiles without reaching the new checks)

### Step 3.4 -- Extend Validate()

- [ ] In `internal/staging/metadata.go`, inside `Validate()`, after the existing `validateTags()` call (near line 106+), add:

```go
	if err := validateDataSubject(m.DataSubject); err != nil {
		return err
	}
	if err := ValidateAudience(m.Audience, m.DataSubject != ""); err != nil {
		return fmt.Errorf("audience: %w", err)
	}
```

- [ ] Add helper at bottom of `metadata.go`:

```go
func validateDataSubject(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > 256 {
		return fmt.Errorf("data_subject exceeds 256 characters")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("data_subject contains control characters")
		}
	}
	return nil
}
```

### Step 3.5 -- Run tests to confirm pass

- [ ] Run: `go test ./internal/staging/ -run "TestValidate_(DataSubject|Audience)" -v`
- [ ] Expected: all PASS

### Step 3.6 -- Run full package + vet

- [ ] Run: `go test ./internal/staging/...`
- [ ] Run: `go vet ./internal/staging/...`
- [ ] Expected: all green, no regressions

### Step 3.7 -- Commit

- [ ] Run:

```bash
git add internal/staging/metadata.go internal/staging/metadata_test.go
git commit -m "$(cat <<'EOF'
staging: validate DataSubject length + Audience enum at commit time (glovebox-hcm2)

Extends Validate() with the rules from spec 11 §6: data_subject ≤256
chars, no control characters; audience via ValidateAudience() primitive
including cross-field rules (empty subject + role token, public alone,
household alone, empty array).

Follows the existing validateIdentity/validateTags pattern.
EOF
)"
```

### Step 3.8 -- Close bead

- [ ] Run: `bd close glovebox-hcm2`

**Exit criteria:** all new validation tests pass, existing staging tests still pass, `go vet` clean.

---

## Task 4: Rule + MatchResult + RuleMatcher.Match() Extension

**Beads:** `glovebox-82kv`
**Depends on:** (none — can run parallel)
**Blocks:** 6

**Files:**
- Modify: `connector/rule.go` (lines 24-34 for structs; ~line 51 for Match method)
- Modify: `connector/rule_test.go`

### Step 4.1 -- Claim the bead

- [ ] Run: `bd update glovebox-82kv --claim`

### Step 4.2 -- Write failing tests

- [ ] Append to `connector/rule_test.go`:

```go
func TestRuleMatcher_PropagatesDataSubjectAndAudience(t *testing.T) {
	rules := []Rule{
		{
			Match:       "foo",
			Destination: "agent-a",
			DataSubject: "bee",
			Audience:    []string{"subject", "parents"},
		},
		{
			Match:       "*",
			Destination: "agent-b",
			Audience:    []string{"household"},
		},
	}
	rm := NewRuleMatcher(rules)

	got, ok := rm.Match("foo")
	if !ok {
		t.Fatal("expected match")
	}
	if got.DataSubject != "bee" {
		t.Errorf("DataSubject: got %q, want %q", got.DataSubject, "bee")
	}
	if len(got.Audience) != 2 || got.Audience[0] != "subject" || got.Audience[1] != "parents" {
		t.Errorf("Audience: got %v", got.Audience)
	}

	got, ok = rm.Match("anything-else")
	if !ok {
		t.Fatal("expected wildcard match")
	}
	if got.DataSubject != "" {
		t.Errorf("expected empty DataSubject on wildcard rule, got %q", got.DataSubject)
	}
	if len(got.Audience) != 1 || got.Audience[0] != "household" {
		t.Errorf("Audience: got %v", got.Audience)
	}
}

func TestRuleMatcher_AudienceSliceIsCopied(t *testing.T) {
	// Defensive: mutating the returned slice must not affect the rule.
	rules := []Rule{
		{Match: "*", Destination: "a", Audience: []string{"household"}},
	}
	rm := NewRuleMatcher(rules)
	got, _ := rm.Match("x")
	got.Audience[0] = "public"

	got2, _ := rm.Match("x")
	if got2.Audience[0] != "household" {
		t.Errorf("rule audience was mutated: got %q", got2.Audience[0])
	}
}
```

### Step 4.3 -- Run tests to confirm failure

- [ ] Run: `go test ./connector/ -run TestRuleMatcher_ -v`
- [ ] Expected: compile error -- unknown fields `DataSubject`, `Audience` on `Rule`

### Step 4.4 -- Extend structs and propagation

- [ ] In `connector/rule.go`, extend both structs:

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

- [ ] In `RuleMatcher.Match()` (line ~51), extend the successful-match return to copy the new fields. If the existing code builds `MatchResult` literally, add both fields. For the `Audience` slice specifically, **copy** (do not share pointer) to avoid aliasing:

```go
	// (existing destination + tags population)
	result := MatchResult{
		Destination: rule.Destination,
		Tags:        copyStringMap(rule.Tags),   // or whatever the existing helper is
		DataSubject: rule.DataSubject,
	}
	if rule.Audience != nil {
		result.Audience = append([]string(nil), rule.Audience...)
	}
	return result, true
```

(If the existing code uses inline construction rather than a helper, adapt accordingly. The important invariants are: `MatchResult.Audience` is a **fresh copy** of `rule.Audience`, and `MatchResult.DataSubject` is `rule.DataSubject`.)

### Step 4.5 -- Run tests to confirm pass

- [ ] Run: `go test ./connector/ -run TestRuleMatcher_ -v`
- [ ] Expected: both new tests PASS

### Step 4.6 -- Run full connector tests + vet

- [ ] Run: `go test ./connector/...`
- [ ] Run: `go vet ./connector/...`
- [ ] Expected: all green

### Step 4.7 -- Commit

- [ ] Run:

```bash
git add connector/rule.go connector/rule_test.go
git commit -m "$(cat <<'EOF'
connector: propagate DataSubject + Audience through rule matching (glovebox-82kv)

Extends Rule and MatchResult structs per spec 11 §4.2. RuleMatcher.Match()
now carries data_subject and audience from the first matching rule into
MatchResult, with a defensive slice copy to prevent aliasing into the
rule's configured audience.
EOF
)"
```

### Step 4.8 -- Close bead

- [ ] Run: `bd close glovebox-82kv`

**Exit criteria:** both new tests pass, full connector tests still pass, `go vet` clean.

---

## Task 5: BaseConfig Defaults + Config-Load Validation

**Beads:** `glovebox-q8m0`
**Depends on:** `glovebox-o3sh`
**Blocks:** 6

**Files:**
- Modify: `connector/runner.go` (BaseConfig at lines 19-24; config load path)
- Modify: `connector/runner_test.go` (or the existing runner-config test file)

### Step 5.1 -- Claim the bead

- [ ] Run: `bd update glovebox-q8m0 --claim`

### Step 5.2 -- Locate the config-load path

- [ ] Run: `grep -n "BaseConfig" connector/*.go | head -20` and `grep -n "json.Unmarshal" connector/runner*.go` to find where BaseConfig is populated from the config file at startup. If the unmarshal happens in `runner.go`, the load-validation call goes there. If it happens later in `Setup`, the validation goes there.

### Step 5.3 -- Write failing tests

- [ ] Append to the appropriate test file (likely `connector/runner_test.go`):

```go
func TestBaseConfig_AudienceDefaultRejectedOnLoad(t *testing.T) {
	cases := []struct {
		name       string
		json       string
		wantSubstr string
	}{
		{
			"unknown-token",
			`{"rules":[{"match":"*","destination":"a"}],"audience_default":["grandparents"]}`,
			"unknown audience token",
		},
		{
			"public-with-others",
			`{"rules":[{"match":"*","destination":"a"}],"audience_default":["public","subject"]}`,
			"public must appear alone",
		},
		{
			"empty-array",
			`{"rules":[{"match":"*","destination":"a"}],"audience_default":[]}`,
			"audience must be omitted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg BaseConfig
			if err := json.Unmarshal([]byte(tc.json), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			err := ValidateBaseConfig(&cfg) // introduce this exported helper
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestBaseConfig_DataSubjectDefaultLengthRejected(t *testing.T) {
	cfg := BaseConfig{
		Rules:              []Rule{{Match: "*", Destination: "a"}},
		DataSubjectDefault: strings.Repeat("x", 300),
	}
	if err := ValidateBaseConfig(&cfg); err == nil {
		t.Fatal("expected error for oversized data_subject_default")
	}
}

func TestBaseConfig_GoodDefaultsAccepted(t *testing.T) {
	cfg := BaseConfig{
		Rules:              []Rule{{Match: "*", Destination: "a"}},
		DataSubjectDefault: "bee",
		AudienceDefault:    []string{"subject", "parents"},
	}
	if err := ValidateBaseConfig(&cfg); err != nil {
		t.Errorf("valid defaults should pass, got %v", err)
	}
}
```

### Step 5.4 -- Run tests to confirm failure

- [ ] Run: `go test ./connector/ -run "TestBaseConfig_" -v`
- [ ] Expected: compile error -- `DataSubjectDefault`, `AudienceDefault` unknown; `ValidateBaseConfig` unknown.

### Step 5.5 -- Extend BaseConfig and add validator

- [ ] In `connector/runner.go`, extend `BaseConfig`:

```go
type BaseConfig struct {
	Rules              []Rule          `json:"rules"`
	Routes             []Rule          `json:"routes"`
	ConfigIdentity     *ConfigIdentity `json:"identity,omitempty"`
	FetchLimits        FetchLimits     `json:"fetch_limits"`
	DataSubjectDefault string          `json:"data_subject_default,omitempty"`
	AudienceDefault    []string        `json:"audience_default,omitempty"`
}
```

- [ ] Add to `connector/runner.go` (or a new `connector/baseconfig_validate.go` if runner is getting long):

```go
import "github.com/leftathome/glovebox/internal/staging"

// ValidateBaseConfig enforces spec 11 §5.1 startup-time rules on the
// data-subject and audience defaults. Called from the config-load path
// before the runner starts.
func ValidateBaseConfig(c *BaseConfig) error {
	if len(c.DataSubjectDefault) > 256 {
		return fmt.Errorf("data_subject_default exceeds 256 characters")
	}
	for _, r := range c.DataSubjectDefault {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("data_subject_default contains control characters")
		}
	}
	hasSubject := c.DataSubjectDefault != ""
	if err := staging.ValidateAudience(c.AudienceDefault, hasSubject); err != nil {
		return fmt.Errorf("audience_default: %w", err)
	}
	return nil
}
```

- [ ] In the existing config-load path (wherever `BaseConfig` is unmarshaled at startup -- `Run()` or a helper), add a call to `ValidateBaseConfig(&cfg)` immediately after unmarshal and before any connector work starts. If it fails, return it as a permanent error from `Run()`.

### Step 5.6 -- Run tests to confirm pass

- [ ] Run: `go test ./connector/ -run "TestBaseConfig_" -v`
- [ ] Expected: all PASS

### Step 5.7 -- Run full connector tests + vet

- [ ] Run: `go test ./connector/...`
- [ ] Run: `go vet ./connector/...`

### Step 5.8 -- Commit

- [ ] Run:

```bash
git add connector/runner.go connector/runner_test.go
git commit -m "$(cat <<'EOF'
connector: BaseConfig DataSubjectDefault + AudienceDefault with load validation (glovebox-q8m0)

Extends BaseConfig per spec 11 §4.1. ValidateBaseConfig enforces the same
rules as item-time validation (length + control-chars on data_subject;
enum + cross-field rules on audience) at startup, so malformed defaults
fail fast rather than at first item commit.
EOF
)"
```

### Step 5.9 -- Close bead

- [ ] Run: `bd close glovebox-q8m0`

**Exit criteria:** all three new tests pass, existing connector tests still pass, `go vet` clean, malformed defaults cause `Run()` to return a permanent error at startup.

---

## Task 6: ItemOptions + Merge Logic in staging.go

**Beads:** `glovebox-u1sv`
**Depends on:** `glovebox-4ahf`, `glovebox-hcm2`, `glovebox-82kv`, `glovebox-q8m0`
**Blocks:** 8

**Files:**
- Modify: `connector/staging.go` (ItemOptions lines 15-27; `buildMetadata()` line 94)
- Modify: `connector/staging_test.go`

### Step 6.1 -- Claim the bead

- [ ] Run: `bd update glovebox-u1sv --claim`

### Step 6.2 -- Write failing tests

- [ ] Append to `connector/staging_test.go`:

```go
func TestBuildMetadata_DataSubjectMergePrecedence(t *testing.T) {
	cases := []struct {
		name        string
		configDef   string
		ruleVal     string
		itemVal     string
		want        string
	}{
		{"per-item-wins", "config", "rule", "item", "item"},
		{"rule-wins-when-no-item", "config", "rule", "", "rule"},
		{"config-wins-when-no-item-no-rule", "config", "", "", "config"},
		{"empty-when-nothing-set", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newTestStagingWriter(t)
			w.SetConfigDataSubject(tc.configDef)
			opts := ItemOptions{
				Source: "test", Sender: "s", Subject: "x",
				Timestamp:        time.Now().UTC(),
				DestinationAgent: "a",
				ContentType:      "text/plain",
				DataSubject:      tc.itemVal,
			}
			item, _ := w.NewItem(opts)
			// inject rule-match result
			item.ruleDataSubject = tc.ruleVal
			_ = item.WriteContent([]byte("x"))
			if err := item.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			got := readStagedDataSubject(t, w.StagingDir(), item.ID())
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildMetadata_AudienceMergePrecedence(t *testing.T) {
	// Analogous to DataSubject. Tests same precedence for audience slice.
	// ... (mirror the above with audience values)
}
```

Note: the exact helper names (`SetConfigDataSubject`, `ruleDataSubject`, `readStagedDataSubject`) are suggestive; adapt to the existing test helpers in `staging_test.go`. The specific behavior to test is: for each of `{DataSubject, Audience}`, verify that a non-empty per-item value overrides rule, which overrides config default; and that an empty value at a layer falls through to the next.

### Step 6.3 -- Run tests to confirm failure

- [ ] Run: `go test ./connector/ -run "TestBuildMetadata_(DataSubject|Audience)Merge" -v`
- [ ] Expected: FAIL -- merge logic not implemented; fields on ItemOptions may not exist yet.

### Step 6.4 -- Extend ItemOptions

- [ ] In `connector/staging.go`, extend:

```go
type ItemOptions struct {
	Source           string
	Sender           string
	Subject          string
	Timestamp        time.Time
	DestinationAgent string
	ContentType      string
	Ordered          bool
	AuthFailure      bool
	Identity         *Identity
	Tags             map[string]string
	RuleTags         map[string]string
	DataSubject      string
	Audience         []string
	RuleDataSubject  string
	RuleAudience     []string
}
```

`RuleDataSubject` and `RuleAudience` are populated by the connector from
`MatchResult` and ferried through `ItemOptions` the same way `RuleTags` is
today (see `staging.go` line 162 `mergeTags`).

### Step 6.5 -- Implement merge in buildMetadata()

- [ ] In `connector/staging.go`, extend `StagingItem.buildMetadata()` (around line 94):

```go
	// After existing identity + tag merge, before validation:
	md.DataSubject = mergeDataSubject(si.writer.configDataSubjectDefault, si.opts.RuleDataSubject, si.opts.DataSubject)
	md.Audience = mergeAudience(si.writer.configAudienceDefault, si.opts.RuleAudience, si.opts.Audience)
```

- [ ] Add helpers at the bottom of `staging.go`:

```go
// mergeDataSubject applies per-item > rule > config default > empty precedence.
func mergeDataSubject(configDefault, rule, item string) string {
	if item != "" {
		return item
	}
	if rule != "" {
		return rule
	}
	return configDefault
}

// mergeAudience applies per-item > rule > config default > nil precedence.
// Returns a fresh slice to prevent aliasing into the caller's storage.
func mergeAudience(configDefault, rule, item []string) []string {
	switch {
	case len(item) > 0:
		return append([]string(nil), item...)
	case len(rule) > 0:
		return append([]string(nil), rule...)
	case len(configDefault) > 0:
		return append([]string(nil), configDefault...)
	}
	return nil
}
```

- [ ] Add setters on `StagingWriter` to receive the config defaults from the runner:

```go
func (w *StagingWriter) SetConfigDataSubject(s string)      { w.configDataSubjectDefault = s }
func (w *StagingWriter) SetConfigAudience(a []string)       { w.configAudienceDefault = append([]string(nil), a...) }
```

And declare the fields on `StagingWriter` alongside the existing identity-default field.

- [ ] In `connector/runner.go` (the path that calls `NewStagingWriter` and wires up defaults), add:

```go
	writer.SetConfigDataSubject(cfg.DataSubjectDefault)
	writer.SetConfigAudience(cfg.AudienceDefault)
```

### Step 6.6 -- Run tests to confirm pass

- [ ] Run: `go test ./connector/ -run "TestBuildMetadata_(DataSubject|Audience)Merge" -v`
- [ ] Expected: all PASS

### Step 6.7 -- Run full connector tests + vet

- [ ] Run: `go test ./...`
- [ ] Run: `go vet ./...`
- [ ] Expected: all green (no regressions across any package)

### Step 6.8 -- Commit

- [ ] Run:

```bash
git add connector/staging.go connector/staging_test.go connector/runner.go
git commit -m "$(cat <<'EOF'
connector: merge DataSubject + Audience per-item > rule > config (glovebox-u1sv)

Implements spec 11 §5 precedence semantics. ItemOptions gains DataSubject,
Audience, RuleDataSubject, RuleAudience fields mirroring the existing tags
plumbing. buildMetadata() resolves the final values before validation.
Slice returns are fresh copies to prevent aliasing.
EOF
)"
```

### Step 6.9 -- Close bead

- [ ] Run: `bd close glovebox-u1sv`

**Exit criteria:** merge tests pass for both fields across all four precedence cases; `go test ./...` and `go vet ./...` all green.

---

## Task 7: AuditEntry Extension

**Beads:** `glovebox-ibzt`
**Depends on:** `glovebox-4ahf`
**Blocks:** 8

**Files:**
- Modify: `internal/audit/logger.go` (AuditEntry lines 14-27)
- Modify: `internal/audit/logger_test.go`

### Step 7.1 -- Claim the bead

- [ ] Run: `bd update glovebox-ibzt --claim`

### Step 7.2 -- Write failing test

- [ ] Append to `internal/audit/logger_test.go`:

```go
func TestAuditEntry_DataSubjectAndAudienceRoundtrip(t *testing.T) {
	e := AuditEntry{
		Timestamp:     "2026-04-22T00:00:00Z",
		Source:        "schoology",
		Sender:        "Mr. Rodriguez",
		ContentHash:   "abc",
		ContentLength: 10,
		Verdict:       "pass",
		Destination:   "school",
		DataSubject:   "bee",
		Audience:      []string{"subject", "parents"},
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"data_subject":"bee"`) {
		t.Errorf("missing data_subject in JSON: %s", data)
	}
	if !strings.Contains(string(data), `"audience":["subject","parents"]`) {
		t.Errorf("missing audience in JSON: %s", data)
	}
}

func TestAuditEntry_OmitEmptyForNewFields(t *testing.T) {
	e := AuditEntry{Timestamp: "t", Source: "s", Verdict: "pass", Destination: "d"}
	data, _ := json.Marshal(e)
	if strings.Contains(string(data), "data_subject") {
		t.Errorf("expected data_subject omitted: %s", data)
	}
	if strings.Contains(string(data), "audience") {
		t.Errorf("expected audience omitted: %s", data)
	}
}
```

### Step 7.3 -- Run tests to confirm failure

- [ ] Run: `go test ./internal/audit/ -run TestAuditEntry_ -v`
- [ ] Expected: compile error -- unknown fields `DataSubject`, `Audience`.

### Step 7.4 -- Extend AuditEntry

- [ ] In `internal/audit/logger.go`:

```go
type AuditEntry struct {
	Timestamp      string                `json:"timestamp"`
	Source         string                `json:"source"`
	Sender         string                `json:"sender"`
	ContentHash    string                `json:"content_hash"`
	ContentLength  int64                 `json:"content_length"`
	Signals        []engine.Signal       `json:"signals"`
	TotalScore     float64               `json:"total_score"`
	Verdict        string                `json:"verdict"`
	Destination    string                `json:"destination"`
	ScanDurationMs int64                 `json:"scan_duration_ms"`
	Identity       *staging.ItemIdentity `json:"identity,omitempty"`
	Tags           map[string]string     `json:"tags,omitempty"`
	DataSubject    string                `json:"data_subject,omitempty"`
	Audience       []string              `json:"audience,omitempty"`
}
```

- [ ] Find the caller(s) that populate `AuditEntry` from `ItemMetadata`. Grep: `grep -rn "AuditEntry{" internal/`. Extend the population site(s) to copy `m.DataSubject` and `m.Audience` through.

### Step 7.5 -- Run tests to confirm pass

- [ ] Run: `go test ./internal/audit/ -run TestAuditEntry_ -v`
- [ ] Run: `go test ./internal/audit/...`
- [ ] Run: `go vet ./internal/audit/...`

### Step 7.6 -- Commit

- [ ] Run:

```bash
git add internal/audit/logger.go internal/audit/logger_test.go
# plus any other files you touched to propagate DataSubject/Audience into AuditEntry
git commit -m "$(cat <<'EOF'
audit: record DataSubject + Audience on every item (glovebox-ibzt)

Extends AuditEntry per spec 11 §7 for forensic traceability of declared
visibility intent. omitempty preserves existing audit-log shape for
subject-less / audience-less items.
EOF
)"
```

### Step 7.7 -- Close bead

- [ ] Run: `bd close glovebox-ibzt`

**Exit criteria:** roundtrip + omitempty tests pass; audit-log writer populates the new fields from `ItemMetadata`; full test suite clean.

---

## Task 8: Integration Test + CHANGELOG + Regression Verification

**Beads:** `glovebox-2rdq`
**Depends on:** `glovebox-u1sv`, `glovebox-ibzt`
**Blocks:** (none — final task)

**Files:**
- Modify: `connector/integration_test.go` (existing end-to-end harness)
- Modify: `CHANGELOG.md`

### Step 8.1 -- Claim the bead

- [ ] Run: `bd update glovebox-2rdq --claim`

### Step 8.2 -- Write end-to-end test

- [ ] Append to `connector/integration_test.go`:

```go
func TestIntegration_DataSubjectAudienceEndToEnd(t *testing.T) {
	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := NewStagingWriter(stagingDir, "schoology")
	if err != nil {
		t.Fatal(err)
	}

	rules := []Rule{
		{
			Match:       "schoology:bee:grade",
			Destination: "school",
			DataSubject: "bee",
			Audience:    []string{"subject", "parents"},
		},
		{
			Match:       "schoology:*:flyer",
			Destination: "school",
			Audience:    []string{"public"},
		},
	}
	matcher := NewRuleMatcher(rules)

	// Item 1: Bee's grade
	result, _ := matcher.Match("schoology:bee:grade")
	item, err := writer.NewItem(ItemOptions{
		Source:           "schoology",
		Sender:           "Mr. Rodriguez",
		Subject:          "Math grade posted",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: result.Destination,
		ContentType:      "text/plain",
		RuleDataSubject:  result.DataSubject,
		RuleAudience:     result.Audience,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("87%")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	// Item 2: flyer (subjectless, public)
	result2, _ := matcher.Match("schoology:bee:flyer")
	item2, err := writer.NewItem(ItemOptions{
		Source:           "schoology",
		Sender:           "School",
		Subject:          "Spring carnival",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: result2.Destination,
		ContentType:      "text/plain",
		RuleDataSubject:  result2.DataSubject,
		RuleAudience:     result2.Audience,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = item2.WriteContent([]byte("flyer body"))
	if err := item2.Commit(); err != nil {
		t.Fatal(err)
	}

	// Assertions: read metadata.json files and verify shape.
	entries, _ := os.ReadDir(stagingDir)
	foundGrade, foundFlyer := false, false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta := readMetadata(t, stagingDir, e.Name())
		switch meta.DataSubject {
		case "bee":
			foundGrade = true
			if len(meta.Audience) != 2 {
				t.Errorf("bee's grade audience: got %v", meta.Audience)
			}
		case "":
			foundFlyer = true
			if len(meta.Audience) != 1 || meta.Audience[0] != "public" {
				t.Errorf("flyer audience: got %v", meta.Audience)
			}
		}
	}
	if !foundGrade {
		t.Error("did not find bee's grade in staging output")
	}
	if !foundFlyer {
		t.Error("did not find flyer in staging output")
	}

	_ = stateDir // ensure state-dir plumbing still works (exercised elsewhere)
}
```

(If `readMetadata` is not an existing helper in the integration test file, add a small one that opens `<stagingDir>/<id>/metadata.json` and unmarshals into `staging.ItemMetadata`.)

### Step 8.3 -- Run the integration test

- [ ] Run: `go test ./connector/ -run TestIntegration_DataSubjectAudienceEndToEnd -v`
- [ ] Expected: PASS

### Step 8.4 -- Full regression suite

- [ ] Run: `go test ./...`
- [ ] Expected: all PASS (all existing connectors still build and test clean with additive schema).
- [ ] Run: `go vet ./...`
- [ ] Expected: no output.

### Step 8.5 -- Update CHANGELOG

- [ ] Edit `CHANGELOG.md`, add at the top under `## Unreleased` (or create a new `## v0.3.0` section):

```markdown
## v0.3.0 -- 2026-04-22

### Added
- `data_subject` (string) and `audience` ([]string enum) fields on
  `metadata.json`, `ItemOptions`, `Rule`, `MatchResult`, `BaseConfig`
  defaults, and `AuditEntry`. See `docs/specs/11-data-subject-and-audience-design.md`.
- Commit-time validation of `data_subject` length/control-chars and
  `audience` enum + cross-field rules (`public` alone, `household` alone,
  role tokens require `data_subject`).
- Config-load-time validation of `data_subject_default` and
  `audience_default`: malformed defaults fail startup, not first-item
  commit.

### Notes
- Purely additive schema extension. Existing connectors produce
  byte-identical `metadata.json` files.
- V1 is metadata-only: Glovebox validates and stamps these fields but
  does not filter or route on them.
```

### Step 8.6 -- Commit

- [ ] Run:

```bash
git add connector/integration_test.go CHANGELOG.md
git commit -m "$(cat <<'EOF'
spec 11: end-to-end integration test + v0.3.0 CHANGELOG (glovebox-2rdq)

Exercises the full spec-11 path: rule → match → staging → metadata.json →
audit log for both a data-subject-bearing item (Bee's grade, audience
[subject, parents]) and a subjectless item (flyer, audience [public]).

Full go test ./... and go vet ./... clean against all existing
connectors -- additive schema extension causes no regressions.
EOF
)"
```

### Step 8.7 -- Close bead

- [ ] Run: `bd close glovebox-2rdq`

**Exit criteria:** integration test passes; `go test ./...` and `go vet ./...` both green across the entire repository; CHANGELOG entry committed.

---

## Final Verification (After Task 8)

- [ ] Run: `git log --oneline origin/main..HEAD` -- confirm 8 implementation commits (plus any spec cleanup commits already present).
- [ ] Run: `bd list --status=open | grep "spec 11 impl"` -- should return empty; all eight task beads closed.
- [ ] Run: `bd show glovebox-m2b9` -- the parent spec bead; close it with `bd close glovebox-m2b9 --reason="spec implemented via glovebox-{o3sh,4ahf,hcm2,82kv,q8m0,u1sv,ibzt,2rdq}"`.
- [ ] Push to origin: `git push`.
- [ ] After CI is green on main, tag: `git tag -a v0.3.0 -m "Spec 11: data_subject and audience metadata"` then `git push --tags`.
  - Per the release-workflow memory: **do not tag until CI green**; the v0.2.2 incident is the reason.

---

## Notes and Caveats

- Tests are written in the **table-driven + subtests** style already used in `connector/identity_test.go` and `internal/staging/metadata_test.go`. Don't invent a new style.
- Slice copies matter. Several tasks explicitly copy audience slices to prevent aliasing bugs. Treat `append([]string(nil), src...)` as the canonical idiom for "defensive copy of a string slice."
- `staticcheck` is not in CI. Stick to `go vet`; don't add new tooling.
- Per CLAUDE.md: if a container is involved, test in a container. The connector tests are all unit/in-process; no containers needed for this plan. Schoology/PowerSchool connector plans (future) may need container tests.
- **Never skip hooks** (`--no-verify`) on these commits. If a hook fires, investigate and fix.

---

## Out of Scope for This Plan

Per spec 11 §2.2, the following are deliberately deferred and MUST NOT be added here:
- Audience-aware rule matching / routing.
- Enforcement gates (quarantine on audience mismatch).
- Named audience shorthands (`"student-private"`).
- Multi-subject items (`data_subject` as array).
- Extended-family tokens.
- Cross-connector subject reconciliation.
- Schoology / PowerSchool connector code. (Those live in separate specs and plans yet to be written; this plan is the framework prerequisite only.)
