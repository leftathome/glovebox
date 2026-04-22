# Spec 11 -- Data Subject and Audience Metadata Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `data_subject` and `audience` fields to Glovebox's item metadata, plus validation, plumbing, audit-log support, and reader-side default, per `docs/specs/11-data-subject-and-audience-design.md`. Metadata-only v1; no routing/enforcement.

**Architecture:** Additive-only. New audience-validation primitive in `internal/staging/audience.go` (pure function, no deps). Reader-side default helper colocated. Struct extensions in `internal/staging/types.go`, `internal/audit/logger.go`, `connector/rule.go`, `connector/runner.go`, `connector/staging.go`. Merge semantics (per-item > rule > config default > omitted) mirror the existing `MergeIdentity` pattern. Validation runs through the existing `Validate(meta, allowlist) []ValidationError` entry point for items; a parallel `ValidateBaseConfig` runs at config load for defaults.

**Tech Stack:** Go 1.26, standard library only. `go test ./...` and `go vet ./...`. No new dependencies. No staticcheck (not in CI).

**Target version:** v0.3.0 (minor, additive under 0.x semver).

**Spec:** `docs/specs/11-data-subject-and-audience-design.md` (v1.1+).

**Tracking:** All tasks have beads issues. Root: `glovebox-m2b9` (spec) → `glovebox-{o3sh, 4ahf, hcm2, 82kv, q8m0, u1sv, ibzt, 2rdq}` (implementation).

---

## File Structure

| File | Status | Responsibility |
|------|--------|----------------|
| `internal/staging/audience.go` | **new** | Audience enum constants, `ValidateAudience()` pure function, `EffectiveAudience()` reader-side default helper |
| `internal/staging/audience_test.go` | **new** | Table-driven tests for every valid token, every rejected combination, reader-side default |
| `internal/staging/types.go` | modify | Add `DataSubject`, `Audience` fields to `ItemMetadata` |
| `internal/staging/metadata.go` | modify | Extend `Validate()` to add `data_subject` length/control-char rules and delegate to `ValidateAudience()` |
| `internal/staging/metadata_test.go` | modify | New ValidationError cases for data_subject + audience |
| `internal/audit/logger.go` | modify | Add `DataSubject`, `Audience` fields to `AuditEntry` |
| `internal/audit/logger_test.go` | modify | JSON roundtrip for new fields |
| `internal/routing/pass.go` | modify | Populate DataSubject + Audience from `item.Metadata` when logging |
| `internal/routing/quarantine.go` | modify | Same, from `item.Metadata` |
| `internal/routing/reject.go` | modify | Same, with nil-safe handling of `metadata *ItemMetadata` |
| `connector/rule.go` | modify | Add `DataSubject`, `Audience` to `Rule` and `MatchResult`; propagate in `Match()` with defensive slice copy |
| `connector/rule_test.go` | modify | Match result propagation + slice-aliasing tests |
| `connector/runner.go` | modify | Add `DataSubjectDefault`, `AudienceDefault` to `BaseConfig`; call `ValidateBaseConfig` on load |
| `connector/runner_test.go` | modify | Config-load-time rejection of malformed defaults |
| `connector/staging.go` | modify | Add `DataSubject`, `Audience`, `RuleDataSubject`, `RuleAudience` to `ItemOptions`; add merge helpers + config-default setters on `StagingWriter`; populate in `buildMetadata()` |
| `connector/staging_test.go` | modify | Merge precedence tests (per-item > rule > config default > omitted) for both fields |
| `connector/integration_test.go` | modify | End-to-end: schoology-style rules → matching → staging → metadata.json has new fields |
| `CHANGELOG.md` | modify | v0.3.0 entry |

Each file has **one clear responsibility**. Tasks are ordered so each produces a compilable, testable commit; dependencies are explicit below.

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

Ready to start in parallel at T=0: Tasks 1, 2, 4. After those land, Tasks 3, 5, 7 unblock. Task 6 gates on 2+3+4+5. Task 8 is terminal.

---

## Conventions (Read Before Starting)

- **Test layout:** `*_test.go` alongside source files. Table-driven subtests (see `internal/staging/metadata_test.go` and `connector/identity_test.go` for the canonical style).
- **Error types:** `internal/staging` uses `ValidationError{Field, Message string}` returned as `[]ValidationError`, NOT `error`. The caller assembles the slice into a human-readable `error` via `fmt.Errorf`. Do not deviate from this pattern when adding validation rules.
- **Control-char check:** reuse existing `hasControlChars(s string) bool` in `internal/staging/metadata.go`. It whitelists `\n\r\t`. Do NOT write a second control-char policy.
- **Defensive slice copies:** for `[]string` returns that are meant to be caller-owned, use `append([]string(nil), src...)`. Prevents aliasing into rule configs or caller storage.
- **Commit messages:** one-line summary + blank + body. Include `(glovebox-<id>)` in the subject. No emoji.
- **Beads hygiene:** `bd update <id> --claim` at task start, `bd close <id>` at task end. If you get stuck, `bd update <id> --notes "..."` to record context before asking for help.
- **No hook skipping.** Never `--no-verify` a commit. If a pre-commit hook fails, investigate and fix.

---

## Task 1: Audience Enum + Validator + Reader-Side Default

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

// makeAudience returns a slice of n copies of "household", for testing the
// max-entries cap in isolation from the duplicate-token check (length is
// checked before duplicates per ValidateAudience's ordering).
func makeAudience(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "household"
	}
	return out
}

func TestValidateAudience_ValidCombinations(t *testing.T) {
	cases := []struct {
		name           string
		audience       []string
		hasDataSubject bool
	}{
		{"subject-and-parents", []string{"subject", "parents"}, true},
		{"all-role-tokens", []string{"subject", "parents", "siblings"}, true},
		{"subject-only", []string{"subject"}, true},
		{"parents-only", []string{"parents"}, true},
		{"siblings-only", []string{"siblings"}, true},
		{"household-with-subject", []string{"household"}, true},
		{"household-without-subject", []string{"household"}, false},
		{"public-with-subject", []string{"public"}, true},
		{"public-without-subject", []string{"public"}, false},
		{"nil-with-subject", nil, true},
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
		{"empty-array", []string{}, true, "must be omitted"},
		{"duplicates", []string{"subject", "subject"}, true, "duplicate"},
		{"too-many", makeAudience(17), true, "too many"},
		{"public-with-subject-token", []string{"public", "subject"}, true, "public must appear alone"},
		{"public-with-household", []string{"public", "household"}, true, "public must appear alone"},
		{"household-with-parents", []string{"household", "parents"}, true, "household must appear alone"},
		{"household-with-subject-token", []string{"household", "subject"}, true, "household must appear alone"},
		{"subject-token-without-data-subject", []string{"subject"}, false, "requires data_subject"},
		{"parents-without-data-subject", []string{"parents"}, false, "requires data_subject"},
		{"role-plus-household-without-data-subject", []string{"siblings"}, false, "requires data_subject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAudience(tc.audience, tc.hasDataSubject)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q did not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestEffectiveAudience_DefaultWhenNil(t *testing.T) {
	m := ItemMetadata{}
	got := EffectiveAudience(m)
	if len(got) != 1 || got[0] != AudienceHousehold {
		t.Errorf("expected [household] default, got %v", got)
	}
}

func TestEffectiveAudience_PassthroughWhenSet(t *testing.T) {
	m := ItemMetadata{Audience: []string{"subject", "parents"}}
	got := EffectiveAudience(m)
	if len(got) != 2 || got[0] != "subject" || got[1] != "parents" {
		t.Errorf("expected [subject parents], got %v", got)
	}
}
```

### Step 1.3 -- Run tests to confirm they fail

- [ ] Run: `go test ./internal/staging/ -run "TestValidateAudience|TestEffectiveAudience" -v`
- [ ] Expected: compile errors -- `undefined: ValidateAudience`, `undefined: EffectiveAudience`, `undefined: AudienceHousehold`, field `Audience` on `ItemMetadata` may also be unknown (that's covered by Task 2 — that's fine; just confirm the failure is a compile/undefined failure, not a runtime assertion miss).

Note: the `ItemMetadata.Audience` reference means this test file won't compile standalone until Task 2 is merged too, OR you add the field to `ItemMetadata` as part of Task 1. **Recommendation: do Tasks 1 and 2 on the same branch, in that order, to avoid compile-broken intermediate commits.** If strictly separating, land Task 2 first (it's the simpler change), then come back to Task 1's Step 1.3.

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

// roleRelativeTokens are tokens that require a data_subject to be meaningful
// per spec 11 §3.5.
var roleRelativeTokens = map[string]bool{
	AudienceSubject:  true,
	AudienceParents:  true,
	AudienceSiblings: true,
}

// ValidateAudience enforces the spec 11 §3.5 cross-field rules on an audience
// slice. A nil slice is treated as "not set" and returns nil. An empty but
// non-nil slice is rejected. Check order: length cap > token recognition >
// duplicate > token-specific standalone rules > cross-field.
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

// EffectiveAudience returns the audience as consumers should interpret it,
// applying the spec 11 §3.6 default (["household"]) when the field was
// omitted. Callers should use this rather than reading m.Audience directly.
func EffectiveAudience(m ItemMetadata) []string {
	if m.Audience == nil {
		return []string{AudienceHousehold}
	}
	return m.Audience
}
```

### Step 1.5 -- Run tests to confirm they pass

- [ ] Run: `go test ./internal/staging/ -run "TestValidateAudience|TestEffectiveAudience" -v`
- [ ] Expected: all PASS (once `ItemMetadata.Audience` exists — see Step 1.3 note).

### Step 1.6 -- Vet

- [ ] Run: `go vet ./internal/staging/...`
- [ ] Expected: no output.

### Step 1.7 -- Commit

- [ ] Run:

```bash
git add internal/staging/audience.go internal/staging/audience_test.go
git commit -m "$(cat <<'EOF'
staging: audience token enum + ValidateAudience + EffectiveAudience (glovebox-o3sh)

Pure-function validator for the audience role-token set defined in spec 11
§3.4. Enforces cross-field rules from §3.5: unknown-token rejection,
duplicate rejection, 16-entry cap, public-must-be-alone, household-must-
be-alone, role-tokens-require-data_subject.

EffectiveAudience() applies the spec §3.6 reader-side default
(["household"]) when audience is omitted; consumers should use this
rather than reading m.Audience directly.

Foundation for commit-time validation (glovebox-hcm2) and config-load
validation (glovebox-q8m0).
EOF
)"
```

### Step 1.8 -- Close bead

- [ ] Run: `bd close glovebox-o3sh`

**Exit criteria:** new file committed, all audience tests pass, `go vet` clean. Reader-side default helper tested both directions (nil → household; set → passthrough).

---

## Task 2: ItemMetadata DataSubject + Audience Fields

**Beads:** `glovebox-4ahf`
**Depends on:** (none — parallel with Tasks 1 and 4)
**Blocks:** 3, 6, 7

**Files:**
- Modify: `internal/staging/types.go` (existing struct around lines 19-30)
- Create or modify: `internal/staging/types_test.go`

### Step 2.1 -- Claim the bead

- [ ] Run: `bd update glovebox-4ahf --claim`

### Step 2.2 -- Write the failing test

- [ ] Check if `internal/staging/types_test.go` exists: `ls internal/staging/types_test.go`
- [ ] If absent, create it with:

```go
package staging

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)
```

- [ ] Append (or add, if new file):

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
		t.Errorf("expected data_subject omitted: %s", data)
	}
	if strings.Contains(string(data), "audience") {
		t.Errorf("expected audience omitted: %s", data)
	}
}
```

### Step 2.3 -- Run tests to confirm failure

- [ ] Run: `go test ./internal/staging/ -run TestItemMetadata_DataSubject -v`
- [ ] Expected: compile errors -- `unknown field DataSubject in struct literal of type ItemMetadata`, same for `Audience`.

### Step 2.4 -- Add fields to ItemMetadata

- [ ] Edit `internal/staging/types.go`, extend the `ItemMetadata` struct (append the two new fields after `Tags`):

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
- [ ] Expected: both PASS.

### Step 2.6 -- Full package tests + vet

- [ ] Run: `go test ./internal/staging/...`
- [ ] Expected: all PASS (no regressions).
- [ ] Run: `go vet ./internal/staging/...`
- [ ] Expected: no output.

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

## Task 3: Commit-Time Validation (ValidationError Style)

**Beads:** `glovebox-hcm2`
**Depends on:** `glovebox-o3sh`, `glovebox-4ahf`
**Blocks:** 6

**Files:**
- Modify: `internal/staging/metadata.go` (existing `Validate()` at line 56)
- Modify: `internal/staging/metadata_test.go`

### Step 3.1 -- Claim the bead

- [ ] Run: `bd update glovebox-hcm2 --claim`

### Step 3.2 -- Read the existing Validate signature

- [ ] Confirm: `Validate(meta ItemMetadata, allowlist []string) []ValidationError` (returns a slice of errors, does NOT short-circuit on first error). New rules must `append` to the `errs` slice, matching the style of `validateIdentity` / `validateTags`.

- [ ] Confirm the existing `hasControlChars` helper exists in the same file and will be reused.

### Step 3.3 -- Write failing tests

- [ ] Find or add a small helper in `internal/staging/metadata_test.go` that returns a minimally valid `ItemMetadata` and a matching allowlist. If one already exists (e.g. `validBaseMetadata`), reuse it; otherwise add:

```go
func validBaseMetadata() (ItemMetadata, []string) {
	m := ItemMetadata{
		Source:           "test",
		Sender:           "test-sender",
		Subject:          "subject line",
		Timestamp:        time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC),
		DestinationAgent: "test-agent",
		ContentType:      "text/plain",
	}
	return m, []string{m.DestinationAgent}
}

// hasFieldError returns true if errs contains a ValidationError whose Field
// equals the given field path.
func hasFieldError(errs []ValidationError, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}
```

(If equivalent helpers already exist, reuse them and skip these definitions.)

- [ ] Append the new validation tests:

```go
func TestValidate_DataSubjectLength(t *testing.T) {
	m, allow := validBaseMetadata()
	m.DataSubject = strings.Repeat("a", 257)
	errs := Validate(m, allow)
	if !hasFieldError(errs, "data_subject") {
		t.Errorf("expected data_subject error for >256 chars, got errs=%v", errs)
	}
}

func TestValidate_DataSubjectControlChars(t *testing.T) {
	m, allow := validBaseMetadata()
	m.DataSubject = "bee\x00charlie"
	errs := Validate(m, allow)
	if !hasFieldError(errs, "data_subject") {
		t.Errorf("expected data_subject error for control chars, got errs=%v", errs)
	}
}

func TestValidate_DataSubjectEmptyIsOmitted(t *testing.T) {
	// Per spec §6: Go zero value for data_subject is treated as omission.
	m, allow := validBaseMetadata()
	m.DataSubject = ""
	m.Audience = nil
	errs := Validate(m, allow)
	for _, e := range errs {
		if e.Field == "data_subject" {
			t.Errorf("empty data_subject should not produce an error: %v", e)
		}
	}
}

func TestValidate_AudienceValid(t *testing.T) {
	cases := []struct {
		name        string
		dataSubject string
		audience    []string
	}{
		{"subject-and-parents", "bee", []string{"subject", "parents"}},
		{"household-with-subject", "bee", []string{"household"}},
		{"household-without-subject", "", []string{"household"}},
		{"public", "", []string{"public"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, allow := validBaseMetadata()
			m.DataSubject = tc.dataSubject
			m.Audience = tc.audience
			errs := Validate(m, allow)
			if hasFieldError(errs, "audience") {
				t.Errorf("expected no audience error, got errs=%v", errs)
			}
		})
	}
}

func TestValidate_AudienceInvalid(t *testing.T) {
	cases := []struct {
		name        string
		dataSubject string
		audience    []string
	}{
		{"unknown-token", "bee", []string{"grandparents"}},
		{"public-not-alone", "bee", []string{"public", "subject"}},
		{"household-not-alone", "bee", []string{"household", "parents"}},
		{"role-token-without-subject", "", []string{"subject"}},
		{"empty-array", "bee", []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, allow := validBaseMetadata()
			m.DataSubject = tc.dataSubject
			m.Audience = tc.audience
			errs := Validate(m, allow)
			if !hasFieldError(errs, "audience") {
				t.Errorf("expected audience error, got errs=%v", errs)
			}
		})
	}
}
```

### Step 3.4 -- Run tests to confirm failure

- [ ] Run: `go test ./internal/staging/ -run "TestValidate_(DataSubject|Audience)" -v`
- [ ] Expected: FAIL -- existing `Validate` does not produce `data_subject` or `audience` errors.

### Step 3.5 -- Extend Validate()

- [ ] In `internal/staging/metadata.go`, inside `Validate()`, after the `if len(meta.Tags) > 0 { errs = append(errs, validateTags(meta.Tags)...) }` block (around line 116), add:

```go
	// data_subject validation (spec 11 §6).
	if meta.DataSubject != "" {
		if len(meta.DataSubject) > 256 {
			errs = append(errs, ValidationError{"data_subject", "exceeds 256 characters"})
		}
		if hasControlChars(meta.DataSubject) {
			errs = append(errs, ValidationError{"data_subject", "contains control characters"})
		}
	}

	// audience validation (spec 11 §3.5). Delegates to the audience primitive
	// and converts any single error into a ValidationError entry.
	if err := ValidateAudience(meta.Audience, meta.DataSubject != ""); err != nil {
		errs = append(errs, ValidationError{"audience", err.Error()})
	}
```

**Important:** do NOT early-return. The `Validate` function accumulates all errors and returns them as a slice. Append and fall through.

### Step 3.6 -- Run tests to confirm pass

- [ ] Run: `go test ./internal/staging/ -run "TestValidate_(DataSubject|Audience)" -v`
- [ ] Expected: all PASS.

### Step 3.7 -- Full package tests + vet

- [ ] Run: `go test ./internal/staging/...`
- [ ] Run: `go vet ./internal/staging/...`
- [ ] Expected: all green, no regressions.

### Step 3.8 -- Commit

- [ ] Run:

```bash
git add internal/staging/metadata.go internal/staging/metadata_test.go
git commit -m "$(cat <<'EOF'
staging: validate DataSubject + Audience at commit time (glovebox-hcm2)

Extends Validate() with the rules from spec 11 §6 using the existing
ValidationError slice pattern (append, don't early-return). data_subject
length/control-char rules mirror identity field checks; audience rules
delegate to the ValidateAudience primitive from internal/staging/audience.go.

Reuses hasControlChars so the control-char policy matches every other
metadata field.
EOF
)"
```

### Step 3.9 -- Close bead

- [ ] Run: `bd close glovebox-hcm2`

**Exit criteria:** all new validation tests pass; existing staging tests still pass; `go vet` clean; errors accumulate via `append`, no early-return.

---

## Task 4: Rule + MatchResult + RuleMatcher.Match() Extension

**Beads:** `glovebox-82kv`
**Depends on:** (none — parallel)
**Blocks:** 6

**Files:**
- Modify: `connector/rule.go` (Rule lines 22-28, MatchResult lines 30-34, Match lines 48-68)
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
		t.Fatal("expected match for 'foo'")
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
		t.Errorf("rule audience was mutated via returned slice: got %q", got2.Audience[0])
	}
}
```

### Step 4.3 -- Run tests to confirm failure

- [ ] Run: `go test ./connector/ -run TestRuleMatcher_ -v`
- [ ] Expected: compile errors -- `unknown field DataSubject in struct literal of type Rule`, same for `Audience`.

### Step 4.4 -- Extend the structs and Match()

- [ ] Edit `connector/rule.go`. Extend `Rule`:

```go
type Rule struct {
	Match       string            `json:"match"`
	Destination string            `json:"destination"`
	Tags        map[string]string `json:"tags,omitempty"`
	DataSubject string            `json:"data_subject,omitempty"`
	Audience    []string          `json:"audience,omitempty"`
}
```

- [ ] Extend `MatchResult`:

```go
type MatchResult struct {
	Destination string
	Tags        map[string]string
	DataSubject string
	Audience    []string
}
```

- [ ] Replace the body of `(rm *RuleMatcher) Match(key string) (MatchResult, bool)` (currently lines ~51-68) with:

```go
func (rm *RuleMatcher) Match(key string) (MatchResult, bool) {
	for _, rule := range rm.rules {
		if rule.Match == key || rule.Match == "*" {
			var tags map[string]string
			if len(rule.Tags) > 0 {
				tags = make(map[string]string, len(rule.Tags))
				for k, v := range rule.Tags {
					tags[k] = v
				}
			}
			var audience []string
			if len(rule.Audience) > 0 {
				audience = append([]string(nil), rule.Audience...)
			}
			return MatchResult{
				Destination: rule.Destination,
				Tags:        tags,
				DataSubject: rule.DataSubject,
				Audience:    audience,
			}, true
		}
	}
	return MatchResult{}, false
}
```

Note the preserved pattern: tags are copied element-by-element into a fresh map; audience is copied via `append([]string(nil), src...)`. Both prevent aliasing.

### Step 4.5 -- Run tests to confirm pass

- [ ] Run: `go test ./connector/ -run TestRuleMatcher_ -v`
- [ ] Expected: both new tests PASS, plus any existing RuleMatcher tests.

### Step 4.6 -- Full connector tests + vet

- [ ] Run: `go test ./connector/...`
- [ ] Run: `go vet ./connector/...`
- [ ] Expected: all green.

### Step 4.7 -- Commit

- [ ] Run:

```bash
git add connector/rule.go connector/rule_test.go
git commit -m "$(cat <<'EOF'
connector: propagate DataSubject + Audience through rule matching (glovebox-82kv)

Extends Rule and MatchResult structs per spec 11 §4.2. RuleMatcher.Match()
carries data_subject and audience from the first matching rule into
MatchResult, with a defensive slice copy to prevent aliasing into the
rule's configured audience. Same pattern as the existing per-match tag
copy.
EOF
)"
```

### Step 4.8 -- Close bead

- [ ] Run: `bd close glovebox-82kv`

**Exit criteria:** both new tests pass; slice-aliasing test confirms defensive copy; full connector tests pass; `go vet` clean.

---

## Task 5: BaseConfig Defaults + Config-Load Validation

**Beads:** `glovebox-q8m0`
**Depends on:** `glovebox-o3sh`
**Blocks:** 6

**Files:**
- Modify: `connector/runner.go` (BaseConfig at lines 19-24; config load path)
- Modify: `connector/runner_test.go`

### Step 5.1 -- Claim the bead

- [ ] Run: `bd update glovebox-q8m0 --claim`

### Step 5.2 -- Locate the config-load path

- [ ] Run: `grep -n "BaseConfig\|Unmarshal\|ReadFile" connector/runner.go`. Note the line(s) where `BaseConfig` is populated from the config file at startup. Load-time validation goes immediately after unmarshal and before the runner starts executing.

### Step 5.3 -- Write failing tests

- [ ] Append to `connector/runner_test.go` (add any missing imports: `encoding/json`, `strings`, `testing`):

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
			"must be omitted",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cfg BaseConfig
			if err := json.Unmarshal([]byte(tc.json), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			err := ValidateBaseConfig(&cfg)
			if err == nil {
				t.Fatalf("expected error, got nil")
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

func TestBaseConfig_DataSubjectDefaultControlCharsRejected(t *testing.T) {
	cfg := BaseConfig{
		Rules:              []Rule{{Match: "*", Destination: "a"}},
		DataSubjectDefault: "bee\x00charlie",
	}
	if err := ValidateBaseConfig(&cfg); err == nil {
		t.Fatal("expected error for control chars in data_subject_default")
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

func TestBaseConfig_ZeroDefaultsAccepted(t *testing.T) {
	// No defaults set at all -- must be valid.
	cfg := BaseConfig{
		Rules: []Rule{{Match: "*", Destination: "a"}},
	}
	if err := ValidateBaseConfig(&cfg); err != nil {
		t.Errorf("zero defaults should pass, got %v", err)
	}
}
```

### Step 5.4 -- Run tests to confirm failure

- [ ] Run: `go test ./connector/ -run "TestBaseConfig_" -v`
- [ ] Expected: compile errors -- `unknown field DataSubjectDefault`, `unknown field AudienceDefault`, `undefined: ValidateBaseConfig`.

### Step 5.5 -- Extend BaseConfig

- [ ] In `connector/runner.go`, extend the existing `BaseConfig`:

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

### Step 5.6 -- Add ValidateBaseConfig

- [ ] Add a new import to `runner.go` if not already present:

```go
"github.com/leftathome/glovebox/internal/staging"
```

- [ ] Add the validator function in `runner.go` (near the other config-related code):

```go
// ValidateBaseConfig enforces spec 11 §5.1 startup-time rules on the
// data-subject and audience defaults. Called from the config-load path
// before the runner starts.
func ValidateBaseConfig(c *BaseConfig) error {
	if len(c.DataSubjectDefault) > 256 {
		return fmt.Errorf("data_subject_default exceeds 256 characters")
	}
	if staging.HasControlCharsExported(c.DataSubjectDefault) {
		return fmt.Errorf("data_subject_default contains control characters")
	}
	hasSubject := c.DataSubjectDefault != ""
	if err := staging.ValidateAudience(c.AudienceDefault, hasSubject); err != nil {
		return fmt.Errorf("audience_default: %w", err)
	}
	return nil
}
```

**Note:** `hasControlChars` in `internal/staging/metadata.go` is currently lowercase (unexported). To call it from the `connector` package, either (a) add a thin exported wrapper `HasControlCharsExported` in `internal/staging/` (preferred: keeps the existing unexported helper internal to staging), or (b) inline the same byte-range check in `connector/runner.go`. **Preferred (a).** Add to `internal/staging/metadata.go`:

```go
// HasControlChars is the exported wrapper around the package-internal
// control-char predicate, used by the connector package's config-load
// validator. Whitelists \n \r \t per the internal policy.
func HasControlChars(s string) bool {
	return hasControlChars(s)
}
```

Then use `staging.HasControlChars(c.DataSubjectDefault)` in the validator.

- [ ] Locate the `Run()` (or equivalent top-level) function in `runner.go` that loads the config. Immediately after the successful `json.Unmarshal` into `BaseConfig`, and before any other initialization, add:

```go
	if err := ValidateBaseConfig(&cfg.BaseConfig); err != nil {
		return PermanentError(fmt.Errorf("config validation: %w", err))
	}
```

Adapt the field access (`cfg.BaseConfig` vs `cfg` vs `baseCfg`) to match the actual struct naming in `Run()`.

### Step 5.7 -- Run tests to confirm pass

- [ ] Run: `go test ./connector/ -run "TestBaseConfig_" -v`
- [ ] Expected: all PASS.

### Step 5.8 -- Full connector tests + vet

- [ ] Run: `go test ./...`
- [ ] Run: `go vet ./...`
- [ ] Expected: all green (including the new exported `HasControlChars` in the staging package).

### Step 5.9 -- Commit

- [ ] Run:

```bash
git add connector/runner.go connector/runner_test.go internal/staging/metadata.go
git commit -m "$(cat <<'EOF'
connector: BaseConfig defaults + load-time validation (glovebox-q8m0)

Extends BaseConfig with DataSubjectDefault and AudienceDefault per spec 11
§4.1. New ValidateBaseConfig runs the same rules as item-time validation
(length + control-chars on data_subject; enum + cross-field on audience)
at startup, so malformed defaults fail fast rather than at first item
commit.

Exports staging.HasControlChars as a thin wrapper so the connector
package uses the same control-char policy as the internal validator.
EOF
)"
```

### Step 5.10 -- Close bead

- [ ] Run: `bd close glovebox-q8m0`

**Exit criteria:** all new tests pass; existing connector tests still pass; malformed defaults cause `Run()` to return a permanent error at startup; `go vet` clean across the repo.

---

## Task 6: ItemOptions + Merge Logic in staging.go

**Beads:** `glovebox-u1sv`
**Depends on:** `glovebox-4ahf`, `glovebox-hcm2`, `glovebox-82kv`, `glovebox-q8m0`
**Blocks:** 8

**Files:**
- Modify: `connector/staging.go` (ItemOptions lines 15-27; StagingWriter lines 29-34; buildMetadata lines 92-131)
- Modify: `connector/staging_test.go`
- Modify: `connector/runner.go` (the place that wires config defaults onto the writer, near where `SetConfigIdentity` is already called)

### Step 6.1 -- Claim the bead

- [ ] Run: `bd update glovebox-u1sv --claim`

### Step 6.2 -- Write failing tests

- [ ] Append to `connector/staging_test.go`:

```go
// readMetadataFromCommitted walks the staging directory, finds the single
// committed item, and returns its parsed metadata.json.
func readMetadataFromCommitted(t *testing.T, stagingDir string) staging.ItemMetadata {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("readdir staging: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stagingDir, e.Name(), "metadata.json"))
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}
		var m staging.ItemMetadata
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		return m
	}
	t.Fatal("no committed item found")
	return staging.ItemMetadata{}
}

// newWriterWithDefaults constructs a StagingWriter with the given config
// defaults pre-applied. Used by merge tests.
func newWriterWithDefaults(t *testing.T, stagingDir, configSubject string, configAudience []string) *StagingWriter {
	t.Helper()
	w, err := NewStagingWriter(stagingDir, "test")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	w.SetConfigDataSubject(configSubject)
	w.SetConfigAudience(configAudience)
	return w
}

// commitOne writes and commits a single item with the given options.
func commitOne(t *testing.T, w *StagingWriter, opts ItemOptions) {
	t.Helper()
	opts.Source = "test"
	opts.Sender = "s"
	opts.Subject = "subject"
	opts.Timestamp = time.Now().UTC()
	opts.DestinationAgent = "test-agent"
	opts.ContentType = "text/plain"
	item, err := w.NewItem(opts)
	if err != nil {
		t.Fatalf("NewItem: %v", err)
	}
	if err := item.WriteContent([]byte("x")); err != nil {
		t.Fatalf("WriteContent: %v", err)
	}
	if err := item.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestBuildMetadata_DataSubjectMergePrecedence(t *testing.T) {
	cases := []struct {
		name      string
		configDef string
		ruleVal   string
		itemVal   string
		want      string
	}{
		{"per-item-wins", "config", "rule", "item", "item"},
		{"rule-wins-when-no-item", "config", "rule", "", "rule"},
		{"config-wins-when-no-item-no-rule", "config", "", "", "config"},
		{"empty-when-nothing-set", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stagingDir := t.TempDir()
			w := newWriterWithDefaults(t, stagingDir, tc.configDef, nil)
			commitOne(t, w, ItemOptions{
				DataSubject:     tc.itemVal,
				RuleDataSubject: tc.ruleVal,
			})
			got := readMetadataFromCommitted(t, stagingDir)
			if got.DataSubject != tc.want {
				t.Errorf("DataSubject: got %q, want %q", got.DataSubject, tc.want)
			}
		})
	}
}

func TestBuildMetadata_AudienceMergePrecedence(t *testing.T) {
	cases := []struct {
		name      string
		configDef []string
		ruleVal   []string
		itemVal   []string
		want      []string
	}{
		{
			"per-item-wins",
			[]string{"household"},
			[]string{"subject", "parents"},
			[]string{"public"},
			[]string{"public"},
		},
		{
			"rule-wins-when-no-item",
			[]string{"household"},
			[]string{"subject", "parents"},
			nil,
			[]string{"subject", "parents"},
		},
		{
			"config-wins-when-no-item-no-rule",
			[]string{"household"},
			nil,
			nil,
			[]string{"household"},
		},
		{
			"nil-when-nothing-set",
			nil,
			nil,
			nil,
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stagingDir := t.TempDir()
			// DataSubject matters here: "public" is the only audience valid without data_subject.
			// For the "per-item-wins" case (item = public), we can leave DataSubject empty.
			// For others, we need DataSubject set so role tokens validate.
			subject := "bee"
			if tc.name == "nil-when-nothing-set" {
				subject = ""
			}
			w := newWriterWithDefaults(t, stagingDir, subject, tc.configDef)
			commitOne(t, w, ItemOptions{
				Audience:     tc.itemVal,
				RuleAudience: tc.ruleVal,
			})
			got := readMetadataFromCommitted(t, stagingDir)
			if !slicesEqual(got.Audience, tc.want) {
				t.Errorf("Audience: got %v, want %v", got.Audience, tc.want)
			}
		})
	}
}

// slicesEqual is a local helper; if the test file already has an equivalent,
// reuse it and remove this.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

- [ ] Verify the imports at the top of `connector/staging_test.go` include: `encoding/json`, `os`, `path/filepath`, `strings`, `testing`, `time`, and `github.com/leftathome/glovebox/internal/staging`. Add any that are missing.

### Step 6.3 -- Run tests to confirm failure

- [ ] Run: `go test ./connector/ -run "TestBuildMetadata_(DataSubject|Audience)Merge" -v`
- [ ] Expected: compile errors -- `unknown field DataSubject on ItemOptions`, `unknown method SetConfigDataSubject on StagingWriter`, etc.

### Step 6.4 -- Extend ItemOptions and StagingWriter

- [ ] In `connector/staging.go`, extend `ItemOptions`:

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

- [ ] Extend `StagingWriter` to carry the config defaults:

```go
type StagingWriter struct {
	stagingDir               string
	connectorName            string
	tmpDir                   string
	configIdentity           *ConfigIdentity
	configDataSubjectDefault string
	configAudienceDefault    []string
}
```

- [ ] Add setters alongside `SetConfigIdentity`:

```go
// SetConfigDataSubject sets the config-level data_subject default used as
// the final fallback in the merge chain.
func (w *StagingWriter) SetConfigDataSubject(s string) {
	w.configDataSubjectDefault = s
}

// SetConfigAudience sets the config-level audience default used as the
// final fallback in the merge chain. The slice is copied to prevent
// aliasing into the caller's storage.
func (w *StagingWriter) SetConfigAudience(a []string) {
	if len(a) == 0 {
		w.configAudienceDefault = nil
		return
	}
	w.configAudienceDefault = append([]string(nil), a...)
}
```

- [ ] Extend `StagingItem` to hold the writer's config defaults (currently `configIdentity` is copied from `w.configIdentity`; mirror that):

```go
type StagingItem struct {
	dir                      string
	stagingDir               string
	opts                     ItemOptions
	configIdentity           *ConfigIdentity
	configDataSubjectDefault string
	configAudienceDefault    []string
	commitFunc               func() error
}
```

- [ ] Update `NewItem()` to populate the new `StagingItem` fields:

```go
func (w *StagingWriter) NewItem(opts ItemOptions) (*StagingItem, error) {
	name := fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102-150405"), uuid.New().String()[:8])
	dir := filepath.Join(w.tmpDir, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create item dir: %w", err)
	}
	return &StagingItem{
		dir:                      dir,
		stagingDir:               w.stagingDir,
		opts:                     opts,
		configIdentity:           w.configIdentity,
		configDataSubjectDefault: w.configDataSubjectDefault,
		configAudienceDefault:    w.configAudienceDefault,
	}, nil
}
```

### Step 6.5 -- Implement merge in buildMetadata()

- [ ] In `connector/staging.go`, inside `buildMetadata()`, after the existing `MergeIdentity` block (currently line 111-120) and BEFORE the `meta.DestinationAgent == ""` check (line 122), insert:

```go
	meta.DataSubject = mergeDataSubject(si.configDataSubjectDefault, si.opts.RuleDataSubject, si.opts.DataSubject)
	meta.Audience = mergeAudience(si.configAudienceDefault, si.opts.RuleAudience, si.opts.Audience)
```

- [ ] Add the merge helpers at the bottom of `connector/staging.go`, alongside `mergeTags`:

```go
// mergeDataSubject applies per-item > rule > config default > empty precedence
// per spec 11 §5.
func mergeDataSubject(configDefault, rule, item string) string {
	if item != "" {
		return item
	}
	if rule != "" {
		return rule
	}
	return configDefault
}

// mergeAudience applies per-item > rule > config default > nil precedence
// per spec 11 §5. Returns a fresh slice to prevent aliasing.
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

### Step 6.6 -- Wire config defaults from runner.go

- [ ] In `connector/runner.go`, find the location where the runner creates the `StagingWriter` and calls `SetConfigIdentity`. Immediately after that call, add:

```go
	writer.SetConfigDataSubject(cfg.DataSubjectDefault)
	writer.SetConfigAudience(cfg.AudienceDefault)
```

(Adapt `cfg` to the actual variable name in `Run()`; the two setters take raw values straight from `BaseConfig`.)

### Step 6.7 -- Run tests to confirm pass

- [ ] Run: `go test ./connector/ -run "TestBuildMetadata_(DataSubject|Audience)Merge" -v`
- [ ] Expected: all PASS.

### Step 6.8 -- Full test + vet across repo

- [ ] Run: `go test ./...`
- [ ] Run: `go vet ./...`
- [ ] Expected: all green. No regressions in any existing connector or package.

### Step 6.9 -- Commit

- [ ] Run:

```bash
git add connector/staging.go connector/staging_test.go connector/runner.go
git commit -m "$(cat <<'EOF'
connector: merge DataSubject + Audience per-item > rule > config (glovebox-u1sv)

Implements spec 11 §5 precedence semantics. ItemOptions gains DataSubject,
Audience, RuleDataSubject, RuleAudience fields mirroring the existing tag
plumbing (Tags + RuleTags). StagingWriter tracks config-level defaults
via SetConfigDataSubject/SetConfigAudience setters, parallel to the
existing SetConfigIdentity. buildMetadata() resolves the final values
before validation. Slice returns are fresh copies to prevent aliasing.
EOF
)"
```

### Step 6.10 -- Close bead

- [ ] Run: `bd close glovebox-u1sv`

**Exit criteria:** merge tests pass for both fields across all four precedence cases; `go test ./...` and `go vet ./...` green across the repository; no existing connector is broken.

---

## Task 7: AuditEntry Extension (Three Call Sites)

**Beads:** `glovebox-ibzt`
**Depends on:** `glovebox-4ahf`
**Blocks:** 8

**Files:**
- Modify: `internal/audit/logger.go` (AuditEntry struct)
- Modify: `internal/audit/logger_test.go`
- Modify: `internal/routing/pass.go` (line 42)
- Modify: `internal/routing/quarantine.go` (line 77)
- Modify: `internal/routing/reject.go` (line 23) — nil-safe path

### Step 7.1 -- Claim the bead

- [ ] Run: `bd update glovebox-ibzt --claim`

### Step 7.2 -- Write failing tests for AuditEntry

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
- [ ] Expected: compile errors -- `unknown field DataSubject`, `unknown field Audience`.

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

### Step 7.5 -- Populate at the three call sites

Three call sites assemble `audit.AuditEntry` from `staging.ItemMetadata` (or a nil pointer in reject's case). All three must copy through `DataSubject` and `Audience`.

- [ ] Edit `internal/routing/pass.go` (line ~42). Extend the `AuditEntry` literal:

```go
	if err := logger.LogPass(audit.PassEntry{AuditEntry: audit.AuditEntry{
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		Source:         item.Metadata.Source,
		Sender:         item.Metadata.Sender,
		ContentHash:    hash,
		ContentLength:  int64(len(content)),
		Signals:        scanResult.Signals,
		TotalScore:     scanResult.TotalScore,
		Verdict:        string(scanResult.Verdict),
		Destination:    item.Metadata.DestinationAgent,
		ScanDurationMs: scanDuration.Milliseconds(),
		DataSubject:    item.Metadata.DataSubject,
		Audience:       item.Metadata.Audience,
	}}); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
```

- [ ] Edit `internal/routing/quarantine.go` (line ~77). Extend the `AuditEntry` literal:

```go
	if err := logger.LogReject(audit.RejectEntry{
		AuditEntry: audit.AuditEntry{
			Timestamp:      now.Format(time.RFC3339),
			Source:         item.Metadata.Source,
			Sender:         item.Metadata.Sender,
			ContentHash:    hash,
			ContentLength:  int64(len(content)),
			Signals:        scanResult.Signals,
			TotalScore:     scanResult.TotalScore,
			Verdict:        string(engine.VerdictQuarantine),
			Destination:    item.Metadata.DestinationAgent,
			ScanDurationMs: scanDuration.Milliseconds(),
			DataSubject:    item.Metadata.DataSubject,
			Audience:       item.Metadata.Audience,
		},
		Reason: reason,
	}); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
```

- [ ] Edit `internal/routing/reject.go` (line ~13). The `metadata *staging.ItemMetadata` parameter can be nil; the existing code uses guarded local variables for source/sender/destination. Mirror the pattern:

```go
func RouteReject(itemPath string, reason string, metadata *staging.ItemMetadata, logger *audit.Logger) error {
	source := "unknown"
	sender := "unknown"
	destination := "unknown"
	var dataSubject string
	var audience []string
	if metadata != nil {
		source = metadata.Source
		sender = metadata.Sender
		destination = metadata.DestinationAgent
		dataSubject = metadata.DataSubject
		audience = metadata.Audience
	}

	if err := logger.LogReject(audit.RejectEntry{
		AuditEntry: audit.AuditEntry{
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
			Source:      source,
			Sender:      sender,
			Verdict:     string(engine.VerdictReject),
			Destination: destination,
			DataSubject: dataSubject,
			Audience:    audience,
		},
		Reason: reason,
	}); err != nil {
		return fmt.Errorf("audit log: %w", err)
	}

	os.RemoveAll(itemPath)
	return nil
}
```

### Step 7.6 -- Run tests to confirm pass

- [ ] Run: `go test ./internal/audit/ -v`
- [ ] Run: `go test ./internal/routing/ -v`
- [ ] Run: `go vet ./internal/...`
- [ ] Expected: all green. The existing routing tests must continue to pass (they don't assert on DataSubject/Audience yet, but shouldn't regress).

### Step 7.7 -- Commit

- [ ] Run:

```bash
git add internal/audit/logger.go internal/audit/logger_test.go \
        internal/routing/pass.go internal/routing/reject.go internal/routing/quarantine.go
git commit -m "$(cat <<'EOF'
audit: record DataSubject + Audience on every item (glovebox-ibzt)

Extends AuditEntry per spec 11 §7 for forensic traceability of declared
visibility intent. omitempty preserves existing audit-log shape for
subject-less / audience-less items.

All three routing paths (pass, quarantine, reject) copy the new fields
from ItemMetadata. The reject path is nil-safe because its metadata
pointer can be nil when metadata.json itself failed to parse.
EOF
)"
```

### Step 7.8 -- Close bead

- [ ] Run: `bd close glovebox-ibzt`

**Exit criteria:** roundtrip + omitempty tests pass; all three routing call sites populated; nil-safe reject confirmed; full audit + routing test suites clean.

---

## Task 8: Integration Test + CHANGELOG + Regression Verification

**Beads:** `glovebox-2rdq`
**Depends on:** `glovebox-u1sv`, `glovebox-ibzt`
**Blocks:** (none — terminal)

**Files:**
- Modify: `connector/integration_test.go` (existing end-to-end harness)
- Modify: `CHANGELOG.md`

### Step 8.1 -- Claim the bead

- [ ] Run: `bd update glovebox-2rdq --claim`

### Step 8.2 -- Read the existing integration test shape

- [ ] Open `connector/integration_test.go`. Identify the import list and any existing shared helpers. The test below assumes the standard library imports are already present; add any missing ones.

### Step 8.3 -- Add the end-to-end test

- [ ] Append:

```go
func TestIntegration_DataSubjectAudienceEndToEnd(t *testing.T) {
	stagingDir := t.TempDir()

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
			Match:       "schoology:bee:flyer",
			Destination: "school",
			Audience:    []string{"public"},
		},
	}
	matcher := NewRuleMatcher(rules)

	// Item 1: Bee's grade (data_subject + role-relative audience).
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

	// Item 2: flyer (subjectless, public).
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
	if err := item2.WriteContent([]byte("flyer body")); err != nil {
		t.Fatal(err)
	}
	if err := item2.Commit(); err != nil {
		t.Fatal(err)
	}

	// Verify: read back metadata.json files and confirm shape.
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatal(err)
	}
	foundGrade, foundFlyer := false, false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stagingDir, e.Name(), "metadata.json"))
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}
		var m staging.ItemMetadata
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("unmarshal metadata: %v", err)
		}
		switch m.DataSubject {
		case "bee":
			foundGrade = true
			if len(m.Audience) != 2 || m.Audience[0] != "subject" || m.Audience[1] != "parents" {
				t.Errorf("bee's grade audience: got %v, want [subject parents]", m.Audience)
			}
		case "":
			foundFlyer = true
			if len(m.Audience) != 1 || m.Audience[0] != "public" {
				t.Errorf("flyer audience: got %v, want [public]", m.Audience)
			}
		default:
			t.Errorf("unexpected data_subject %q in committed metadata", m.DataSubject)
		}
	}
	if !foundGrade {
		t.Error("did not find bee's grade in staging output")
	}
	if !foundFlyer {
		t.Error("did not find flyer in staging output")
	}
}
```

- [ ] Verify imports at the top of `connector/integration_test.go` include: `encoding/json`, `os`, `path/filepath`, `strings`, `testing`, `time`, and `github.com/leftathome/glovebox/internal/staging`. Add any missing.

### Step 8.4 -- Run the new integration test

- [ ] Run: `go test ./connector/ -run TestIntegration_DataSubjectAudienceEndToEnd -v`
- [ ] Expected: PASS.

### Step 8.5 -- Full regression suite

- [ ] Run: `go test ./...`
- [ ] Expected: all PASS across the entire repository (all existing connectors still build and test clean under the additive schema).
- [ ] Run: `go vet ./...`
- [ ] Expected: no output.

### Step 8.6 -- Update CHANGELOG

- [ ] Edit `CHANGELOG.md`. Add a new section at the top (under any existing `## Unreleased`, or as a fresh `## v0.3.0 -- 2026-04-22` block if the file uses dated version headers):

```markdown
## v0.3.0 -- 2026-04-22

### Added
- `data_subject` (string) and `audience` ([]string enum) fields on
  `metadata.json`, `ItemOptions`, `Rule`, `MatchResult`, `BaseConfig`
  defaults, and `AuditEntry`. See
  `docs/specs/11-data-subject-and-audience-design.md`.
- Audience enum tokens: `subject`, `parents`, `siblings`, `household`,
  `public`, with validated combinations (spec 11 §3.5).
- `staging.EffectiveAudience()` reader-side helper that applies the
  default `["household"]` when audience is omitted.
- Commit-time validation of `data_subject` length/control-chars and
  `audience` enum + cross-field rules.
- Config-load-time validation of `data_subject_default` and
  `audience_default`: malformed defaults fail startup, not first-item
  commit.

### Notes
- Purely additive schema extension. Existing connectors produce
  byte-identical `metadata.json` files with no code changes.
- V1 is metadata-only: Glovebox validates and stamps these fields but
  does not filter or route on them. Audience-aware routing and
  enforcement are deferred to later specs.
```

### Step 8.7 -- Commit

- [ ] Run:

```bash
git add connector/integration_test.go CHANGELOG.md
git commit -m "$(cat <<'EOF'
spec 11: end-to-end integration test + v0.3.0 CHANGELOG (glovebox-2rdq)

Exercises the full spec-11 path: rule -> match -> staging -> metadata.json
for both a data-subject-bearing item (Bee's grade, audience [subject,
parents]) and a subjectless item (flyer, audience [public]).

Full go test ./... and go vet ./... clean against all existing
connectors -- additive schema extension causes no regressions.
EOF
)"
```

### Step 8.8 -- Close bead

- [ ] Run: `bd close glovebox-2rdq`

**Exit criteria:** integration test passes; `go test ./...` and `go vet ./...` both green across the entire repository; CHANGELOG entry committed.

---

## Final Verification

After Task 8:

- [ ] Run: `git log --oneline origin/main..HEAD` -- confirm 8 implementation commits (plus the spec + plan commits already on the branch).
- [ ] Run: `bd list --status=open | grep "spec 11 impl"` -- should return empty; all eight task beads closed.
- [ ] Close the parent spec bead: `bd close glovebox-m2b9 --reason="spec implemented via glovebox-{o3sh,4ahf,hcm2,82kv,q8m0,u1sv,ibzt,2rdq}"`.
- [ ] Push: `git push`.
- [ ] Wait for CI to pass on main.
- [ ] **Only after CI green:** `git tag -a v0.3.0 -m "Spec 11: data_subject and audience metadata"` then `git push --tags`.
  - Per the release-workflow memory: **do not tag until CI green**. The v0.2.2 incident (test files committed without source, broken immutable release) is why.

---

## Notes and Caveats

- **Tests are table-driven.** See `connector/identity_test.go` and `internal/staging/metadata_test.go` for canonical style. Don't invent a new test convention.
- **Defensive slice copies.** Use `append([]string(nil), src...)` anywhere a returned or stored slice could otherwise alias a caller's storage. Several tasks explicitly test for this.
- **Validation style.** `internal/staging/metadata.go` uses `[]ValidationError` return, not a single `error`. Append rather than early-return. Don't change this style.
- **Control-char policy.** Reuse `hasControlChars` (internal) via the new exported wrapper `HasControlChars` added in Task 5. Don't write a second predicate.
- **`staticcheck` is not in CI.** Use `go vet` only.
- **Per CLAUDE.md:** if a container is involved, test in a container. The tasks here are all in-process unit/integration; no containers required. Future connector implementations (Schoology, PowerSchool) may need container tests — out of scope here.
- **Never skip hooks** (`--no-verify`) on these commits. If a pre-commit hook fires, investigate and fix.

---

## Out of Scope for This Plan

Per spec 11 §2.2:

- Audience-aware rule matching / routing.
- Enforcement gates (quarantine on audience mismatch).
- Named audience shorthands (`"student-private"`).
- Multi-subject items (`data_subject` as array).
- Extended-family tokens.
- Cross-connector subject reconciliation.
- Schoology / PowerSchool connector code (future specs + plans, not here).
