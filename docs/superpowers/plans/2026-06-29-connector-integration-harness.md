# Connector In-Cluster Integration/Smoke Harness — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the shared in-cluster integration/smoke harness + first connectors so every connector can be proven against its live source, per `docs/superpowers/specs/2026-06-29-connector-integration-harness-design.md`.

**Architecture:** A small `connector/integrationtest` package provides *stage-and-readback* over a real `connector.StagingWriter` rooted at `t.TempDir()` (no `StagingItem` internals), assertion helpers over the committed items, and `RequireIntegration`/`RequireCreds` skip guards. Per-connector `//go:build integration` tests wire the real connector (`NewConnector` → `ConnectorContext` → `Poll`) into that writer and assert a full stage round-trip. A scheduled-only GitLab `integration` stage runs them in-cluster with Vault/ESO secrets.

**Tech Stack:** Go (`go test`, build tags), the existing `connector` framework (`StagingWriter`, `Connector`/`Poll`, `RuleMatcher`, `Metrics`), GitLab CI, Vault + ESO.

**Beads:** epic `glovebox-lyku`; tasks `lyku.1` (B1 harness), `lyku.2` (B2 registry), `lyku.3` (B3 CI stage), `lyku.4` (B4 schoology live), `lyku.5` (B5 no-cred batch).

---

## File structure

- Create `connector/integrationtest/harness.go` — `StagedItem` type, `StageToTempDir`, readback.
- Create `connector/integrationtest/assert.go` — `AssertStagedAtLeast`, `AssertContentNonEmpty`, `AssertRouting`, `AssertHasSidecar`.
- Create `connector/integrationtest/skip.go` — `RequireIntegration`, `RequireCreds`.
- Create `connector/integrationtest/harness_test.go` — unit tests (no live network, no build tag).
- Create `docs/connectors/integration-credentials.md` — credential registry (B2).
- Create `connectors/<name>/live_integration_test.go` (`//go:build integration`) for schoology (B4) and rss/hackernews/arxiv/semantic-scholar (B5). Named `live_integration_test.go` to avoid colliding with schoology's existing `integration_test.go`.
- Modify `.gitlab-ci.yml` (+ private `homelab/ci-templates`) — add the scheduled `integration` stage (B3).

The harness package is connector-agnostic and fully unit-testable now (Task 1). The per-connector tests (Tasks 4-5) are `package <connectorpkg>`/`package main` files authored now and verified by compile + skip-clean (no-cred ones also run live locally); their credentialed live runs happen in the nightly CI stage (Task 3).

**Deferred / out of this plan:** the container **image smoke** (spec §4.3) is represented only as a registry column (`image smoke: no` by default) and is not implemented here — added per connector later where it's justified. Task 3.2 (private `homelab/ci-templates` ESO binding) and Task 4's live run depend on cluster/creds access and are marked deferred where they appear.

---

## Task 1 (lyku.1): shared harness `connector/integrationtest`

**Files:**
- Create: `connector/integrationtest/harness.go`, `assert.go`, `skip.go`
- Test: `connector/integrationtest/harness_test.go`

Note: this package is `package integrationtest` and imports `github.com/leftathome/glovebox/connector` and `.../internal/staging`. Its tests are plain unit tests (NO build tag) so they run under `go test ./...`.

- [ ] **Step 1.1: Write the failing harness round-trip test**

`connector/integrationtest/harness_test.go`:
```go
package integrationtest

import (
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

func TestStageToTempDir_RoundTrip(t *testing.T) {
	w, readback := StageToTempDir(t, "unit")

	item, err := w.NewItem(connector.ItemOptions{
		Source:           "unit-source",
		Sender:           "tester",
		Subject:          "hello",
		Timestamp:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		DestinationAgent: "messaging",
		ContentType:      "text/plain",
		DataSubject:      "k1",
		Audience:         []string{"household"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := item.WriteContent([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := item.Commit(); err != nil {
		t.Fatal(err)
	}

	items := readback()
	if len(items) != 1 {
		t.Fatalf("want 1 staged item, got %d", len(items))
	}
	if items[0].Meta.DestinationAgent != "messaging" {
		t.Errorf("DestinationAgent = %q", items[0].Meta.DestinationAgent)
	}
}
```

- [ ] **Step 1.2: Run it — verify FAIL**

Run: `go test ./connector/integrationtest/ -run TestStageToTempDir_RoundTrip`
Expected: build failure (`undefined: StageToTempDir`).

- [ ] **Step 1.3: Implement `harness.go`**

```go
// Package integrationtest provides a shared harness for connector
// integration/smoke tests: stage a connector's output to a temp dir via a
// real StagingWriter and read the committed items back. See
// docs/superpowers/specs/2026-06-29-connector-integration-harness-design.md.
package integrationtest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/staging"
)

// StagedItem is one committed staging item read back from disk.
type StagedItem struct {
	Dir         string                 // absolute item directory
	Meta        staging.ItemMetadata   // parsed metadata.json
	ContentPath string                 // <dir>/content.raw (may not exist)
	Sidecars    []string               // filenames other than content.raw/metadata.json
}

// StageToTempDir returns a StagingWriter rooted at t.TempDir() and a
// readback func that returns every committed item. connectorName is
// required by connector.NewStagingWriter.
func StageToTempDir(t *testing.T, connectorName string) (*connector.StagingWriter, func() []StagedItem) {
	t.Helper()
	dir := t.TempDir()
	w, err := connector.NewStagingWriter(dir, connectorName)
	if err != nil {
		t.Fatalf("integrationtest: NewStagingWriter: %v", err)
	}
	return w, func() []StagedItem { return readStaged(t, dir) }
}

func readStaged(t *testing.T, dir string) []StagedItem {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("integrationtest: read staging dir: %v", err)
	}
	var out []StagedItem
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // skip .tmp and hidden
		}
		itemDir := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(filepath.Join(itemDir, "metadata.json"))
		if err != nil {
			t.Fatalf("integrationtest: read metadata.json in %s: %v", e.Name(), err)
		}
		var meta staging.ItemMetadata
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("integrationtest: parse metadata.json in %s: %v", e.Name(), err)
		}
		si := StagedItem{Dir: itemDir, Meta: meta, ContentPath: filepath.Join(itemDir, "content.raw")}
		files, _ := os.ReadDir(itemDir)
		for _, f := range files {
			n := f.Name()
			if n == "content.raw" || n == "metadata.json" {
				continue
			}
			si.Sidecars = append(si.Sidecars, n)
		}
		out = append(out, si)
	}
	return out
}
```

- [ ] **Step 1.4: Run it — verify PASS**

Run: `go test ./connector/integrationtest/ -run TestStageToTempDir_RoundTrip`
Expected: PASS.

- [ ] **Step 1.5: Commit**

```bash
git add connector/integrationtest/harness.go connector/integrationtest/harness_test.go
git commit -m "feat(integrationtest): stage-and-readback harness (glovebox-lyku.1)"
```

- [ ] **Step 1.6: Write failing assertion-helper tests**

Add to `harness_test.go`: `TestAssertStagedAtLeast`, `TestAssertContentNonEmpty`, `TestAssertRouting` — each stages an item (as in 1.1) and calls the helper against the read-back item, asserting it passes for the happy case. (Use a `*testing.T`-shaped fake or assert no-panic; simplest: call helpers with the real `t` for the passing case.)

- [ ] **Step 1.7: Run — verify FAIL** (`undefined: AssertStagedAtLeast`, …)

- [ ] **Step 1.8: Implement `assert.go`**

```go
package integrationtest

import (
	"os"
	"slices"
	"testing"
)

func AssertStagedAtLeast(t *testing.T, items []StagedItem, n int) {
	t.Helper()
	if len(items) < n {
		t.Errorf("staged %d items, want >= %d", len(items), n)
	}
}

func AssertContentNonEmpty(t *testing.T, item StagedItem) {
	t.Helper()
	fi, err := os.Stat(item.ContentPath)
	if err != nil || fi.Size() == 0 {
		t.Errorf("content.raw missing or empty in %s (err=%v)", item.Dir, err)
	}
}

// WantRouting is the resolved routing to assert on the committed metadata.
type WantRouting struct {
	DataSubject      string
	Audience         []string
	DestinationAgent string
}

// AssertRouting does field equality on the ALREADY-RESOLVED metadata (the
// RuleMatcher ran during Commit); it does not re-run the matcher.
func AssertRouting(t *testing.T, item StagedItem, want WantRouting) {
	t.Helper()
	if item.Meta.DestinationAgent != want.DestinationAgent {
		t.Errorf("DestinationAgent = %q, want %q", item.Meta.DestinationAgent, want.DestinationAgent)
	}
	if item.Meta.DataSubject != want.DataSubject {
		t.Errorf("DataSubject = %q, want %q", item.Meta.DataSubject, want.DataSubject)
	}
	if !slices.Equal(item.Meta.Audience, want.Audience) {
		t.Errorf("Audience = %v, want %v", item.Meta.Audience, want.Audience)
	}
}

// AssertHasSidecar asserts an enrichment sidecar FILE (e.g.
// "content.extracted.md") was produced on disk by the real Commit pipeline
// (runEnrichmentPipeline). It intentionally checks the file, not the
// metadata Enrichments[] record (simpler, and proves the artifact exists).
func AssertHasSidecar(t *testing.T, item StagedItem, name string) {
	t.Helper()
	for _, s := range item.Sidecars {
		if s == name {
			return
		}
	}
	t.Errorf("sidecar %q not found in %s (have %v)", name, item.Dir, item.Sidecars)
}
```

- [ ] **Step 1.9: Run — verify PASS; commit**

```bash
go test ./connector/integrationtest/
git add connector/integrationtest/assert.go connector/integrationtest/harness_test.go
git commit -m "feat(integrationtest): assertion helpers (glovebox-lyku.1)"
```

- [ ] **Step 1.10: Write failing skip-guard tests**

Use this concrete, deterministic pattern (a guard that calls `t.Skip` runs
`runtime.Goexit`, so the deferred closure still runs and `Skipped()` reports
true; `t.Setenv` overrides any ambient value):
```go
func TestRequireIntegration_SkipsWhenUnset(t *testing.T) {
	t.Setenv("GLOVEBOX_INTEGRATION", "")
	var skipped bool
	t.Run("guarded", func(st *testing.T) {
		defer func() { skipped = st.Skipped() }()
		RequireIntegration(st)
		st.Error("RequireIntegration did not skip")
	})
	if !skipped {
		t.Fatal("expected RequireIntegration to skip when GLOVEBOX_INTEGRATION unset")
	}
}

func TestRequireCreds_SkipsWhenMissing(t *testing.T) {
	t.Setenv("FOO_TOKEN", "")
	var skipped bool
	t.Run("guarded", func(st *testing.T) {
		defer func() { skipped = st.Skipped() }()
		RequireCreds(st, "FOO_TOKEN")
		st.Error("RequireCreds did not skip")
	})
	if !skipped {
		t.Fatal("expected RequireCreds to skip when FOO_TOKEN empty")
	}
}
```

- [ ] **Step 1.11: Run — verify FAIL; implement `skip.go`**

Skip/abort messages follow the repo's WHAT/CHECK/FIX convention (spec §9).
```go
package integrationtest

import (
	"os"
	"strings"
	"testing"
)

// RequireIntegration skips unless GLOVEBOX_INTEGRATION=1, so the
// build-tagged suite makes no live calls during an ordinary
// `go test -tags integration ./...` run.
func RequireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("GLOVEBOX_INTEGRATION") != "1" {
		t.Skip("integration disabled\n" +
			"  CHECK: env GLOVEBOX_INTEGRATION\n" +
			"  FIX:   run with GLOVEBOX_INTEGRATION=1 (nightly/manual CI does this)")
	}
}

// RequireCreds skips when any named env var is empty.
func RequireCreds(t *testing.T, envVars ...string) {
	t.Helper()
	for _, k := range envVars {
		if os.Getenv(k) == "" {
			t.Skipf("missing credential\n"+
				"  CHECK: env %s\n"+
				"  FIX:   provide %s (ESO-synced in the in-cluster job; see docs/connectors/integration-credentials.md)", k, k)
		}
	}
}

// SkipOnRateLimit turns an upstream throttle into a skip-with-warning so a
// nightly run does not go red on provider rate limiting (spec §9). Best
// effort: matches common 429/rate-limit error text. Call as:
//   if err := c.Poll(ctx, cp); err != nil { integrationtest.SkipOnRateLimit(t, err); t.Fatal(err) }
func SkipOnRateLimit(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "429") || strings.Contains(s, "rate limit") || strings.Contains(s, "too many requests") {
		t.Skipf("upstream rate-limited (skip, not fail)\n"+
			"  CHECK: %v\n"+
			"  FIX:   re-run later; the source throttled this account", err)
	}
}
```
Per-test bounded retry + wall-clock timeout (spec §9): each connector test wraps
its `Poll` with a `context.WithTimeout(ctx, 60*time.Second)` and may retry a
transient error once before failing; documented in Tasks 4-5.

- [ ] **Step 1.12: Run full package, vet, staticcheck; commit**

```bash
go test ./connector/integrationtest/ && go vet ./connector/integrationtest/
git add connector/integrationtest/skip.go connector/integrationtest/harness_test.go
git commit -m "feat(integrationtest): skip guards + harness complete (glovebox-lyku.1)"
```

---

## Task 2 (lyku.2): credential registry doc

**Files:** Create `docs/connectors/integration-credentials.md`

- [ ] **Step 2.1** Write the registry: the table schema from spec §5 (connector, cred source, vault path, secret shape, image smoke) with a row for each of the 23 source connectors. Fill `cred source` for all (test-account / real-readonly / none per §5); leave vault path / secret shape as `TBD (private ci-templates)` where not yet provisioned. Note `schoology-auth-refresher` excluded (auth helper) and importers (apple/mbox/walhelm) as a separate file-driven track. State that infra-sensitive ESO/Vault specifics live in the private `homelab/ci-templates`.
- [ ] **Step 2.2** Review for completeness (every source connector has a row); commit:
```bash
git add docs/connectors/integration-credentials.md
git commit -m "docs(connectors): integration credential registry (glovebox-lyku.2)"
```

---

## Task 3 (lyku.3): GitLab scheduled `integration` stage

**Files:** Modify `.gitlab-ci.yml`; add the credentialed/in-cluster specifics in the private `homelab/ci-templates` (not the public-mirrored file).

- [ ] **Step 3.1** Add an `integration` stage to `.gitlab-ci.yml` after `test`, with a job template gated `rules: if $CI_PIPELINE_SOURCE == "schedule" || $CI_PIPELINE_SOURCE == "web"`, **no `needs:` on `test`/`build`**, that exports `GLOVEBOX_INTEGRATION=1` and runs `go test -tags integration ./connectors/<name>/...`. Public file references the private template for ESO secret wiring + node selectors.
- [ ] **Step 3.2** In the private `homelab/ci-templates`, bind each connector job's ESO secret (per the registry) and the in-cluster runner/node pin. (Infra repo; out of this repo's tree.)
- [ ] **Step 3.3** Validate `glab ci lint` (or YAML parse) passes; commit the public `.gitlab-ci.yml` change:
```bash
git add .gitlab-ci.yml
git commit -m "ci: scheduled in-cluster connector integration stage (glovebox-lyku.3)"
```
- [ ] **Step 3.4 (DEFERRED — after Task 5 lands + private template wired)** A manual (`web`) pipeline run executes the no-cred connector jobs green (they exist only once Task 5 lands) and lists skipped connectors. `lyku.3` closes here.

---

## Task 4 (lyku.4): schoology live integration test (reference)

**Files:** Create `connectors/schoology/live_integration_test.go` (`//go:build integration`). Keep the existing `integration_test.go` untouched.

> **Compile warning:** schoology's existing `integration_test.go` has **no
> build tag** (it's `package schoology`), so under `-tags integration` both
> files compile together. The new file MUST **reuse** the existing helpers
> (`integrationMatcher`, `integrationConfig`, `newTestCheckpoint`) — do NOT
> redeclare them (duplicate-symbol error). Use `integrationtest.StageToTempDir`
> for staging instead of the file-local `newTestWriter`.

- [ ] **Step 4.1** Author a live test that: `RequireIntegration(t)`; `RequireCreds(t, <schoology creds env, e.g. credentials-file path + Browserless URL/token>)`; loads the connector's effective config via the existing `integrationConfig(t)`; `w, readback := integrationtest.StageToTempDir(t, "schoology")`; wires the **real** schoology client (the production client constructor — NOT the file-local `fakeClient`; naming/constructing it is **deferred until creds + the real client wiring are available**) via `NewConnector(realClient, cfg, version)` + `c.Wire(connector.ConnectorContext{Writer: w, Matcher: integrationMatcher(), Metrics: m})`; calls `c.Poll(ctxWithTimeout, newTestCheckpoint(t))`, routing `Poll` errors through `integrationtest.SkipOnRateLimit`; then `AssertStagedAtLeast(readback(),1)` + `AssertContentNonEmpty` + `AssertRouting`.
- [ ] **Step 4.2** Verify it COMPILES and SKIPS clean without creds:
```bash
go vet -tags integration ./connectors/schoology/
GLOVEBOX_INTEGRATION= go test -tags integration ./connectors/schoology/ -run Live -v   # expect SKIP
```
- [ ] **Step 4.3** Commit. (Live execution happens in the nightly CI stage with real creds.)
```bash
git add connectors/schoology/live_integration_test.go
git commit -m "test(schoology): live integration test on shared harness (glovebox-lyku.4)"
```

---

## Task 5 (lyku.5): no-credential connector live tests

**Files:** Create `connectors/{rss,hackernews,arxiv,semantic-scholar}/live_integration_test.go` (`//go:build integration`).

> **Critical wiring notes (from plan review):**
> - These tests are **`package main`** (the connectors expose no exported
>   `NewConnector`; the struct + fields are package-main). Import the harness
>   as `github.com/leftathome/glovebox/connector/integrationtest`.
> - The connector struct's collaborators (`httpClient`, `linkPolicy`,
>   `robotsChecker`, `config`) are built in **`main()` BEFORE `connector.Run`**,
>   not in the `Setup` callback — so the test must construct the full connector
>   exactly as `main()` does, then set the three fields the `Setup` callback
>   sets. For rss, `Setup` does `c.writer = cc.Backend` (a `StagingBackend`,
>   NOT `cc.Writer`); a `*connector.StagingWriter` satisfies `StagingBackend`
>   (compile-time assert in `connector/backend.go`), so assign our writer there.
> - Drive a single `c.Poll(ctx, cp)` with a fresh checkpoint:
>   `cp, err := connector.NewCheckpoint(t.TempDir())` (it returns
>   `(Checkpoint, error)` — check the error). A fresh empty checkpoint
>   processes everything; these connectors have no window gating, unlike
>   schoology.
> - The shipped `config.json` uses **placeholder** sources (e.g.
>   example.com); the live test MUST supply its own effective config pointing
>   at a **real public source whose name matches a `feed:<name>`/query rule**,
>   or `matcher.Match` returns `!ok` and zero items stage.

- [ ] **Step 5.x.1** Author `live_integration_test.go`. Concrete shape for rss (adapt per connector to its struct/constructors — follow that connector's `main.go`):

```go
//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/connector/content"
	"github.com/leftathome/glovebox/connector/integrationtest"
)

func TestLive_RSS(t *testing.T) {
	integrationtest.RequireIntegration(t) // public source: no RequireCreds

	// Effective config with a REAL public feed whose name matches a feed:<name> rule.
	cfg := Config{ /* Feeds: [{Name:"goblog", URL:"https://go.dev/blog/feed.atom"}], Rules: [{Match:"feed:goblog", Destination:"messaging", ...}] */ }

	httpClient := connector.NewHTTPClient(connector.HTTPClientOptions{})
	c := &RSSConnector{
		config:        cfg,
		linkPolicy:    content.NewLinkPolicy(cfg.LinkPolicy),
		httpClient:    httpClient,
		robotsChecker: connector.NewRobotsChecker(httpClient),
	}
	w, readback := integrationtest.StageToTempDir(t, "rss")
	c.writer = w                                     // *StagingWriter satisfies StagingBackend
	c.matcher = connector.NewRuleMatcher(cfg.Rules)  // use the connector's actual rules field
	c.fetchCounter = connector.NewFetchCounter(cfg.FetchLimits)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cp, err := connector.NewCheckpoint(t.TempDir()) // returns (Checkpoint, error)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Poll(ctx, cp); err != nil {
		integrationtest.SkipOnRateLimit(t, err)
		t.Fatalf("Poll: %v", err)
	}

	items := readback()
	integrationtest.AssertStagedAtLeast(t, items, 1)
	if len(items) == 0 {
		t.FailNow() // AssertStagedAtLeast uses Errorf; stop before indexing items[0]
	}
	integrationtest.AssertContentNonEmpty(t, items[0])
	integrationtest.AssertRouting(t, items[0], integrationtest.WantRouting{DestinationAgent: "messaging" /* + DataSubject/Audience per rule */})
}
```
Confirm the exact field names / rule + fetch-limit accessors against each connector's `connector.go`/`config.go` while implementing (rss shown; HN/arxiv/semantic-scholar differ in client + query-rule naming).
- [ ] **Step 5.x.2** Verify compile + skip-clean:
```bash
go vet -tags integration ./connectors/<name>/
GLOVEBOX_INTEGRATION= go test -tags integration ./connectors/<name>/ -run Live -v   # expect SKIP
```
- [ ] **Step 5.x.3** Verify it actually passes live locally (these need no creds, only network):
```bash
GLOVEBOX_INTEGRATION=1 go test -tags integration ./connectors/<name>/ -run Live -v   # expect PASS (or skip-with-warning on upstream 429)
```
- [ ] **Step 5.x.4** Commit per connector:
```bash
git add connectors/<name>/live_integration_test.go
git commit -m "test(<name>): live integration test (glovebox-lyku.5)"
```

---

## Final verification

- [ ] `go test ./...` green (integration tests excluded by tag).
- [ ] `go test -tags integration ./...` with `GLOVEBOX_INTEGRATION` unset: no live calls (all per-connector tests skip; offline root `integration_test.go` may run).
- [ ] `go vet ./...` + staticcheck clean on new files.
- [ ] Each task committed; branch pushed to GitLab; MR opened (gitlab-first), synced to GitHub on merge.
- [ ] Close `lyku.1`/`lyku.2`/`lyku.4`/`lyku.5` as landed; `lyku.3` closes when the private template + scheduled pipeline are wired and a manual run is green.
```
