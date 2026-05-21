# Spec 12 -- Schoology Connector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Multiple Wave 1 tasks can be **dispatched in parallel** via superpowers:dispatching-parallel-agents because they touch disjoint files. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the Schoology connector for Glovebox per spec 12. Single container ingests assignments, feed posts, inbox messages, and attachments from a parent Schoology account via the `schoology-go` library, emitting per-kid `data_subject` items with spec 11 v1.2 `audience` tokens.

**Architecture:** A new Go connector under `connectors/schoology/` implements `Connector` + `Watcher` + `Listener` from the framework (spec 05). Library access via a `SchoologyClient` interface (production wrapper around `*schoology.Client`; mock for tests). Windowed scheduling with per-day random splay. Parse failures preserved as quarantine-bound forensic items via a small Glovebox-internal routing-layer enhancement (Q-EARLY). Three-layer telemetry: framework + schoology-specific metrics, OTel traces, structured event logs.

**Tech Stack:** Go 1.26, standard library + existing project dependencies (`go.opentelemetry.io/otel`, `github.com/leftathome/schoology-go`). No new external deps. `go test ./...` and `go vet ./...` on every commit.

**Target version:** v0.6.0 (minor bump under 0.x semver; new connector + new Glovebox routing rule; additive).

**Spec:** `docs/specs/12-schoology-connector-design.md` (v1.0, commits `07ea86a` + `5af477c`).

**Tracking:** Umbrella: `glovebox-qhlk`. Implementation: 16 task beads (see Dependency Graph). Q-EARLY routing prerequisite: `glovebox-8sc2`.

**Branch:** `spec-12-schoology-connector` (already created, currently empty modulo this plan + spec doc).

**Push remote:** `gitlab` (github HTTPS auth still broken per `glovebox-push-github-https-auth-via-credential-helper` memory).

---

## File Structure

| File | Status | Responsibility |
|------|--------|----------------|
| `internal/routing/quarantine.go` | modify (Task 1) | Detect `parse_status: degraded\|failure_receipt` tag and route via quarantine path (Q-EARLY) |
| `internal/routing/quarantine_test.go` | modify (Task 1) | Tag-based quarantine routing tests |
| `connectors/schoology/client.go` | **new** (Task 2) | `SchoologyClient` interface + production wrapper struct |
| `connectors/schoology/client_test.go` | **new** (Task 2) | Mock builder for tests in other tasks |
| `connectors/schoology/config.go` | **new** (Task 3) | `Config` struct + JSON loading + validation |
| `connectors/schoology/config_test.go` | **new** (Task 3) | Config validation tests |
| `connectors/schoology/scheduler.go` | **new** (Task 4) | Window + splay scheduler |
| `connectors/schoology/scheduler_test.go` | **new** (Task 4) | Scheduler determinism + weekday skip tests |
| `connectors/schoology/checkpoint.go` | **new** (Task 5) | Highest-ID checkpoint helpers |
| `connectors/schoology/checkpoint_test.go` | **new** (Task 5) | Dedup + below-checkpoint warning tests |
| `connectors/schoology/parse_failure.go` | **new** (Task 6) | Receipt builder + degraded-item builder + schema-drift counter + receipt dedup |
| `connectors/schoology/parse_failure_test.go` | **new** (Task 6) | Receipt structure + dedup + counter behavior tests |
| `connectors/schoology/assignments.go` | **new** (Task 7) | Assignments processor |
| `connectors/schoology/assignments_test.go` | **new** (Task 7) | Assignments tests with mock client |
| `connectors/schoology/feed.go` | **new** (Task 8) | Feed processor |
| `connectors/schoology/feed_test.go` | **new** (Task 8) | Feed tests with mock client |
| `connectors/schoology/messages.go` | **new** (Task 9) | Messages processor |
| `connectors/schoology/messages_test.go` | **new** (Task 9) | Messages tests with mock client |
| `connectors/schoology/attachments.go` | **new** (Task 10) | Attachments handler (shared between feed and messages) |
| `connectors/schoology/attachments_test.go` | **new** (Task 10) | Size-cap + dedup + skip-with-warning tests |
| `connectors/schoology/trigger.go` | **new** (Task 11) | HTTP trigger endpoint handler |
| `connectors/schoology/trigger_test.go` | **new** (Task 11) | Auth + debounce + 202 async tests |
| `connectors/schoology/telemetry.go` | **new** (Task 12) | Schoology-specific Prometheus metrics + OTel tracer setup |
| `connectors/schoology/telemetry_test.go` | **new** (Task 12) | Metrics registration + tracer initialization tests |
| `connectors/schoology/connector.go` | **new** (Task 13) | `SchoologyConnector` struct implementing Connector/Watcher/Listener; shared `pollNow()` |
| `connectors/schoology/connector_test.go` | **new** (Task 13) | Wiring tests; verify all paths converge on pollNow |
| `connectors/schoology/main.go` | **new** (Task 13) | Entry point: load config, build client, call `connector.Run()` |
| `connectors/schoology/Dockerfile` | **new** (Task 14) | Multi-stage build matching existing connector pattern |
| `connectors/schoology/integration_test.go` | **new** (Task 15) | End-to-end with mock client across multiple polls |
| `docs/AUTH-RECOVERY.md` | **new** (Task 16) | Operator step-by-step for Schoology session expiry recovery |
| `CHANGELOG.md` | modify (Task 16) | v0.6.0 entry |

---

## Dependency Graph

```
                                          Wave 1 (parallel — 7 tasks)
                                          ──────────────────────────
                                          glovebox-8sc2  (Q-EARLY routing)
                                          glovebox-nilo  (SchoologyClient interface)
                                          glovebox-yg0m  (Config)
                                          glovebox-zzko  (Scheduler)
                                          glovebox-rjeo  (Checkpoint helpers)
                                          glovebox-gqup  (Trigger endpoint)
                                          glovebox-h6ef  (Docs + CHANGELOG)
                                                  │
                                                  ▼
                                          Wave 2
                                          ──────
                                          glovebox-p5wy  (Parse failure helpers)
                                                  │
                                                  ▼
                                          Wave 3 (parallel — 4 tasks)
                                          ──────────────────────────
                                          glovebox-v8up  (Attachments handler)
                                          glovebox-pphw  (Assignments processor)
                                          glovebox-zi71  (Feed processor) ── needs v8up
                                          glovebox-jl0b  (Messages processor) ── needs v8up
                                                  │
                                                  ▼
                                          Wave 4
                                          ──────
                                          glovebox-f7v3  (Telemetry)
                                                  │
                                                  ▼
                                          Wave 5 (terminal)
                                          ─────────────────
                                          glovebox-k5mr  (Connector + main wiring)
                                                  │
                                              ┌───┴───┐
                                              ▼       ▼
                                  glovebox-lwe5     glovebox-eo02
                                  (Dockerfile)      (Integration test)
```

Wave 1 fans out 7-way: 6 independent code tasks + 1 docs task. Wave 3 fans out 4-way once Wave 2 lands. Each wave ends at a synchronization point; the next wave dispatches after the current wave's beads are all closed.

---

## Conventions (Read Before Starting)

- **Test layout:** `*_test.go` alongside source files. Table-driven subtests in the existing connector style. Reuse the framework's test patterns (`StagingWriter` writes to `t.TempDir()`, read back metadata.json + content.raw).
- **Library access:** EVERY library call goes through the `SchoologyClient` interface (Task 2). Production code passes the real `*schoology.Client`; tests pass a fake. Never call `*schoology.Client` directly outside `client.go`.
- **Defensive slice copies** anywhere a `[]string` is returned and the caller could mutate it. Same idiom as spec 11 implementation: `append([]string(nil), src...)`.
- **No `git add -A`.** Stage only the files each task touches.
- **No `--no-verify`.** No emoji.
- **Push to gitlab**, not origin: `git push gitlab spec-12-schoology-connector`.
- **Beads hygiene:** `bd update <id> --claim` at task start, `bd close <id>` at task end.
- **PII discipline (spec 12 §10):** test config uses `k1`, `k2` for kid labels. Do NOT use family nicknames or legal names in any test data, even hand-written.
- **Subagent extraction hints:** when a piece of code is a clear candidate for the future "connector primitive base" (window scheduler, client interface pattern, parse-failure receipts, trigger handler), add `// TODO: candidate for extraction to connector primitive base type` as a comment.

---

## Task 1: Q-EARLY Routing Tag-Based Quarantine

**Beads:** `glovebox-8sc2`
**Depends on:** (none — Wave 1)
**Blocks:** Task 13 wiring (indirectly, via the tag emission in parse_failure helpers)

**Files:**
- Modify: `internal/routing/quarantine.go`
- Modify: `internal/routing/quarantine_test.go`
- Maybe modify: `internal/routing/pass.go` (the routing entry point that decides pass vs quarantine)

### Step 1.1 -- Claim the bead

- [ ] `bd update glovebox-8sc2 --claim`

### Step 1.2 -- Read the existing routing entry point

Find the function that decides between `RoutePass` and `RouteQuarantine`. Likely in `internal/routing/pass.go` or wherever scanner verdicts are translated to routing decisions.

- [ ] `grep -rn "RouteQuarantine\|RoutePass" internal/routing/ | head -20`
- [ ] Read the dispatcher function in full before editing.

### Step 1.3 -- Write failing tests

Add to `internal/routing/quarantine_test.go`:

```go
func TestParseStatusTag_RoutesToQuarantine(t *testing.T) {
	cases := []struct {
		name       string
		parseStatus string
	}{
		{"degraded", "degraded"},
		{"failure_receipt", "failure_receipt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := staging.ItemMetadata{
				Source:           "schoology",
				Sender:           "test",
				Subject:          "test",
				Timestamp:        time.Now().UTC(),
				DestinationAgent: "school",
				ContentType:      "text/plain",
				Tags:             map[string]string{"parse_status": tc.parseStatus},
			}
			// Simulate the routing decision with a passing scanner verdict
			// (the tag should override and force quarantine).
			verdict := engine.ScanResult{Verdict: engine.VerdictPass}
			decision := routingDecisionFor(meta, verdict) // expose-for-test or via existing dispatcher
			if decision != "quarantine" {
				t.Errorf("expected quarantine, got %q", decision)
			}
		})
	}
}

func TestParseStatusTag_OtherValues_DoNotForceQuarantine(t *testing.T) {
	// Items with parse_status set to something else, or unset, follow normal scanner verdict.
	meta := staging.ItemMetadata{
		Source:           "schoology",
		Sender:           "test",
		Subject:          "test",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "school",
		ContentType:      "text/plain",
		Tags:             map[string]string{"parse_status": "ok"},
	}
	verdict := engine.ScanResult{Verdict: engine.VerdictPass}
	decision := routingDecisionFor(meta, verdict)
	if decision != "pass" {
		t.Errorf("expected pass, got %q", decision)
	}
}
```

If the dispatcher function isn't named/shaped to be testable directly, expose a small helper (e.g., `func shouldForceQuarantine(meta ItemMetadata) bool`) and test that.

- [ ] Run: `go test ./internal/routing/ -run "TestParseStatusTag" -v`
- [ ] Expected: FAIL (no parse_status handling yet).

### Step 1.4 -- Implement the tag-based force-quarantine

In whichever function decides pass vs quarantine, add (BEFORE consulting the scanner verdict):

```go
// Tag-driven quarantine override (spec 12 §12 / Q-EARLY): items marked
// with parse_status: "degraded" or "failure_receipt" by the connector
// are forensic artifacts of broken upstream parsers and bypass the
// scanner verdict directly to quarantine.
if status, ok := meta.Tags["parse_status"]; ok {
	if status == "degraded" || status == "failure_receipt" {
		return "quarantine"  // or whatever the quarantine constant is
	}
}
```

Also update `audit.AuditEntry` to include a `QuarantineReason` string field (if it doesn't already), set to `"parse_status_tag"` when this path triggers, so the forensic record is clear.

### Step 1.5 -- Run tests + vet

- [ ] `go test ./internal/routing/... -v`
- [ ] `go vet ./internal/...`
- [ ] All pass / clean.

### Step 1.6 -- Commit + push + close

```bash
git add internal/routing/quarantine.go internal/routing/quarantine_test.go
# plus any other file you touched in routing
git commit -m "$(cat <<'EOF'
routing: tag-based quarantine for parse_status (glovebox-8sc2)

Q-EARLY prerequisite for spec 12 Schoology connector. When item metadata
carries tags.parse_status = "degraded" or "failure_receipt", routing
overrides the scanner verdict and sends the item to quarantine. Audit
log records QuarantineReason = "parse_status_tag" for forensic clarity.

Connectors that emit forensic parse-failure artifacts (spec 12 §11.3)
rely on this routing rule to preserve those items via the quarantine
path instead of dropping them.
EOF
)"
git push gitlab spec-12-schoology-connector
bd close glovebox-8sc2
```

**Exit criteria:** New tests pass; existing routing tests still pass; `go vet` clean; pushed.

---

## Task 2: SchoologyClient Interface + Production Wrapper

**Beads:** `glovebox-nilo`
**Depends on:** (none — Wave 1)
**Blocks:** Tasks 6, 7, 8, 9, 10 (any task that touches the library)

**Files:**
- Create: `connectors/schoology/client.go`
- Create: `connectors/schoology/client_test.go`

### Step 2.1 -- Claim the bead

- [ ] `bd update glovebox-nilo --claim`

### Step 2.2 -- Inspect the schoology-go library surface

- [ ] Read `../schoology-go/STATUS.md` and the public Go API to identify the exact method signatures we'll use.
- [ ] `grep -n "^func.*\*Client\)" ../schoology-go/*.go` to see what's public.

### Step 2.3 -- Write the interface and production wrapper

Create `connectors/schoology/client.go`:

```go
// Package schoology is the Glovebox connector for the Schoology LMS.
//
// All library access flows through the SchoologyClient interface; the
// production code wraps schoology-go's *schoology.Client, and tests pass
// a hand-rolled fake. This keeps connector-level tests offline and
// allows the library to evolve without churning connector tests.
//
// TODO: candidate for extraction to connector primitive base type
// (the interface-boundary pattern is shared with every future
// LMS-shaped connector).
package schoology

import (
	"context"
	"io"

	schoologylib "github.com/leftathome/schoology-go"
)

// SchoologyClient is the connector's narrowed view of the schoology-go
// library surface. Methods correspond 1:1 to library functions used by
// the connector; new library surfaces require widening this interface.
type SchoologyClient interface {
	GetChildren(ctx context.Context) ([]schoologylib.Child, error)
	GetCoursesForChild(ctx context.Context, uid int64) ([]schoologylib.Course, error)
	GetOverdueSubmissions(ctx context.Context, uid int64) ([]schoologylib.Assignment, error)
	GetFeed(ctx context.Context, uid int64) ([]schoologylib.FeedItem, error)
	GetInbox(ctx context.Context) ([]schoologylib.Message, error)
	DownloadAttachment(ctx context.Context, id int64) (io.ReadCloser, string, error)
}

// productionClient is the real SchoologyClient backed by schoology-go.
type productionClient struct {
	lib *schoologylib.Client
}

// NewProductionClient wraps a schoology-go client.
func NewProductionClient(lib *schoologylib.Client) SchoologyClient {
	return &productionClient{lib: lib}
}

func (c *productionClient) GetChildren(ctx context.Context) ([]schoologylib.Child, error) {
	return c.lib.GetChildren(ctx)
}

func (c *productionClient) GetCoursesForChild(ctx context.Context, uid int64) ([]schoologylib.Course, error) {
	return c.lib.GetCoursesForChild(ctx, uid)
}

func (c *productionClient) GetOverdueSubmissions(ctx context.Context, uid int64) ([]schoologylib.Assignment, error) {
	return c.lib.GetOverdueSubmissions(ctx, uid)
}

func (c *productionClient) GetFeed(ctx context.Context, uid int64) ([]schoologylib.FeedItem, error) {
	return c.lib.GetFeed(ctx, uid)
}

func (c *productionClient) GetInbox(ctx context.Context) ([]schoologylib.Message, error) {
	return c.lib.GetInbox(ctx)
}

func (c *productionClient) DownloadAttachment(ctx context.Context, id int64) (io.ReadCloser, string, error) {
	return c.lib.DownloadAttachment(ctx, id)
}
```

**Important:** The exact type names (`Assignment`, `FeedItem`, `Message`, etc.) must match what the library actually exports. If the library uses different names, adapt accordingly — the interface is OUR shape, not the library's.

If library function signatures differ (e.g., no `ctx` parameter, or different return shapes), adapt — but document in a code comment why the wrapper differs.

### Step 2.4 -- Write the mock helper for other tests

Create `connectors/schoology/client_test.go`:

```go
package schoology

import (
	"context"
	"errors"
	"io"
	"strings"

	schoologylib "github.com/leftathome/schoology-go"
)

// fakeClient is a hand-rolled SchoologyClient for connector tests.
// Each field can be set per-test to control what the fake returns.
type fakeClient struct {
	ChildrenFunc           func(ctx context.Context) ([]schoologylib.Child, error)
	CoursesForChildFunc    func(ctx context.Context, uid int64) ([]schoologylib.Course, error)
	OverdueSubmissionsFunc func(ctx context.Context, uid int64) ([]schoologylib.Assignment, error)
	FeedFunc               func(ctx context.Context, uid int64) ([]schoologylib.FeedItem, error)
	InboxFunc              func(ctx context.Context) ([]schoologylib.Message, error)
	DownloadFunc           func(ctx context.Context, id int64) (io.ReadCloser, string, error)
}

func (f *fakeClient) GetChildren(ctx context.Context) ([]schoologylib.Child, error) {
	if f.ChildrenFunc != nil {
		return f.ChildrenFunc(ctx)
	}
	return nil, errors.New("fakeClient.GetChildren not configured")
}

func (f *fakeClient) GetCoursesForChild(ctx context.Context, uid int64) ([]schoologylib.Course, error) {
	if f.CoursesForChildFunc != nil {
		return f.CoursesForChildFunc(ctx, uid)
	}
	return nil, errors.New("fakeClient.GetCoursesForChild not configured")
}

func (f *fakeClient) GetOverdueSubmissions(ctx context.Context, uid int64) ([]schoologylib.Assignment, error) {
	if f.OverdueSubmissionsFunc != nil {
		return f.OverdueSubmissionsFunc(ctx, uid)
	}
	return nil, errors.New("fakeClient.GetOverdueSubmissions not configured")
}

func (f *fakeClient) GetFeed(ctx context.Context, uid int64) ([]schoologylib.FeedItem, error) {
	if f.FeedFunc != nil {
		return f.FeedFunc(ctx, uid)
	}
	return nil, errors.New("fakeClient.GetFeed not configured")
}

func (f *fakeClient) GetInbox(ctx context.Context) ([]schoologylib.Message, error) {
	if f.InboxFunc != nil {
		return f.InboxFunc(ctx)
	}
	return nil, errors.New("fakeClient.GetInbox not configured")
}

func (f *fakeClient) DownloadAttachment(ctx context.Context, id int64) (io.ReadCloser, string, error) {
	if f.DownloadFunc != nil {
		return f.DownloadFunc(ctx, id)
	}
	return io.NopCloser(strings.NewReader("")), "application/octet-stream", nil
}

// Compile-time check.
var _ SchoologyClient = (*fakeClient)(nil)

// TestSchoologyClient_InterfaceImplementations is a sanity check that
// production wrappers and fakes both compile against the interface.
// No behavior is exercised here — this just guards against drift.
func TestSchoologyClient_InterfaceImplementations(t *testing.T) {
	var _ SchoologyClient = (*productionClient)(nil)
	var _ SchoologyClient = (*fakeClient)(nil)
}
```

**Note:** add `import "testing"` to the file.

### Step 2.5 -- Run tests + vet

- [ ] `go test ./connectors/schoology/...`
- [ ] `go vet ./connectors/schoology/...`
- [ ] Both clean.

### Step 2.6 -- Commit + push + close

```bash
git add connectors/schoology/client.go connectors/schoology/client_test.go
git commit -m "$(cat <<'EOF'
schoology: SchoologyClient interface + production wrapper (glovebox-nilo)

Defines the connector's narrowed view of schoology-go's surface. All
library access flows through this interface so tests can use a fake
without hitting Schoology. Production wrapper delegates to a real
*schoology.Client; mock fake is parameterized per-method for use by
other connector test files.

Marked as a candidate for extraction to a future connector primitive
base type (every LMS-shaped connector will have an analogous
interface-boundary).
EOF
)"
git push gitlab spec-12-schoology-connector
bd close glovebox-nilo
```

**Exit criteria:** Interface + production wrapper + fake compile and the sanity test passes; `go vet` clean.

---

## Task 3: Config Struct + JSON Loading + Validation

**Beads:** `glovebox-yg0m`
**Depends on:** (none — Wave 1)
**Blocks:** Tasks 7-10, 13

**Files:**
- Create: `connectors/schoology/config.go`
- Create: `connectors/schoology/config_test.go`

### Step 3.1 -- Claim

- [ ] `bd update glovebox-yg0m --claim`

### Step 3.2 -- Read the framework's BaseConfig + spec 12 §4.2

- [ ] Re-read `connector/runner.go` to see the BaseConfig shape and how it's loaded.
- [ ] Re-read spec 12 §4.2 (config.json shape) to confirm what fields we need.

### Step 3.3 -- Write failing tests

`connectors/schoology/config_test.go`:

```go
package schoology

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLoadConfig_Valid(t *testing.T) {
	j := `{
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
			{"match": "schoology:k1:assignment", "data_subject": "k1", "audience": ["household"], "destination": "school"},
			{"match": "schoology:message",                                "audience": ["guardians"], "destination": "school"}
		],
		"identity": {
			"provider": "schoology",
			"auth_method": "session_cookie",
			"tenant": "wagner-home"
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(j), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := ValidateConfig(&cfg); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(cfg.Kids) != 2 || cfg.Kids[0].Name != "k1" {
		t.Errorf("kids: got %+v", cfg.Kids)
	}
}

func TestValidateConfig_DuplicateKidNames(t *testing.T) {
	cfg := Config{
		Kids: []Kid{
			{Name: "k1", SchoologyUID: 1},
			{Name: "k1", SchoologyUID: 2},
		},
	}
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-name error, got %v", err)
	}
}

func TestValidateConfig_EmptyKidName(t *testing.T) {
	cfg := Config{
		Kids: []Kid{{Name: "", SchoologyUID: 1}},
	}
	if err := ValidateConfig(&cfg); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-name error, got %v", err)
	}
}

func TestValidateConfig_BadWindowTime(t *testing.T) {
	cfg := Config{
		Kids: []Kid{{Name: "k1", SchoologyUID: 1}},
		PollSchedule: PollSchedule{
			Windows: []PollWindow{{Start: "25:00", End: "26:00"}},
		},
	}
	if err := ValidateConfig(&cfg); err == nil {
		t.Fatalf("expected bad-window error")
	}
}

func TestValidateConfig_DefaultsApplied(t *testing.T) {
	cfg := Config{
		Kids: []Kid{{Name: "k1", SchoologyUID: 1}},
	}
	ApplyDefaults(&cfg)
	if cfg.Trigger.DebounceSeconds == 0 {
		t.Errorf("default DebounceSeconds not applied")
	}
	if cfg.Attachments.MaxSizeMB == 0 {
		t.Errorf("default MaxSizeMB not applied")
	}
	if cfg.ParseFailureThreshold == 0 {
		t.Errorf("default ParseFailureThreshold not applied")
	}
}
```

- [ ] Run: `go test ./connectors/schoology/ -run TestLoadConfig -v` → compile FAIL (types not defined).

### Step 3.4 -- Implement config

`connectors/schoology/config.go`:

```go
package schoology

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/leftathome/glovebox/connector"
)

// Config holds the connector's runtime config. Embeds connector.BaseConfig
// for the standard rules/identity/fetch-limits inherited from the framework.
type Config struct {
	connector.BaseConfig

	Kids                  []Kid          `json:"kids"`
	PollSchedule          PollSchedule   `json:"poll_schedule"`
	Trigger               TriggerConfig  `json:"trigger"`
	Attachments           AttachmentsConfig `json:"attachments"`
	ParseFailureThreshold int            `json:"parse_failure_threshold"`
}

// Kid maps an operator-chosen opaque label to a Schoology UID.
// Per spec 12 §10: avoid family nicknames / legal names in Name.
type Kid struct {
	Name         string `json:"name"`
	SchoologyUID int64  `json:"schoology_uid"`
}

// PollSchedule defines when the connector polls. See spec 12 §6.
type PollSchedule struct {
	WeekdaysOnly bool         `json:"weekdays_only"`
	Windows      []PollWindow `json:"windows"`
}

// PollWindow is one daily polling window. Times are local-time HH:MM
// strings interpreted in the connector's timezone (env: SCHOOLOGY_TIMEZONE).
type PollWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// TriggerConfig configures the HTTP trigger endpoint.
type TriggerConfig struct {
	DebounceSeconds int `json:"debounce_seconds"`
	ListenPort      int `json:"listen_port"`
}

// AttachmentsConfig caps per-attachment download size.
type AttachmentsConfig struct {
	MaxSizeMB int `json:"max_size_mb"`
}

// ApplyDefaults fills in zero-value fields with sensible defaults per
// spec 12 §4.2.
func ApplyDefaults(c *Config) {
	if c.Trigger.DebounceSeconds == 0 {
		c.Trigger.DebounceSeconds = 60
	}
	if c.Trigger.ListenPort == 0 {
		c.Trigger.ListenPort = 8081
	}
	if c.Attachments.MaxSizeMB == 0 {
		c.Attachments.MaxSizeMB = 25
	}
	if c.ParseFailureThreshold == 0 {
		c.ParseFailureThreshold = 10
	}
}

// ValidateConfig enforces spec 12 §4 invariants. Run at startup before
// the connector begins polling.
func ValidateConfig(c *Config) error {
	if len(c.Kids) == 0 {
		return fmt.Errorf("kids: at least one kid required")
	}
	seen := make(map[string]bool, len(c.Kids))
	for i, k := range c.Kids {
		if k.Name == "" {
			return fmt.Errorf("kids[%d]: empty name", i)
		}
		if k.SchoologyUID == 0 {
			return fmt.Errorf("kids[%d] (%s): missing schoology_uid", i, k.Name)
		}
		if seen[k.Name] {
			return fmt.Errorf("kids: duplicate name %q", k.Name)
		}
		seen[k.Name] = true
	}
	for i, w := range c.PollSchedule.Windows {
		if err := validateTimeOfDay(w.Start); err != nil {
			return fmt.Errorf("poll_schedule.windows[%d].start: %w", i, err)
		}
		if err := validateTimeOfDay(w.End); err != nil {
			return fmt.Errorf("poll_schedule.windows[%d].end: %w", i, err)
		}
	}
	return nil
}

func validateTimeOfDay(s string) error {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return fmt.Errorf("expected HH:MM, got %q", s)
	}
	hh, err := strconv.Atoi(parts[0])
	if err != nil || hh < 0 || hh > 23 {
		return fmt.Errorf("invalid hour in %q", s)
	}
	mm, err := strconv.Atoi(parts[1])
	if err != nil || mm < 0 || mm > 59 {
		return fmt.Errorf("invalid minute in %q", s)
	}
	return nil
}
```

### Step 3.5 -- Run tests + vet

- [ ] `go test ./connectors/schoology/...`
- [ ] `go vet ./connectors/schoology/...`

### Step 3.6 -- Commit + push + close

```bash
git add connectors/schoology/config.go connectors/schoology/config_test.go
git commit -m "$(cat <<'EOF'
schoology: Config struct + validation (glovebox-yg0m)

Embeds connector.BaseConfig for rules/identity/fetch_limits. Adds
schoology-specific fields per spec 12 §4.2: kids list, poll_schedule
with windows, trigger config, attachment size cap,
parse_failure_threshold. ValidateConfig enforces invariants (kids
non-empty, unique names, non-zero UIDs, valid HH:MM window times).
ApplyDefaults fills in spec-12-default values for omitted fields.
EOF
)"
git push gitlab spec-12-schoology-connector
bd close glovebox-yg0m
```

**Exit criteria:** Config types compile, validation tests pass, defaults applied correctly.

---

## Task 4: Window Scheduler + Splay

**Beads:** `glovebox-zzko`
**Depends on:** (none — Wave 1; uses Config types defined in Task 3 — Wave 1 task ordering should land yg0m before zzko's tests reference Config types, but they can be authored independently as long as the imports compile)

> **Wave 1 dispatch note:** When fanning out, dispatch yg0m before zzko if possible; otherwise zzko's tests will fail to compile until yg0m lands. The implementation itself doesn't depend on yg0m beyond importing `Config` types.

**Files:**
- Create: `connectors/schoology/scheduler.go`
- Create: `connectors/schoology/scheduler_test.go`

### Step 4.1 -- Claim

- [ ] `bd update glovebox-zzko --claim`

### Step 4.2 -- Write failing tests

`connectors/schoology/scheduler_test.go`:

```go
package schoology

import (
	"math/rand"
	"testing"
	"time"
)

func TestScheduler_NextPollTime_Determinism(t *testing.T) {
	// Same date + same RNG seed → same splay time, always.
	cfg := Config{
		PollSchedule: PollSchedule{
			Windows: []PollWindow{
				{Start: "07:00", End: "09:00"},
				{Start: "15:30", End: "17:30"},
			},
		},
	}
	tz := time.UTC
	// Tuesday 2026-05-19 in UTC.
	now := time.Date(2026, 5, 19, 5, 0, 0, 0, tz)
	rng1 := rand.New(rand.NewSource(42))
	rng2 := rand.New(rand.NewSource(42))
	t1, _ := computeNextPollTime(cfg, now, tz, rng1)
	t2, _ := computeNextPollTime(cfg, now, tz, rng2)
	if !t1.Equal(t2) {
		t.Errorf("non-deterministic: %v vs %v", t1, t2)
	}
	// Verify the time is in the first window.
	hh := t1.Hour()
	if hh < 7 || hh >= 9 {
		t.Errorf("not in morning window: %v (hour=%d)", t1, hh)
	}
}

func TestScheduler_SkipWeekends(t *testing.T) {
	cfg := Config{
		PollSchedule: PollSchedule{
			WeekdaysOnly: true,
			Windows: []PollWindow{
				{Start: "07:00", End: "09:00"},
			},
		},
	}
	tz := time.UTC
	// Saturday 2026-05-23.
	now := time.Date(2026, 5, 23, 5, 0, 0, 0, tz)
	rng := rand.New(rand.NewSource(1))
	next, skipped := computeNextPollTime(cfg, now, tz, rng)
	if !skipped {
		t.Errorf("expected Saturday to be skipped, got next=%v", next)
	}
	// Monday should NOT be skipped.
	mon := time.Date(2026, 5, 25, 5, 0, 0, 0, tz)
	next, skipped = computeNextPollTime(cfg, mon, tz, rng)
	if skipped {
		t.Errorf("Monday unexpectedly skipped")
	}
	if next.Weekday() != time.Monday {
		t.Errorf("expected Monday, got %v", next.Weekday())
	}
}

func TestScheduler_AfterAllWindows_RollsToNextDay(t *testing.T) {
	cfg := Config{
		PollSchedule: PollSchedule{
			Windows: []PollWindow{
				{Start: "07:00", End: "09:00"},
				{Start: "15:30", End: "17:30"},
			},
		},
	}
	tz := time.UTC
	// 6pm — past both windows.
	now := time.Date(2026, 5, 19, 18, 0, 0, 0, tz)
	rng := rand.New(rand.NewSource(1))
	next, _ := computeNextPollTime(cfg, now, tz, rng)
	if next.Day() == now.Day() {
		t.Errorf("expected next day, got %v same day as %v", next, now)
	}
}
```

- [ ] `go test ./connectors/schoology/ -run TestScheduler -v` → compile FAIL.

### Step 4.3 -- Implement scheduler

`connectors/schoology/scheduler.go`:

```go
package schoology

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// computeNextPollTime returns the next scheduled poll time given the
// current moment, the timezone, and a random source for splay.
// The second return value is true when "today" should be skipped
// entirely (weekend with weekdays_only=true); callers should still
// honor the returned time (which is the next valid day's first window).
//
// TODO: candidate for extraction to connector primitive base type
// (the windowed-with-splay pattern is generally useful).
func computeNextPollTime(cfg Config, now time.Time, tz *time.Location, rng *rand.Rand) (time.Time, bool) {
	now = now.In(tz)
	day := now

	// Skip ahead through weekend days if weekdays_only.
	skipped := false
	if cfg.PollSchedule.WeekdaysOnly && isWeekend(day) {
		skipped = true
		for isWeekend(day) {
			day = day.AddDate(0, 0, 1)
		}
		// First window of the next weekday, splayed.
		return splayedTimeIn(cfg.PollSchedule.Windows[0], day, tz, rng), true
	}

	// Find the next window today (or roll to tomorrow).
	for _, w := range cfg.PollSchedule.Windows {
		t := splayedTimeIn(w, day, tz, rng)
		if t.After(now) {
			return t, skipped
		}
	}

	// All today's windows are past — roll to next day (or next weekday).
	day = day.AddDate(0, 0, 1)
	for cfg.PollSchedule.WeekdaysOnly && isWeekend(day) {
		day = day.AddDate(0, 0, 1)
	}
	return splayedTimeIn(cfg.PollSchedule.Windows[0], day, tz, rng), skipped
}

func isWeekend(t time.Time) bool {
	wd := t.Weekday()
	return wd == time.Saturday || wd == time.Sunday
}

func splayedTimeIn(w PollWindow, day time.Time, tz *time.Location, rng *rand.Rand) time.Time {
	startH, startM := parseHHMM(w.Start)
	endH, endM := parseHHMM(w.End)
	startSec := startH*3600 + startM*60
	endSec := endH*3600 + endM*60
	if endSec <= startSec {
		// Defensive: invalid window. Validation should have caught this.
		endSec = startSec + 60
	}
	splay := rng.Intn(endSec - startSec)
	totalSec := startSec + splay
	return time.Date(day.Year(), day.Month(), day.Day(),
		totalSec/3600, (totalSec%3600)/60, totalSec%60, 0, tz)
}

func parseHHMM(s string) (int, int) {
	parts := strings.Split(s, ":")
	hh, _ := strconv.Atoi(parts[0])
	mm, _ := strconv.Atoi(parts[1])
	return hh, mm
}

// loadTimezone parses the SCHOOLOGY_TIMEZONE env var (default
// America/Los_Angeles per spec 12 §6.1).
func loadTimezone(envVar string) (*time.Location, error) {
	name := envVar
	if name == "" {
		name = "America/Los_Angeles"
	}
	tz, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", name, err)
	}
	return tz, nil
}
```

### Step 4.4 -- Run tests + vet + commit + push + close

- [ ] `go test ./connectors/schoology/ -run TestScheduler -v` → PASS
- [ ] `go test ./connectors/schoology/...`
- [ ] `go vet ./connectors/schoology/...`
- [ ] Commit (message references `glovebox-zzko`), push, `bd close glovebox-zzko`.

**Exit criteria:** Determinism, weekend skip, and day-rollover tests all pass.

---

## Task 5: Checkpoint Helpers

**Beads:** `glovebox-rjeo`
**Depends on:** (none — Wave 1)
**Blocks:** Tasks 7-10

**Files:**
- Create: `connectors/schoology/checkpoint.go`
- Create: `connectors/schoology/checkpoint_test.go`

### Step 5.1 -- Claim

- [ ] `bd update glovebox-rjeo --claim`

### Step 5.2 -- Implement (test stubs and code together; small scope)

`connectors/schoology/checkpoint.go`:

```go
package schoology

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/leftathome/glovebox/connector"
)

// CheckpointKey builds the framework Checkpoint key for a given content
// surface. Returns "<surface>:<scope>:last_id" -- scope is the kid
// label for per-kid surfaces, or empty for parent-level (messages,
// message attachments).
func CheckpointKey(surface, scope string) string {
	if scope == "" {
		return surface + ":last_id"
	}
	return surface + ":" + scope + ":last_id"
}

// LastSeenID reads the highest-seen ID for a content surface. Returns 0
// when the checkpoint is fresh (i.e., first poll).
func LastSeenID(cp connector.Checkpoint, surface, scope string) int64 {
	v, ok := cp.Load(CheckpointKey(surface, scope))
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		slog.Warn("schoology checkpoint parse error",
			"key", CheckpointKey(surface, scope), "value", v, "error", err)
		return 0
	}
	return n
}

// SaveLastSeenID advances the checkpoint after a successful Commit().
// MUST be called only after Commit() returns nil per the framework
// per-item-checkpoint discipline (spec 05 §3.2).
func SaveLastSeenID(cp connector.Checkpoint, surface, scope string, id int64) error {
	return cp.Save(CheckpointKey(surface, scope), strconv.FormatInt(id, 10))
}

// ShouldStage returns true when an item's ID is strictly greater than
// the checkpoint. When false (id <= last seen), logs a warning if id
// is non-zero but below the threshold (likely out-of-order arrival)
// and increments the dropped-below-checkpoint metric.
//
// TODO: candidate for extraction to connector primitive base type
// (highest-ID dedup is shared with PowerSchool and future LMS connectors).
func ShouldStage(cp connector.Checkpoint, surface, scope string, id int64) bool {
	if id == 0 {
		// Invalid item ID; let the caller decide what to do.
		return false
	}
	last := LastSeenID(cp, surface, scope)
	if id > last {
		return true
	}
	if last > 0 && id < last {
		// Below-threshold: log + metric (metric increment will be done
		// by the caller since it has access to the connector's Metrics).
		slog.Warn("schoology item below checkpoint",
			"surface", surface, "scope", scope,
			"item_id", id, "checkpoint", last)
	}
	return false
}

// FormatID is a small helper for callers who have a string ID.
func FormatID(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse id %q: %w", s, err)
	}
	return n, nil
}
```

`connectors/schoology/checkpoint_test.go`:

```go
package schoology

import (
	"testing"

	"github.com/leftathome/glovebox/connector"
)

func TestCheckpointKey(t *testing.T) {
	if got := CheckpointKey("assignment", "k1"); got != "assignment:k1:last_id" {
		t.Errorf("per-kid key: got %q", got)
	}
	if got := CheckpointKey("message", ""); got != "message:last_id" {
		t.Errorf("parent-level key: got %q", got)
	}
}

func TestShouldStage_Advances(t *testing.T) {
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !ShouldStage(cp, "feed", "k1", 100) {
		t.Errorf("first item should be stageable")
	}
	if err := SaveLastSeenID(cp, "feed", "k1", 100); err != nil {
		t.Fatal(err)
	}
	if ShouldStage(cp, "feed", "k1", 100) {
		t.Errorf("equal id should be skipped")
	}
	if ShouldStage(cp, "feed", "k1", 99) {
		t.Errorf("below-checkpoint id should be skipped")
	}
	if !ShouldStage(cp, "feed", "k1", 101) {
		t.Errorf("higher id should be stageable")
	}
}

func TestShouldStage_ZeroIDRejected(t *testing.T) {
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if ShouldStage(cp, "feed", "k1", 0) {
		t.Errorf("zero ID should be rejected")
	}
}
```

### Step 5.3 -- Test, vet, commit, push, close

- [ ] `go test ./connectors/schoology/ -run "TestCheckpoint|TestShouldStage" -v` → PASS
- [ ] Commit, push, close `glovebox-rjeo`.

**Exit criteria:** Key formats, advance behavior, and zero-ID rejection all tested green.

---

## Task 6: Parse Failure Helpers

**Beads:** `glovebox-p5wy`
**Depends on:** `glovebox-nilo` (Task 2 — needs SchoologyClient error type signatures)
**Blocks:** Tasks 7, 8, 9

**Files:**
- Create: `connectors/schoology/parse_failure.go`
- Create: `connectors/schoology/parse_failure_test.go`

### Step 6.1 -- Claim

- [ ] `bd update glovebox-p5wy --claim`

### Step 6.2 -- Implement

Per spec 12 §11.3:

`connectors/schoology/parse_failure.go`:

```go
package schoology

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// ReceiptDedup tracks (parser, error_class) tuples observed during a
// single pollNow() invocation. Reset at the start of each poll.
// One receipt emitted per unique (parser, error_class) per poll.
type ReceiptDedup struct {
	mu    sync.Mutex
	seen  map[string]int  // key = parser + "|" + error_class -> count of occurrences
	bytes map[string][]byte
}

// NewReceiptDedup returns a fresh dedup tracker for one poll.
func NewReceiptDedup() *ReceiptDedup {
	return &ReceiptDedup{
		seen:  make(map[string]int),
		bytes: make(map[string][]byte),
	}
}

// Observe records a parse failure. Returns true if this is the FIRST
// occurrence of (parser, error_class) in this poll (caller should
// emit a receipt). Subsequent calls return false but increment the
// affected-count.
func (r *ReceiptDedup) Observe(parser, errorClass string, sampleBytes []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := parser + "|" + errorClass
	count := r.seen[key]
	r.seen[key] = count + 1
	if count == 0 {
		// First occurrence; keep the sample bytes.
		r.bytes[key] = sampleBytes
		return true
	}
	return false
}

// AffectedCount returns the count of occurrences for a given key.
func (r *ReceiptDedup) AffectedCount(parser, errorClass string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[parser+"|"+errorClass]
}

// BuildReceiptOptions constructs ItemOptions for a parse failure
// receipt staging item per spec 12 §11.3.
func BuildReceiptOptions(parser, errorClass, errorMsg, sourceURL, targetKid, targetContentType, libVersion string, sample []byte, affectedCount int) connector.ItemOptions {
	subject := fmt.Sprintf("[parse-failure] %s: %s (target: %s/%s)",
		parser, summarize(errorMsg, 80), pickTarget(targetKid), targetContentType)
	return connector.ItemOptions{
		Source:           "schoology-parse-failure",
		Sender:           "schoology-connector",
		Subject:          subject,
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "school",
		ContentType:      "application/octet-stream",
		Tags: map[string]string{
			"parse_status":              "failure_receipt",
			"parser":                     parser,
			"error":                      truncate(errorMsg, 1024),
			"error_class":                errorClass,
			"source_url":                 sourceURL,
			"target_kid":                 targetKid,
			"target_content_type":        targetContentType,
			"schoology_library_version":  libVersion,
			"affected_count":             fmt.Sprintf("%d", affectedCount),
		},
		Audience: []string{"guardians"},
	}
}

// BuildDegradedOptions constructs ItemOptions for a partial parse
// failure (item identified but some fields missing) per spec 12 §11.3.
// `base` is the ItemOptions the caller would have emitted on success;
// this function adds the parse_status tags and clears missing fields.
func BuildDegradedOptions(base connector.ItemOptions, errorMsg, missingFields, itemID, itemType string) connector.ItemOptions {
	if base.Tags == nil {
		base.Tags = make(map[string]string)
	}
	base.Tags["parse_status"] = "degraded"
	base.Tags["parse_error"] = truncate(errorMsg, 1024)
	base.Tags["parse_missing_fields"] = missingFields
	base.Tags["schoology_item_id"] = itemID
	base.Tags["schoology_item_type"] = itemType
	return base
}

// SchemaDriftCounter tracks consecutive polls that produced zero items
// while logging parse errors. Used to escalate persistent breakage to
// PermanentError per spec 12 §11.4.
type SchemaDriftCounter struct {
	mu        sync.Mutex
	count     int
	threshold int
}

// NewSchemaDriftCounter returns a counter that escalates after
// `threshold` consecutive empty-with-errors polls.
func NewSchemaDriftCounter(threshold int) *SchemaDriftCounter {
	return &SchemaDriftCounter{threshold: threshold}
}

// RecordPoll updates the counter. itemsProduced is the total staged
// items in this poll (excluding parse-failure receipts); errorsLogged
// is true if any library/parse error was logged.
func (s *SchemaDriftCounter) RecordPoll(itemsProduced int, errorsLogged bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if itemsProduced > 0 {
		s.count = 0
		return
	}
	if errorsLogged {
		s.count++
	}
}

// ShouldEscalate returns true when the counter has crossed the threshold.
func (s *SchemaDriftCounter) ShouldEscalate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count >= s.threshold
}

// Threshold returns the configured threshold (for logging).
func (s *SchemaDriftCounter) Threshold() int { return s.threshold }

// Count returns the current count.
func (s *SchemaDriftCounter) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func pickTarget(kid string) string {
	if kid == "" {
		return "inbox"
	}
	return kid
}

func summarize(s string, max int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}
```

`connectors/schoology/parse_failure_test.go`:

```go
package schoology

import (
	"strings"
	"testing"
)

func TestReceiptDedup_FirstObservationReturnsTrue(t *testing.T) {
	d := NewReceiptDedup()
	if !d.Observe("feed_body_extractor", "empty_string", []byte("sample 1")) {
		t.Error("first observation should return true")
	}
	if d.Observe("feed_body_extractor", "empty_string", []byte("sample 2")) {
		t.Error("second observation should return false")
	}
	if got := d.AffectedCount("feed_body_extractor", "empty_string"); got != 2 {
		t.Errorf("affected count: got %d, want 2", got)
	}
}

func TestReceiptDedup_DistinctParsers(t *testing.T) {
	d := NewReceiptDedup()
	if !d.Observe("feed_body_extractor", "empty_string", nil) {
		t.Error()
	}
	if !d.Observe("message_body_decoder", "empty_string", nil) {
		t.Error("distinct parsers should each get their first-true")
	}
}

func TestBuildReceiptOptions_StructureMatchesSpec(t *testing.T) {
	opts := BuildReceiptOptions(
		"feed_body_extractor",
		"empty_string",
		"feed body returned empty",
		"https://example.schoology.com/home/feed",
		"k1",
		"feed",
		"v0.1.0",
		[]byte("..."),
		3,
	)
	if opts.Source != "schoology-parse-failure" {
		t.Errorf("Source: got %q", opts.Source)
	}
	if opts.ContentType != "application/octet-stream" {
		t.Errorf("ContentType: got %q", opts.ContentType)
	}
	if opts.Tags["parse_status"] != "failure_receipt" {
		t.Errorf("parse_status tag: got %q", opts.Tags["parse_status"])
	}
	if opts.Tags["parser"] != "feed_body_extractor" {
		t.Errorf("parser tag: got %q", opts.Tags["parser"])
	}
	if opts.Tags["affected_count"] != "3" {
		t.Errorf("affected_count tag: got %q", opts.Tags["affected_count"])
	}
	if len(opts.Audience) != 1 || opts.Audience[0] != "guardians" {
		t.Errorf("Audience: got %v", opts.Audience)
	}
	if !strings.Contains(opts.Subject, "[parse-failure]") {
		t.Errorf("Subject: got %q (should contain '[parse-failure]')", opts.Subject)
	}
}

func TestBuildDegradedOptions_AddsTags(t *testing.T) {
	base := connector.ItemOptions{
		Source:           "schoology",
		Sender:           "test",
		Subject:          "test",
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "school",
		ContentType:      "text/plain",
	}
	degraded := BuildDegradedOptions(base, "body extractor empty", "body", "12345", "feed")
	if degraded.Tags["parse_status"] != "degraded" {
		t.Errorf("got %q", degraded.Tags["parse_status"])
	}
	if degraded.Tags["parse_missing_fields"] != "body" {
		t.Errorf("got %q", degraded.Tags["parse_missing_fields"])
	}
	if degraded.Tags["schoology_item_id"] != "12345" {
		t.Errorf("got %q", degraded.Tags["schoology_item_id"])
	}
}

func TestSchemaDriftCounter_EscalatesAfterThreshold(t *testing.T) {
	c := NewSchemaDriftCounter(3)
	c.RecordPoll(0, true)
	c.RecordPoll(0, true)
	if c.ShouldEscalate() {
		t.Error("should not escalate yet")
	}
	c.RecordPoll(0, true)
	if !c.ShouldEscalate() {
		t.Error("should escalate at threshold")
	}
}

func TestSchemaDriftCounter_ResetsOnSuccess(t *testing.T) {
	c := NewSchemaDriftCounter(3)
	c.RecordPoll(0, true)
	c.RecordPoll(0, true)
	c.RecordPoll(5, false) // success
	c.RecordPoll(0, true)
	if c.ShouldEscalate() {
		t.Error("should not escalate after successful poll reset")
	}
}

func TestSchemaDriftCounter_NoIncrementOnQuietPoll(t *testing.T) {
	// Zero items but ALSO no errors logged — not schema drift, just a quiet day.
	c := NewSchemaDriftCounter(3)
	c.RecordPoll(0, false)
	c.RecordPoll(0, false)
	c.RecordPoll(0, false)
	if c.ShouldEscalate() {
		t.Error("quiet polls (no errors) should not escalate")
	}
}
```

Add imports to test file: `"github.com/leftathome/glovebox/connector"`, `"time"`.

### Step 6.3 -- Test, vet, commit, push, close

- [ ] `go test ./connectors/schoology/ -run "Receipt|Degraded|SchemaDrift" -v` → PASS
- [ ] Commit, push, close `glovebox-p5wy`.

**Exit criteria:** Dedup, receipt structure, degraded structure, counter all tested.

---

## Task 7: Assignments Processor

**Beads:** `glovebox-pphw`
**Depends on:** `glovebox-nilo`, `glovebox-yg0m`, `glovebox-rjeo`, `glovebox-p5wy`
**Blocks:** Tasks 12, 13

**Files:**
- Create: `connectors/schoology/assignments.go`
- Create: `connectors/schoology/assignments_test.go`

### Step 7.1 -- Claim

- [ ] `bd update glovebox-pphw --claim`

### Step 7.2 -- Implement

`connectors/schoology/assignments.go` (sketch — adapt to library's actual `Assignment` struct):

```go
package schoology

import (
	"context"
	"fmt"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// ProcessAssignments fetches the kid's overdue+upcoming assignments,
// dedupes via checkpoint, and stages each new item. Returns (count,
// errors-logged). Per spec 12 §7.1.
func ProcessAssignments(
	ctx context.Context,
	client SchoologyClient,
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	cp connector.Checkpoint,
	dedup *ReceiptDedup,
	kid Kid,
	libVersion string,
) (int, bool) {
	assignments, err := client.GetOverdueSubmissions(ctx, kid.SchoologyUID)
	if err != nil {
		emitReceiptIfNew(writer, matcher, dedup, "GetOverdueSubmissions",
			classifyError(err), err.Error(),
			fmt.Sprintf("schoology://overdue/%d", kid.SchoologyUID),
			kid.Name, "assignment", libVersion, nil)
		return 0, true
	}
	matchKey := fmt.Sprintf("schoology:%s:assignment", kid.Name)
	result, ok := matcher.Match(matchKey)
	if !ok {
		// No rule matched; this is a config error.
		return 0, false
	}
	count := 0
	for _, a := range assignments {
		// Adapt these field accesses to the actual schoology-go Assignment type.
		id := a.ID
		if !ShouldStage(cp, "assignment", kid.Name, id) {
			continue
		}
		opts := connector.ItemOptions{
			Source:           "schoology",
			Sender:           fmt.Sprintf("%s -- %s", a.CourseTitle, a.TeacherName),
			Subject:          a.Title,
			Timestamp:        a.CreatedAt,
			DestinationAgent: result.Destination,
			ContentType:      "text/plain",
			Tags: map[string]string{
				"course":     a.CourseTitle,
				"due_date":   a.DueDate.UTC().Format(time.RFC3339),
				"status":     a.Status,
			},
			DataSubject: result.DataSubject,
			Audience:    result.Audience,
		}
		// Body content: description + due-date prose.
		body := fmt.Sprintf("%s\n\nDue: %s", a.Description, a.DueDate.Format(time.RFC3339))
		if err := stageItem(writer, opts, []byte(body)); err != nil {
			// Per-item commit failure; log + continue (don't advance checkpoint).
			continue
		}
		_ = SaveLastSeenID(cp, "assignment", kid.Name, id)
		count++
	}
	return count, false
}

// stageItem is a thin helper: NewItem -> WriteContent -> Commit.
func stageItem(w *connector.StagingWriter, opts connector.ItemOptions, body []byte) error {
	item, err := w.NewItem(opts)
	if err != nil {
		return err
	}
	if err := item.WriteContent(body); err != nil {
		return err
	}
	return item.Commit()
}

// emitReceiptIfNew calls dedup.Observe() and stages a receipt only on
// the first occurrence per poll.
func emitReceiptIfNew(
	w *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	dedup *ReceiptDedup,
	parser, errorClass, errorMsg, sourceURL, targetKid, targetContentType, libVersion string,
	sample []byte,
) {
	if !dedup.Observe(parser, errorClass, sample) {
		return
	}
	count := dedup.AffectedCount(parser, errorClass)
	opts := BuildReceiptOptions(parser, errorClass, errorMsg, sourceURL, targetKid, targetContentType, libVersion, sample, count)
	// Find the parse-failure routing rule.
	result, ok := matcher.Match("schoology-parse-failure:" + parser)
	if !ok {
		// Wildcard catch-all.
		result, ok = matcher.Match("schoology-parse-failure:*")
	}
	if !ok {
		return
	}
	opts.DestinationAgent = result.Destination
	if len(result.Audience) > 0 {
		opts.Audience = result.Audience
	}
	_ = stageItem(w, opts, sample)
}

// classifyError maps a library error to a short error_class string.
// Adapt to actual library error types/sentinels.
func classifyError(err error) string {
	// Placeholder; refine when wiring against real library errors.
	return "unknown"
}
```

**Note on type adaptation:** the `a.ID`, `a.Title`, `a.CourseTitle`, etc. field names are placeholders. Look at the actual `Assignment` type in schoology-go and adapt. If a field doesn't exist, omit the corresponding tag.

### Step 7.3 -- Tests

`connectors/schoology/assignments_test.go`:

```go
package schoology

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	schoologylib "github.com/leftathome/schoology-go"
)

func TestProcessAssignments_StagesNewItems(t *testing.T) {
	stagingDir := t.TempDir()
	stateDir := t.TempDir()
	writer, _ := connector.NewStagingWriter(stagingDir, "schoology")
	cp, _ := connector.NewCheckpoint(stateDir)
	matcher := connector.NewRuleMatcher([]connector.Rule{
		{Match: "schoology:k1:assignment", Destination: "school", DataSubject: "k1", Audience: []string{"household"}},
	})

	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, uid int64) ([]schoologylib.Assignment, error) {
			return []schoologylib.Assignment{
				{ID: 1001, Title: "Math 4.3", CourseTitle: "Math", TeacherName: "Mr. R", DueDate: time.Now(), Status: "upcoming"},
				{ID: 1002, Title: "English essay", CourseTitle: "English", TeacherName: "Ms. K", DueDate: time.Now(), Status: "upcoming"},
			}, nil
		},
	}
	dedup := NewReceiptDedup()
	count, errsLogged := ProcessAssignments(context.Background(), client, writer, matcher, cp, dedup,
		Kid{Name: "k1", SchoologyUID: 12345678}, "v0.1.0")
	if errsLogged {
		t.Errorf("unexpected error logged")
	}
	if count != 2 {
		t.Errorf("count: got %d, want 2", count)
	}
	// Verify checkpoint advanced.
	if got := LastSeenID(cp, "assignment", "k1"); got != 1002 {
		t.Errorf("checkpoint: got %d, want 1002", got)
	}
	_ = filepath.Join
}

func TestProcessAssignments_DedupesAcrossPolls(t *testing.T) {
	// Same setup but call twice; second call should produce zero items.
	stagingDir := t.TempDir()
	stateDir := t.TempDir()
	writer, _ := connector.NewStagingWriter(stagingDir, "schoology")
	cp, _ := connector.NewCheckpoint(stateDir)
	matcher := connector.NewRuleMatcher([]connector.Rule{
		{Match: "schoology:k1:assignment", Destination: "school", DataSubject: "k1", Audience: []string{"household"}},
	})
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, uid int64) ([]schoologylib.Assignment, error) {
			return []schoologylib.Assignment{
				{ID: 1001, Title: "Math 4.3", CourseTitle: "Math", TeacherName: "Mr. R", DueDate: time.Now(), Status: "upcoming"},
			}, nil
		},
	}
	dedup := NewReceiptDedup()
	ProcessAssignments(context.Background(), client, writer, matcher, cp, dedup, Kid{Name: "k1", SchoologyUID: 1}, "v0.1.0")
	dedup = NewReceiptDedup()
	count, _ := ProcessAssignments(context.Background(), client, writer, matcher, cp, dedup, Kid{Name: "k1", SchoologyUID: 1}, "v0.1.0")
	if count != 0 {
		t.Errorf("second poll should be 0, got %d", count)
	}
}

func TestProcessAssignments_LibraryErrorEmitsReceipt(t *testing.T) {
	stagingDir := t.TempDir()
	stateDir := t.TempDir()
	writer, _ := connector.NewStagingWriter(stagingDir, "schoology")
	cp, _ := connector.NewCheckpoint(stateDir)
	matcher := connector.NewRuleMatcher([]connector.Rule{
		{Match: "schoology:k1:assignment", Destination: "school", DataSubject: "k1", Audience: []string{"household"}},
		{Match: "schoology-parse-failure:*", Destination: "school", Audience: []string{"guardians"}},
	})
	client := &fakeClient{
		OverdueSubmissionsFunc: func(ctx context.Context, uid int64) ([]schoologylib.Assignment, error) {
			return nil, errors.New("parser exploded")
		},
	}
	dedup := NewReceiptDedup()
	count, errsLogged := ProcessAssignments(context.Background(), client, writer, matcher, cp, dedup,
		Kid{Name: "k1", SchoologyUID: 1}, "v0.1.0")
	if !errsLogged {
		t.Errorf("expected errsLogged=true")
	}
	if count != 0 {
		t.Errorf("count: got %d, want 0", count)
	}
	// Verify a receipt was staged.
	entries := readStagedItems(t, stagingDir)
	foundReceipt := false
	for _, m := range entries {
		if m.Source == "schoology-parse-failure" {
			foundReceipt = true
			break
		}
	}
	if !foundReceipt {
		t.Errorf("expected a parse-failure receipt to be staged")
	}
}

// readStagedItems is a helper shared across processor tests.
// Define it in a shared _test.go file if it doesn't already exist.
```

### Step 7.4 -- Test + vet + commit + push + close

- [ ] `go test ./connectors/schoology/ -run "TestProcessAssignments" -v` → PASS
- [ ] Commit, push, close `glovebox-pphw`.

**Exit criteria:** new + dedup + library-error-receipt tests all pass.

---

## Task 8: Feed Processor

**Beads:** `glovebox-zi71`
**Depends on:** `glovebox-nilo`, `glovebox-yg0m`, `glovebox-rjeo`, `glovebox-p5wy`, `glovebox-v8up`
**Blocks:** Tasks 12, 13

**Files:**
- Create: `connectors/schoology/feed.go`
- Create: `connectors/schoology/feed_test.go`

### Step 8.1 -- Implement

Pattern mirrors Task 7. Differences:
- Match key: `schoology:<kid>:feed`.
- `content.raw` is the post body, HTML→plaintext via `connector/content`'s `HTMLToText`.
- After staging the parent item, call `ProcessAttachments(ctx, client, writer, matcher, post.Attachments, kid.Name, "schoology:<kid>:attachment")` (Task 10).
- Tags: `course`, `post_type`.
- Skip if `len(post.Attachments) > 0 && processedAttachmentsErr != nil` -- actually no, attachments errors are non-fatal; the parent item still flows. Track separately.

(Full implementation sketch omitted for brevity; follow the pattern from Task 7 + the §7.2 field mapping table.)

### Step 8.2 -- Tests

Mirror Task 7's three tests (new items, dedup, library-error-receipt). Add one for attachment processing being invoked.

### Step 8.3 -- Commit + push + close

```bash
git commit -m "schoology: feed posts processor (glovebox-zi71)"
git push gitlab spec-12-schoology-connector
bd close glovebox-zi71
```

**Exit criteria:** feed tests pass; attachments handler is invoked when posts have attachments.

---

## Task 9: Messages Processor

**Beads:** `glovebox-jl0b`
**Depends on:** `glovebox-nilo`, `glovebox-yg0m`, `glovebox-rjeo`, `glovebox-p5wy`, `glovebox-v8up`
**Blocks:** Tasks 12, 13

**Files:**
- Create: `connectors/schoology/messages.go`
- Create: `connectors/schoology/messages_test.go`

### Step 9.1 -- Implement

Pattern mirrors Tasks 7 & 8. Differences from feed:
- Match key: `schoology:message` (NO kid; spec 12 §7.3).
- `DataSubject` left empty (will be set by the rule, which omits it).
- Audience comes from the matching rule (which is `[guardians]`).
- Message attachments use match key `schoology:message-attachment` with no kid.

### Step 9.2 -- Tests + commit + push + close

Same pattern as Task 7/8.

**Exit criteria:** messages processor tests pass; checkpoint advances on `message:last_id`; data_subject is empty; audience is `[guardians]`.

---

## Task 10: Attachments Handler

**Beads:** `glovebox-v8up`
**Depends on:** `glovebox-nilo`, `glovebox-yg0m`, `glovebox-rjeo`
**Blocks:** Tasks 8, 9

**Files:**
- Create: `connectors/schoology/attachments.go`
- Create: `connectors/schoology/attachments_test.go`

### Step 10.1 -- Implement

```go
package schoology

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"

	"github.com/leftathome/glovebox/connector"
)

// ProcessAttachments downloads each attachment in `atts`, enforces the
// size cap from cfg, dedupes via per-(surface, scope) checkpoint, and
// stages each one as its own item. surface is "feed-attachment" or
// "message-attachment"; scope is the kid name or empty for parent-level.
// Returns count of successfully staged attachments.
func ProcessAttachments(
	ctx context.Context,
	client SchoologyClient,
	writer *connector.StagingWriter,
	matcher *connector.RuleMatcher,
	cp connector.Checkpoint,
	atts []Attachment,
	parentID int64,
	parentType string,
	matchKey string,
	checkpointSurface string,
	checkpointScope string,
	maxSizeMB int,
) int {
	count := 0
	maxBytes := int64(maxSizeMB) * 1024 * 1024
	for _, a := range atts {
		if !ShouldStage(cp, checkpointSurface, checkpointScope, a.ID) {
			continue
		}
		rc, mimeType, err := client.DownloadAttachment(ctx, a.ID)
		if err != nil {
			slog.Warn("schoology DownloadAttachment failed",
				"attachment_id", a.ID, "filename", a.Filename, "error", err)
			continue
		}
		data, sizeErr := readLimited(rc, maxBytes+1)
		rc.Close()
		if int64(len(data)) > maxBytes {
			slog.Warn("schoology attachment too large",
				"attachment_id", a.ID, "filename", a.Filename, "size_bytes", len(data), "max_bytes", maxBytes)
			continue
		}
		if sizeErr != nil && sizeErr != io.EOF {
			slog.Warn("schoology attachment read error",
				"attachment_id", a.ID, "filename", a.Filename, "error", sizeErr)
			continue
		}
		result, ok := matcher.Match(matchKey)
		if !ok {
			continue
		}
		opts := connector.ItemOptions{
			Source:           "schoology",
			Sender:           a.ParentSender,
			Subject:          fmt.Sprintf("%s — %s", a.ParentSubject, a.Filename),
			Timestamp:        a.ParentTimestamp,
			DestinationAgent: result.Destination,
			ContentType:      mimeType,
			Tags: map[string]string{
				"parent_id":   strconv.FormatInt(parentID, 10),
				"parent_type": parentType,
				"filename":    a.Filename,
				"size_bytes":  strconv.Itoa(len(data)),
			},
			DataSubject: result.DataSubject,
			Audience:    result.Audience,
		}
		if err := stageItem(writer, opts, data); err != nil {
			continue
		}
		_ = SaveLastSeenID(cp, checkpointSurface, checkpointScope, a.ID)
		count++
	}
	return count
}

// Attachment is a per-attachment shape adapted from the library's type.
// May be inlined into the feed/messages processors if the library
// exposes the same shape directly.
type Attachment struct {
	ID              int64
	Filename        string
	ParentSender    string
	ParentSubject   string
	ParentTimestamp time.Time
}

// readLimited reads up to limit bytes from r. Returns the bytes read
// + the error from the read (nil or io.EOF on clean end).
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}
```

Add `import "time"` to the file.

### Step 10.2 -- Tests + commit + push + close

Tests should cover:
- Happy path: attachments staged, checkpoint advances.
- Size cap: 26MB attachment skipped with warning, no staging.
- Dedup: same attachment ID on next poll → skipped.
- Download error: skipped, no panic.

**Exit criteria:** Size-cap + dedup + error-skip all tested green.

---

## Task 11: HTTP Trigger Endpoint

**Beads:** `glovebox-gqup`
**Depends on:** (none — Wave 1)
**Blocks:** Task 13

**Files:**
- Create: `connectors/schoology/trigger.go`
- Create: `connectors/schoology/trigger_test.go`

### Step 11.1 -- Implement

```go
package schoology

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// TriggerHandler implements connector.Listener for the POST /v1/poll
// endpoint. Bearer-token auth, 60-second debounce. On accepted requests,
// signals the connector to poll via the supplied channel.
//
// TODO: candidate for extraction to connector primitive base type
// (any read-mostly connector might want a trigger endpoint).
type TriggerHandler struct {
	BearerToken      string
	DebounceDuration time.Duration
	PollSignal       chan<- struct{}

	mu          sync.Mutex
	lastTrigger time.Time
}

// NewTriggerHandler constructs a handler.
func NewTriggerHandler(token string, debounce time.Duration, pollSignal chan<- struct{}) *TriggerHandler {
	return &TriggerHandler{
		BearerToken:      token,
		DebounceDuration: debounce,
		PollSignal:       pollSignal,
	}
}

// Handler returns the http.Handler implementing the Listener interface.
func (h *TriggerHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/poll", h.handlePoll)
	return mux
}

func (h *TriggerHandler) handlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	auth := r.Header.Get("Authorization")
	expected := "Bearer " + h.BearerToken
	if auth != expected {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	h.mu.Lock()
	since := time.Since(h.lastTrigger)
	if since < h.DebounceDuration {
		remaining := h.DebounceDuration - since
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(remaining.Seconds())+1))
		h.mu.Unlock()
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	h.lastTrigger = time.Now()
	h.mu.Unlock()

	// Non-blocking send; drop if the channel buffer is full (the consumer
	// is already going to poll).
	select {
	case h.PollSignal <- struct{}{}:
	default:
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"poll_queued_at": time.Now().UTC().Format(time.RFC3339),
	})
}
```

### Step 11.2 -- Tests

```go
func TestTriggerHandler_Accepts(t *testing.T) {
	signal := make(chan struct{}, 1)
	h := NewTriggerHandler("secret", time.Minute, signal)
	req := httptest.NewRequest(http.MethodPost, "/v1/poll", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("status: got %d, want 202", rec.Code)
	}
	select {
	case <-signal:
		// good
	case <-time.After(100 * time.Millisecond):
		t.Error("PollSignal not fired")
	}
}

func TestTriggerHandler_RejectsBadToken(t *testing.T) {
	h := NewTriggerHandler("secret", time.Minute, make(chan struct{}, 1))
	req := httptest.NewRequest(http.MethodPost, "/v1/poll", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d", rec.Code)
	}
}

func TestTriggerHandler_Debounces(t *testing.T) {
	signal := make(chan struct{}, 1)
	h := NewTriggerHandler("secret", time.Minute, signal)

	// First trigger: accepted.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/poll", nil)
	req1.Header.Set("Authorization", "Bearer secret")
	rec1 := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusAccepted {
		t.Errorf("first trigger: got %d", rec1.Code)
	}

	// Second trigger (immediately): debounced.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/poll", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	rec2 := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("second trigger: got %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("Retry-After missing")
	}
}
```

### Step 11.3 -- Test + vet + commit + push + close

- [ ] `go test ./connectors/schoology/ -run "Trigger" -v` → PASS
- [ ] Commit + push + close `glovebox-gqup`.

**Exit criteria:** Accept, reject-bad-token, and debounce tests all pass.

---

## Task 12: Telemetry (Metrics + OTel Traces + Structured Logs)

**Beads:** `glovebox-f7v3`
**Depends on:** `glovebox-pphw`, `glovebox-zi71`, `glovebox-jl0b`
**Blocks:** Task 13

**Files:**
- Create: `connectors/schoology/telemetry.go`
- Create: `connectors/schoology/telemetry_test.go`
- Modify: the processors from Tasks 7-10 to call the metrics + tracing helpers

### Step 12.1 -- Build the schoology-specific metrics + tracer

```go
package schoology

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/metric"
	// ... prometheus exporter via the existing framework Metrics
)

// Telemetry holds the connector's schoology-specific metrics and tracer.
type Telemetry struct {
	PollsTotal              metric.Int64Counter
	ParseFailuresTotal      metric.Int64Counter
	ParseFailureReceipts    metric.Int64Counter
	AttachmentsBytesTotal   metric.Int64Counter
	AttachmentsSkippedTotal metric.Int64Counter
	TriggerRequestsTotal    metric.Int64Counter
	ViewChildSwitchesTotal  metric.Int64Counter
	Tracer                  trace.Tracer
}

// NewTelemetry wires the metrics into the OTel meter provider and
// returns a tracer from the global provider.
func NewTelemetry(meterName, tracerName string) (*Telemetry, error) {
	meter := otel.Meter(meterName)
	pollsTotal, err := meter.Int64Counter("schoology_polls_total",
		metric.WithDescription("Polls performed, by trigger source"))
	if err != nil { return nil, err }
	// ... similar for the rest
	return &Telemetry{
		PollsTotal: pollsTotal,
		// ...
		Tracer: otel.Tracer(tracerName),
	}, nil
}
```

### Step 12.2 -- Wire into processors

Add metric increments at the right spots:
- `assignments.go`: on each successful stage, increment `connector_items_produced_total` (framework) AND on each parse-failure-receipt, increment `schoology_parse_failure_receipts_total`.
- `feed.go`: same pattern.
- `messages.go`: same.
- `attachments.go`: on each download size pass, increment `schoology_attachments_downloaded_bytes_total{kid, content_type}` with the byte count.
- `trigger.go`: on each request, increment `schoology_trigger_requests_total{outcome}`.

Add tracing spans:
- `pollNow()` wraps everything in `tracer.Start(ctx, "schoology.poll")` with `trigger_source` attribute.
- Per-kid: `tracer.Start(ctx, "schoology.poll.kid")` with `kid` attribute.
- Per library call: `tracer.Start(ctx, "schoology.lib.GetOverdueSubmissions")` with `uid` attribute.
- Per staging commit: `tracer.Start(ctx, "schoology.staging.commit")` with `item_id`/`destination`/`data_subject` attributes.

### Step 12.3 -- Tests

Verify metrics are registered and incrementable. Sketch:

```go
func TestTelemetry_MetricsRegister(t *testing.T) {
	tel, err := NewTelemetry("test", "test")
	if err != nil {
		t.Fatalf("NewTelemetry: %v", err)
	}
	tel.PollsTotal.Add(context.Background(), 1)
	// No assertion here beyond no-panic; full metric assertions are
	// handled by integration tests.
}
```

### Step 12.4 -- Commit + push + close

- [ ] `go test ./connectors/schoology/...` → all PASS
- [ ] `bd close glovebox-f7v3`

**Exit criteria:** Metrics constructible, processors call them at the right spots, traces wrap pollNow + library calls.

---

## Task 13: Connector Struct + pollNow + main.go

**Beads:** `glovebox-k5mr`
**Depends on:** all prior tasks (Wave 5)
**Blocks:** Tasks 14, 15

**Files:**
- Create: `connectors/schoology/connector.go`
- Create: `connectors/schoology/connector_test.go`
- Create: `connectors/schoology/main.go`

### Step 13.1 -- Implement SchoologyConnector

Per spec 12 §3, implement `Connector` + `Watcher` + `Listener`. The struct holds:

- `client SchoologyClient`
- `cfg Config`
- `writer *connector.StagingWriter`
- `matcher *connector.RuleMatcher`
- `cp connector.Checkpoint`
- `tel *Telemetry`
- `tz *time.Location`
- `rng *rand.Rand`
- `trigger *TriggerHandler`
- `pollSignal chan struct{}` (buffered, 1)
- `driftCounter *SchemaDriftCounter`
- `libVersion string`

Methods:
- `Poll(ctx, cp) error` — catch-up; calls `pollNow(ctx, "catch_up")`.
- `Watch(ctx, cp) error` — long-running loop. Each iteration:
  1. Compute next poll time via `computeNextPollTime`.
  2. `select` on `time.After(next - now)` vs `c.pollSignal` vs `ctx.Done()`.
  3. On wake (either path): call `pollNow(ctx, "scheduled" or "triggered")`.
  4. If `driftCounter.ShouldEscalate()`: return PermanentError.
- `Handler() http.Handler` — returns `c.trigger.Handler()`.

The `pollNow(ctx, source string) error` function:
1. Start root span.
2. Increment polls counter with source label.
3. Build a fresh `ReceiptDedup` for this poll.
4. For each kid: switch view, process assignments, process feed.
5. Process inbox messages (parent-level).
6. Drive `driftCounter.RecordPoll(itemsProduced, errorsLogged)`.
7. End root span.

### Step 13.2 -- main.go

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	schoologylib "github.com/leftathome/schoology-go"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connectors/schoology"
)

func main() {
	configFile := os.Getenv("GLOVEBOX_CONNECTOR_CONFIG")
	if configFile == "" {
		configFile = "/etc/connector/config.json"
	}
	cfgBytes, err := os.ReadFile(configFile)
	if err != nil {
		slog.Error("read config", "error", err)
		os.Exit(1)
	}
	var cfg schoology.Config
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil {
		slog.Error("parse config", "error", err)
		os.Exit(1)
	}
	schoology.ApplyDefaults(&cfg)
	if err := schoology.ValidateConfig(&cfg); err != nil {
		slog.Error("validate config", "error", err)
		os.Exit(1)
	}

	credsPath := os.Getenv("SCHOOLOGY_CREDENTIALS_FILE")
	host := os.Getenv("SCHOOLOGY_HOST")
	creds, err := schoologylib.LoadCredentials(credsPath)
	if err != nil {
		slog.Error("load credentials", "path", credsPath, "error", err)
		os.Exit(1)
	}
	lib, err := schoologylib.NewClient(host, creds)
	if err != nil {
		slog.Error("new client", "error", err)
		os.Exit(1)
	}
	client := schoology.NewProductionClient(lib)

	c := schoology.NewConnector(client, cfg)

	connector.Run(connector.Options{
		Name:       "schoology",
		StagingDir: os.Getenv("GLOVEBOX_STAGING_DIR"),
		StateDir:   os.Getenv("GLOVEBOX_STATE_DIR"),
		ConfigFile: configFile,
		Connector:  c,
		Setup: func(cc connector.ConnectorContext) error {
			c.Wire(cc) // attach writer/matcher/etc.
			return nil
		},
	})
}
```

### Step 13.3 -- connector_test.go: convergence test

Test that all three call paths (`Poll`, `Watch`'s scheduled trigger, `Handler`'s HTTP trigger) call the same internal pollNow function with distinguishable `trigger_source` values. Verify via captured metric counter increments.

### Step 13.4 -- Test + vet + commit + push + close

- [ ] `go test ./connectors/schoology/... -v`
- [ ] `go vet ./connectors/schoology/...`
- [ ] Commit, push, close `glovebox-k5mr`.

**Exit criteria:** connector builds; all three interface methods present; convergence test passes.

---

## Task 14: Dockerfile + Container Smoke Test

**Beads:** `glovebox-lwe5`
**Depends on:** `glovebox-k5mr`
**Blocks:** (none)

**Files:**
- Create: `connectors/schoology/Dockerfile`
- Create: `connectors/schoology/config.json` (example)
- Optionally: a small `container_test.sh` or CI workflow stub

### Step 14.1 -- Dockerfile

Multi-stage build matching existing connector pattern:

```dockerfile
FROM docker.io/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/schoology/

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /connector /connector
COPY connectors/schoology/config.json /etc/connector/config.json
USER nonroot:nonroot
ENTRYPOINT ["/connector"]
```

### Step 14.2 -- config.json (example)

Mirror spec 12 §4.2 with sensible defaults. Use `k1`/`k2` opaque kid labels and placeholder Schoology UIDs.

### Step 14.3 -- Container smoke test

Build the image, start it briefly with mocked env, hit `/healthz`, verify 200. Either bash script or Go integration test depending on CI shape.

### Step 14.4 -- Commit + push + close

- [ ] `docker build -f connectors/schoology/Dockerfile -t glovebox-schoology:test .` from repo root.
- [ ] Build succeeds.
- [ ] Commit + push + close `glovebox-lwe5`.

**Exit criteria:** image builds clean; smoke test verifies healthz.

---

## Task 15: End-to-End Integration Test

**Beads:** `glovebox-eo02`
**Depends on:** `glovebox-k5mr`
**Blocks:** (none)

**Files:**
- Create: `connectors/schoology/integration_test.go`

### Step 15.1 -- Test scenario

End-to-end via the fake `SchoologyClient`:

1. Set up a `SchoologyConnector` with a fake that returns:
   - 2 assignments for k1 on first poll, 1 NEW one on second poll
   - 3 feed posts for k1 (one with 2 attachments)
   - 1 inbox message for the parent (with 1 attachment)
   - On third poll, return a library error to verify receipts.
2. Call `pollNow()` three times via the appropriate trigger.
3. Verify staging directory contents:
   - Counts per content surface.
   - Tag presence (data_subject, audience, parse_status).
   - Attachment items emitted separately.
   - Parse failure receipt staged on third poll.
4. Verify checkpoint state after each poll.

### Step 15.2 -- Commit + push + close

- [ ] `go test ./connectors/schoology/ -run TestIntegration -v` → PASS
- [ ] Commit, push, close `glovebox-eo02`.

**Exit criteria:** end-to-end test exercises all four content surfaces, dedup across polls, receipt emission, and checkpoint persistence.

---

## Task 16: AUTH-RECOVERY.md + CHANGELOG

**Beads:** `glovebox-h6ef`
**Depends on:** (none — can run in Wave 1 in parallel)

**Files:**
- Create: `docs/AUTH-RECOVERY.md`
- Modify: `CHANGELOG.md`

### Step 16.1 -- AUTH-RECOVERY.md

Operator-friendly procedure per spec 12 §5.1 / §11.1. Step-by-step:

1. Detect: `kubectl logs` shows the PermanentError message.
2. On workstation: run `schoology-go auth.Login <SCHOOLOGY_HOST>`.
3. Library writes credentials JSON to default path.
4. Open 1Password item `schoology-session-<household>`, replace contents with the new JSON.
5. ESO syncs within ~60s; pod auto-restarts.
6. Verify via `kubectl logs` that the connector resumed.

### Step 16.2 -- CHANGELOG.md

Add v0.6.0 entry above v0.5.0:

```markdown
## [0.6.0] - 2026-05-XX

### Added

- **Schoology connector** (`connectors/schoology/`) -- ingests assignments,
  faculty feed posts, inbox messages, and attachments from a parent
  Schoology account via the schoology-go library. Single-container deployment
  serving all kids in a household. See `docs/specs/12-schoology-connector-design.md`.
- Routing-layer tag-based quarantine: items with `tags.parse_status` set to
  `degraded` or `failure_receipt` are routed to quarantine regardless of
  scanner verdict. Audit log records `QuarantineReason: "parse_status_tag"`.
  Enables forensic preservation of parse failures for bug-patrol.
- `docs/AUTH-RECOVERY.md` -- operator procedure for Schoology session
  expiry recovery.

### Notes

- Schoology session cookies expire approximately every 14 days; the connector
  surfaces expiry as PermanentError with a recovery-instruction message and
  exits non-zero so K8s reports CrashLoopBackOff for alerting.
- Uses spec 11 v1.2 audience vocabulary (`guardians`, `caregivers`); inbox
  messages route with `audience: ["guardians"]` standalone (parent-level,
  no specific kid).
- Per-kid `data_subject` values are operator-chosen opaque labels
  (`k1`/`k2`) to avoid placing PII (nicknames, legal names) into
  metadata and audit logs.
- Marks several patterns as candidates for extraction to a future
  "connector primitive base type" when PowerSchool (spec 13) lands.
```

### Step 16.3 -- Commit + push + close

```bash
git add docs/AUTH-RECOVERY.md CHANGELOG.md
git commit -m "docs: AUTH-RECOVERY.md + CHANGELOG v0.6.0 (glovebox-h6ef)"
git push gitlab spec-12-schoology-connector
bd close glovebox-h6ef
```

**Exit criteria:** operator doc exists and is accurate; CHANGELOG entry committed.

---

## Final Verification

After all tasks complete:

- [ ] `git log --oneline gitlab/main..HEAD` -- expect spec + plan + 16 task commits.
- [ ] `bd list --status=open | grep "spec 12"` -- empty.
- [ ] Close umbrella: `bd close glovebox-qhlk --reason="v0.6.0 connector + impl landed via task beads"`.
- [ ] `go test ./... && go vet ./...` clean.
- [ ] Open MR on gitlab: `glab mr create --target-branch=main --title="Spec 12 / v0.6.0: Schoology connector"`.
- [ ] Watch MR + main pipelines green.
- [ ] Tag v0.6.0; create gitlab release.
- [ ] Once github auth restored: push main + v0.6.0 to origin.

---

## Notes

- **Parallel dispatch**: Wave 1's 7 tasks touch disjoint files and may be dispatched in parallel via superpowers:dispatching-parallel-agents. Same for Wave 3's 4 tasks (after Wave 2 lands).
- **Library type adaptation**: the field names in the code sketches (`a.ID`, `a.Title`, etc.) are placeholders. Each task that touches library types MUST verify against the actual `schoology-go` types and adapt. Don't paste sketch code blindly.
- **Audit log QuarantineReason**: Task 1 may need to add this field to `audit.AuditEntry` if it doesn't already exist. If it doesn't, that's a tiny audit.go update inside Task 1's scope.
- **Per-task subagent dispatch**: each task has clear file boundaries; a subagent can be dispatched per task with the task's full text as its prompt. The two-stage review (spec compliance + code quality) per the subagent-driven-development skill applies.
