# External Ingest Auth -- Design Specification

**Version 1.0 -- May 2026** *(promoted from v0.1 stub)*

*This document specifies the bearer-token authentication layer Glovebox applies to non-trivially-trusted ingest paths. It promotes the v0.1 stub into an implemented design. v1.0 scope: authenticate the new `/v1/archives` endpoint (spec 13). The existing `/v1/ingest` endpoint retains its no-auth behavior on the in-cluster trust boundary (per spec 08 §3.10) and is out of scope for v1; a future spec extends this design to cover it.*

---

## 1. Purpose

The v0.1 stub identified three problems with unauthenticated ingest: no client identity, no transport encryption, no per-caller accountability. Two of those are unresolved by stub-status alone (transport encryption is handled by Traefik termination at the cluster edge per existing convention; this spec does not duplicate that work) and the third -- caller identity -- is now required by spec 13's archive delivery endpoint, which carries multi-GB content with non-trivial value and must record provenance.

This spec promotes the stub to actual design + implementation contract.

The scope reduction from the stub is deliberate. The stub considered uniform vs dual-endpoint auth (§4.3) and deferred. v1 chooses **dual-endpoint**: spec 13's `/v1/archives` requires bearer tokens; spec 08's `/v1/ingest` does not. Justification:

- Spec 13's archive delivery is the actual trigger that forces this work; doing only what spec 13 needs ships the value.
- The 20-ish in-cluster connectors that POST to `/v1/ingest` would need token plumbing in every chart and Secret, which is connector churn for marginal benefit in a single-operator homelab.
- Uniform auth is a one-spec-future migration: this spec's machinery (Vault loader, validation, audit) all generalizes; `/v1/ingest` opt-in is a configuration flag and per-connector chart update.

## 2. Scope

### 2.1 In Scope

- Bearer-token format and provisioning.
- Vault KV v2 storage schema for tokens.
- ESO projection to consumer namespaces.
- Server-side token loading at startup and on SIGHUP.
- HTTP request validation: `Authorization: Bearer <token>` header, constant-time compare, 401 response shape.
- Per-IP rate limit on 401 attempts.
- Provenance integration: `delivered_by` field, spec 06 §5 Identity block, audit log.
- Rotation procedure for v1 (coordinated rotation; no overlap window).

### 2.2 Out of Scope

- Authentication on `/v1/ingest`. Reserved for a future spec.
- mTLS. Heavyweight for single-user homelab; deferred unless a compelling reason arises.
- OIDC / identity-provider integration. Over-engineered for this use case.
- TLS termination at the ingest listener. Cluster ingress (Traefik) handles TLS for external traffic.
- Per-token scope (e.g., "this token may only deliver `archive/mbox`"). All authenticated tokens authorize any allowed media type on `/v1/archives`. Adding per-token scope is a future extension.
- Token expiry / lease semantics. v1 tokens are long-lived; rotation is operator-driven.
- Overlap-window rotation (`token_previous` field). Reserved as the §11 future extension once rotation frequency requires it.

## 3. Token Model

### 3.1 Vault Storage Schema

Tokens live in Vault KV v2 at path `secret/glovebox/ingest-tokens/<source-id>`. Each entry contains:

```json
{
  "token": "<64-char lowercase hex>",
  "notes": "<free-form operator notes>",
  "created_at": "<RFC 3339 UTC>"
}
```

Field details:

- `token` -- 256 bits of CSPRNG output, encoded as lowercase hex (64 characters). MUST be unique across all source-ids. Tokens are opaque; clients pass them verbatim and the server compares verbatim.
- `notes` -- operator-only string for context ("recognizer v1.4 deploy 2026-05-21", "Alice's home laptop", etc.). Never sent to clients. Optional.
- `created_at` -- RFC 3339 UTC timestamp. Recorded by the operator (or by a future token-mint tool) when the entry is created. Used in audit logs to age-bound a compromise window. Optional but recommended.

The server reads only `token` for validation; the other fields are operator metadata.

### 3.2 Source IDs

A source-id is a stable string naming a CLIENT (the entity that holds the token), not a USER. Format: `^[a-z][a-z0-9-]{0,63}$`. Examples:

- `recognizer` -- the in-cluster recognizer service (spec 13's first consumer).
- `workstation-mbox-importer` -- a future mbox importer running on an operator workstation.
- `friend-alice` -- a future external delivery from a household friend.

Source-ids are operator-chosen and must not contain PII; they appear in metric labels, audit logs, and staged-archive metadata.json. Per spec 12 §10's opaque-label convention, names should be functional (`recognizer`) rather than personal (`steves-laptop`).

A single source-id has exactly one valid token at any given time (until the §11 overlap-window extension lands). Rotating a token replaces the value at the same Vault path.

### 3.3 ESO Projection to Consumers

For each in-cluster consumer that needs a token, the operator deploys an `ExternalSecret` (in the consumer's namespace) that pulls from `secret/glovebox/ingest-tokens/<source-id>` and materialises a K8s Secret containing the `token` field. The consumer's Deployment / Job projects the Secret value as an env var (`GLOVEBOX_INGEST_TOKEN` by convention).

Example `ExternalSecret`:

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: glovebox-ingest-token
  namespace: openclaw-recognizer
spec:
  refreshInterval: 1m
  secretStoreRef:
    kind: ClusterSecretStore
    name: vault-default
  target:
    name: glovebox-ingest-token
    creationPolicy: Owner
  data:
    - secretKey: token
      remoteRef:
        key: glovebox/ingest-tokens/recognizer
        property: token
```

The consumer mounts:

```yaml
env:
  - name: GLOVEBOX_INGEST_TOKEN
    valueFrom:
      secretKeyRef:
        name: glovebox-ingest-token
        key: token
```

Out-of-cluster consumers (workstation imports, friend imports) retrieve their token via `vault kv get -field=token secret/glovebox/ingest-tokens/<source-id>` and place it in their local secret store (1Password, plain file mode 0600 in `$HOME/.config/glovebox/`, etc.). Spec is consumer-side and not enforced here.

## 4. Server-Side Validation

### 4.1 Loading at Startup

Glovebox uses Vault's Kubernetes auth method to authenticate (same pattern as spec 12 §5's schoology refresher: pod ServiceAccount JWT → Vault role → Vault token). The Vault role MUST grant `read + list` on `secret/data/glovebox/ingest-tokens/*` and `read + list` on `secret/metadata/glovebox/ingest-tokens/*`.

Loading procedure:

1. Authenticate via `vaultapi.NewKubernetesAuth(role)`; obtain a Vault client.
2. `client.KVv2("secret").List(ctx, "glovebox/ingest-tokens")` to enumerate source-ids.
3. For each source-id, `client.KVv2("secret").Get(ctx, "glovebox/ingest-tokens/<source-id>")`. Extract the `token` field.
4. Validate the source-id name from Vault's `List` response against `^[a-z][a-z0-9-]{0,63}$` (§3.2 format). Malformed source-ids are skipped, logged ERROR (`glovebox ingest source_id malformed`), and the per-source error counter (`glovebox_ingest_token_load_errors_total{source_id="<malformed-value-truncated-and-sanitized>"}`) is incremented. This catches Vault-side typos and prevents a malformed source-id from injecting into log lines or metric labels.
5. Validate token format: 64 lowercase hex characters. Tokens that fail validation are logged at ERROR (`glovebox ingest token malformed`) AND increment `glovebox_ingest_token_load_errors_total{source_id="<source-id>"}`. The malformed entry is skipped; loading continues for the remaining entries. The metric exists so operators can alert on "this source-id has a malformed Vault entry" without needing to scrape logs.
6. Build the in-memory `[]tokenEntry` slice per §4.3's storage shape. NOT a map — see §4.3 for the side-channel rationale.
7. Swap the slice atomically with `sync.RWMutex` so in-flight requests do not race a partial reload. The renumbered steps that follow (was step 5/6 in the prior draft) preserve the same swap-under-write-lock semantics.

On startup, if the Vault load FAILS:

- If at least one previous successful load is cached (subsequent reload failure): WARN log; keep the prior map; continue serving.
- If this is the first load (no cache): ERROR log; **refuse to bind the `/v1/archives` listener** at all. The process continues to serve `/v1/ingest` (which doesn't need this layer) and exposes /healthz, but `/v1/archives` requests get 503 with `glovebox archive listener unavailable: token load failed`. This is a deliberate hardening choice: an unauthenticated archive endpoint is worse than no archive endpoint.

### 4.2 Reload Triggers

Two reload triggers:

- **SIGHUP.** Operator-initiated. Synchronous reload; the SIGHUP handler returns only after the load completes (success or failure). The reload's success is logged.
- **Periodic re-pull.** A goroutine reloads from Vault on a configurable interval (default 5 minutes). Cheap for the expected source-id count (single digits). Failure of a periodic reload is logged WARN but does not affect the in-memory map.

A reload's success replaces the in-memory map atomically. A reload's failure leaves the map unchanged.

There is NO partial-reload semantic. The map is rebuilt entirely; there is no "merge new tokens into the existing map" path. This means a Vault entry that has been deleted disappears from the map on the next reload.

### 4.3 Validation on Every Request

**Storage shape.** The in-memory token store is a slice (NOT a map), structured as `[]tokenEntry` where each `tokenEntry` holds `{token [32]byte; sourceID string}`. A map keyed by token bytes was rejected: Go's map lookup is not constant-time (hash bucketing, internal hashtable resizing, key-equality short-circuit), and the lookup time would leak information about which bucket the matching token landed in. A slice with linear scan and explicit constant-time compare is the right shape.

For each request to a protected endpoint (`/v1/archives*` in v1):

1. Read the `Authorization` header. Missing → 401 (see §5.2).
2. Validate format: exactly `Bearer <token-bytes>` where `<token-bytes>` is 64 lowercase hex characters. Wrong format → 401.
3. Decode the hex into a 32-byte fixed-size array.
4. With `sync.RWMutex.RLock`, iterate the entire `tokenEntry` slice. Accumulate match information using bitwise-OR so the iteration time is independent of which entry (if any) matches:

   ```go
   var (
       matched   uint32
       sourceID  string
   )
   for _, e := range entries {
       eq := uint32(subtle.ConstantTimeCompare(e.token[:], req[:]))
       // Update matched + sourceID without branching on eq's value:
       //   if eq == 1, set matched=1 and copy sourceID
       //   if eq == 0, leave matched + sourceID alone
       // ConstantTimeSelect handles the branchless update for the source-id copy.
       prev := matched
       matched |= eq
       // Branchless copy of e.SourceID into sourceID iff eq==1 AND prev==0
       // (we keep the FIRST match in iteration order to avoid leaking
       // "two entries matched" information; in practice tokens are
       // unique, so this is defensive)
       if eq == 1 && prev == 0 {
           sourceID = e.sourceID
       }
   }
   ```

   The `if eq == 1 && prev == 0` branch on the source-id copy is the one place where the implementation must accept a residual data-dependent branch, since copying a Go `string` header out of a slice element is not constant-time. The mitigation: at most one entry in the slice has a matching token (tokens are required to be globally unique, §3.1), so the branch fires at most once per request. The total work is `O(N)` ConstantTimeCompare invocations PLUS one cheap string-copy on success. No early-exit on first match.

5. After the loop: if `matched == 1`, set request context `delivered_by = sourceID`, mark `accepted`, proceed to handler.
6. If `matched == 0`, return 401, increment the rejection metric (and feed the §5.3 rate limiter).

The iteration cost is `O(N × 32 bytes)` per request. For N ≤ 20 source-ids, this is ~640 byte-comparisons — below the noise floor of any HTTP request handler. The slice is rebuilt on every reload (§4.2); there is no resizing optimization that could leak timing.

**No map / hashmap upgrade path.** If the source-id population ever exceeds ~1,000, the engineering response is to invest in BLINDED-LOOKUP token storage (e.g., bcrypt-hashed tokens with operator-aware lookup keys, or HMAC-keyed indices) — NOT a Go map, which would reintroduce the side channel this storage choice exists to avoid.

### 4.4 Constant-Time Compare Discipline

The implementation MUST:

- Use `crypto/subtle.ConstantTimeCompare` for every byte-level token comparison.
- NOT use `bytes.Equal`, `==`, or any other operator that could short-circuit.
- NOT short-circuit the map iteration on first match.
- NOT log the bytes of a rejected token attempt (NEVER, not even truncated; see §6.3 audit log rules).

The test suite includes a constant-time assertion: for a fixed in-memory token map, validation against a wrong token whose first byte matches a real token must take the same time as validation against a wrong token whose first byte differs. This is a coarse check (Go's `testing.B` is noisy at sub-microsecond resolution) and is documented as best-effort rather than rigorous timing-attack-proof.

## 5. HTTP Contract

### 5.1 Header Format

Requests carry exactly `Authorization: Bearer <token>` where `<token>` is the 64-character lowercase hex string. No other authentication schemes are accepted (Basic, Digest, etc. all return 401).

Token format violations (wrong length, non-hex characters, missing `Bearer ` prefix, multiple `Authorization` headers, etc.) all return 401 with the SAME body and headers as a token mismatch. Format errors and mismatch errors are indistinguishable to the client by design (do not leak the failure mode).

### 5.2 401 Response Shape

```
HTTP/1.1 401 Unauthorized
WWW-Authenticate: Bearer realm="glovebox-ingest"
Content-Type: application/json
Content-Length: <len>

{"error":"unauthorized"}
```

The response body is the same JSON literal for every 401 regardless of cause. No `details` field, no `hint` field, no `error_code` field. A client that supplies a bad token learns nothing beyond "not accepted."

### 5.3 Per-IP Rate Limit on Rejected Attempts

To slow brute-force attempts, the server maintains a per-IP token bucket of rejected attempts:

- Default: 10 rejected attempts per 60-second sliding window per source IP.
- On exceeding: respond 429 Too Many Requests with `Retry-After: <seconds-until-bucket-refills>`.
- Successful authentications DO NOT consume bucket capacity. A correctly-authenticated client is never rate-limited (per spec 13's recognizer client which retries on transient failures).
- The bucket uses `golang.org/x/time/rate.Limiter` per-IP.
- Bucket state is in-memory; lost on restart. Acceptable: a process restart resets a brute-forcer's progress to zero, which is no worse than the pre-rate-limit state.

**Trusted-proxy IP extraction.** The server's "IP" for bucket-keying purposes is derived as follows:

1. Read `r.RemoteAddr` (the immediate TCP peer).
2. If `r.RemoteAddr` falls within the configured **trusted-proxy CIDR list** (default: `<traefik-namespace-pod-cidr>`, operator-configured via `ingest.auth.trustedProxyCIDRs`), THEN parse the right-most `X-Forwarded-For` entry as the real client IP. Otherwise IGNORE forwarded headers entirely and use `r.RemoteAddr`.
3. A client speaking directly to the cluster IP cannot forge a forwarded header because step 2 only honors XFF when the immediate peer is itself a trusted reverse proxy.

**LRU bound.** The per-IP state is capped at **1,000 entries** (NOT 10,000) via LRU eviction. Smaller bound reduces the eviction-bypass surface where a flood of synthetic IPs could push a real attacker's bucket out of the LRU and reset their attempt count.

**Global 401 backstop.** Additionally, the server maintains a SINGLE process-wide rejected-attempt counter with a 60-second sliding window, default cap 100 rejections per window. When the global counter trips, ALL `/v1/archives*` requests return 429 + `Retry-After` for the remainder of the window, regardless of source IP. This is the defense against the LRU-eviction bypass: even if an attacker rotates through 10,001 synthetic IPs to flush the LRU, the global counter detects the rejection burst. Successful authentications do not consume the global counter.

**Rate-limit scope.** The rate limiter is wrapped around handlers for `/v1/archives*` ONLY. Health endpoints (`/healthz`, `/readyz`, `/metrics`) bypass the limiter — a misconfigured prober hitting `/healthz` with a wrong path doesn't burn rate-limit budget. The `/v1/ingest` endpoint (no auth in v1, §7) is also outside the limiter's scope.

### 5.4 Other Response Codes Affected by Auth

- 401 → unauthorized (this spec).
- 429 → rate-limited (this spec).
- 403 → NOT used by this spec; v1 has no scope checks. Future per-token scope adds 403.

## 6. Provenance Integration

### 6.1 `delivered_by` Field

After successful validation, the request context carries `delivered_by = <source-id>`. This value flows into:

- The staged archive's `metadata.json` (spec 13 §4.8): `"delivered_by": "<source-id>"`.
- The staged archive's per-item Identity block (spec 06 §5.2): see §6.2.
- The audit log entry for the request (§6.3).
- Every metric label that includes `source_id` (spec 13 §7.1).

### 6.2 Identity Block (Spec 06 §5.2)

For each archive delivered, the staged Identity block is populated as:

```json
{
  "provider": "ingest",
  "auth_method": "bearer_token",
  "account_id": "<source-id>"
}
```

This satisfies spec 06's identity contract: `provider` identifies the ingestion path, `auth_method` identifies the trust mechanism, `account_id` identifies the specific caller. Downstream consumers (scanner, routing rules) treat `account_id` as the canonical provenance.

### 6.3 Audit Log

Every authentication event (accept OR reject) emits a structured log line. Field rules:

- **Acceptance.** `glovebox ingest authenticated` at INFO level. Fields: `source_id`, `remote_addr` (the parsed real IP), `endpoint` (e.g., `POST /v1/archives`), `archive_id` (if the request path identifies one). NO token bytes.
- **Rejection.** `glovebox ingest auth rejected` at WARN level. Fields: `remote_addr`, `endpoint`, `reason_bucket` ∈ {`bad_request`, `unknown_token`}. `bad_request` covers ALL pre-decode failures (missing `Authorization` header, wrong scheme, malformed token format — these are operator-actionable as a single category: "the client is misconfigured, not actively attacking"). `unknown_token` is reserved for the "format was valid but no entry matched" case (potentially a leaked-and-revoked token, OR an active probe). The two-bucket split is deliberate: it gives operators ONE actionable signal (`unknown_token` rate spike → real concern) without leaking the precise reason a client failed (`missing_header` vs `wrong_scheme` would tell an attacker which check to circumvent first). **NEVER log the token attempt**, not even hashed or truncated.
- **Rate-limit trigger.** `glovebox ingest auth rate limited` at WARN level. Fields: `remote_addr`, `attempts_in_window` (count), `window_seconds`.

The accept log line's `archive_id` is a useful pivot for forensic searches ("show me everything `recognizer` delivered in the last 24 hours"). Absent for endpoints that don't carry an archive_id (e.g., `OPTIONS`).

The audit log is the existing connector framework's audit log (per spec 06 §5.4), not a separate stream. Operators query it via the cluster's normal log aggregation.

### 6.4 Metric Surface

Per spec 13 §7.1:

- `glovebox_ingest_auth_total{endpoint, status}` -- counter; `status` ∈ {`accepted`, `rejected`, `rate_limited`}.
- `glovebox_ingest_auth_rejected_total{remote_ip_bucket}` -- counter; `remote_ip_bucket` is a low-cardinality bucketing of source IPs (e.g., /24 subnets) to enable "which subnet is probing" dashboarding without per-IP cardinality explosion.

Counts are emitted via the framework's OTel-on-Prometheus exporter.

## 7. Endpoint Scope (v1)

**Bearer-token auth applies to**: `/v1/archives*` (spec 13).

**Bearer-token auth does NOT apply to**: `/v1/ingest` (spec 08), `/healthz`, `/readyz`, `/metrics`.

The endpoint scope is hard-coded in v1: the auth middleware wraps the `/v1/archives` handler only. There is no per-endpoint configuration. A future spec extends this with a config-driven scope map.

## 8. Rotation

### 8.1 Operator Procedure (v1)

1. Generate a new token: `openssl rand -hex 32`.
2. Write to Vault: `vault kv put secret/glovebox/ingest-tokens/<source-id> token="<new-token>" notes="rotated 2026-05-21 due to X" created_at="$(date -u +%FT%TZ)"`.
3. Cause glovebox to reload: `kubectl exec -n openclaw deploy/glovebox -- kill -HUP 1` OR `kubectl rollout restart -n openclaw deploy/glovebox`.
4. ESO refreshes the consumer's K8s Secret within `refreshInterval` (default 1 minute).
5. Consumer pod re-rolls (if its Deployment has the standard checksum annotation on the Secret) OR the consumer reads the new token on its next request (if it loads the token per-request).

During the window between step 3 (glovebox reloads) and step 5 (consumer picks up the new token), the consumer's requests with the OLD token are rejected (401). This is a known v1 limitation; the operator's coordinated procedure minimizes the window to ~60 seconds.

### 8.2 Compromise Procedure (Emergency)

1. Generate a new token immediately.
2. Write to Vault.
3. Force glovebox reload IMMEDIATELY (don't wait for the periodic re-pull).
4. The compromised token is dead for NEW requests the moment glovebox reloads.
5. Update consumer Secret out-of-band (the gap is acceptable for emergency).

**Caveat: in-flight uploads are NOT terminated by reload.** A multi-GB tus.io upload PATCH-stream that was in progress when the reload completed continues to drain to disk under the old token's validated request context (the validation happened at PATCH start; subsequent chunks within the same PATCH don't re-validate). The compromised token is dead for new POST/HEAD/PATCH/DELETE requests but cannot retroactively kill an upload already mid-PATCH. For full-stop emergency termination, the operator must additionally:

- Identify any in-flight uploads under the compromised source-id via `glovebox_archive_upload_in_flight{source_id}` metric.
- `kubectl rollout restart` the glovebox deployment to terminate the in-flight TCP connections.
- The `.tmp-archives/<upload-id>` files from those terminated uploads are orphaned and cleaned per spec 13 §5.5.

This caveat is unique to bytes-already-in-flight cases. The reload-only path is correct for token rotation; the rollout-restart path is the additional step for active-compromise containment.

Note that the overlap-window extension (§11) would NOT help in a compromise -- you want zero overlap then. The lack of an overlap-window in v1 is actually beneficial for the emergency case.

### 8.3 Revocation Without Rotation

To revoke a source-id entirely (e.g., remove `friend-alice`'s access permanently):

1. Delete the Vault entry: `vault kv metadata delete secret/glovebox/ingest-tokens/friend-alice`.
2. Reload glovebox.
3. The source-id no longer exists in the in-memory map; all requests with that token return 401.

The consumer's local copy of the token still exists but is now useless.

## 9. Configuration

New Helm values block (under the existing `ingest:` key):

```yaml
ingest:
  auth:
    enabled: true                       # gates spec 13's /v1/archives auth wiring
    vault:
      addr: "http://vault.vault.svc.cluster.local:8200"
      k8sRole: "glovebox-ingest"
      tokensPath: "glovebox/ingest-tokens"
      kvMount: "secret"
    reloadIntervalSeconds: 300          # periodic re-pull
    perIPRateLimit:
      window: 60s
      maxRejected: 10
      lruCapacity: 10000
```

The Vault K8s auth role `glovebox-ingest` requires a policy granting:

```hcl
path "secret/data/glovebox/ingest-tokens/*" { capabilities = ["read"] }
path "secret/metadata/glovebox/ingest-tokens/*" { capabilities = ["read", "list"] }
```

The Helm chart includes the relevant `ClusterSecretStore` reference and `ExternalSecret` examples; the Vault role + policy itself is operator-provisioned out-of-band.

## 10. Failure Modes

| Symptom | Cause | Server response | Recovery |
|---|---|---|---|
| 401 on every request from a known consumer | Vault entry deleted; in-memory map dropped the source-id on last reload | 401 | Re-create Vault entry with the same source-id and (likely) the same token; reload glovebox |
| 401 immediately after rotation | Glovebox reloaded but consumer hasn't picked up new token | 401 | Wait for ESO sync + consumer rollout; coordinated rotation (§8.1) minimizes the window |
| 429 on a known consumer | Burst of mismatches (token rotated mid-burst) tripped the rate limit | 429 + Retry-After | Wait the Retry-After interval; reload glovebox if rotation is the cause |
| `/v1/archives` returns 503 on startup | Vault token load failed at boot (no cached map) | 503 | Fix Vault connectivity (check the role, the secret-store, network policy from glovebox ns to vault ns); glovebox auto-recovers on the next periodic re-pull |
| Constant-time-compare timing-attack signature on Grafana | Hypothetical | -- | The constant-time compare is best-effort; rigorous mitigation deferred until an actual timing attack signature appears |
| Token-bytes leaked in logs | A bug in the audit-log code | -- | Treat as a SECURITY incident: rotate ALL tokens, audit logs, fix the bug. Per §6.3 the discipline is "never log the bytes." The repo MUST carry a `semgrep` rule (configured in `.semgrep/auth-leakage.yml`) banning `r.Header.Get("Authorization")` and `r.Header["Authorization"]` outside the package containing the auth middleware. CI fails if the rule fires. |

## 11. Future Extensions

These are deliberate v1 omissions; documented so the future shape is obvious.

- **Overlap-window rotation (`token_previous` field).** Vault entry grows a `token_previous` field and a `previous_expires_at` field. Server accepts BOTH the current and previous token until `previous_expires_at`. Enables zero-downtime rotation. Schema change is additive; v1 code that reads only `token` continues to work against entries that carry the additional fields.
- **Per-token scope.** Vault entry grows an `allowed_endpoints` list and an `allowed_media_types` list. Server enforces scope checks after validation. Adds a 403 response code. *Migration path*: schema change is additive (entries without the field default to "all endpoints, all media types"); the validator gains a scope check after the constant-time compare; existing tokens continue working until an operator narrows their scope.
- **Auth on `/v1/ingest`.** Extension of this spec applying the same machinery to the connector-scale endpoint. Requires a per-connector chart change to inject `GLOVEBOX_INGEST_TOKEN` and a per-connector Vault entry. *Migration path*: gated by `ingest.auth.coverIngest` flag, default `false` in the v1 release that lands the feature; operator flips to `soft_warn` mode (server logs missing/invalid tokens but accepts the request) for an observation window, then to `enforce` mode once all connectors have been updated.
- **Token expiry.** Vault entry grows an `expires_at` field; server rejects (with 401) any token whose entry has aged past expiry. Pairs with Vault's existing lease semantics for short-lived workflows. *Migration path*: schema change is additive (entries without the field default to no expiry); the validator checks `expires_at` after the constant-time compare; legacy tokens continue working until an operator backfills the field.
- **HMAC-signed token payloads** (JWT-style). Token carries claims (source-id, scope, expiry) signed by a glovebox-held key. Removes the need for a server-side map lookup. Heavier on protocol surface; deferred unless multi-tenant or fine-grained scope arrives.

## 12. Out of Scope

- mTLS. Heavyweight for single-user homelab. May revisit if multi-cluster federation lands.
- OIDC. No user-facing browser flows; all clients are services.
- Browser-based auth flows of any kind.
- IP allow-listing as a primary control. NetworkPolicy at the cluster boundary is the existing layer for that; this spec is the *application* layer.
- Transport encryption. Traefik termination at the cluster edge handles TLS for external traffic per existing convention.

## 13. Related Specs

- **Spec 06 (Connector Auth and Provenance)** -- the Identity block, audit log, and `delivered_by` provenance pattern this spec emits into.
- **Spec 08 (HTTP Ingest API)** -- the unauthenticated connector-scale endpoint this spec deliberately leaves alone in v1.
- **Spec 13 (Archive Delivery API)** -- the consumer of this auth layer; the reason v1 of this spec exists.
- **Spec 12 (Schoology Connector) §5** -- the precedent for Vault K8s auth used here (and §12 for the schoology refresher implementation that exercises the same pattern).
