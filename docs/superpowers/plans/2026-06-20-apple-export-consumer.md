# Apple Export Consumer (`glovebox-5lud`) — Implementation Plan

**Status:** scoped 2026-06-20. Not started.
**Bead:** `glovebox-5lud` (P2, feature). Downstream acceptance: OpenClaw `openclaw-e6f`.
**Producer contract:** `docs/handoffs/recognizer-apple-export-delivery.md` (mirror of
recognizer `docs/handoffs/recognizer-apple-export-delivery.md`).
**Test export on disk:** `/mnt/c/Users/steve/Downloads/apple-export` (per `openclaw-e6f`).

## 1. Goal

Consume the recognizer's Apple "Get a copy of your data" deliveries and stage the
**iTunes/App Store purchase data** (the `media-services` bucket) so OpenClaw can answer
natural-language questions about the user's purchases. iTunes purchases are the priority
(the `openclaw-e6f` acceptance); the other five buckets are secondary.

## 2. Scope decision — "thin unpack + stage" (chosen 2026-06-20)

Glovebox **untars buckets, unpacks nested zips, parses the purchase CSVs into normalized
structured items, and stages them** through the existing staging -> delivery model.
OpenClaw's memory does the querying. **Glovebox does NOT build a query engine/index.**

Rationale: `openclaw-e6f` only requires "a correct answer sourced from the ingested data,"
not a query API. Building an index inside a content-scanning service would be out of role.
(An earlier investigation gloss claimed glovebox must build a query index — that over-read
the acceptance.)

**Noted, not built — the structured-data boundary.** OpenClaw's own refined design says
structured/schema'd data (Apple purchases, like Apple Music/Sonos/Plex) can be accessed
directly by agents via MCP and need not pass through glovebox's injection scanner; only
human free-text must. Apple purchase CSVs are structured, so a future "re-home structured
buckets, glovebox scans only free-text parts (AppleCare notes, marketing bodies)" path is
the more architecturally correct long-term move — but it renegotiates recognizer's delivery
target and is explicitly out of scope for this plan. Revisit with the OpenClaw/recognizer
side if the structured volume grows.

## 3. Design decisions

- **D1 — wire format (DECIDED 2026-06-20: preserve `matcher_id`).** Recognizer delivers every
  bucket as `media_type=archive/generic-tarball` + `matcher_id=apple/<bucket>`. **Glovebox
  parses `matcher_id` but discards it** — it is validated in `metadata.go` but never written to
  the finalize receipt / `metadata.json`, so the importer cannot route on it today.
  **Decision: persist it.** Add `MatcherID` to `FinalizeReceipt`, populate from the parsed
  Upload-Metadata in `finalize.go`; the importer validates `media_type=archive/generic-tarball`
  and branches on `receipt.MatcherID` prefix `apple/`. This is a small, *general* glovebox-only
  fix (matcher_id is a stable producer correlation key worth keeping regardless of Apple) and
  needs **no recognizer change**, keeping the producer contract stable. *Rejected alternative:*
  per-bucket media_types `archive/apple/*` — would require a recognizer-side change and discard
  matcher_id's finer-grained (sub-bucket) routing signal.
- **D2 — role:** thin unpack + stage (see §2).
- **D3 — MVP scope:** `media-services` first (iTunes purchases). Other five buckets get a
  minimal passthrough-stage (raw files staged as-is) or are deferred.
- **D4 — data_subject:** the export is the user's personal data -> stamp `data_subject=e_111111`
  (Steve) via the importer's config-level `data_subject_default`, `audience=["subject"]`. This
  rides the v0.6.0 hardening so purchases route to Steve's agent, not household-wide.
- **D5 — idempotency:** deliveries are idempotent on `archive_id` (recognizer side); the
  importer is a one-shot CLI invoked per finalized delivery, mirroring walhelm.

## 4. Architecture

New `importers/apple/` mirroring `importers/walhelm/` (the finalized-archive importer model):
the importer is pointed at a finalized `archives/<id>/` dir (`metadata.json` + `tree/`),
validates the media_type, walks `tree/`, and stages items via
`Backend.NewItem -> WriteContent -> Commit`.

**New capabilities (no existing reuse in the repo — confirmed):**
- **Nested zip unpacking** (`archive/zip`) for `Apple_Media_Services.zip` (100+ CSVs),
  `Retail Store Receipts.zip`, AppleCare `*.zip`. Must guard against zip-slip.
- **CSV parsing** (`encoding/csv`) for the purchase history files.
Neither exists anywhere in glovebox today; both are net-new utilities for this importer.

## 5. Phases (each a bead; TDD)

1. **Ingest plumbing (D1 — preserve matcher_id).** Add `MatcherID string` to `FinalizeReceipt`
   (`internal/ingest/archives/finalize.go`) and populate it from the already-parsed
   `Metadata.MatcherID`. `archive/generic-tarball` is already in the allow-list — no media_type
   change, no recognizer change. Tests: finalize populates `MatcherID`; receipt round-trips it
   through `metadata.json`.
2. **apple-importer skeleton.** `importers/apple/{main.go,importer.go,ingest.go,config.json}`
   mirroring walhelm. Implements the `importer.Importer` interface; validates the Apple media_type;
   walks `tree/`. Config sets `data_subject_default=e_111111`, `audience_default=["subject"]`.
   Tests: media_type validation, empty-tree rejection, item staging shape.
3. **Nested-zip + CSV utilities.** `importers/apple/unpack.go` (zip-slip-safe nested unzip) +
   `importers/apple/csv.go` (header-aware CSV reader). Unit tests with fixture zips/CSVs.
4. **import-reference parsing.** Parse `apple-import-reference.json` (per-bucket `data_files`,
   `guide_refs`, `notes`) to drive which files to normalize in each bucket. Tests with the
   real fixture from the test export.
5. **media-services normalization (priority).** Parse the purchase/redownload CSVs into
   normalized purchase records; stage as structured items (a JSON record + a human-readable
   `content.extracted.md` summary per the existing enrichment convention) with `data_subject`/
   `audience` set. Tests assert staged `metadata.json` carries `data_subject=e_111111` and the
   purchase fields are present. Use `guide_refs` to map CSV columns.
6. **Other buckets (deferred/minimal).** Passthrough-stage raw files for the remaining buckets
   so nothing is lost; full normalization deferred.
7. **Build/deploy wiring.** `importers/apple/Dockerfile` (enricher-runtime base), `.github/
   workflows/ci.yml` (binary loop + docker matrix entry `glovebox-apple-importer`),
   `release.yml`, and `charts/apple-importer/` (Job + ConfigMap + values), mirroring
   `charts/mbox-importer/`.
8. **E2E.** Run the importer against the real test export
   (`/mnt/c/Users/steve/Downloads/apple-export`, recognizer-delivered) and confirm
   media-services purchase items are staged and contain the iTunes purchase data
   `openclaw-e6f` needs. Hand off to OpenClaw for the natural-language acceptance.

## 6. Risks / open items

- **CSV schema variance** across Apple export versions — lean on `guide_refs`/`import-reference`
  rather than hardcoding columns.
- **Volume** — `Apple_Media_Services.zip` is 100+ CSVs; bound memory (stream, don't slurp).
- **Zip-slip** on nested unzip — validate entry paths (reuse the tar-untar safety posture).
- **D1** — resolved as a glovebox-only change (persist `matcher_id`); no recognizer
  coordination required, so phase 1 is unblocked.
- **Structured-data boundary** (§2) — if this grows, re-home structured buckets out of glovebox.

## 7. Cross-repo

- **Recognizer:** none required — D1 resolved glovebox-side (preserve matcher_id); recognizer's
  current delivery (generic-tarball + matcher_id) is unchanged. Recognizer side already shipped
  (bead `archiver-m4b`, recognizer 0.11.0).
- **OpenClaw:** `openclaw-e6f` is the end-to-end acceptance (separate beads DB).
