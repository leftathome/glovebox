# Mutual TLS on `/v1/ingest`

Spec 08 §3.10 left the connector ingest endpoint unauthenticated, gated only
by a Kubernetes NetworkPolicy `podSelector`. This document describes the mTLS
layer that closes that gap and how to migrate onto it without a flag day.

## Why a NetworkPolicy was not enough

The policy admits pods labelled `app.kubernetes.io/component: connector`. A
label is not an identity — any workload in the namespace can set one — so it
bounds *reachability*, not *who is calling*. Three consequences:

1. **Spoofable provenance.** The handler took `metadata.source`,
   `identity.provider` and `destination_agent` on faith. A compromised
   connector — and every connector holds external credentials and parses
   hostile content — could stamp another connector's source onto an item and
   route it to any allowlisted agent. The audit log then recorded the lie.
2. **Port sharing.** `/v1/archives` and `/v1/ingest` share port 9091, so the
   spec-13 NetworkPolicy that grants the recognizer *namespace* access to the
   archive endpoint also grants it unauthenticated ingest.
3. **Plaintext transport.** Item content — email bodies, and under spec 15
   health data — crossed the pod network unencrypted.

mTLS addresses all three, but the first is the reason to do it: encryption
alone would leave the endpoint credulous. **The point is binding the claim to
a verified identity**, not the encryption.

## Why application-level mTLS rather than a service mesh

A mesh (Linkerd, Istio) gives transparent mTLS with no code change, but:

- it encrypts without handing the application the peer identity needed to
  reject a spoofed `source`, unless you also adopt mesh authorization policy;
- it is a large operational dependency for a single-node homelab; and
- glovebox's posture is a minimal surface with no egress — more sidecars in
  the data path works against that.

cert-manager is likely already present for edge TLS, and a Vault PKI issuer
drops in without changing anything below.

## Identity model

Client certificates carry a SPIFFE URI SAN:

```
spiffe://glovebox/connector/<name>     e.g. spiffe://glovebox/connector/rss
spiffe://glovebox/producer/recognizer
```

Identity is read from the URI SAN, not the Common Name: CN is free text with
no structure or uniqueness guarantee, while a URI SAN is what SPIFFE-aware
issuers populate and what a policy can match exactly. A certificate naming a
different trust domain is refused even if it chains to the configured CA.

**Use a CA dedicated to the ingest plane.** Do not reuse the cluster edge CA:
a certificate issued for any other purpose must not be able to ingest.

## Configuration

```json
{
  "ingest": {
    "port": 9091,
    "tls": {
      "mode": "permissive",
      "port": 9092,
      "cert_file": "/etc/glovebox/tls/tls.crt",
      "key_file": "/etc/glovebox/tls/tls.key",
      "client_ca_file": "/etc/glovebox/tls/ca.crt",
      "trust_domain": "glovebox"
    }
  }
}
```

| Key | Meaning |
|-----|---------|
| `mode` | `disabled` (default), `permissive`, `required` |
| `port` | mTLS listener; defaults to `ingest.port + 1` |
| `cert_file` / `key_file` | server keypair; re-read on change, so rotation needs no restart |
| `client_ca_file` | CA that client certificates are verified against — **required** whenever mTLS is on |
| `trust_domain` | expected SPIFFE trust domain (default `glovebox`) |
| `enforce_source_match` | defaults to **true** whenever mTLS is active |

`enforce_source_match` defaults on deliberately. Turning mTLS on and leaving
the endpoint trusting whatever `source` the caller claims would be the
encrypted version of the original problem. Set it false only while migrating a
connector whose source label does not yet match its certificate name.

Connectors are configured with three environment variables, read by the
framework, so no per-connector code changes:

```
GLOVEBOX_INGEST_CA=/etc/glovebox/tls/ca.crt
GLOVEBOX_INGEST_CLIENT_CERT=/etc/glovebox/tls/tls.crt
GLOVEBOX_INGEST_CLIENT_KEY=/etc/glovebox/tls/tls.key
```

Setting only some of them is an error rather than a silent fall back to
plaintext: a fallback would keep the connector working while quietly undoing
the control, which is exactly how such a mistake survives unnoticed.

## Certificate rotation

Both ends re-read their keypair when the files change on disk. cert-manager
renews a Secret and the kubelet updates the mount in place, so a 24h
certificate is practical — which is what makes a stolen key a small problem.
A failed reload (half a rotation visible on disk) keeps the last good keypair
rather than failing every handshake.

## Migration

The mode ladder exists so no flag day is needed:

1. **`disabled`** — current behaviour; plaintext only.
2. **`permissive`** — both listeners serve. Issue certificates, then move
   connectors one at a time by setting the three environment variables and
   pointing `GLOVEBOX_INGEST_URL` at `https://…:9092`. Watch the `transport`
   label on `glovebox_items_received_total` drain from `plaintext` to `mtls`.
3. **`required`** — once the plaintext count is zero, flip. The plaintext
   listener is not opened, so no path remains that skips peer identity.
   Tighten the NetworkPolicy to the mTLS port.

## Rejections

| Condition | Response |
|-----------|----------|
| No client certificate | TLS handshake failure |
| Certificate from an untrusted CA | TLS handshake failure |
| Valid certificate, no SPIFFE URI SAN | `403 unrecognized_client_identity` |
| Valid certificate, foreign trust domain | `403 unrecognized_client_identity` |
| `metadata.source` ≠ certificate name | `403` and the item is **not** staged |
| Enforcement on, request without a peer | `401` |

## Not yet covered

- **Helm chart wiring** (cert-manager `Certificate` resources per connector,
  Secret mounts, NetworkPolicy port change) — follow-up.
- **`/v1/archives`** still uses spec 10 bearer tokens. A cert SAN is a
  strictly stronger caller identity, so a later spec can retire the tokens or
  keep them as a second factor for archive-scale sources. Splitting archives
  onto its own port would also close the cross-namespace exposure noted above.
