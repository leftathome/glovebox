# Record Journal Part C Implementation Plan: the openclaw consumer (rollout steps 4-6)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rollout steps 4, 5 and 6 of the record journal (spec `docs/superpowers/specs/2026-10-16-record-journal-design.md`, sections 6 and 9): the openclaw school patrol reads the journal in **shadow** next to today's item fold and the two are compared until every difference is explained (step 4); then the patrol switches to the journal for **both** school sources in **one** step (step 5); then the connectors stop staging items for what they already send as records, keeping posts and messages (step 6).

**The cutover rule (Steve, 2026-10-29), restated because every task below depends on it:** openclaw cuts over to the journal **only when both PowerSchool and Schoology emit records**, and both sources switch in **one** step -- never PowerSchool alone first. B1 (PowerSchool) and B2 (Schoology) are merged to glovebox main and off by default; either may run in shadow on its own before that, but nothing openclaw shows anyone changes until both do. The patrol enforces this in code (Task 11: journal mode has no per-source switch) and the rollout checks it with data (Task 16).

**Architecture:** One binary, one new mode switch. `schoolpatrol` (openclaw, `image/cmd/schoolpatrol/`) gains `--school-source items|shadow|journal` (default `items`, today's behaviour byte for byte). A small journal client (`image/internal/journalclient`) reads `GET /v1/state`, `/v1/events`, `/v1/healthz` with the gateway's projected ServiceAccount token, naming exactly one `reader` per request (the person-agent's roster id), never a list and never grants. Every row and event passes an openclaw-side pre-write check (`image/internal/journalguard`) that re-derives entitlement from `household.json` with openclaw's own audience resolver, independent of the journal's filter, before anything reaches a workspace. In `shadow`, journal-derived state and digests are written to side files and compared with the item-derived ones; in `journal`, they replace them and items of journaled kinds are marked handled without delivery. Step 6 is a glovebox connector change (suppress the item for exactly the rows sent as records) enabled through gitops.

**Tech Stack:** openclaw: Go (module `gitlab.orac.local/agentic/openclaw/image`, `go 1.26.3`), `net/http`, stdlib only; tests run in `image/Dockerfile`'s `go test` step. glovebox (Tasks 4 and 17): as Part B (`GOTOOLCHAIN=go1.27.1`). gitops: Flux, HelmRelease values, one object per file.

**Prerequisites:** Part A (journal core) and Parts B1/B2 merged (they are). Nothing is enabled in the cluster yet: as of 2026-11-02 `clusters/orac/apps/glovebox/glovebox.yaml` has no `journal` block, so rollout step 1 (the core plan's "Gitops checklist for rollout steps 0 and 1") is still to do and is Task 2 here. Read the B1 plan's "Rules" and "Part A deviations" sections first.

**Repos touched:** openclaw (code, tests, ops log), gitops (values, gateway deployment, NetworkPolicy), glovebox (Tasks 1, 4, 17; docs). This plan lives in glovebox, which is **public**: no names, schools, grades or real ids here or in anything this plan adds to glovebox. Placeholders: `e_parent1`, `e_parent2`, `e_child1`, `e_child2`, agents `agent-parent1` ... Before Task 5, copy this plan into openclaw as `docs/superpowers/plans/2026-11-02-record-journal-part-c.md` (openclaw's CLAUDE.md: a project that implements one step of a large plan keeps a copy and records progress in it); tick boxes there for openclaw tasks and here for glovebox and gitops tasks.

---

## What openclaw does today (the shape this plan replaces)

Found by reading openclaw `main` (b961dfa) and the gateway volume snapshot (`ops/gateway-volume/`); it is what shaped the tasks.

- **Path of a school item.** Connector -> glovebox core -> delivery volume -> the `glovebox-courier` cron job (every 5 min) copies it into the gateway inbox -> `triage` (every 15 min) routes it by `data_subject`/`audience` into `audiences/group-*/inbox/<id>/` (school items are record-class and always kept, `openclaw-et5y`). Person-agents recall those group directories through `memorySearch.extraPaths` (when `memory_search` works; it times out for person-agents, `openclaw-90k5`).
- **The patrol.** `/app/bin/schoolpatrol` (deterministic Go, no model). Cron job `school-patrol`, `55 7,16,19 * * 1-5` America/Los_Angeles, timeout 1500 s, command `/app/bin/schoolpatrol --household /seed/household.json --root /home/node/workspace --no-wake --backfill`. **Wakes are off**: it writes digests and state files and runs no agent turn. It is **not declared in gitops** (cron-reconcile cannot express a timezone or timeout yet, `openclaw-bgxc`), so the volume is its only source and every change to it is a logged volume change. `school-patrol-health` (declared in gitops) runs `schoolpatrol --status` hourly and pages on unhealthy.
- **What it writes** (design: openclaw `docs/superpowers/specs/2026-10-09-school-patrol-design.md`): per agent `workspace/school/digest-<UTC>.md` (new items, max 40), `school/state.md` + `state.json` (schema 1, the state projection of `openclaw-59lb`, section 15: a fold of on-disk items, newest item per heuristic key, facts parsed from body lines, an "evidence rule" that marks rows unconfirmed after 30 days); per child group `audiences/<group>/school-state/state.json`; shared `patrols/school/{state.json,runs.jsonl,metrics.prom,household-summary.md}` (ids and counts only, by rule).
- **Entitlement** is enforced twice: `pendingFor` iterates `audience.EntitledGroups`, and `assertPrivacy` re-derives it per item before any write; a violation writes nothing for that agent. A child gets only themself, a guardian every child they are guardian of, `main` nothing. Person-agents have no `exec` and a workspace-confined `read`, so the files in their own `school/` folder are their only view. Their `AGENTS.md` "School updates" section points them at `school/state.md` for standing and the newest digest for news.
- **The roster** is the `openclaw-household` ConfigMap, mounted into the gateway at `/seed/household.json` **with `subPath`**, so a gitops change to it does not reach the running gateway until a restart (glovebox mounts it as a directory and reloads within seconds). This matters for the roster-hash check; decision C5 keeps the mount and restarts the gateway after each roster change.
- **The gateway** runs as ServiceAccount `openclaw-gateway` with `automountServiceAccountToken: false`, under a namespace default-deny NetworkPolicy. It has no token for the journal and no egress to it yet.

What the journal changes: rows have stable keys, typed fields, `confirmed_at` and removals; changes are events with an observed window, importance and a group key. The fold, the body-line parsing and the evidence rule go away (spec 6, last bullet), but only after the journal path has been compared against them on live data.

---

## Decisions for Steve (decided 2026-11-02: every recommendation accepted, with Steve's notes on C1 and C5 recorded in those rows)

| # | Question | Where the spec stands | Recommendation (implemented, easy to flip) |
|---|---|---|---|
| C1 | **The seed (rollout step 3).** Part B deferred it (S15). Does Part C include it, depend on it, or neither? | Section 7: a one-time Job over items retained since 2026-10-10, `approximate`, `complete=false`. | **Neither: Part C does not include or depend on the seed, and the plan recommends not running one.** Reason found while planning: 4.4 step 4 stores a snapshot as `late` (no events, no state change) when its scope already has a later applied snapshot, and the seed must use the live connectors' scopes and keys. So a seed run **after** connector records are enabled is a no-op for every live scope; it would only matter if run between journal enable and records enable. Current state does not need it: PowerSchool scopes are complete, so the first poll after records-on holds every course, assignment (no lookback), term grade and the current term's attendance; Schoology's first listing holds the current window. What a seed would add is approximate history, which stays readable in the retained items. **If Steve wants the seed, decide before Task 2 step 2.3**: the seed plan must then run between 2.2 and 2.3, and Task 15's comparison gains an "approximate" category. **Decided 2026-11-02 (Steve): no seed is necessary.** |
| C2 | **Where the journal code lives.** | Spec 6: "the school patrol reads ...". | In `schoolpatrol`, behind `--school-source items|shadow|journal`; the client and the guard in new `image/internal/` packages so a later notes tool or dashboard reuses them. No new cron job. |
| C3 | **Where shadow files go.** Shadow state and digests hold child content. | "builds state from the journal into a side file". | Per agent, beside the real ones: `<agent>/workspace/school/shadow/{state.md,state.json,digest-<UTC>.md,diff.md}`, written through the same pre-write check and the same safe-write code. The shared `patrols/school/shadow.jsonl` carries ids, categories and counts only (the shared-location rule of patrol spec section 2 is kept). Agents' `AGENTS.md` is not changed during shadow, so they keep reading `school/state.md`. |
| C4 | **Shadow length and pass criteria.** | "at least five school days. Every difference is explained before continuing." | At least **10 school days**, spanning a weekend, ending with **3 consecutive school days** with zero unexplained differences; zero privacy differences on every day; plus the content checklist in Task 15 (a settled attendance day, a scored assignment, a Schoology status change or an explicit note that none occurred). |
| C5 | **The gateway's `subPath` roster mount.** With `subPath` the gateway keeps its start-time copy of `household.json` until restart, so after any roster change its hash differs from the journal's indefinitely and the pre-write check (which compares them) fails safe on every run. | 4.9b: mismatches shorter than ten minutes are expected. | Mount the ConfigMap as a directory (`/seed/household/household.json`, no `subPath`) and move every consumer's `--household` path in the same gitops change (declared jobs in `configmap-openclaw-cron.yaml`; the undeclared `school-patrol` by a logged volume edit). Fixes the same staleness for triage today. Alternative: keep `subPath` and make "restart the gateway" part of every roster change. **Decided 2026-11-02 (Steve): the alternative. Roster changes are expected to be infrequent and a gateway restart when one happens is acceptable, so the `subPath` mount and every `--household /seed/household.json` path stay as they are; every roster change ends with a gateway restart (Task 3.2).** |
| C6 | **Event cursor at cutover.** | One cursor per agent. | Shadow mode keeps the per-reader cursors live (it writes shadow digests), so at cutover the cursors are current and the first real digest holds only that day's changes. A reader with no cursor (new member) starts at the head: its first run pages `/v1/events` to the end and records `next` without a digest. |
| C7 | **Digest overflow.** Items wait as pending when a digest is full; events are a cursor. | "grouped by `group_key`, high importance first". | The cursor always advances past every event fetched. A digest lists high and normal events in full up to 60 groups, then "N more changes not shown; `school/state.md` has how things stand"; low-importance events appear as counts per record type. State, not the digest, is the complete view. |
| C8 | **The notes tool (`attendance_note`) and the `attendance_followup` purpose.** | Spec 6: part of step 5. | **Out of Part C, in its own plan.** It needs a person-agent tool whose `on_behalf_of` the gateway binds to the calling agent and the model cannot choose; person-agents have no `exec` and openclaw has no per-agent identity on MCP calls today. Part C gives the gateway `read` and `counts` only (no `write_types`), renders notes in `state.md` if any exist (none will), and shows `push`/`prompt` advice at the top of the digest without purpose text (wakes stay off, `openclaw-16h2`). |
| C9 | **The counts-only household summary** (`openclaw-5nyn`: due tomorrow, due within 7 days, past due, new posts, per child). | "parents both; counts-only household view"; `/v1/counts` exists. | In journal mode the three assignment counts are computed from **that child's own reader view** (rows whose audience contains `subject`), so no count reflects a guardians-only row; "new posts" stays from items (posts are items). `/v1/counts` (live counts and events by importance per record type) is not the same measure; it is recorded in `shadow.jsonl` for Steve to compare, not used. |
| C10 | **Group state files** (`audiences/<group>/school-state/state.json`, "only what every member may see"). | Silent; step 7 moves the dashboard to the API. | Keep them in journal mode, computed as the intersection of the members' views by `(record_type, data_subject, key)`, then pre-write-checked for every member. Retire them at step 7. |
| C11 | **Step 6 granularity.** A surface's items are not all records: a degraded Schoology detail read (T2) and a term-comment page without a confirmed term are items but never records. | "record surfaces stop staging items". | Suppress the item **for exactly the rows sent as records in that poll**, not per surface. Rows that were not records keep their items. PowerSchool then stages almost nothing; Schoology keeps posts, messages, attachments and degraded detail reads. Off by default (`records.suppress_items`). |
| C12 | **Leftover items of journaled kinds in journal mode.** Between steps 5 and 6 they duplicate journal rows; after step 6 only the non-record ones (C11) remain. | Silent. | `--journaled-items ignore|deliver` (default `ignore`): `ignore` marks them handled without delivery; Task 18 flips the patrol to `deliver` with step 6, so leftovers reach digests as plain items (not state). |
| C13 | **Windowed rows.** `lms_assignment` with `in_latest_snapshot=false` means "left the listing, status unknown"; for `lms_assignment_detail` the flag means nothing (Schoology doc). | 5.2: present as "no longer listed; status unknown". | `lms_assignment` rows not in the latest listing move to a `no_longer_listed` list for 14 days after `confirmed_at`, then are counted only. They never appear in `overdue_lms` or `due_soon`. `lms_assignment_detail` is used only joined by key to its `lms_assignment`; its flag is never read. |
| C14 | **`state.json` schema.** | Spec 6: every row gets record type and key, `as_of` from the confirming snapshot. | Schema **2**, written in shadow and journal modes (schema 1 stays in items mode): every row gains `record_type`, `key`, `as_of` (= `confirmed_at`), `approximate`; `unconfirmed` is kept but now means "its scope was not re-observed for `--state-evidence-days`" (confirmed_at), not "no new item". Removed rows are dropped. The AGENTS.md text is updated at cutover (Task 16). |
| C15 | **Cron ownership at cutover.** | -- | Cut over by a logged volume edit of the undeclared `school-patrol` job (`openclaw cron edit`); `openclaw-bgxc` (declare it in gitops) is welcome but not a blocker. |
| C16 | **When the importance rules land.** Rules apply only to events written after them (4.10). | -- | With the shadow enabling (Task 2): `journal.rules` with the two attendance rules documented in `values.yaml`, so shadow digests already carry importance and groups. |
| C17 | **Journal unavailable or a reader refused in journal mode.** | Spec 6: pre-write mismatch aborts that agent and alerts. | Never fall back to items (that would mix sources). Keep the agent's previous files, record `journal_unavailable` / `journal_reader_refused` / `journal_guard_abort`, exit 0 (scheduler rule), and make `--status` unhealthy after 3 consecutive runs. A roster-hash mismatch younger than 10 minutes is a warning only. |
| C18 | **When step 6 may follow step 5.** Rolling back step 5 after step 6 needs items again. | Each step its own release. | Step 6 starts no earlier than 10 school days on journal mode with no rollback and no open guard abort. |
| C19 | **The gateway's journal token and `main`.** The token is mounted in the gateway container, where `main` has `exec`; `readers: "*"` lets it read as anyone. | 4.9: the gateway reads as any member. | Accept: `main` can already read every audience directory through `exec`, so this adds no reach. Mount the token in the gateway's main container only (not sidecars), give the gateway no `write_types` (C8), and note it in the capability map. Person-agents have no `exec` and a workspace-confined `read`, so they cannot reach the token file. |

---

## How openclaw authenticates and what it may read

Taken from `docs/journal-api.md` and `docs/journal-roster.md`; nothing here is new glovebox behaviour.

- **Identity:** ServiceAccount `openclaw/openclaw-gateway`. The gateway pod keeps `automountServiceAccountToken: false` and gains one **projected** token volume, audience `glovebox-journal`, `expirationSeconds: 600`, mounted read-only at `/var/run/secrets/glovebox-journal/token` in the main container. The client re-reads the file for every request (kubelet rotates it).
- **Policy entry** (gitops HelmRelease values): `journal.services."openclaw/openclaw-gateway": {read: true, counts: true, readers: "*"}`. `"*"` admits any id at the policy step only; the journal still requires the reader to be on the roster and not erased, and computes its grants from the roster (`openclaw-household-v1`): `(m, subject)`, `(c, guardians)` for each `c` in `guardian_of` (and `("", guardians)`), `(s, siblings)` for each `s` in `m.siblings`, `(x, household)` for household members.
- **Network:** `journal.networkPolicy.from: [{podSelector: {matchLabels: {app: openclaw, component: gateway}}}]` (glovebox side) and an egress rule in `networkpolicies-openclaw.yaml` from the gateway to the glovebox core pods on 9095 (`journal-roster.md` 7.4). URL `http://glovebox-glovebox-journal:9095` (release `glovebox`, Service `<fullname>-journal`).
- **What it may call:** `GET /v1/state`, `GET /v1/events`, `GET /v1/counts`, `GET /v1/healthz`. Each read names `reader=<one roster id>` and only the endpoint's own filters; any other access parameter is `400`. Not `POST /v1/notes` (C8), never `/v1/admin/*`.
- **What comes back:** only rows and events the reader's computed grants cover, filtered twice inside glovebox (SQL and Go; a disagreement is never served and fires `GloveboxJournalVisibilityMismatch`). `roster_sha256` on every state answer and on healthz. Withheld values read `withheld: failed content scan` or `withheld: different audience`.
- **What openclaw checks on top** (Task 7): the row's `data_subject` is a child the reader may see (itself, or a child it is guardian of), the reader is in openclaw's own `audience.Resolver(...).Resolve(data_subject, audience)` members, the scope's `data_subject` part equals the row's, and the roster hash the journal used equals the hash of the `household.json` bytes the patrol loaded. Any failure aborts that agent's whole write.
- **Error answers the patrol acts on:** `401`/`403 forbidden`/`503 auth_unavailable` = deployment fault (alert, no write); `403 policy` for one reader = that reader refused (off the roster or erased; no write for that reader, previous files kept, `journal_reader_refused`); `503 unavailable` with `Retry-After` = retry within the run up to a budget, then `journal_unavailable`.

---

## File structure

**openclaw** (`image/`):

| File | Responsibility |
|---|---|
| `internal/journalclient/client.go` | HTTP client: token file per request, `Authorization: Bearer`, timeouts, strict decoding of answers, refusal shape (`status`, `reason`, `detail`). |
| `internal/journalclient/types.go` | Wire types for `/v1/state`, `/v1/events`, `/v1/counts`, `/v1/healthz` (openclaw's own; glovebox's `journalapi` is an internal package). |
| `internal/journalclient/events.go` | Paging `/v1/events` from a cursor to the end (`next`), page limit 1000. |
| `internal/journalclient/testdata/*.json` | Golden answers copied from glovebox (Task 4), with the glovebox commit they came from. |
| `internal/journalclient/journaltest/fake.go` | Fake journal: rows and events in memory, visibility from a `household.json` fixture written independently of glovebox's adapter, refusals on demand. |
| `internal/journalguard/guard.go` | Pre-write check for rows and events; roster hash comparison. |
| `cmd/schoolpatrol/journal_mode.go` | `--school-source`, `--journal-url`, `--journal-token-file`, `--journaled-items`; mode dispatch in `run.go`. |
| `cmd/schoolpatrol/journal_state.go` | Rows -> schema-2 child state (per record type mapping). |
| `cmd/schoolpatrol/journal_digest.go` | Events -> digest (grouping, ordering, notify section, observed window, withheld markers). |
| `cmd/schoolpatrol/shadow.go` | Side files, the comparison, `shadow.jsonl`, shadow metrics. |
| `cmd/schoolpatrol/journal_summary.go` | Household summary counts (C9) and group files (C10) from journal views. |
| `cmd/schoolpatrol/*_test.go` | `journal_state_test.go`, `journal_digest_test.go`, `shadow_test.go`, `journal_isolation_test.go`, `journal_mode_test.go`. |
| `cmd/schoolpatrol/state.go` (modify) | Per-reader `journal_cursor` (additive; state version stays 2). |
| `cmd/schoolpatrol/status.go` (modify) | Journal and shadow lines; unhealthy rules of C17 and the shadow privacy category. |
| `cmd/schoolpatrol/canary.go` (modify) | Canary self-test also in journal mode, against the fake. |
| `docs/superpowers/specs/2026-10-09-school-patrol-design.md` | New section 16 "Journal source"; section 15 marked as items-mode only. |
| `docs/specs/05-agent-tool-capability-map.md` | Gateway journal token, C19. |
| `ops/gateway-volume/CHANGELOG.md`, `cron-jobs.json` | Every volume change of Tasks 14, 16, 18, 19. |

**gitops:** `clusters/orac/apps/glovebox/glovebox.yaml` (values), `deployment-openclaw-gateway.yaml` (projected token; the `subPath` roster mount stays, C5), `networkpolicies-openclaw.yaml` (egress 9095), plus the core plan's step-1 files.

**glovebox:** `internal/journal/httpapi/golden_test.go` and `docs/journal-api-examples/` (Task 4); `connector/records` and both connectors for `suppress_items` (Task 17); chart values; `docs/journal-api.md`, `docs/upgrading.md`, `docs/connectors/*.md`, `CHANGELOG.md`.

---

## How to work this plan

**Beads.** File one under the glovebox epic: `bd create --parent glovebox-wekc "Record journal Part C: openclaw reads the journal (rollout steps 4-6)"`, with children for shadow (step 4), cutover (step 5) and stop-items (step 6), and one openclaw bead for the patrol work linked to it (`openclaw-59lb` is superseded once Task 19 lands; do not close it before). Cross-link `glovebox-n1x1`, `glovebox-wekc.18.1`, `glovebox-wekc.18.3`, `glovebox-wekc.15` (superseded by step 6), `openclaw-16h2`, `openclaw-bgxc`.

**Feedback loop, openclaw tasks** (from `image/`):

```bash
gofmt -l ./cmd/schoolpatrol ./internal/journalclient ./internal/journalguard
go build ./...
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@latest ./cmd/schoolpatrol/... ./internal/journalclient/... ./internal/journalguard/...
go test ./cmd/schoolpatrol/... ./internal/... -count=1 -race
# before every push: the image build runs the same tests in the container
podman build -f image/Dockerfile -t openclaw-schoolpatrol:part-c .   # from the repo root
```

**Feedback loop, glovebox tasks:** as Part B ("How to work this plan" in the B1 plan), plus `scripts/journal-test-db.sh` for Task 4 and Task 17's end-to-end test, and `REQUIRE_BASE_REF=1 scripts/chart-checks.sh` for chart changes.

**Rules:**

- ASCII only, no emoji, anywhere.
- glovebox is public: placeholders only (`e_child1`, `e_parent1`, `c1`, `EXAMPLE SCIENCE 7`). openclaw test data uses the same placeholders.
- No live calls from tests. The fake journal and golden answers stand in; the shadow period on the cluster is the end-to-end test (spec 8).
- Items mode stays byte for byte as today: `TestItemsModeUnchanged` (Task 11) pins it.
- A scheduled patrol run exits 0 whatever happens (scheduler disables a job after 10 failures, `openclaw-pxt5`); `--status` is the alarm.
- Every hand change on the gateway volume (cron edits, AGENTS.md edits, state resets) is logged in `ops/gateway-volume/CHANGELOG.md` with `ops/gateway-volume/snapshot.sh` run, in the same piece of work (openclaw CLAUDE.md). Check cron ownership first: `school-patrol` is undeclared (volume), `school-patrol-health`, `triage*`, `glovebox-*` are declared in gitops.
- No kubectl writes from an agent session; gitops changes are merged to main and applied by Flux.

---

## Phase 1 -- prerequisites (gitops, glovebox docs)

### Task 1: Glovebox docs: the current state of the blockers

`docs/journal-api.md` ("Known follow-ups") and `docs/upgrading.md` ("Record journal") still say gitops-kxpb blocks enabling. It was closed 2026-10-29 (gitops !112, a ValidatingAdmissionPolicy limiting the watchdog to the gateway, verified live).

- [x] **Step 1.1:** Update both docs: kxpb fixed by gitops !112; enabling is no longer blocked by it. Keep the sentence about why it mattered.
- [x] **Step 1.2:** In `docs/journal-api.md` "Known follow-ups", add the Part C gates: `glovebox-n1x1` must be closed and deployed before the cutover (Task 16), `glovebox-wekc.18.3` confirmed before the shadow passes (Task 15).
- [x] **Step 1.3: Commit** (`docs(journal): kxpb is fixed; Part C gates`).

### Task 2: Gitops: enable the journal and both connectors' records, in shadow

Each step is its own gitops change, merged to main, applied by Flux, verified before the next. The checklists are not repeated here; follow them exactly.

- [ ] **Step 2.1: Journal core (rollout step 1).** Do the core plan's "Gitops checklist for rollout steps 0 and 1" (`docs/superpowers/plans/2026-10-16-record-journal-core.md`) and `docs/journal-roster.md` section 7, in their order: step-0 verification, Vault entry, CNPG role and database, ExternalSecrets, operator ServiceAccount, the roster ValidatingAdmissionPolicy and binding, the roster immutability test (record its output in the change), then `journal.enabled`. Add `journal.rules` with the two attendance rules documented in `values.yaml` in the same change (C16). Verify: `glovebox_journal_ready` 1, healthz `roster_sha256` equals `sha256sum` of the ConfigMap's `household.json`.
- [x] **Step 2.2: Decide C1.** Decided 2026-11-02: no seed. Continue with 2.3.
- [ ] **Step 2.3: PowerSchool records.** `docs/upgrading.md` "PowerSchool records (Part B1)", enabling checklist steps 1-4. Pre-checked 2026-11-02 by reading gitops: the PowerSchool rules name `audience` explicitly, `teacher_comments` is off, no `loginOnDemand`. Re-check at the time.
- [ ] **Step 2.4: Schoology records.** `docs/upgrading.md` "Schoology records (Part B2)", steps 1-4. Pre-checked: `schoologyConnector.state.persistent: true` is already set, each kid has exact `assignment` and `assignment_detail` rules with explicit audiences. Steps 2.3 and 2.4 may land in either order or together; either may run alone in shadow.
- [ ] **Step 2.5: Observe** for one poll window: both spools empty, `glovebox_journal_snapshots_total{status="applied"}` growing for both owners, no held snapshot, no `GloveboxJournal*` alert, items unchanged (`schoolpatrol --status` healthy, item counts in `runs.jsonl` as before).
- [ ] **Step 2.6: Rollback** for any step: the checklist's own rollback (records off; journal off). Nothing openclaw reads has changed yet.

### Task 3: Gitops: the gateway as a journal reader

One gitops change (it touches the gateway pod; a gateway restart drops in-flight Telegram messages, so merge it at a quiet time).

- [ ] **Step 3.1:** `deployment-openclaw-gateway.yaml`: a projected volume `glovebox-journal-token` (`serviceAccountToken: {audience: glovebox-journal, expirationSeconds: 600, path: token}`) mounted read-only at `/var/run/secrets/glovebox-journal` in the main container only. Keep `automountServiceAccountToken: false`.
- [ ] **Step 3.2 (C5, decided: keep `subPath`):** no mount or path change. Instead make the gateway restart part of every roster change: in glovebox `docs/journal-roster.md` (the section on changing the roster) and openclaw's household docs (`image/internal/household/household.go` package comment points there), add the step "after the `openclaw-household` change has reconciled, restart the openclaw gateway at a quiet time (a restart drops in-flight Telegram messages), then check that `sha256sum /seed/household.json` in the gateway equals the journal healthz `roster_sha256`". Until that restart the pre-write check fails safe: agents' school files are not rewritten, previous files stay, and `--status` reports the mismatch (C17's handling).
- [ ] **Step 3.3:** `networkpolicies-openclaw.yaml`: egress from the gateway pods to `app.kubernetes.io/name: glovebox, app.kubernetes.io/component: scanner` on TCP 9095.
- [ ] **Step 3.4:** `glovebox.yaml`: `journal.services."openclaw/openclaw-gateway": {read: true, counts: true, readers: "*"}` and the gateway peer in `journal.networkPolicy.from` (check the pod labels: `kubectl -n openclaw get pod -l app=openclaw,component=gateway`, read-only).
- [ ] **Step 3.5: Verify** (read-only): from the gateway container, `curl -s -H "Authorization: Bearer $(cat /var/run/secrets/glovebox-journal/token)" http://glovebox-glovebox-journal:9095/v1/healthz` answers `status: ok` with the roster hash, and `sha256sum /seed/household.json` matches it. A read as a non-member id answers `403 policy`.
- [ ] **Step 3.6:** Update openclaw `docs/specs/05-agent-tool-capability-map.md` (C19) in the openclaw repo.

### Task 4: Glovebox: golden answers for consumers

openclaw cannot import glovebox's `internal/journal/journalapi`. Its decoder is pinned to answers produced by the real API instead.

**Files:** Create `internal/journal/httpapi/golden_test.go`, `docs/journal-api-examples/{state,events,events-empty,counts,healthz,refusal-policy,refusal-bad-request,refusal-unavailable}.json`.

- [ ] **Step 4.1: Write the test.** Behind `JOURNAL_TEST_DATABASE_URL` (as Part A's DB tests): assemble the API as B1 Task 23 did, apply PowerSchool and Schoology snapshots for `e_child1`/`e_child2` (one course mark, one scored assignment update, a removed attendance mark, a windowed `lms_assignment` that left the listing, a withheld text value, an audience change), read as `e_parent1` and `e_child1`, and write the answers with volatile fields (times, seqs, hashes) normalised. With `-update` it rewrites the files; without, it fails on any difference.
- [ ] **Step 4.2: Run** `scripts/journal-test-db.sh ./internal/journal/httpapi/ -run TestGoldenAnswers` (WSL: `JOURNAL_PG_HOST_NETWORK=1 ... -p 1`). PASS.
- [ ] **Step 4.3:** `docs/journal-api.md` links the examples ("consumers pin their decoders to these").
- [ ] **Step 4.4: Commit** (`test(journal): golden answers for consumers`).

---

## Phase 2 -- the patrol reads the journal (openclaw code, TDD)

### Task 5: `journalclient`

**Files:** `image/internal/journalclient/{client.go,types.go,events.go}`, tests, `testdata/` (copied from Task 4 with a `SOURCE` file naming the glovebox commit).

- [ ] **Step 5.1: Failing tests:** decode every golden answer strictly (unknown keys are an error, so a glovebox change shows up as a failing test, not a silent drop); a refusal decodes to `(code, reason)`; the token file is read for every request and never logged (a test greps the log buffer); a query carries `reader` exactly once and nothing else about access; `EventsAfter(reader, cursor)` follows `next` to an empty page and returns the last `next`; `Retry-After` on 503 is honoured within a run budget; a response over 32 MiB is refused.
- [ ] **Step 5.2: Implement**, stdlib only, timeouts 60 s per request.
- [ ] **Step 5.3: Run the loop; commit** (`feat(journalclient): read-only journal client`).

### Task 6: The fake journal

**Files:** `image/internal/journalclient/journaltest/fake.go`, test.

- [ ] **Step 6.1: Failing tests:** the fake serves state and events from in-memory rows, computing visibility from a `household.json` fixture by the rule in `journal-roster.md` section 2 (written here from the doc, not from glovebox code); it can be told to answer 401, 403 policy for one reader, 503 with `Retry-After`, a wrong `roster_sha256`, or (for Task 12) to ignore `reader` and serve everything.
- [ ] **Step 6.2: Implement; commit** (`test(journalclient): fake journal`).

### Task 7: `journalguard`, the pre-write check

**Files:** `image/internal/journalguard/guard.go`, test.

- [ ] **Step 7.1: Failing tests** (fixture: `e_parent1`, `e_parent2` guardians of `e_child1`, `e_child2`; the children siblings of each other):
  - a `[subject, guardians]` row about `e_child1` passes for `e_child1`, `e_parent1`, `e_parent2`, fails for `e_child2`;
  - a `[guardians]` row (term comment) fails for `e_child1`;
  - a row whose scope `data_subject` part differs from its `data_subject` fails;
  - a row about an id not in the roster, about `""`, or with an audience the openclaw resolver cannot resolve fails (fail closed, same as items' `audience_unresolved`);
  - an event is checked on its current `audience`; an `updated` event's old values are accepted only as the journal sends them (withheld markers pass through as text);
  - **asymmetric siblings:** a roster where `e_child1` lists `e_child2` as sibling but not the reverse: glovebox would grant `e_child1` `(e_child2, siblings)`, openclaw's resolver gives `Siblings(e_child2) = []`; a `[siblings]` row about `e_child2` read as `e_child1` fails the guard. This is the stricter answer and is pinned on purpose (school rows carry no `siblings` today);
  - roster hash: equal passes; different and younger than 10 minutes (by the patrol's own record of when it first saw the difference) is a warning and **no write**; older is `journal_guard_abort` with an alert.
- [ ] **Step 7.2: Implement** on `household.Load` and `audience.NewResolver(...).Resolve`; no shared code with the item path's `assertPrivacy` beyond those two packages.
- [ ] **Step 7.3: Commit** (`feat(journalguard): pre-write check for journal rows`).

### Task 8: Journal rows to child state (schema 2)

**Files:** `image/cmd/schoolpatrol/journal_state.go`, test.

Mapping (the lists of patrol spec section 15, from typed fields instead of tags and body lines):

| List | From | Rule |
|---|---|---|
| `courses` | `course_mark` | course, period, teacher; mark and percent of the most recent term whose `<term>_letter` or `<term>_percent` is not null, in the connector's term order (`q1` ... `s2`; confirm the order against `RecordTypes()` before coding) |
| `missing`, `incomplete`, `late` | `assignment` | the bool flag; `teacher_comment`, `description` as text |
| `due_soon` | `assignment` (`due` within `--state-due-days`, not scored, collected or exempt) and `lms_assignment` (`due` within the window, `in_latest_snapshot` true) | the cross-system match of section 15 ("one assignment, two systems") is unchanged |
| `overdue_lms` | `lms_assignment` `status=overdue`, `in_latest_snapshot` true, no gradebook match | the evidence rule's drop is gone: a row that left the listing goes to `no_longer_listed` (C13) |
| `no_longer_listed` | `lms_assignment`, `in_latest_snapshot` false, `confirmed_at` within 14 days | new list; counted after 14 days |
| `recently_scored` | `assignment` `updated` events whose `changed` has `score` going from null to a value, `observed_at` within `--state-recent-days` | read with `type=assignment` from a per-reader seq recorded 14 days ago (the patrol keeps a small date-to-seq map in `patrols/school/state.json`: seqs only) |
| `attendance.totals` | `attendance_total` | absences, tardies |
| `attendance.recent` | `attendance_day` within `--state-attendance-days`, with its `attendance_mark` rows by date | classification, the counts, kinds; a day with `unknown_codes > 0` is flagged; the household note for that date beside it if one exists (C8) |
| `term_grades` | `term_grade` | course, term, grade |
| `freshness` | per source, the newest `confirmed_at` of the child's rows | stale after 3 school days, as today |

- [ ] **Step 8.1: Failing tests** with fake-journal rows: each list, removed rows dropped, `approximate` labelled, `unconfirmed` from `confirmed_at` (C14), withheld markers rendered as data, a detail row joined to its listing row by key and never by title, bounds and the 64 KiB rebuild ladder unchanged, two runs with the same `--now` byte-identical.
- [ ] **Step 8.2: Implement** reusing the existing renderer (`schoolstate_render.go`) and sanitiser; only the input changes. `state.md` gains the record type and key per row in a compact column.
- [ ] **Step 8.3: Commit** (`feat(schoolpatrol): state from journal rows (schema 2)`).

### Task 9: Journal events to digests

**Files:** `image/cmd/schoolpatrol/journal_digest.go`, test; `state.go` (cursor).

- [ ] **Step 9.1: Failing tests:** events grouped by `group_key` (six period marks of one day are one entry), high before normal, low as counts per type (C7); each change shows "between <prev_observed_at> and <observed_at>"; `updated` shows old -> new per field, withheld markers verbatim; `removed` reads "no longer in <source>"; events whose `notify` has `push` or `prompt` for this reader go to a "Needs attention" section at the top, without purpose text (C8); the framing of patrol spec section 4 (untrusted text quoted after a generated prefix) is unchanged; the cursor is saved only after the digest file is on disk; a reader with no cursor starts at the head with no digest (C6).
- [ ] **Step 9.2: Implement.** The per-reader cursor is `readers.<eid>.journal_cursor` in `patrols/school/state.json` (additive; version stays 2, per section 5's rule).
- [ ] **Step 9.3: Commit** (`feat(schoolpatrol): digests from journal events`).

### Task 10: Shadow mode and the comparison

**Files:** `image/cmd/schoolpatrol/shadow.go`, test.

`--school-source shadow` runs today's items path unchanged, then per reader reads the journal, runs the guard, and writes `school/shadow/{state.md,state.json,digest-<UTC>.md,diff.md}` (C3). It never changes what agents read.

What is compared, per reader and child, after the guard:

1. **State rows.** Item-fold records are matched to journal rows by key: `assignment` by assignment id (item tag, journal key suffix); `course_mark`, `attendance_total` by the connector's course key; `term_grade` by the item key (equal to the record key, S5); `lms_assignment(_detail)` by assignment id; attendance by (date, course key). Each pair or loner gets one category:
   - `equal` -- same values (ignoring `as_of`).
   - `journal_only_lookback` -- an assignment older than the item path's lookback (records have none). Explained.
   - `items_only_history` -- a record whose items predate the journal's first snapshot of its scope (no seed, C1). Explained.
   - `items_only_left_window` -- an LMS assignment the journal holds with `in_latest_snapshot=false`. Explained.
   - `items_only_removed_at_source` -- the journal has it `removed` (the evidence rule kept it). Explained: this is the improvement.
   - `differs_parse` -- a value differs where the item value came from a body line (a fact conflict blanked it, a forged line, a 40-byte cut). Explained when the journal value equals the source's typed value in the item's tags or body under the documented rule; otherwise unexplained.
   - `differs_withheld` -- the journal withheld a text value. Explained only if `glovebox_journal_scan_withheld_total` counts a real scan failure (not a cold-start verdict error, wekc.18.3).
   - `unexplained` -- anything else.
2. **Events against new items.** Every new item of a journaled kind in the run should have a journal event for the same record with `observed_at` within the item's poll; events with no item are expected for `updated` Schoology fields (the seen-set suppresses those items) and for `removed`; anything else is `unexplained`.
3. **Privacy.** A journal row that passes the guard for a reader whose item counterpart was withheld from that reader (misrouted, audience narrower), or any guard abort, is category `privacy` -- never explained, stops the shadow (Task 15).
4. **Roster.** The journal's `roster_sha256` equals the hash of the patrol's `household.json`.

- [ ] **Step 10.1: Failing tests** for each category with item fixtures and fake-journal rows built from the same placeholder data; `diff.md` lists every non-equal pair with both values (it is in the reader's own workspace, same entitlement); `shadow.jsonl` holds per reader and category counts and record keys hashed (SHA-256, first 12 hex), nothing else; metrics `school_patrol_shadow_rows{category}`; `--status` prints the last shadow line and is unhealthy on `privacy > 0`, not on other categories.
- [ ] **Step 10.2: Implement.** A shadow failure (journal down) is a warning and never affects the items path or `result`.
- [ ] **Step 10.3: Commit** (`feat(schoolpatrol): shadow comparison against the journal`).

### Task 11: Journal mode

**Files:** `image/cmd/schoolpatrol/journal_mode.go`, `journal_summary.go`, tests; `run.go`, `status.go`.

- [ ] **Step 11.1: Failing tests:**
  - `TestItemsModeUnchanged`: default flags produce byte-identical files and state to `main` over the existing fixtures;
  - **both sources or none:** `--school-source journal` takes no per-source option; the patrol refuses (usage error, exit 2) any `--sources` value other than both school sources while in journal mode;
  - items of journaled kinds (PowerSchool all kinds; Schoology `assignment`, `assignment_detail`) are marked handled without delivery under `--journaled-items ignore`, delivered as plain digest items (not state) under `deliver` (C12); posts and messages flow as today in both;
  - household summary from each child's own view (C9); group files as the intersection (C10), withheld when any member's guard fails;
  - C17: journal unavailable -> previous files kept, `journal_unavailable`, exit 0, `--status` unhealthy after 3 runs; `403 policy` for one reader -> that reader only, `journal_reader_refused`.
- [ ] **Step 11.2: Implement.**
- [ ] **Step 11.3: Commit** (`feat(schoolpatrol): journal mode for both school sources`).

### Task 12: Isolation tests (spec 6, spec 8)

**Files:** `image/cmd/schoolpatrol/journal_isolation_test.go`; `canary.go`.

- [ ] **Step 12.1: Tests** over the fake journal with two child agents and two guardian agents, in shadow and journal mode: no child's files (state, digest, shadow, diff) contain a row, key or text of the other child or any `[guardians]`-only row; each guardian's files hold both children; `main` gets nothing; with the fake told to ignore `reader` (the "deliberately wrong reader" of spec 6), the guard aborts the affected agents and nothing is written for them; group files hold only the intersection.
- [ ] **Step 12.2:** `--selftest-canary` gains a journal leg against an in-process fake (hermetic, as today).
- [ ] **Step 12.3: Commit** (`test(schoolpatrol): journal isolation and canary`).

### Task 13: Docs, image, container check

- [ ] **Step 13.1:** Patrol spec: new section 16 "Journal source" (modes, the guard, mapping, digests, shadow categories, failure rules, what it still cannot know: Schoology rows that left the window, attendance before the journal's first grid); section 15 marked items-mode. `ops/gateway-volume/README.md`: shadow files are regenerated, not hand-edited.
- [ ] **Step 13.2:** `podman build` the image (runs every test); then `podman run --read-only` the image's `schoolpatrol --school-source journal --dry-run` against a fake journal container or host process and a fixture root, checking it starts, reads the token file, prints the plan, logs no token.
- [ ] **Step 13.3:** MR to openclaw, review, merge; the gateway image moves through the normal image automation.

---

## Phase 3 -- rollout (cluster; volume changes are logged)

### Task 14: Start the shadow (rollout step 4)

Requires Tasks 2, 3 and 13 deployed. Either source may already be in shadow; the comparison counts only once both are.

- [ ] **Step 14.1:** `schoolpatrol --school-source shadow --dry-run` by hand on the gateway; read the plan.
- [ ] **Step 14.2:** `openclaw cron edit` of `school-patrol` (undeclared, C15): add `--school-source shadow --journal-url http://glovebox-glovebox-journal:9095 --journal-token-file /var/run/secrets/glovebox-journal/token`, and the new `--household` path (C5). Run `ops/gateway-volume/snapshot.sh`, add the CHANGELOG entry (what, why, undo: the previous command line, bead), commit.
- [ ] **Step 14.3:** After the first scheduled run: `runs.jsonl` result ok, `shadow.jsonl` has a line per reader, the guard aborted nothing, `school/shadow/` exists for the four person-agents and not for `main`.

### Task 15: The shadow period

- [ ] **Step 15.1:** Each school day, read `shadow.jsonl` and a guardian's `school/shadow/diff.md`; for every `unexplained` row, find the cause and either fix it (patrol mapping, connector record, journal) or add an explained category with a test. Note each in the openclaw plan copy under this task.
- [ ] **Step 15.2: Pass criteria (C4):** at least 10 school days with both sources in shadow, spanning a weekend; the last 3 consecutive school days with `unexplained = 0`; `privacy = 0` and no guard abort on every day (any privacy row stops the shadow until its cause is fixed and the count restarts); `GloveboxJournalVisibilityMismatch` never fired; no held snapshot left unresolved; spools empty at each check; roster hashes equal; `glovebox-wekc.18.3` closed and `differs_withheld` rows only for real scan failures; content seen at least once: a settled `attendance_day`, an `assignment` scored (`updated` score), a removal or an explicit note that none occurred, a Schoology `updated` or an explicit note.
- [ ] **Step 15.3:** Record the result (counts per category per day) in the openclaw plan copy and the Part C bead; Steve signs off.

### Task 16: The cutover, both sources in one step (rollout step 5)

**Preconditions (all, checked and written into the CHANGELOG entry):** Task 15 passed; `glovebox-n1x1` closed **and** its connector release deployed and observed in shadow for at least 3 school days; both connectors' records on and applied; `--status` healthy.

- [ ] **Step 16.1:** Prepare the `AGENTS.md` "School updates" text for schema 2 (adult and child versions: rows carry record type and key, "as of" is the last time the school system confirmed it, "no longer listed" means status unknown, "Needs attention" at the top of a digest). Back up each file beside it (`AGENTS.md.bak-<date>`), same text in the gitops seed (write-if-absent).
- [ ] **Step 16.2:** `openclaw cron edit` of `school-patrol`: `--school-source journal --journaled-items ignore` (other flags as in 14.2). Both sources switch here, together; there is no intermediate state.
- [ ] **Step 16.3:** Run once by hand (`--no-wake` as the job does), then: each child's `state.json` has `"schema": 2` and only their own rows; each guardian's has both; digests carry only the day's events; household summary and group files written; `--status` healthy.
- [ ] **Step 16.4:** Snapshot, CHANGELOG entry (preconditions, commands, AGENTS.md change, undo), commit.
- [ ] **Step 16.5: Rollback:** `cron edit` back to `--school-source shadow` (agents get schema-1 files from items on the next run, since items still flow) and restore `AGENTS.md.bak-<date>`. Logged the same way.

### Task 17: Glovebox: suppress items for rows sent as records (code for step 6)

**Files:** `connector/records/config.go` (`suppress_items`), both connectors' emit paths, chart values (`connectors.powerschool.records.suppressItems`, `schoologyConnector.records.suppressItems`), docs.

- [ ] **Step 17.1: Failing tests:** with `suppress_items` off (default) items are byte-identical to today (extend PowerSchool's `TestRecordsOnStagesTheSameItems` and the Schoology equivalent); with it on, an item is not staged exactly when the same poll put its row into a snapshot that reached the spool (C11); a degraded Schoology detail read and an unconfirmed term-comment page still stage items; posts, messages and attachments always stage; a spool write failure stages the items (never lose both); the item checkpoint is still advanced for suppressed rows, so turning suppression off later does not re-stage history; the config is refused with records off.
- [ ] **Step 17.2: Implement;** chart refuses `suppressItems` without `records.enabled`.
- [ ] **Step 17.3:** Docs: both connector docs, `upgrading.md` (an enabling checklist for step 6 with rollback), CHANGELOG; spec 9 step 6 amended ("exactly the rows sent as records", Part C decision C11).
- [ ] **Step 17.4:** Release; commit and MR in glovebox.

### Task 18: Stop the items (rollout step 6)

Not before C18: 10 school days on journal mode, no rollback, no open guard abort.

- [ ] **Step 18.1:** One gitops change: `suppressItems: true` for **both** connectors (the same both-together rule; a PowerSchool-only suppression would leave the patrol consistent but is not needed and is not done).
- [ ] **Step 18.2:** The same day, `cron edit` of `school-patrol`: `--journaled-items deliver` (C12). Log it.
- [ ] **Step 18.3: Verify:** PowerSchool item arrivals drop to the non-record rows only; Schoology posts and messages keep arriving and reach digests; triage queue normal; journal snapshots unchanged; `household-summary.md` "new posts" unchanged; patrol `--status` healthy. The freshness lines in state files come from `confirmed_at` (Task 8), so they do not go stale when items stop; the household summary's per-source "newest item" line is replaced by the journal's newest `confirmed_at` per source in the same task (it would otherwise report PowerSchool stale).
- [ ] **Step 18.4: Rollback:** `suppressItems: false` (items resume from the checkpoint, no history replayed), `--journaled-items ignore`.

### Task 19: Close-out

- [ ] **Step 19.1:** After 10 more school days: delete the item fold, body-line parsing and evidence rule from `schoolpatrol` (spec 6, last bullet), keep `--school-source items` only as long as Steve wants a rollback path (recommend: remove `shadow` and the fold together; `items` mode then means "no state files").
- [ ] **Step 19.2:** Close `openclaw-59lb` (superseded), `glovebox-wekc.15` (superseded), the Part C beads; amend spec 6 and 9 with C-decisions as built.

---

## Known blockers and open beads

| Bead / issue | Effect on Part C | Gate |
|---|---|---|
| Journal not enabled in gitops (rollout step 1 undone) | Nothing to read. | Task 2.1. |
| `gitops-kxpb` | Closed 2026-10-29 (gitops !112); glovebox docs still list it as blocking. | Task 1 fixes the docs. |
| `glovebox-n1x1` (Schoology detail shared across kids) | A detail record for kid 2 can carry comments fetched while viewing kid 1. Items behave the same today, but records persist it per subject. Fixed in glovebox !127 (per-kid reads after a view-child switch, Steve approved the switch GET 2026-11-02). | Closed and deployed before the cutover; at least 3 school days of shadow on the fix (Task 16). |
| `glovebox-wekc.18.3` (scanner cold start) | First scans after a core restart can time out, storing text as withheld until a rescan: shows as `differs_withheld`. | Closed 2026-11-02: the warm-up scan before readiness was already on main (`journal_listener.go` `warmScanner`). After any core restart during shadow, `POST /v1/admin/rescan` from the operator pod. |
| `glovebox-wekc.18.1` (erase fan-out) | Journal-derived copies in workspaces (state files, digests, shadow files) are not erased by a journal erase. After an erase the erased member's reads are `403`, so their own files stop updating and stay; guardians' state files drop the erased child's rows on the next run; digests stay until `--keep-digests`. | Not a gate (P3, decided a later improvement). Document in patrol spec section 16; the erase runbook adds "delete the member's `school/` folder" by hand, logged. |
| Gateway `subPath` roster mount | Roster hash differs from the journal's after any roster change until the gateway restarts; the guard then writes nothing. | Not a gate (C5 decided): every roster change ends with a gateway restart (Task 3.2). |
| `openclaw-bgxc` (cron-reconcile tz/timeout) | `school-patrol` stays undeclared; cutover is a logged volume edit. | Not a gate (C15). |
| `openclaw-16h2` (wake safety gates) | Pushes and prompts stay off; shown at the top of digests. | Not a gate. |
| `openclaw-90k5` (person-agent `memory_search`) | After step 6, new school data is no longer in audience inboxes for recall; agents read `school/state.md` and digests (as they do today, because recall is broken). | Not a gate; note in AGENTS.md text. |
| Notes tool | Out of Part C (C8); its own plan. | -- |

---

## Out of this plan

- The seed Job (C1).
- The notes tool, `attendance_followup` purpose text, pushes and prompts (C8, `openclaw-16h2`).
- The dashboard reading the API (rollout step 7) and retiring group files (C10).
- Erase fan-out (`glovebox-wekc.18.1`).
- Linking one assignment across PowerSchool and Schoology (spec 10).
