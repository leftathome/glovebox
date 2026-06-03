# Health-Data Provenance and Subject Resolution -- Design Specification

**Version 0.1 (DRAFT) -- June 2026**

*This document specifies the Glovebox-side foundation for ingesting health data
obtained from credentialed external sources (initially Kaiser Permanente
Washington, via the `walhelm-go` library used by the recognizer service). It
extends the archive-delivery contract (spec 13) so a producer can assert, per
delivery, both the **acquisition identity** (what credential fetched the data
from the source) and the **subject principal** (whose data this is), and it
extends the data-subject / audience model (spec 11) with a Glovebox-held
**known-subjects registry**, a **subject resolver**, and a **fail-closed
quarantine gate** for items whose subject cannot be resolved. It is the first
of three sub-projects (SP1) in the larger "health connector via recognizer"
program; SP2 (`walhelm-go` record-download + proxy) and SP3 (recognizer
walhelm-fetch capability) build on the contract pinned down here. SP1 touches
only this repository and is testable without any contact with a real health
portal.*

---

## 1. Purpose

Glovebox exists to scan content from untrusted sources for prompt injection and
to route it to the right destination with the right visibility. Health data
raises the stakes on the *routing* half: mis-routing one person's medical record
to the wrong audience is the single worst outcome the system can produce, and it
is far worse than dropping the item.

The existing connectors and the existing archive path both assume the producer
either *is* the data subject or that a **rule matcher** can infer the subject
from item content (sender, label, list-id -- see `importers/mbox/ingest.go`).
Neither assumption is safe for health data:

- The recognizer fetches from KP using **one credential** (the signed-in member)
  that may be authorized for **several patients** (the member plus proxied
  dependents). "What credential fetched this" and "whose record is this" are
  *different facts* that diverge under the proxy model.
- Inferring the subject from scanned bytes is an integrity hole: the scanned
  bytes are precisely the untrusted thing Glovebox distrusts. A crafted message
  body that trips a "this is Dad's data" rule could mis-route a child's labs.

The governing principle this spec encodes:

> **The data-subject is provenance, not content.** It must be asserted by the
> fetcher on a trusted channel, travel alongside the content, and be preserved
> verbatim through routing -- never re-derived from the scanned bytes.

Spec 11 anticipated this work explicitly. Its §2.2 defers medical role tokens
and sensitivity escalators "until the walhelm (medical/health) connector lands
with concrete use cases"; its §10.2 defers cross-connector identity
normalization to "a later identity-reconciliation layer"; its §10.4 defers a
"hard gate that refuses to release items" to a later spec. SP1 is that later
spec for the resolution gate and the normalization bridge. (The medical
*vocabulary* additions remain deferred -- see §9.)

## 2. Scope

### 2.1 In Scope

- **Three-part provenance** on the archive-delivery path (spec 13 amendment):
  - *Delivery identity* -- who handed Glovebox the archive. Already present
    (`delivered_by`, spec 13 §3.3). Unchanged.
  - *Acquisition identity* -- what source credential fetched the data. New,
    producer-asserted.
  - *Subject principal* -- whose data this is, as a connector-scoped identifier.
    New, producer-asserted.
- A new **`Upload-Metadata`** field set carrying the acquisition identity and
  the per-archive subject principal and (optional) asserted audience.
- A new **media type** `archive/walhelm-export` (tar-shaped) and a
  **walhelm importer** that stamps every staged item with the producer-asserted
  provenance instead of running the rule matcher to guess it.
- A Glovebox **known-subjects registry**: an operator-maintained allowlist that
  maps subject principals (e.g. `walhelm:steve`) to canonical `data_subject`
  identifiers and optional default audience.
- A **subject resolver** that runs at routing time: items carrying a subject
  principal are resolved against the registry; the canonical `data_subject` is
  stamped on the item.
- A **fail-closed quarantine gate**: an item whose subject principal does not
  resolve is quarantined, audited, and surfaced for human review -- never routed
  to a destination agent.
- Backward-compatibility guarantees for every existing connector and the
  existing mbox/recognizer archive flow.

### 2.2 Out of Scope (Deferred)

- **`walhelm-go` work** (SP2): record-download/zip reverse-engineering, proxy /
  multi-patient enumeration. SP1 only defines the contract those will feed.
- **Recognizer implementation** (SP3): the walhelm-fetch loop, the
  principal-mapping table the recognizer owns, archive packaging. SP1 defines
  the wire contract SP3 targets.
- **Medical-care audience vocabulary** (`medical_providers`, `spouse`,
  sensitivity escalators). Remains deferred per spec 11 §2.2; §9 shows the
  existing tokens cover walhelm v0.1.
- **Deep extraction of record-download zips** (CCDA XML / PDF unpacking). A
  record-download blob is staged as a single opaque item for scanning in SP1;
  content enrichment is spec 14 territory and a later follow-on.
- **Multi-subject within a single archive** (a per-item subject manifest). SP1
  uses one subject principal per archive (§4.3); the manifest form is a
  documented future extension (§10.1).
- **Relationship / authorization roster** (who-may-see-whose-data). The registry
  is a flat known-subjects allowlist, *not* a family roster; fine-grained
  authorization stays downstream per spec 11 §3.4 (§5.4).
- **Audience-aware routing lanes** (spec 11 §10.3). The gate here keys on
  subject *resolvability*, not on audience values.

## 3. The Three Provenance Facts

| Fact | Question it answers | Who asserts it | Where it lives today | SP1 change |
|---|---|---|---|---|
| **Delivery identity** | Who handed Glovebox this archive? | Glovebox ingest auth (spec 10) | `delivered_by` + `identity` block in `metadata.json` (spec 13 §3.3) | none |
| **Acquisition identity** | What source credential fetched the data? | The recognizer (it ran the walhelm login) | nowhere | new `Upload-Metadata` keys (§4.2) |
| **Subject principal** | Whose data is this? | The recognizer (it fetched under a patient context) | nowhere on the archive path | new `Upload-Metadata` key (§4.2), resolved to canonical `data_subject` (§5) |

In walhelm v0.1 (single signed-in member) the acquisition identity and the
subject principal collapse onto one person. Under the proxy model (SP2 v0.2)
they diverge: one acquisition identity (the member's KP login) legitimately
fetches data for several subject principals (the member + each dependent). SP1
models them as separate fields *now* so proxy support is additive, not a
re-architecture.

## 4. Archive-Delivery Contract Extension (amends spec 13)

### 4.1 New media type

| `media_type` | Shape | Importer |
|---|---|---|
| `archive/walhelm-export` | tar (`MediaTar`, like `archive/google-takeout-subtree`) | new walhelm importer (§6) |

The recognizer packages a pull as an uncompressed tar whose entries are
per-item files (one file per message thread, lab panel, or record), plus any
record-download blobs as-is. Glovebox stream-untars into
`archives/<archive_id>/tree/` exactly as it does for Takeout subtrees; no
spec-13 finalize change is needed for the tar handling itself.

> **"Existing media types" means the code's allow-list, not spec 13 §4.5.**
> Throughout this spec, "the four existing media types" refers to the current
> `mediaAllowList` in `internal/ingest/archives/metadata.go` (`archive/mbox`,
> `archive/google-takeout-subtree`, `archive/generic-tarball`,
> `archive/imap-export`). Spec 13 §4.5's prose lists only two and is stale; the
> code is authoritative. `archive/walhelm-export` is the fifth entry.

### 4.2 New `Upload-Metadata` keys

Spec 13 §4 carries `archive_id`, `archive_filename`, `media_type`,
`matcher_id`, `provider`, `sha256`, `subtree_relative_path`. SP1 adds:

| Key | Required | Format | Meaning |
|---|---|---|---|
| `acq_provider` | yes for walhelm media | `^[a-z][a-z0-9-]{0,63}$` | Source system the credential authenticated to, e.g. `kp-wa`. |
| `acq_account_id` | yes for walhelm media | ≤256 chars, no control chars | The source account/login used to fetch (the member's KP user id). |
| `acq_auth_method` | yes for walhelm media | enum: `browser_session` (v1) | How the credential authenticated. Mirrors `Identity.auth_method`. |
| `data_subject` | yes for walhelm media | ≤256 chars, no control chars | The **subject principal** -- a connector-scoped identifier such as `walhelm:steve` or `walhelm:<kp-subject-id>`. Opaque to Glovebox until resolved (§5). |
| `audience` | no | comma-separated enum tokens (spec 11 §3.4) | Producer-asserted audience applied to every item in the archive. If omitted, the registry default (§5.1) applies; if the registry has none, spec 11 §3.6's reader default applies. |

`acq_*` describe the **acquisition identity**; they are distinct from the
server-set `delivered_by` (the recognizer's ingest token / delivery identity).
Unlike `delivered_by` / `delivered_at`, the `acq_*` and `data_subject` keys are
**client-set and required** for walhelm media -- the recognizer is the only
component that knows them. `delivered_by` / `delivered_at` remain reserved and
still 400 if a client sends them (spec 13 §3.3 unchanged).

> **Implementation note -- this is a parser restructuring, not a pure field
> addition.** `ParseUploadMetadata` (`internal/ingest/archives/metadata.go`)
> today validates one fixed required-key set uniformly for every media type. SP1
> requires the new keys to be **required for `archive/walhelm-export` and
> ignored/optional for the other media types**, so the parser must gain
> **per-media-type required-key sets**. "Additive only" in §7 refers to the
> *wire schema and existing media types* being unchanged; the validator code
> itself genuinely changes shape. Plans should size this as a conditional-
> validation refactor, not a struct-field append.

> **Subject value disambiguation (cf. spec 11 §3.1).** The token `data_subject`
> now names a value that changes meaning across the resolution boundary:
> - **Wire / `Upload-Metadata` `data_subject`** = the *subject principal* (a
>   connector-scoped, pre-resolution identifier, e.g. `walhelm:steve`).
> - **Post-resolution item `data_subject`** (the spec 11 metadata field stamped
>   on the staged item after §5.2) = the *canonical* identifier (e.g. `steve`).
>
> Implementers MUST NOT stamp the raw principal where the canonical is expected;
> resolution (§5.2) is the only step that converts one to the other.

### 4.3 One subject principal per archive

SP1 binds exactly one `data_subject` principal to an archive. A pull that spans
multiple subjects (proxy/multi-patient) is delivered as **multiple archives**,
one per subject -- which mirrors spec 13's existing "one archive per subtree"
decomposition and matches how `walhelm-go` will fetch (one patient context at a
time). The per-item subject manifest, for genuinely mixed-subject archives, is a
documented future extension (§10.1) and is not built in SP1.

### 4.4 metadata.json after finalize

The finalize receipt (`internal/ingest/archives/finalize.go`) gains an
`acquisition` block alongside the existing `identity` (delivery) block, and the
producer-asserted `data_subject` / `audience` are recorded:

```json
{
  "archive_id": "...",
  "media_type": "archive/walhelm-export",
  "delivered_by": "recognizer-v1",
  "identity":    { "provider": "ingest",  "auth_method": "bearer_token", "account_id": "recognizer-v1" },
  "acquisition": { "provider": "kp-wa",   "auth_method": "browser_session", "account_id": "leftathome" },
  "data_subject": "walhelm:steve",
  "audience": ["subject"],
  "sha256_verified": true,
  "staged_path": "archives/<id>/tree/"
}
```

`acquisition` reuses the existing `Identity` Go type (spec 06) -- it is an
identity block, just describing the *source* credential rather than the
*delivery* credential.

## 5. Subject Resolution (amends spec 11)

### 5.1 Known-subjects registry

A new operator-maintained config object, loaded by `internal/config` from the
Glovebox values/ConfigMap (not from any connector config). It is a flat
allowlist -- **not** a relationship roster (§5.4):

```yaml
subjects:
  - canonical: "steve"
    principals: ["walhelm:steve", "walhelm:leftathome"]   # connector-scoped aliases
    default_audience: ["subject"]
  - canonical: "bee"
    principals: ["walhelm:bee-kp-id", "schoology:bee"]     # cross-connector normalization
    default_audience: ["subject", "guardians"]
```

| Field | Meaning |
|---|---|
| `canonical` | The canonical `data_subject` identifier stamped on resolved items. The value space spec 11 §10.2 deferred to "a later normalization layer" -- this is that layer. |
| `principals` | Connector-scoped principals that resolve to this subject. The bridge between the recognizer's namespaced ids and Glovebox's canonical ids. |
| `default_audience` | Audience applied when an item neither the rule nor the producer set one. Validated at config-load per spec 11 §6. |

A principal MUST map to at most one canonical subject (config-load rejects
duplicates). The registry is the *only* place that knows the principal↔canonical
correspondence; the recognizer holds the source-id↔principal map (SP3), and the
two are deliberately separate so neither component holds the whole chain.

### 5.2 The resolver

Resolution runs at routing time (`internal/routing`), after an item is staged
and scanned, before it is released to a destination agent. For each item:

1. If the item carries **no** `data_subject` -- legacy connectors, subjectless
   flyers -- it is **unaffected**. Resolution does not apply; spec 11 §3.6's
   household default stands. (This is what preserves backward compatibility --
   §7.)
2. If the item carries a `data_subject` that is a **registry principal**, stamp
   the canonical subject in its place and, if the item declared no audience,
   apply the registry `default_audience`. Route normally.
3. If the item carries a `data_subject` that is **already a canonical** registry
   value (e.g. a connector that emits canonical ids directly, like Schoology
   after normalization), accept it as-is. Route normally.
4. Otherwise the subject is **unresolved** -- quarantine (§5.3).

### 5.3 Fail-closed quarantine gate

An unresolved subject is never a routing decision toward a destination. The item
is:

- routed to **quarantine** (the existing quarantine lane; no destination agent
  receives it),
- recorded in the **audit log** with the unresolved principal and the reason
  (`subject_unresolved`),
- surfaced for **human review**: the operator either adds the principal to the
  registry (and the item can be re-driven) or discards it.

Rationale: "I don't know whose this is" must degrade to "release to no one until
a human says," never to the household default. This is the enforcement gate spec
11 §10.4 deferred, scoped narrowly to subject *existence*.

### 5.4 What the registry is NOT

Per spec 11 §3.4 ("Glovebox does not hold a family roster"), the registry holds
*known subjects*, not *relationships*. It answers "is this a subject I
recognize?" It does **not** encode "is Steve one of Child 1's guardians" or "is this
tutor authorized for this content" -- those resolve at the consuming boundary
(downstream agents, using audience tokens + their own role registries). SP1 does
not move that line; it adds only the minimal subject allowlist the quarantine
gate needs.

## 6. The walhelm importer

A new importer under `importers/walhelm/` (sibling to `importers/mbox/`),
reusing the spec-09 one-shot importer scaffolding (`importer.RunOneShot`). It
diverges from the mbox importer in exactly one way that matters here: **it does
not call the rule matcher to derive the subject.** It reads the producer-asserted
provenance from the finalized `metadata.json` and stamps every item with it.

Per staged item, `BuildItemOptions` sets:

- `DataSubject` = the archive's asserted subject principal (resolved later, §5.2).
- `Audience` = the archive's asserted audience (or unset, deferring to registry
  default / spec 11 §3.6).
- `Identity` = the **acquisition** identity (`acq_*`), so the per-item audit
  trail records what credential fetched it.
- `ContentType` per entry: `message/rfc822` or a structured `application/json`
  for messages/labs/records; `application/zip` for a record-download blob
  (staged opaque; deep extraction deferred, §2.2).
- `DestinationAgent` from the configured matcher (the matcher still chooses the
  *destination*; it just no longer owns the *subject*).

The rule matcher remains the destination authority; the producer is the subject
authority. This split is the whole point.

## 7. Backward Compatibility

- **Additive only.** New `Upload-Metadata` keys are required *only* for
  `archive/walhelm-export`; the four existing media types are unchanged. New
  receipt fields use `omitempty`.
- **Existing mbox / recognizer archive flow unchanged.** The mbox importer still
  derives item options from the rule matcher; it emits no `data_subject`, so the
  resolver's step 1 (§5.2) leaves its items untouched.
- **Every existing connector unchanged.** RSS/IMAP/Schoology/etc. that emit no
  `data_subject` are unaffected by the gate. A connector that *does* emit a
  `data_subject` (Schoology) only becomes subject to the gate once its value is
  registered as a canonical or principal in the registry -- so rollout is
  opt-in per subject. Until then, to avoid surprise quarantines, the gate is
  enabled by a config flag (`subjects.enforce: true`); with the registry empty
  and enforcement off, behavior is byte-identical to today.
- **Spec 11 reader default** (`["household"]` when audience omitted) is unchanged
  for subjectless items.

## 8. Testing Strategy

SP1 is fully testable in-repo with **synthetic archives** -- no KP contact:

- **Metadata parsing**: `acq_*` / `data_subject` / `audience` accepted for
  `archive/walhelm-export`; rejected (400) when missing for that media type;
  `delivered_by`/`delivered_at` still rejected; `acq_*` ignored/optional for
  other media types.
- **Importer stamping**: a fixture walhelm tar produces staged items whose
  `data_subject`, `audience`, and acquisition `Identity` match the manifest, and
  whose `DestinationAgent` still comes from the matcher.
- **Resolver**: principal → canonical mapping; canonical pass-through;
  audience-default application; multi-principal aliasing; config-load rejection
  of duplicate principal mappings and malformed `default_audience`.
- **Quarantine gate**: unresolved principal → quarantine + audit entry
  (`subject_unresolved`) + no destination delivery; enforcement-off bypass;
  subjectless item bypass (backward-compat); and the canonical-collision edge --
  an unregistered string that merely *looks* canonical (e.g. `"steve"` against an
  empty or non-matching registry) quarantines under enforcement rather than
  passing through §5.2 step 3.
- **End-to-end**: synthetic `archive/walhelm-export` through finalize → importer
  → scan → resolver → route, asserting a resolved item lands at its destination
  with canonical subject and an unresolved one lands in quarantine.

All new Go passes `go vet` + `staticcheck`; existing tests stay green.

## 9. Audience Vocabulary: why no new tokens in SP1

Spec 11 §2.2 says the adult-patient case "genuinely needs new tokens." On
inspection, walhelm v0.1 does not yet:

- **Member's own data** (Steve's labs/messages/records) → `audience: ["subject"]`.
  Existing token, exact fit.
- **Pediatric data** under the proxy model (e.g. Child 1) → `["subject", "guardians"]`
  or `["guardians"]`. Existing tokens; spec 11 §2.2 already cites this case as
  covered.

The genuinely new needs -- `medical_providers`, `spouse`, and sensitivity
escalators for highly-sensitive categories (mental health, reproductive) -- have
no concrete content to validate against until SP2/SP3 land real data. Per spec
11's own discipline (add tokens when there is real content), SP1 introduces
**no** audience tokens and leaves §2.2's deferral in place. This is a deliberate
YAGNI call, revisited when SP3 surfaces real items.

## 10. Design Decisions and Deferred Work

### 10.1 Per-archive subject vs per-item manifest

SP1 binds one subject per archive (§4.3). A per-item manifest (a sidecar mapping
each tar entry → principal/audience) would allow genuinely mixed-subject
archives, but it adds contract surface for a case walhelm v0.1 never produces
(single member) and v0.2 handles by per-patient archives. Deferred until a
producer needs it.

### 10.2 Acquisition identity as full chain-of-custody

SP1 records the immediate acquisition credential (the KP member login). It does
**not** model delegation chains (e.g. "this agent acted on behalf of the member
who proxied for the dependent"). If a future producer needs a custody chain
rather than a single acquisition identity, the `acquisition` block can become an
ordered list without breaking the single-entry readers.

### 10.3 Where resolution runs

SP1 places the resolver at routing time so it applies uniformly to *any* item
carrying a `data_subject` (archive-delivered or connector-emitted), not just
walhelm. An alternative -- resolving inside the importer -- would scope it to
archives only and duplicate logic per importer. Routing-time resolution is the
single choke point and the natural home for the §10.4-deferred gate.

### 10.4 Enforcement default

The gate ships **off** (`subjects.enforce: false`) with an empty registry, so
landing SP1 changes no behavior until the operator populates the registry and
flips enforcement on. This lets SP1 merge ahead of SP3 without risk.

## 11. Resolved Decisions

These were open during review and were settled by the operator (2026-06-01):

1. **Registry location** -- **ConfigMap.** The known-subjects registry is a
   `subjects:` block in the Glovebox Helm values, rendered to a ConfigMap and
   loaded by `internal/config`. It is non-secret operator config and follows the
   same delivery path as rules/matchers. (Vault-synced object / CRD rejected as
   unnecessary for non-secret config.)
2. **Principal namespace convention** -- **stable opaque ids.** Principals take
   the form `walhelm:<kp-subject-id>`; the human-legible name lives only in the
   registry's `canonical` field, so display-name changes never churn the wire
   contract.
3. **Re-drive mechanism after registry update** -- **manual in SP1.** When the
   operator adds a previously-unresolved principal to the registry, they re-drive
   the item by re-pointing the importer at the staged archive. Automatic
   re-resolution of the quarantine lane is a documented follow-on, not SP1 scope.

## 12. Acceptance Criteria

An implementation of SP1 must:

1. Accept and validate the new `Upload-Metadata` keys (`acq_provider`,
   `acq_account_id`, `acq_auth_method`, `data_subject`, `audience`) for
   `archive/walhelm-export`, rejecting deliveries of that media type that omit
   the required keys, and leaving the four existing media types unchanged.
2. Record an `acquisition` identity block plus producer-asserted
   `data_subject` / `audience` in the finalized `metadata.json`.
3. Provide a walhelm importer that stamps items from producer-asserted
   provenance (subject + audience + acquisition identity) while still taking the
   destination from the matcher.
4. Load and validate a known-subjects registry (canonical, principals,
   default_audience) at config time, rejecting duplicate principal mappings and
   malformed audiences.
5. Resolve subject principals → canonical at routing time, apply registry
   default audience, and pass through already-canonical subjects.
6. Quarantine + audit (`subject_unresolved`) any item whose declared subject does
   not resolve, with no destination delivery, when enforcement is on.
7. Leave all existing connectors and the existing archive flow byte-identical in
   behavior when the registry is empty and enforcement is off.
8. Produce clean `go vet` / `staticcheck` and add the tests in §8.
