# Design spec: synchronous sanitize gate (`POST /v1/sanitize`)

- **Status:** draft (brainstorm converged 2026-07-02)
- **Bead:** glovebox-t6fz
- **Repo:** glovebox (this repo)
- **Related:** nagus acquisition-watcher design (`../nagus/docs/design/2026-07-01-nagus-design.md`);
  openclaw acquisition-watcher spec (`../openclaw/docs/superpowers/specs/2026-07-01-acquisition-watcher-design.md`)

## 1. Purpose

nagus (the acquisition/watch subsystem) fetches untrusted marketplace listings
(eBay, Craigslist, ...) and, before any of that free-text reaches extraction,
storage, or an agent, must pass it through the **prompt-injection boundary**.
nagus's design names that boundary a `listing.Sanitizer` and today stubs it with
`sanitize.Passthrough` (copies bytes verbatim, stamps provenance) with a note
that production "routes through the real out-of-process glovebox gate."

glovebox already *is* that gate for its own connectors, but only as an
**asynchronous** pipeline: content is staged to disk and a watcher scans it
later. There is no synchronous "give me text, tell me if it's safe" surface for
an out-of-process caller.

This spec adds that surface: a synchronous **`POST /v1/sanitize`** endpoint on
the existing scanner that runs glovebox's real injection-detection engine and
returns a verdict. It is the concrete contract nagus's `Sanitizer` will call.

## 2. Scope decision (why this shape)

During brainstorming the operator chose **"glovebox = sanitizer boundary only"**
over "glovebox owns the connectors." Consequences, and what this spec is NOT:

- glovebox does **not** build an eBay connector, a Craigslist RSS config, or an
  item-handoff. nagus keeps its own in-process connectors (it already has a
  code-complete, offline-tested eBay Browse connector — live verification is
  gated on eBay EPN approval, not a glovebox concern — and a Craigslist
  connector).
- glovebox exposes the sanitize gate; nagus calls it. The **nagus-side client**
  (`glovebox.Sanitizer` replacing `sanitize.Passthrough`) is a **follow-up
  bead**, implemented against the contract this spec defines — not part of this
  deliverable.
- This keeps the untrusted-listing prompt-injection boundary exactly where the
  security model wants it (out-of-process, behind glovebox) without duplicating
  nagus's connector code.

## 3. Goals and non-goals

**Goals**
- A synchronous request/response gate that runs the *same* detection rules the
  async scanner enforces (no second, drifting rule set).
- A machine-readable, standard, tool-supported contract that nagus (and any
  future caller) generates a typed client from.
- Reuse glovebox's existing auth (bearer token) and detection engine; add only
  the thin synchronous surface.

**Non-goals**
- Rewriting/redacting content. The gate **classifies**; it does not return a
  cleaned body (see Section 6).
- Replacing or changing the async ingest/scan pipeline.
- The nagus client, connectors, extraction, or storage (separate repo / beads).
- Streaming, batch, or multi-part payloads. One JSON body in, one JSON body out.

## 4. The contract is OpenAPI-first, generated

The endpoint is a synchronous JSON-over-HTTP operation, so the right contract
tool is **OpenAPI 3.0.3** (not protobuf/gRPC — glovebox has no gRPC transport;
introducing one for a single JSON endpoint would fragment the service). The
spec is authoritative and the Go plumbing is generated from it, not hand-rolled:

- **`api/openapi.yaml`** (OpenAPI 3.0.3 -- downgraded from 3.1 during
  implementation: our schema uses only the 3.0/3.1 common subset and
  oapi-codegen v2.7.1 fully supports 3.0.x but only partially 3.1) is the
  single source of truth: the
  `SanitizeRequest` / `SanitizeResponse` / `Signal` schemas, the `bearerAuth`
  security scheme, and every response code.
- **`oapi-codegen`** (`std-http-server` target — Go 1.22+ `net/http` `ServeMux`
  method patterns, no web framework) generates the request/response **types** and
  the **server interface** into a committed `internal/sanitizeapi/` package via
  `go generate`. The nagus follow-up generates its **typed client** from the same
  file.
- **CI drift check**: a step re-runs `go generate ./...` and fails on any
  `git diff`, so the spec and the generated Go can never silently diverge.
- We **hand-write only**: the detection wrapper (`internal/scan`) and the one
  handler method that binds them (maps a `ScanResult` to `SanitizeResponse`).

Rationale is recorded in project memory as
`pattern-openapi-first-codegen-for-synchronous-json-over`.

## 5. Architecture

```
nagus  --HTTPS POST /v1/sanitize (Bearer <token>)-->  glovebox scanner process
                                                         |
                                        auth.Middleware (existing, bearer/Vault)
                                                         |
                                 generated server interface (oapi-codegen)
                                                         |
                                   sanitizeHandler.Sanitize (hand-written)
                                                         |
                                internal/scan.Scanner.Scan(content, contentType)
                                     (engine.Preprocess -> engine.ScanContent
                                        -> engine.ScoreSignals; pure, no I/O)
                                                         |
                              ScanResult{Verdict, TotalScore, Signals} --> 200 JSON
```

### 5.1 `internal/scan` (new package; refactor)

glovebox's detection primitives (`engine.Preprocess`, `engine.ScanContent`,
`engine.ScoreSignals`, the `detector` registry) are already synchronous, pure,
and side-effect-free. But the *bootstrap* that compiles rules/boosts/threshold
into matchers+detectors currently lives in the root `package main`
(`buildScanFuncs`, boost/threshold assembly). We lift that into a new
`internal/scan` package:

```go
package scan

// Scanner holds the compiled matchers, detectors, boost rules, and threshold —
// the same inputs the async worker pool uses.
type Scanner struct { /* matchers, detectors, boosts, threshold */ }

// New builds a Scanner from loaded rules + config (the bootstrap lifted out of
// package main), so the daemon and the sanitize endpoint share ONE compiled
// rule set.
func New(rules engine.RuleConfig, cfg ScanConfig) (*Scanner, error)

// Scan runs Preprocess -> ScanContent -> ScoreSignals with no filesystem,
// queue, or goroutine coupling. This is the worker's scan path minus I/O.
// It returns an error when the underlying engine.ScanContent fails (e.g. a
// detector error); the handler maps that to a FAIL-CLOSED 5xx (Section 7) --
// never to a pass. A gate that errored must never look like a clean listing.
func (s *Scanner) Scan(content []byte, contentType string) (engine.ScanResult, error)
```

The async daemon (`main.go`) is refactored to build its scan path through
`scan.New(...).Scan(...)` too, so there is exactly one rule-compilation path.
This is the one intentional refactor of existing code; it is in-scope because
sharing the rule set is the whole point (the gate must enforce what the scanner
enforces).

### 5.2 Route on the existing scanner ingest server

The handler mounts on the existing `ingestMux` alongside `/v1/ingest` and
`/v1/archives`, in the same scanner process and image (chosen over a standalone
binary: it reuses the already-compiled scanner + one deploy). oapi-codegen's
`std-http-server` emits a `HandlerFromMux(si, mux)` that registers the Go 1.22
method-pattern route (`"POST /v1/sanitize"`) onto a supplied `*http.ServeMux` —
we pass the existing `ingestMux`, so the generated route coexists with the
current plain-pattern `/v1/ingest` and `/v1/archives` handlers (fine on Go
1.26). It is wrapped in the **existing** `auth.Middleware` (bearer token,
64-hex, constant-time lookup, Vault/ESO-sourced) — unlike `/v1/ingest` today,
the sanitize route **requires** auth, with a dedicated source-id (e.g. `nagus`).

## 6. Behavior: classify, don't rewrite

glovebox's engine returns a **verdict + score + signals**; it does not rewrite
the passing payload. This maps exactly onto nagus's `Sanitizer` gate:

| glovebox verdict | meaning | nagus action (out of scope, documented) |
|---|---|---|
| `pass` | score below threshold | keep the original bytes, stamp `Boundary` provenance |
| `quarantine` | score >= threshold | drop the listing (do not extract/store) |

The scan path (`engine.ScoreSignals`) only ever emits **`pass` or
`quarantine`** — those are the two verdict values the gate returns. glovebox's
third engine value `reject` is a *routing-layer* concept (invalid destination
etc.) that does not apply to a stateless text gate, so it is **not** part of
this contract. An engine/scan **error** is not a verdict at all: it surfaces as
a **fail-closed 5xx** (Section 7), which the caller treats as "drop," so a gate
failure can never be mistaken for a clean listing.

The gate never returns a "cleaned" body — the caller uses the original bytes on
`pass` and drops otherwise. This matches nagus's existing `Passthrough` byte
semantics, now with a real verdict gating it, and keeps the boundary a
*decision*, not a lossy transform.

## 7. Request / response contract

`POST /v1/sanitize` — `Content-Type: application/json`,
`Authorization: Bearer <64-hex-token>`.

**Request** (`SanitizeRequest`):
```json
{ "content": "<untrusted listing text>", "content_type": "text/plain" }
```
- `content` (string, required): the untrusted text. nagus concatenates the
  listing's title + description (+ aspects) or calls per field — its choice; the
  gate is field-agnostic and scans whatever text it is given.
- `content_type` (string, optional, default `text/plain`): drives preprocessing
  (`text/html` strips tags before matching, per `engine.Preprocess`).

**Response 200** (`SanitizeResponse`):
```json
{
  "verdict": "pass",
  "total_score": 0.0,
  "signals": [ { "name": "prompt_template_structure", "weight": 0.6, "matched": "<...>" } ]
}
```
- `verdict`: `pass` | `quarantine` (the only two values the scan path emits;
  see Section 6).
- `total_score`: weighted signal sum after boosts.
- `signals`: the matched signals (name/weight/matched) for the caller's audit
  log. Any `matched` echo is a substring of the *input* the caller already holds;
  it introduces no new untrusted content.

**Errors** (reusing the ingest conventions; all non-2xx = the caller drops the
listing, i.e. the gate fails closed):
- `400` malformed JSON / missing `content`.
- `401` missing or invalid bearer token (opaque).
- `413` body exceeds the shared max size (reuses the ingest size cap).
- `429` rate-limited (reuses the ingest rate limiter; `Retry-After`).
- `500` engine/scan error (`Scan` returned an error) — fail closed, never a
  verdict.
- `503` scanner not ready.

## 8. Security

- Same trust posture as the rest of glovebox: the endpoint takes untrusted text
  and returns typed fields; it never executes or reflects it as instructions.
- Auth required (bearer/Vault/ESO), per-source-id, constant-time lookup, opaque
  401 — reuses `internal/ingest/auth` verbatim.
- Size cap + rate limit reused from ingest to bound resource use / abuse.
- No content is persisted by the sanitize path (unlike `/v1/ingest`, which
  stages to disk); the gate is stateless request/response. This *reduces* the
  data-at-rest surface versus routing the same text through async ingest.
- The gate shares the scanner's rule set, so a rule tightening protects both
  paths at once.

## 9. Testing

- **`internal/scan` unit tests**: known-benign text -> `pass`; known injection
  fixtures (`<system>`, `---BEGIN INSTRUCTIONS---`, template-structure,
  encoding-anomaly) -> `quarantine`; the `text/html` tag-strip path; threshold
  boundary behavior; parity with the async worker on the same input.
- **Handler contract tests**: auth required (401 without token), happy-path JSON
  shape, `400`/`413`/`429` paths, and — because the contract is OpenAPI-first —
  responses validated against `api/openapi.yaml` (schema-conformance test using
  the generated types / a kin-openapi validator) so the implementation and the
  spec are proven in lockstep.
- **Codegen drift test/CI step**: `go generate ./...` then assert clean
  `git diff` for the generated package.
- Rides the existing scanner image, so `go vet` + `staticcheck` + `go test` +
  the image build in CI cover it with no new pipeline.

## 10. CI

No new pipeline. The endpoint compiles into the existing `glovebox` scanner
binary/image (built by both GitLab `npsj` and GitHub `ci.yml`), and its tests
run under the existing `test` job (`go test ./... -race`). The only addition is
the codegen-drift check described in Section 9, added to the test job.

## 11. Out of scope / follow-ups

- **nagus `glovebox.Sanitizer` client** (replaces `sanitize.Passthrough`,
  generated from `api/openapi.yaml`): follow-up bead in nagus.
- **Provisioning**: a Vault-stored bearer token for source-id `nagus` +
  ExternalSecret wiring (operator/gitops), mirroring the ingest-token pattern.
- Folding the existing `/v1/ingest` and `/v1/archives` surfaces into the same
  `api/openapi.yaml`: tracked separately (glovebox-x3li OpenAPI-retrofit
  investigation).

## 12. Open decisions

1. **`internal/scan` config surface** — does `scan.New` take the already-loaded
   `engine.RuleConfig` + a small `ScanConfig`, or the raw config file? (Lean:
   loaded structs, so `main.go` owns file I/O and `internal/scan` stays pure.)
2. **Size cap / rate-limit values** — reuse the ingest values as-is, or a
   dedicated (smaller) cap for sanitize? (Lean: reuse ingest for v1.)
3. **Whether the `matched` field is returned at all** — it is a substring of the
   caller's own input (no new untrusted content), but if we want to be maximally
   conservative we can omit it and return only `name`/`weight`. (Lean: return
   it; the caller already holds the text.)
