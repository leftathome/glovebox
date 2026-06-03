# SP1: Health-Data Provenance and Subject Resolution — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a producer (the recognizer) deliver health-data archives to Glovebox with trusted, producer-asserted provenance — acquisition identity + an opaque subject principal — and have Glovebox resolve that principal to an opaque `entity_id`, route on it, and fail closed to quarantine when it cannot resolve.

**Architecture:** Three changes layered onto the existing archive-delivery path (spec 13) and data-subject model (spec 11). (1) The archive metadata parser + finalize receipt gain producer-asserted provenance fields for the new `archive/walhelm-export` media type. (2) A new `internal/subject` package holds a known-subjects registry (opaque `entity_id` ← opaque principals, optional non-functional `display`) and a pure resolver. (3) The routing decision point gains a resolver gate: subjectless items bypass, principals resolve to `entity_id`, unresolved subjects quarantine with reason `subject_unresolved`. A new `importers/walhelm/` (mirroring `importers/mbox/`) stamps items from the receipt's provenance instead of guessing via the rule matcher. The gate ships disabled (`subjects_enforce: false`, empty registry) so landing it is a behavioral no-op.

**Tech Stack:** Go (standard `go test`, `go vet`, `staticcheck`); tus.io archive ingest; Helm; GitHub Actions matrix builds. Spec: `docs/specs/15-health-provenance-and-subject-resolution-design.md`.

**Baseline gate (do first):** Before Task 1, from the worktree run `go test ./...` and record the result. If anything is red at baseline, STOP and report — do not start on a dirty baseline.

**Conventions:** All paths are relative to the worktree root `/root/.config/superpowers/worktrees/glovebox/spec-15`. No emoji in any Go source or strings (project rule). Commit after each task with the message shown.

**Spec-14 (content-enrichment) integration — incorporates beads afq4.12–.17 (2026-06-03):** afq4.12–.17 are content-enrichment (spec 14) follow-ups. Verified fact: this branch's base (`da12b0f`) shares merge-base `4d4aeb4` with `spec-14-content-enrichment`, so it already has the enrich framework + the `staging.Commit()` pipeline hook; and `git diff da12b0f spec-14 --` shows **zero changes** to every core file SP1 touches (`metadata.go`, `finalize.go`, `internal/config`, `internal/staging`, `internal/routing`, `main.go`, `connector/staging.go`, `connector/http_backend.go`). So **Tasks 1–6 are conflict-free on the current base**; only the importer tasks change. Concretely:
- **Mirror the spec-14 *rebased* mbox importer, not this base's version.** spec-14 changed `importers/mbox/{Dockerfile,main.go}` (enricher-runtime base + enricher wiring). T7–T10 below must mirror those rebased files (read them from `spec-14-content-enrichment`), so the walhelm importer is enrichment-consistent.
- **afq4.15/.13 → T10 Dockerfile:** base on `ghcr.io/leftathome/glovebox-enricher-runtime:latest` like the rebased mbox Dockerfile, carry the same inline "TODO: pin to immutable tag" comment (afq4.15 will sweep it). pandoc-version (afq4.13) is a runtime-image concern SP1 inherits, not an SP1 change.
- **afq4.12 → new verification (T6/T9/T13):** the resolver reads `data_subject`/`audience` from the staged `metadata.json`. Verify these + the acquisition `Identity` survive the importer's delivery path (filesystem backend writes them in `Commit()`; HTTP backend must serialize them in the ingest POST). If the HTTP path drops them, the resolver can't see them. Add an assertion.
- **afq4.17 → T8:** the walhelm importer stages **one item per tree file** with its own `ContentType`, which sidesteps afq4.17's single-ContentType multipart-dispatch gap. v0.1 walhelm has no attachment files yet. Just set accurate per-file content-types.
- **afq4.14, afq4.16:** spec-14-internal (enricher LookPath refactor; smoke-image CI). **Not SP1.**
- **Merge order:** land SP1 *after* spec-14 reaches main (the walhelm Dockerfile references the enricher-runtime image spec-14 publishes). SP1 + spec-14 do not conflict (core files disjoint; `importers/walhelm/` is new).

---

## File Structure

**New files**
- `internal/subject/registry.go` — `SubjectRegistry`, `SubjectEntry`, JSON load, validation, `Resolve(principalOrEntity string) (entityID string, ok bool)`.
- `internal/subject/registry_test.go`
- `internal/subject/resolve.go` — `Outcome` enum + `ResolveItem(meta *staging.ItemMetadata, reg *SubjectRegistry) Outcome` (pure; mutates `meta.DataSubject`/`Audience`; never reads/writes `display`).
- `internal/subject/resolve_test.go`
- `importers/walhelm/main.go` — flags + `importer.RunOneShot` wiring (mirror `importers/mbox/main.go`).
- `importers/walhelm/importer.go` — `walhelmImporter` implementing `importer.Importer`.
- `importers/walhelm/ingest.go` — `BuildItemOptions(entry, receipt, matcher, sourceName)`.
- `importers/walhelm/importer_test.go`, `importers/walhelm/ingest_test.go`
- `importers/walhelm/Dockerfile`, `importers/walhelm/config.json`
- `charts/glovebox/subjects.json` — default empty registry (`{"enforce": false, "subjects": []}`).

**Modified files**
- `internal/ingest/archives/metadata.go` — add walhelm media type; add `AcqProvider/AcqAccountID/AcqAuthMethod/DataSubject/Audience` to `Metadata`; per-media-type required-key validation.
- `internal/ingest/archives/metadata_test.go`
- `internal/ingest/archives/finalize.go` — add `Acquisition *ingest.Identity`, `DataSubject string`, `Audience []string` to `FinalizeReceipt`; populate from `st.Meta`.
- `internal/ingest/archives/finalize_test.go`
- `internal/config/config.go` — add `SubjectsFile string` + `SubjectsEnforce bool`; load registry; validate.
- `internal/config/config_test.go`
- the scan→route decision site (located in Task 6) — call the resolver gate before pass/quarantine.
- `.github/workflows/ci.yml` — add `./importers/walhelm/` to binary loop + a `glovebox-walhelm-importer` docker matrix entry.
- `charts/glovebox/values.yaml` + `charts/glovebox/templates/configmap.yaml` — render `subjects.json`; add `config.subjectsFile` + `config.subjectsEnforce`.
- `CHANGELOG.md`

---

## Phase A — Archive contract (metadata + finalize)

### Task 1: Add `archive/walhelm-export` media type + producer-asserted metadata fields

**Files:**
- Modify: `internal/ingest/archives/metadata.go`
- Test: `internal/ingest/archives/metadata_test.go`

Context: `Metadata` (metadata.go:22-31), `mediaAllowList` (metadata.go:67-72), `ParseUploadMetadata` (metadata.go:124-193), sentinels (metadata.go:83-94). Today every media type shares one required-key set; SP1 needs per-media-type required keys (spec 15 §4.2 implementation note).

- [ ] **Step 1: Write failing tests** in `metadata_test.go` using the existing `validHeader(overrides)` / `enc()` helpers (test file lines 12-63):
  - `archive/walhelm-export` with all of `acq_provider`,`acq_account_id`,`acq_auth_method`,`data_subject` (+ optional `audience`) parses; assert the five new struct fields.
  - `archive/walhelm-export` missing any one required `acq_*`/`data_subject` → error `errors.Is(err, ErrMetadataMissing)`.
  - `acq_provider` violating `^[a-z][a-z0-9-]{0,63}$` → `ErrMetadataInvalid`.
  - `audience` with an invalid token → `ErrMetadataInvalid` (reuse `internal/staging` audience validation).
  - An existing media type (`archive/mbox`) with NO `acq_*`/`data_subject` still parses (additive; those keys not required there) and a stray `acq_provider` on `archive/mbox` is ignored (not an error).
  - `delivered_by` still → `ErrMetadataReservedKey` (regression).

- [ ] **Step 2: Run, verify fail** — `go test ./internal/ingest/archives/ -run TestParseUploadMetadata -v` → FAIL (unknown fields / media type).

- [ ] **Step 3: Implement.**
  - Add to `mediaAllowList`: `"archive/walhelm-export": MediaTar,`.
  - Extend `Metadata`:
    ```go
    AcqProvider   string
    AcqAccountID  string
    AcqAuthMethod string
    DataSubject   string
    Audience      []string
    ```
  - Define the per-media-type required set near the allow-list:
    ```go
    // requiredProvenance lists media types that MUST carry producer-asserted
    // provenance keys (spec 15 §4.2). Other media types ignore these keys.
    var requiredProvenance = map[string]bool{"archive/walhelm-export": true}
    ```
  - In `ParseUploadMetadata`, after the existing required-key/media-type checks, when `requiredProvenance[m.MediaType]`: require non-empty `acq_provider` (regex `providerRe`, reuse metadata.go:79), `acq_account_id` (≤256, no control chars — reuse the helpers used for `Provider`/strings), `acq_auth_method` (must equal `"browser_session"` for v1 → else `ErrMetadataInvalid`), `data_subject` (≤256, no control chars). Parse optional `audience` by splitting on `,` and validating via `staging.ValidateAudience(tokens, hasDataSubject)` — **note the second arg**: its real signature is `ValidateAudience(audience []string, hasDataSubject bool) error`. For walhelm media `data_subject` is required, so pass `hasDataSubject=true` (this is what makes `subject`/`siblings` tokens legal — see spec 11 §3.5). Empty/absent audience → nil. Always copy `acq_*`/`data_subject`/`audience` into the struct when present, regardless of media type (so they round-trip), but only *enforce presence* for `requiredProvenance` types.

- [ ] **Step 4: Run, verify pass** — `go test ./internal/ingest/archives/ -run TestParseUploadMetadata -v` → PASS.

- [ ] **Step 5: Commit** — `git add internal/ingest/archives/metadata.go internal/ingest/archives/metadata_test.go && git commit -m "feat(archives): walhelm-export media type + producer-asserted provenance metadata (spec 15 §4)"`

### Task 2: Carry provenance into the finalize receipt

**Files:**
- Modify: `internal/ingest/archives/finalize.go`
- Test: `internal/ingest/archives/finalize_test.go`

Context: `FinalizeReceipt` (finalize.go:55-67), construction (finalize.go:156-166), `ingest.Identity` (audit_provenance.go:34-38), `ingest.BuildIdentity` (audit_provenance.go:45-55).

- [ ] **Step 1: Write failing test** — drive `Finalize` (finalize.go:112) with an `UploadState` whose `Meta` is an `archive/walhelm-export` metadata carrying `acq_*`+`data_subject`+`audience`; assert the receipt JSON has `acquisition` (`{provider,auth_method,account_id}` from `acq_*`), `data_subject`, and `audience`; and that an `archive/mbox` finalize still omits them (`omitempty`).

- [ ] **Step 2: Run, verify fail** — `go test ./internal/ingest/archives/ -run TestFinalize -v` → FAIL.

- [ ] **Step 3: Implement.**
  - Extend `FinalizeReceipt`:
    ```go
    Acquisition *ingest.Identity `json:"acquisition,omitempty"`
    DataSubject string           `json:"data_subject,omitempty"`
    Audience    []string         `json:"audience,omitempty"`
    ```
  - In the receipt construction (finalize.go:156-166), after the existing fields:
    ```go
    if st.Meta.AcqProvider != "" {
        receipt.Acquisition = &ingest.Identity{
            Provider:   st.Meta.AcqProvider,
            AuthMethod: st.Meta.AcqAuthMethod,
            AccountID:  st.Meta.AcqAccountID,
        }
    }
    receipt.DataSubject = st.Meta.DataSubject
    receipt.Audience = st.Meta.Audience
    ```
  - (`ingest.Identity` is already the receipt's `Identity` type, so no import change.)

- [ ] **Step 4: Run, verify pass** — `go test ./internal/ingest/archives/ -v` → PASS.

- [ ] **Step 5: Commit** — `git add internal/ingest/archives/finalize.go internal/ingest/archives/finalize_test.go && git commit -m "feat(archives): record acquisition identity + subject/audience in finalize receipt (spec 15 §4.4)"`

---

## Phase B — Registry + resolver (pure, no wiring)

### Task 3: `internal/subject` registry — parse, validate, resolve

**Files:**
- Create: `internal/subject/registry.go`, `internal/subject/registry_test.go`

- [ ] **Step 1: Write failing tests** covering:
  - `Load` parses `{"enforce":true,"subjects":[{"entity_id":"e_1","display":"Steve","principals":["walhelm:9f2a"],"default_audience":["subject"]}]}`.
  - `Resolve("walhelm:9f2a")` → `("e_1", true)`; `Resolve("e_1")` (a registered entity_id) → `("e_1", true)`; `Resolve("walhelm:zzz")` → `("", false)`; `Resolve("e_unknown")` → `("", false)`.
  - Duplicate principal across two entries → `Load` error.
  - Malformed `default_audience` (bad token / `["public","subject"]`) → `Load` error (reuse `staging.ValidateAudience(tokens, hasDataSubject bool)` — pass `hasDataSubject=true` for a registry entry, since an entity is itself the subject, so `subject`/`siblings` tokens are legal).
  - `DefaultAudience("e_1")` → `["subject"]`.
  - `display` is parsed but there is **no accessor that returns it** (it must be unreachable to routing/audit by construction — assert the public API surface has no Display getter; a comment-test is fine).

- [ ] **Step 2: Run, verify fail** — `go test ./internal/subject/ -v` → FAIL (package missing).

- [ ] **Step 3: Implement** `registry.go`:
  ```go
  package subject

  // SubjectEntry is one known subject. display is intentionally unexported and
  // has no accessor: it is at-rest operator legibility only and must never reach
  // an item, route, or audit record (spec 15 §5.1 PHI firewall).
  type SubjectEntry struct {
      EntityID        string   `json:"entity_id"`
      display         string   `json:"-"`
      Principals      []string `json:"principals"`
      DefaultAudience []string `json:"default_audience,omitempty"`
  }

  type SubjectRegistry struct {
      enforce  bool
      entities map[string]*SubjectEntry // entity_id -> entry
      byPrinc  map[string]*SubjectEntry // principal  -> entry
  }

  func (r *SubjectRegistry) Enforce() bool { return r != nil && r.enforce }

  // Resolve maps a principal OR a registered entity_id to its canonical
  // entity_id. ok=false means the value is unknown (caller quarantines).
  func (r *SubjectRegistry) Resolve(s string) (string, bool) { ... }
  func (r *SubjectRegistry) DefaultAudience(entityID string) []string { ... }
  func Load(path string) (*SubjectRegistry, error) { ... } // empty path -> empty, enforce=false
  ```
  Use a private wire struct with an exported `Display` json field decoded then dropped into the unexported `display`, OR decode into a temp and never expose it. Validate at load: non-empty unique `entity_id`s; each principal maps to ≤1 entity (else error); `default_audience` via `staging.ValidateAudience`.

- [ ] **Step 4: Run, verify pass** — `go test ./internal/subject/ -v` → PASS. Then `go vet ./internal/subject/`.

- [ ] **Step 5: Commit** — `git add internal/subject/registry.go internal/subject/registry_test.go && git commit -m "feat(subject): known-subjects registry with opaque entity_id resolution (spec 15 §5.1)"`

### Task 4: The pure resolver

**Files:**
- Create: `internal/subject/resolve.go`, `internal/subject/resolve_test.go`

Context: `staging.ItemMetadata` carries `DataSubject string` and `Audience []string` (see `internal/staging`; mirrored on `connector` side at staging.go:19-35).

- [ ] **Step 1: Write failing tests** for `ResolveItem(meta, reg) Outcome`:
  - `meta.DataSubject == ""` → `OutcomeSubjectless`; `meta` unchanged.
  - principal present, registered → `OutcomeResolved`; `meta.DataSubject` rewritten to the `entity_id`; if `meta.Audience` was empty, it becomes the registry default; if non-empty, it is left as-is.
  - registered `entity_id` present → `OutcomeResolved`; unchanged value.
  - unknown principal/entity → `OutcomeUnresolved`; `meta` unchanged (caller decides quarantine).
  - nil registry → any non-empty subject is `OutcomeUnresolved` (fail closed).

- [ ] **Step 2: Run, verify fail** — `go test ./internal/subject/ -run TestResolveItem -v` → FAIL.

- [ ] **Step 3: Implement** `resolve.go`:
  ```go
  package subject

  type Outcome int
  const (
      OutcomeSubjectless Outcome = iota // no data_subject; not our concern
      OutcomeResolved                   // mapped to a known entity_id
      OutcomeUnresolved                 // declared a subject we don't know
  )

  // ResolveItem rewrites meta.DataSubject from a principal to its entity_id and
  // fills meta.Audience from the registry default when the item declared none.
  // It never references display (spec 15 §5.1).
  func ResolveItem(meta *staging.ItemMetadata, reg *SubjectRegistry) Outcome {
      if meta == nil || meta.DataSubject == "" {
          return OutcomeSubjectless
      }
      entityID, ok := reg.Resolve(meta.DataSubject)
      if !ok {
          return OutcomeUnresolved
      }
      meta.DataSubject = entityID
      if len(meta.Audience) == 0 {
          meta.Audience = reg.DefaultAudience(entityID)
      }
      return OutcomeResolved
  }
  ```

- [ ] **Step 4: Run, verify pass** — `go test ./internal/subject/ -v` → PASS; `staticcheck ./internal/subject/`.

- [ ] **Step 5: Commit** — `git add internal/subject/resolve.go internal/subject/resolve_test.go && git commit -m "feat(subject): pure resolver maps principal->entity_id, applies default audience (spec 15 §5.2)"`

---

## Phase C — Config + routing gate

### Task 5: Load the registry through `internal/config`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

Context: `Config` (config.go:75-91), `LoadConfig` (config.go:93-157), `Validate` (config.go:231-260), env overrides (config.go:159-229).

- [ ] **Step 1: Write failing test** — a config JSON with `"subjects_file":"<tmp subjects.json>"` and `"subjects_enforce":true` loads and `Validate()` succeeds; a `subjects_file` pointing at a malformed registry fails `Validate()`; absent `subjects_file` → enforce defaults false and registry is empty/valid.

- [ ] **Step 2: Run, verify fail** — `go test ./internal/config/ -v` → FAIL.

- [ ] **Step 3: Implement** — add to `Config`:
  ```go
  SubjectsFile    string `json:"subjects_file"`
  SubjectsEnforce bool   `json:"subjects_enforce"`
  ```
  Add env overrides `GLOVEBOX_SUBJECTS_FILE` / `GLOVEBOX_SUBJECTS_ENFORCE` alongside the others (config.go:159-229). In `Validate()`, if `SubjectsFile != ""`, call `subject.Load(c.SubjectsFile)` and return any error (do not retain the registry on `Config` if that introduces an import cycle — instead expose a `LoadSubjectRegistry()` helper the scanner calls at startup, see Task 6). Keep enforce flag on `Config`.

- [ ] **Step 4: Run, verify pass** — `go test ./internal/config/ -v` → PASS.

- [ ] **Step 5: Commit** — `git add internal/config/config.go internal/config/config_test.go && git commit -m "feat(config): subjects_file + subjects_enforce; validate registry at load (spec 15 §5.1, §11)"`

### Task 6: Wire the resolver gate into the scan→route decision

**Files:**
- Locate + Modify: the scan pipeline that calls `routing.RoutePass` / `routing.RouteQuarantine` (search: `grep -rn "RoutePass\|RouteQuarantine" internal main.go`). Likely `internal/pipeline/` or `main.go`.
- Test: a new `*_test.go` next to the decision site, plus reuse of the `internal/routing` harness pattern (quarantine_test.go:16-47).

Context: `RouteQuarantine(... reason string)` (routing/quarantine.go:42), `RoutePass(...)` (routing/pass.go:14), `RejectEntry.Reason` (audit/logger.go:35-38), `AuditEntry.DataSubject/Audience` already populated (quarantine.go:88-106).

> **Verified wiring point:** the decision site is the package-level function `deliverResult(...)` in `main.go` (RouteQuarantine at lines 370/389/403/424, RoutePass at 434). Add a `*subject.SubjectRegistry` parameter to `deliverResult` and thread it from its call site in the scan loop (it is a function arg, not a constructor). No import cycle: `internal/staging` and `internal/config` are leaves w.r.t. glovebox internal packages.

- [ ] **Step 1: Write failing test** at the decision site:
  - Build the scanner/pipeline with a registry mapping `walhelm:9f2a → e_1` and `enforce=true`.
  - Item with `DataSubject="walhelm:9f2a"`, clean scan → routed to its destination; assert the destination copy's `metadata.json` has `data_subject:"e_1"` (resolved) and the resolved default audience.
  - Item with `DataSubject="walhelm:unknown"`, clean scan → quarantined; assert `rejected.jsonl` entry has `reason:"subject_unresolved"` and no destination copy exists.
  - Item with `DataSubject=""` (subjectless) → routed normally (bypass).
  - Same unknown-subject item with `enforce=false` → routed normally (gate disabled); assert a warn-level log but no quarantine.

- [ ] **Step 2: Run, verify fail** → FAIL.

- [ ] **Step 3: Implement** — at the decision site, after scan verdict is known and BEFORE choosing pass vs quarantine:
  ```go
  switch subject.ResolveItem(&item.Metadata, reg) {
  case subject.OutcomeUnresolved:
      if reg.Enforce() {
          return routing.RouteQuarantine(item, scanResult, quarantineDir, notifyDir, logger, threshold, scanDuration, "subject_unresolved")
      }
      // enforcement off: log and fall through to normal routing
      slog.Warn("subject unresolved; enforcement off, passing", "principal", item.Metadata.DataSubject)
  }
  // OutcomeResolved mutated item.Metadata in place; OutcomeSubjectless left it alone.
  // continue with the existing scan-score pass/quarantine logic
  ```
  The resolver MUST run before the metadata is copied to the destination so the destination/audit carry `entity_id`. Thread the `*subject.SubjectRegistry` from startup (built via `subject.Load(cfg.SubjectsFile)` with `enforce = cfg.SubjectsEnforce`) into the pipeline constructor.

- [ ] **Step 4: Run, verify pass** → PASS; `go vet ./...` on touched packages.

- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(routing): fail-closed subject resolution gate before pass/quarantine (spec 15 §5.2-5.3)"`

---

## Phase D — The walhelm importer

### Task 7: Scaffold `importers/walhelm/` (mirror mbox)

**Files:** Create `importers/walhelm/main.go`, `importer.go` (skeleton), `config.json`.

Context to mirror: `importers/mbox/main.go` (flags lines 39-54; `RunOneShot` call line 145), `mboxImporter` (importer.go:35-44), `importer.Importer` interface (importer/importer.go:59-97), `importer.RunOneShot` (called main.go:145).

> **Mirror the spec-14 REBASED mbox importer, not this base's version.** Before copying, read `git show spec-14-content-enrichment:importers/mbox/main.go` — the rebased version added ~12 lines wiring the enrichment pipeline. Mirror those so the walhelm importer is enrichment-consistent (so filesystem-backend staging enriches like the other connectors). The base version is stale for this purpose.

- [ ] **Step 1:** Copy the **spec-14 rebased** `importers/mbox/main.go` → `importers/walhelm/main.go`; rename flagset to `walhelm-importer`; `--source` now means the staged archive dir (`archives/<id>/`) rather than an mbox file; default `--source-name walhelm`. Keep `--ingest-url`/`--staging-dir`/`--state-dir`/`--config`/`--concurrency`/`--health-port` and the enricher-wiring lines from the rebased version.
- [ ] **Step 2:** Create `walhelmImporter` struct + stub all six `importer.Importer` methods returning `nil`/empty so it compiles; wire `importer.RunOneShot(ctx, fw, imp, cfg)`.
- [ ] **Step 3:** `go build ./importers/walhelm/` → succeeds.
- [ ] **Step 4: Commit** — `git add importers/walhelm/ && git commit -m "scaffold(walhelm): importer skeleton mirroring mbox (spec 15 §6)"`

### Task 8: `BuildItemOptions` — stamp from receipt provenance, not the matcher

**Files:** Create `importers/walhelm/ingest.go`, `ingest_test.go`.

Context: mbox `BuildItemOptions` (importers/mbox/ingest.go:54-113) derives subject from the matcher — walhelm must NOT. `FinalizeReceipt` (Task 2) carries `Acquisition`,`DataSubject`,`Audience`. `connector.ItemOptions` (staging.go:19-35) has `DataSubject`,`Audience`,`Identity`,`DestinationAgent`.

- [ ] **Step 1: Write failing test** — given a `FinalizeReceipt{DataSubject:"walhelm:9f2a", Audience:["subject"], Acquisition:{provider:"kp-wa",auth_method:"browser_session",account_id:"leftathome"}}` and a tar entry (path + bytes + a content-type hint), `BuildItemOptions` returns `ItemOptions` with `DataSubject=="walhelm:9f2a"` (principal, unresolved here), `Audience==["subject"]`, `Identity` = the acquisition identity, `ContentType` per entry (`message/rfc822` / `application/json` / `application/zip`), and `DestinationAgent` from the matcher. Assert the matcher is consulted ONLY for destination (subject/audience come from the receipt).

- [ ] **Step 2: Run, verify fail** → FAIL.

- [ ] **Step 3: Implement** `BuildItemOptions(entry walhelmEntry, receipt *archives.FinalizeReceipt, matcher *connector.RuleMatcher, sourceName string) (connector.ItemOptions, error)`:
  - `DestinationAgent` from `matcher.Match(ruleKeyForEntry(entry))` (error if unmatched + no wildcard, mirroring mbox lines 68-82).
  - `DataSubject = receipt.DataSubject`; `Audience = receipt.Audience`.
  - `Identity = &connector.Identity{Provider: receipt.Acquisition.Provider, AuthMethod: receipt.Acquisition.AuthMethod, AccountID: receipt.Acquisition.AccountID}` (guard nil).
  - `ContentType` from the entry's classification — set it ACCURATELY per file (`message/rfc822`, `application/json`, `application/zip`, etc.), because `enrich.Applies()` dispatches on it. One-item-per-file (this design) sidesteps afq4.17's single-ContentType multipart-dispatch gap by construction. `Source = sourceName`; `Tags["origin_archive"] = receipt.ArchiveID + ":" + entry.Path`.

- [ ] **Step 4: Run, verify pass** → PASS.
- [ ] **Step 5: Commit** — `git add importers/walhelm/ingest.go importers/walhelm/ingest_test.go && git commit -m "feat(walhelm): BuildItemOptions stamps subject/audience/identity from receipt; matcher only sets destination (spec 15 §6)"`

### Task 9: `Import()` — read receipt + walk the tree, stage each entry

**Files:** Modify `importers/walhelm/importer.go`; add `importer_test.go`.

Context: mbox `Import` worker-pool pattern (importer.go:175-195): `NewItem -> WriteContent -> Commit`. Reuse `importers/mbox/workers.go` pattern (copy `WorkerPool` or extract to a shared helper — prefer copy in SP1 to avoid touching mbox).

- [ ] **Step 1: Write failing test** — point `Import` at a synthetic staged archive dir containing `metadata.json` (a walhelm `FinalizeReceipt`) and `tree/` with two entries; assert two items are staged (via a fake `Backend`) each carrying the receipt's `DataSubject`/`Audience`/acquisition `Identity`.
- [ ] **Step 2: Run, verify fail** → FAIL.
- [ ] **Step 3: Implement** `Import`: read `<source>/metadata.json` into a `FinalizeReceipt`; reject if `media_type != "archive/walhelm-export"` or `DataSubject == ""`; walk `<source>/tree/`; for each file classify content-type, `BuildItemOptions`, then `NewItem/WriteContent/Commit` via a bounded worker pool. (Survey/LoadSurvey/LoadManifest/LoadFilter/ClearState can be minimal: no survey for v1 — return empty/no-op consistent with the interface.)
- [ ] **Step 4: Run, verify pass** → PASS; `go vet ./importers/walhelm/`.
- [ ] **Step 5: Commit** — `git add importers/walhelm/ && git commit -m "feat(walhelm): Import reads receipt provenance and stages tree entries (spec 15 §6)"`

---

## Phase E — Delivery + CI

### Task 10: Dockerfile + config.json
- [ ] Copy the **spec-14 rebased** mbox Dockerfile as the template: `git show spec-14-content-enrichment:importers/mbox/Dockerfile`. It is a two-stage build whose runtime stage is `FROM ghcr.io/leftathome/glovebox-enricher-runtime:latest` (debian + poppler-utils/tesseract/pandoc) and carries an inline `# TODO: pin to immutable enricher-runtime tag ...` comment. Reproduce that exact base + the pin-TODO comment (afq4.15 will sweep it across all importers/connectors once spec-14 hits main and CI publishes an immutable tag). Adjust the build target to `./importers/walhelm/`. Do NOT use the plain-Go base from this branch's stale mbox Dockerfile.
- [ ] Create `importers/walhelm/config.json` with a rules block (destination matcher) mirroring mbox's.
- [ ] `docker build -f importers/walhelm/Dockerfile .` succeeds (or `go build ./importers/walhelm/` if docker is unavailable in the sandbox — note which was run).
- [ ] Commit: `chore(walhelm): Dockerfile (enricher-runtime base, pin-TODO) + example config`.

### Task 11: CI matrix
**File:** `.github/workflows/ci.yml`

> **Correction (verified):** ci.yml currently has **no mbox/importer entry to mirror** — the binary loop (line 107) lists only `.` and `./connectors/*`, and its naming logic produces `${name}-connector`. So you are *adding* importer support, not copying it, and you must override the name so the binary is `walhelm-importer` (not `walhelm-connector`).

- [ ] In the binary build loop (ci.yml ~76-112), add importer handling so `./importers/walhelm/` builds as `walhelm-importer`. Either add a dedicated build line after the connector loop:
  ```bash
  CGO_ENABLED=0 GOOS=${{ matrix.goos }} GOARCH=${{ matrix.goarch }} go build -o "walhelm-importer${{ matrix.ext || '' }}" ./importers/walhelm/
  ```
  or extend the loop's name derivation to map a `./importers/*` target to `<name>-importer`. Do NOT route it through the `${name}-connector` branch.
- [ ] In the docker build matrix (ci.yml ~145-177), add a fresh entry modeled on a connector entry (e.g. `glovebox-rss`):
  ```yaml
  - image: glovebox-walhelm-importer
    dockerfile: ./importers/walhelm/Dockerfile
    context: .
  ```
- [ ] Commit: `ci: build + image for walhelm importer`.

### Task 12: Helm registry delivery
**Files:** `charts/glovebox/subjects.json` (new), `charts/glovebox/values.yaml`, `charts/glovebox/templates/configmap.yaml`
- [ ] Create `charts/glovebox/subjects.json` = `{"enforce": false, "subjects": []}`.
- [ ] In `configmap.yaml`, render it next to `rules.json` (configmap.yaml ~line 76 pattern `{{- .Files.Get "subjects.json" | nindent 4 }}` under a `subjects.json:` key) and add `"subjects_file": "/etc/glovebox/subjects.json"`, `"subjects_enforce": {{ .Values.config.subjectsEnforce }}` to the rendered `config.json`.
- [ ] In `values.yaml` add `config.subjectsEnforce: false` and document the `subjects.json` block (entity_id/principals/default_audience; optional non-functional `display`). Mount path matches `subjects_file`.
- [ ] `helm template charts/glovebox` renders without error. Commit: `feat(chart): deliver subjects registry via ConfigMap, enforcement default off (spec 15 §11)`.

---

## Phase F — End-to-end + close-out

### Task 13: End-to-end integration test
**File:** `internal/ingest/archives/` or a top-level `*_integration_test.go`.
- [ ] Synthetic flow: craft an `archive/walhelm-export` upload (valid `acq_*`+`data_subject`+`audience`) → `Finalize` → run `walhelmImporter.Import` against the staged dir → scan → resolver gate. Two assertions: a registered principal lands at its destination with `data_subject==entity_id`; an unregistered one lands in quarantine with `reason==subject_unresolved`.
- [ ] **afq4.12 round-trip assertion:** assert the staged `metadata.json` the resolver consumes actually carries `data_subject`, `audience`, and the acquisition `Identity` for BOTH backend modes the importer supports — filesystem (`--staging-dir`, fields written in `Commit()`) and, if exercised, HTTP (`--ingest-url`, fields must be serialized in the ingest POST and persisted server-side). If the HTTP path drops any of the three, the resolver can't see them — file a bug rather than working around it. Commit: `test: e2e walhelm-export provenance + resolution + backend round-trip`.

### Task 14: Close-out
- [ ] `go test ./...`, `go vet ./...`, `staticcheck ./...` all clean from the worktree.
- [ ] `CHANGELOG.md`: add an entry summarizing spec 15 SP1 (provenance metadata, finalize receipt fields, subjects registry/resolver/gate, walhelm importer; enforcement default off).
- [ ] Commit: `docs: changelog for spec 15 SP1`.

---

## Notes for the implementer
- **Enforcement is off by default and the registry ships empty** — every existing connector and the mbox/recognizer flow must stay byte-identical. Task 6's subjectless-bypass and enforce-off tests are the guardrails; do not skip them.
- **`display` must be unreachable from routing/audit by construction** (unexported, no getter). If you ever need it, you are about to break the firewall — stop.
- **The resolver runs once, at the routing decision**, not in the importer. The importer stamps the *principal*; routing converts it to the *entity_id*. Keep that split (spec 15 §5.2, §10.3).
- **Mirror, don't refactor, mbox.** SP1 copies the importer/worker-pool pattern; extracting shared importer plumbing is out of scope and risks touching the live mbox path.
