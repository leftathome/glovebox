# Glovebox — Action Plan

**Date:** 2026-08-20. Consolidates the state assessment, security review, and
competitive landscape into a prioritized, sequenced plan. Each item names the
problem, the fix, evidence, and rough effort. Priority tiers: **P0** (ship
first — the product's core purpose or a live security hole), **P1** (important),
**P2** (hygiene / strategic).

Companion reports: `state-assessment.md`, `security-review.md`,
`competitive-landscape.md` (+ `state-assessment-raw.md` appendix).

---

## P0 — Scanner efficacy: close the byte-for-byte bypasses

These defeat glovebox's stated purpose *today*: crafted injections reach the
agent unflagged. All are in the scan path, not the roadmap.

### P0-1 Homoglyph / confusable folding
- **Problem:** NFKC does not fold Cyrillic/Greek homoglyphs; ASCII matchers
  miss "ignоre previоus" (Cyrillic о). Spec §6.2 wrongly claims it does.
  (`internal/engine/preprocess.go:34`)
- **Fix:** add a UTS-39 confusable-skeleton / homoglyph-table folding step to
  the *normalized* buffer used for matching (original still delivered
  byte-identical). Flag mixed-script tokens as a signal. Correct the spec.
- **Effort:** ~1.5 days incl. tests.

### P0-2 Strip the full invisible/format set incl. Unicode Tags block
- **Problem:** only 7 zero-width runes stripped; Tags block (E0000–E007F),
  soft hyphen, Mongolian vowel separator pass through — the classic invisible
  prompt-injection vector. (`internal/engine/preprocess.go:20`)
- **Fix:** strip the whole Tags block + Cf/format + deprecated invisibles;
  make `encoding_anomaly` fire/boost on *any* tag-block codepoint, not a count
  threshold.
- **Effort:** ~1 day.

### P0-3 Decode-then-scan for encoded payloads
- **Problem:** base64/hex/URL/entity payloads are flagged (weight 0.7) but
  never decoded, and 0.7 < 0.8 threshold ⇒ PASS; sub-50-char runs dodge the
  regex entirely. (`internal/detector/encoding.go`, `configs/default-rules.json:61`)
- **Fix:** decode common encodings into a scratch buffer and scan the decoded
  form (same pattern HTML-strip already uses — does not violate byte-identical
  delivery). At minimum raise the encoding weight to threshold and quarantine a
  base64 block that itself matches an injection rule.
- **Effort:** ~2 days.

### P0-4 Real streaming or whole-content detectors + size cap
- **Problem:** custom detectors run on only first 64 KB + last 64 KB
  (`io.ReadAll` then sample); mid-document injections evade
  template/encoding/language detectors, and the spec's bounded-memory claim is
  false. (`internal/engine/stream.go:20`, spec §6.6)
- **Fix:** implement chunked streaming with pattern-length overlap as specified,
  OR run global detectors over full content with an explicit size cap that
  quarantines oversized items. Update §6.6 to match reality.
- **Effort:** ~2–3 days.

### P0-5 Scan the channels that currently route around the engine
- **MEDIUM-6 metadata:** run `subject`/`sender` through the detection engine
  and/or render them inert in the quarantine notification the way
  `content.sanitized` is; document that the review agent treats metadata as
  untrusted. (`internal/routing/notify.go:34`)
- **MEDIUM-7 recognizer-scan:** run `content.extracted.md` through
  `internal/scan.Scanner` before publishing it for the operator agent, so the
  archive lane inherits the connector lane's boundary.
  (`internal/ingest/archives/scan_extract.go`, `finalize.go:318`)
- **Effort:** ~2 days combined.

**P0 acceptance:** a new adversarial corpus (see P1-4) with homoglyph,
tag-character, encoded, mid-document, and metadata-channel cases goes from
mostly-PASS to mostly-QUARANTINE, gated in CI.

---

## P0 — Live security holes

### P0-6 SSRF in connector link-fetching
- **Problem:** `LinkPolicy.Check` fails **open** on DNS-lookup error, does not
  pin the resolved IP, and the fetch client (`http.DefaultTransport`)
  re-resolves DNS and follows redirects with no `CheckRedirect` — DNS-rebinding
  and redirect-to-metadata reach cloud metadata / internal services over
  unrestricted connector egress.
  (`connector/content/linkpolicy.go:56,64`, `connector/httpclient.go:34`)
- **Fix:** custom `DialContext` that resolves once and rejects a
  private/link-local/metadata *connected* IP; `CheckRedirect` re-running
  `LinkPolicy.Check` per hop with a hop cap; treat lookup error as **deny**;
  tighten connector egress NetworkPolicy to block RFC1918/link-local/metadata.
- **Effort:** ~2 days incl. tests.

### P0-7 mTLS for `/v1/ingest` (+ fix cross-namespace port exposure)
- **Problem:** ingest is unauthenticated by design (spec 08 §3.10); metadata
  identity is spoofable; transport is plaintext; and because `/v1/archives` and
  `/v1/ingest` share port 9091, the recognizer-namespace NetworkPolicy exposes
  unauthenticated ingest. (`internal/ingest/server.go`,
  `charts/glovebox/templates/{networkpolicy,archive-networkpolicy}.yaml`)
- **Fix:** application-level mTLS via cert-manager with SPIFFE-style client-cert
  SANs; identity middleware that enforces `metadata.source` == verified peer and
  stamps `ingested_by` into the audit log; framework-level client (one change,
  all connectors inherit via 3 env vars); `disabled|permissive|required`
  migration with a `transport` metric label. Split the archive port (or fold
  archives into the same mTLS regime, later retiring spec 10 bearer tokens).
  **Full design in `security-review.md` §"mTLS recommendation".**
- **Effort:** ~6–8 days total (server+middleware 2–3, framework client 1,
  chart+certs 1–2, migration+tests 2).

### P0-8 Flip Vault TLS verification default to on
- **Problem:** chart ships `ingest.auth.vault.tlsSkipVerify: true` →
  `VAULT_SKIP_VERIFY=true` on the token-retrieval path; a spoofed Vault could
  feed attacker tokens. Same for schoology-auth-refresher.
  (`charts/glovebox/values.yaml:1271`)
- **Fix:** default `false`, document mounting the Vault CA, keep skip as
  explicit opt-in.
- **Effort:** ~0.5 day.

---

## P1 — Important correctness, docs accuracy, security depth

### P1-1 Fix the two misleading install-path doc items
- README connector image list, "all 10", "Round 1", `--version 0.2.0`, and the
  key-features list are wrong/stale; `docs/deployment.md` lists only 10 images.
  Regenerate both from the CI image matrix (single source of truth). *(State
  gaps #1, #4)* — ~0.5 day.

### P1-2 Fix `release.yml` to build all connectors + importers
- Release archives omit 14 connectors and all importers while README promises
  "all connector binaries." Drive the build list from the same matrix ci.yml
  uses. *(State gap #2)* — ~0.5 day.

### P1-3 Cut a release and reconcile versions
- Nothing released since 0.6.4 (2026-06-26) despite sanitize gate + health
  probes + chart changes; chart appVersion pinned 0.6.1. Add the missing
  **sanitize-gate CHANGELOG entry**, bump appVersion, tag a release. *(State
  gaps #5, #6)* — ~0.5 day.

### P1-4 Adversarial test corpus + regression gate
- No red-team corpus exists; the P0 efficacy bugs are exactly what one would
  catch. Add a checked-in corpus (homoglyph, tag-char, encoded, mid-document,
  metadata-channel, plus benign negatives to bound false positives) with a CI
  gate. Seed from AgentDojo / LivePI / Vault's public holdout; publish
  detection/FP numbers (every credible 2026 entrant does). — ~2–3 days.

### P1-5 Fuzz the untrusted-input parsers
- `go test -fuzz` targets: `archives/untar.go`, `archives/metadata.go` (base64
  tus metadata), `internal/ingest/handler.go` multipart, `connector/content`
  HTML/MIME, RSS/Atom XML. — ~2 days to stand up + corpus seeds.

### P1-6 Rule-update integrity
- Rules load once from a ConfigMap with no signing/checksum; ConfigMap-edit
  access silently weakens every boundary. Implement the spec §16 checksum
  verification, audit-log rule changes, and consider a signature on
  `rules.json`. — ~2 days.

### P1-7 Enricher isolation
- `pandoc`/`tesseract`/`pdftotext` run on adversarial bytes (RCE surface
  despite fixed argv + stdin). Add seccomp/gVisor, resource limits, upstream-CVE
  tracking to the enricher-runtime. — ~2 days.

### P1-8 Lower-severity security fixes
- LOW-9 `template_structure` suppression bypass; LOW-10 reject `..` archive_id
  at parse time; LOW-11 restrict `/metrics` ingress to monitoring namespace;
  LOW-12 scope CI `packages/id-token/attestations` permissions to build/push
  jobs. — ~1 day combined.

---

## P2 — Hygiene and strategy

### P2-1 Documentation reconciliation sweep
- Un-"coming soon" the connector guide (#3); check off / delete
  `PLAN-onedrive` and `PLAN-teams` (#7); amend
  `nagus-connector-integration.md` to record the dropped ebay/craigslist scope
  (#8); reconcile "notification placeholders" wording (#13); either emit
  per-connector READMEs or fix README's claim (#11); refresh or re-record the
  v0.2-era examples and add ingest/archives/sanitize/enrichment examples (#10).
  — ~1–2 days.

### P2-2 Retire the legacy `docker.yml` workflow
- Redundant with ci.yml on tags; delete or clearly scope it. *(State gap #9)* —
  ~0.25 day.

### P2-3 Adopt proven ideas from the field (competitive report §4)
- **Optional local ML detector** (PromptGuard-2-22M, ONNX, CPU, no API) as a
  weighted detector alongside regex — fixes regex's recall ceiling while keeping
  "no cloud LLM dependency." Fits the spec §16 classifier extension point.
- **Content-marking / spotlighting sidecar** — a trust-label/provenance sidecar
  per item (payload still byte-identical) so agents can apply Rule-of-Two-style
  policies.
- **Canary tokens** for downstream exfiltration detection.
- **Signed audit receipts** (Ed25519, cf. pipelock) — upgrade the JSONL log to
  verifiable evidence.
- **Multi-pass normalization** framing (cf. pipelock's 6-pass, Vault's L0
  decoder) — packages P0-1/2/3 into a coherent pre-scan normalization stage.

### P2-4 Positioning
- Map rules to **OWASP LLM Top 10 / Agentic Top 10**; document glovebox
  explicitly as the *detection + quarantine* layer of a defense-in-depth stack
  (next to action-gating tools like ClawGuard), **not** a complete shield — this
  matches the 2026 consensus (CaMeL, Meta Rule of Two) and is the honest,
  defensible claim.
- Consider a **scan-only / MCP-proxy ingestion mode** to meet users where
  ingestion actually happens in 2026 (Airbyte/n8n/OpenClaw-native connectors,
  MCP tool responses) without competing on connector count — the moat is the
  scan+quarantine+handoff combination, not the connectors.
- Address project visibility: 1 star / 0 forks despite a hot, under-served
  niche. A release with published benchmark numbers and an OWASP-mapped README
  is the cheapest lever.

---

## Suggested sequencing

1. **Week 1 — P0 security holes + efficacy quick wins:** P0-8 (Vault, 0.5d),
   P0-6 (SSRF, 2d), P0-2 (invisibles, 1d), P0-1 (homoglyph, 1.5d). Stand up
   P1-4 corpus scaffolding alongside so each fix lands with regression cases.
2. **Week 2 — remaining efficacy + channels:** P0-3 (decode, 2d), P0-4
   (streaming, 2.5d), P0-5 (metadata + extracted, 2d), corpus gate green in CI.
3. **Week 3 — mTLS + release hygiene:** P0-7 (mTLS, spread ~6–8d), interleaved
   with the fast P1 doc/release fixes (P1-1, P1-2, P1-3 — ~1.5d total) so a
   correct, honestly-described release ships.
4. **Weeks 4+ — depth & strategy:** P1-5 fuzzing, P1-6 rule integrity, P1-7
   enricher isolation, P1-8 lows; then the P2 documentation sweep and the
   strategic adoptions (ML detector, spotlighting, benchmarks, positioning).

**Rationale for ordering:** the P0 efficacy bugs and SSRF are exploitable now
and undermine the product's reason to exist, so they lead. mTLS (the specific
`/v1/ingest` ask) is high-value but larger and depends on cert-manager
plumbing, so it runs in parallel with the low-cost documentation/release fixes
that make the next release trustworthy. Strategy work (ML detector, positioning)
is real but should follow a correct, honestly-documented, released baseline.
