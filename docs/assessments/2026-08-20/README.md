# Project Assessment & Security Review — 2026-08-20

A point-in-time assessment of glovebox at HEAD `a0a6a58`: state vs. stated
goals, a security review of the plan/architecture/roadmap, a competitive/
ecosystem scan, and a consolidated action plan.

## Contents

| Report | What it covers |
|--------|----------------|
| [`state-assessment.md`](state-assessment.md) | Stated goals vs. actual capabilities, per-spec implementation status, documentation-drift gap list. |
| [`state-assessment-raw.md`](state-assessment-raw.md) | Detailed appendix: per-connector inventory table (24 dirs) and per-spec evidence with file citations. |
| [`security-review.md`](security-review.md) | 12 findings (verified against source) ordered by severity, the `/v1/ingest` **mTLS recommendation**, roadmap/process gaps, and verified positive security properties. |
| [`competitive-landscape.md`](competitive-landscape.md) | Whether glovebox has been superseded — 10+ competitors evaluated (LlamaFirewall, pipelock, Vault/vaultmcp, NeMo Guardrails, Invariant/Snyk, ClawGuard family, archived llm-guard/Rebuff, …), connector-framework overlap, verdict, and ideas to adopt. |
| [`action-plan.md`](action-plan.md) | Prioritized, sequenced plan (P0/P1/P2) bridging every gap and finding, with effort estimates. |

## Headlines

- **Capability:** every design spec (04–15) and the three superpowers specs is
  implemented and tested; all 24 connectors are real. Capabilities meet or
  exceed the stated goals — the main debt is **documentation lagging code**
  (README/deployment/release still describe the ~10-connector v0.2 era).
- **Security:** the scanning engine has **efficacy gaps that let injections
  through byte-for-byte today** — homoglyph evasion (NFKC doesn't fold
  Cyrillic), the Unicode Tags block passing unstripped, encoded payloads flagged
  but never decoded (and below threshold), and custom detectors sampling only
  128 KB. Plus an **SSRF** in connector link-fetching and the unauthenticated
  `/v1/ingest` path (mTLS design provided). Auth, tar-extraction, fail-closed
  behaviors, and pod/CI hardening are done well.
- **Landscape:** the niche (deterministic, offline, connector-integrated,
  human-quarantine, byte-identical) is **partially served but not superseded**;
  OpenClaw core declined to build it and the closest analogs were archived. The
  field has moved to "detection is triage, capability control is the boundary" —
  glovebox's quarantine framing fits, but it should stop implying complete
  defense and publish benchmark numbers.

Start with `action-plan.md` for the sequenced work; the other three are its
evidence base.
