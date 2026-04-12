# External Ingest Auth and TLS -- Design Specification (Stub)

**Version 0.1 -- April 2026**

*This is a placeholder spec capturing the external-ingest-auth follow-up project. The glovebox `/v1/ingest` endpoint today has no application-layer authentication and serves plain HTTP. This is appropriate for the in-cluster trust boundary enforced by NetworkPolicy but prevents external clients (e.g., a mbox importer running on a user's workstation, a friend importing their own archive remotely) from safely pushing content. This stub identifies the problem, sketches the scope of the change, enumerates the open decisions, and names the trigger conditions under which the real design work should happen. No implementation is attempted in V1.*

---

## 1. Why This Exists as a Stub

Spec 09 (mbox importer) deliberately confines V1 to in-cluster execution (K8s Job reading from a PVC) so that external-access concerns don't bottleneck the importer delivery. But the need for external access is real and will recur:

- Running importers on a user's local workstation without scp-into-NAS first.
- A future "archive ingestion gateway" for friends or family members to push their own Takeouts to their personal glovebox instance.
- Any future non-cluster-native client that needs to land content through ingest.

Capturing this as a spec stub (rather than leaving it as a scattered TODO in the mbox importer spec) keeps the eventual design work visible and prevents it from being rediscovered as an emergency later.

## 2. Current State

The ingest endpoint is in `internal/ingest/`:

- `internal/ingest/server.go`: `StartServer(handler *Handler, port int, timeout time.Duration)` wraps the handler in an `http.Server` on a plain TCP listener. No TLS.
- `internal/ingest/handler.go`: `ServeHTTP` does readiness/method/backpressure checks, parses multipart, validates metadata against a source allowlist, writes to staging. No `Authorization` header inspection, no bearer token, no mTLS client cert inspection.
- `connector/http_backend.go`: the `HTTPStagingBackend` connectors use to push to ingest does not set any auth header.
- The cluster-level protection is NetworkPolicy / Cilium ingress rules scoped to in-cluster callers.

## 3. Problem Statement

External callers cannot safely push to ingest because:

1. **No authentication.** Any party reachable to the ingest port can POST arbitrary content with any `source` field value in the metadata (subject only to the source allowlist, which is trivial to guess).
2. **No transport encryption.** Content metadata and bodies are in the clear. For mbox import this includes the contents of private emails.
3. **No per-caller accountability.** There is no concept of "which client pushed this content" -- source metadata is self-asserted.

Exposing the endpoint through Traefik (adding TLS) solves #2 but not #1 or #3.

## 4. Scope of the Eventual Design

At minimum, the follow-up project must resolve:

### 4.1 Authentication Mechanism

Leading candidates:
- **Pre-shared bearer tokens.** One or more tokens configured via Vault/ESO, issued per client (importer instance, external workstation, friend's laptop). Simple, homelab-appropriate, rotates cleanly.
- **mTLS.** Heavyweight for single-user homelab; deferred unless a compelling reason arises.
- **OIDC / identity provider.** Over-engineered for this use case.

Default assumption: bearer tokens, validated by middleware in the ingest handler, provisioned through Vault and delivered to clients via however that specific client gets secrets (for in-cluster: ESO sync to Secret; for external workstations: manual retrieval from Vault CLI or similar).

### 4.2 Transport Security

TLS terminated at Traefik (cert-manager issuing certs). Ingest listens plain HTTP behind Traefik for external traffic; continues to listen plain HTTP directly for in-cluster traffic. An IngressRoute with appropriate host/path routing handles the external entry.

### 4.3 Uniform vs. Dual-Endpoint Auth

The key design fork. Options:

- **Uniform auth (all callers need a token).** Cleaner posture -- one code path, one security model, one set of threat assumptions. Requires provisioning tokens to every existing in-cluster connector (~20). Churn across each connector's Helm chart and Secret.
- **Dual endpoint.** `/v1/ingest` remains in-cluster-only (unauthenticated); a separate externally-exposed endpoint (e.g., `/v1/ingest-ext` through Traefik) requires bearer tokens. In-cluster connectors unchanged. Two code paths to reason about, but no connector churn.

This spec does not pick. Either answer is defensible; both are worth prototyping when the project is picked up.

### 4.4 Token Model

- How many tokens? (One shared, one per client, one per connector?)
- Rotation semantics.
- Revocation without redeploying all clients.
- Scope: does a token allow pushing with any `source` value, or is the token bound to a specific source?

### 4.5 Rate Limiting

External access is a DoS surface. Per-token rate limits at Traefik or in ingest itself, beyond the existing queue-depth backpressure.

## 5. Code Changes Expected

Non-exhaustive, to be refined in the real spec:

- `internal/ingest/auth.go` (new): middleware that validates bearer tokens against a configured set, populating a request context value with the validated client identity.
- `internal/ingest/handler.go`: wrap `ServeHTTP` in the auth middleware (or conditionally, for the dual-endpoint path).
- `internal/ingest/server.go`: TLS support if we go that route; likely unchanged if Traefik terminates TLS.
- `connector/http_backend.go`: accept a bearer token in the constructor, set `Authorization` header on requests.
- `connectors/*/main.go`: read token from Secret-backed env var, pass to `HTTPStagingBackend` constructor. Only required for the uniform-auth variant.
- Helm chart values: token references per client, optionally a Traefik `IngressRoute` for external access.
- ESO + Vault configuration: `SecretStore` pointing at Vault, `ExternalSecret` resources per consumer.

## 6. Non-Goals for V1 of This Eventual Project

- Multi-tenant glovebox. Single-user homelab remains the design center.
- User-facing auth flows (browser OAuth). Clients are Go binaries or scripts, not browsers.
- Backwards compatibility with pre-auth clients once auth is rolled out. Migration is a cutover, not a parallel support period.

## 7. Trigger Conditions

This project should be picked up when any of the following become true:

- A concrete external-caller use case is requested or blocked (e.g., running the mbox importer on a workstation rather than in-cluster).
- Glovebox is made accessible outside the homelab LAN for any reason (port-forward for remote access, cloud instance, etc.) -- at that point unauthenticated ingest is a hard security problem.
- A second user (friend, family member) wants their own glovebox-backed ingestion path.

Until one of these materializes, the mbox importer spec's in-cluster K8s Job deploy model is sufficient. Deferring this work is not a quality compromise; it's a scope discipline that lets the importer ship on its own merits without dragging an auth project through with it.

## 8. Related Specs

- **Spec 08 (HTTP Ingest API Design)** -- current ingest endpoint design; this stub modifies it.
- **Spec 09 (Mbox Importer Design)** -- first client that will benefit from external access but ships without it in V1.
- **Archiver Spec 01 (Archive Importer Pattern)** -- architectural context for why external importers are a natural class of client.
