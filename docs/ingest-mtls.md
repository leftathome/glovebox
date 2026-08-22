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
2. **Port sharing.** `/v1/archives` and `/v1/ingest` share port 9091 by
   default, so the spec-13 NetworkPolicy that grants the recognizer
   *namespace* access to the archive endpoint also grants it unauthenticated
   ingest. mTLS does not fix this one — see [Which listener serves
   what](#which-listener-serves-what).
3. **Plaintext transport.** Item content — email bodies, and under spec 15
   health data — crossed the pod network unencrypted.

mTLS addresses the first and third, and the first is the reason to do it:
encryption alone would leave the endpoint credulous. **The point is binding
the claim to a verified identity**, not the encryption.

## Which listener serves what

Three route families with three different auth models live on this plane:

| Route | Authenticated by | Listener |
|-------|------------------|----------|
| `POST /v1/ingest` | mTLS peer identity, or nothing under `mode: disabled` | `ingest.port` (plaintext) and/or `ingest.tls.port` |
| `/v1/archives*` | spec 10 bearer token | `ingest.bearer_port` |
| `POST /v1/sanitize` | spec 10 bearer token | `ingest.bearer_port` |

`ingest.bearer_port` defaults to `0`, meaning **share `ingest.port`** — the
layout every install has had. The two bearer endpoints are served in every
`mode`, including `required`: they authenticate themselves and have nothing
to do with the connector transport. Under `required` the plaintext listener
still exists for them, but `/v1/ingest` is not registered on it, so the only
route to the connector intake remains the mTLS listener.

Setting `ingest.bearer_port` to a distinct port (chart:
`config.ingest.bearerPort`) opens a second listener for the bearer endpoints
and leaves `ingest.port` carrying `/v1/ingest` alone. That is what closes
consequence 2 above: the recognizer namespace is then granted the bearer port
and cannot reach the connector intake at all. It is a **coordinated change** —
the recognizer and any `/v1/sanitize` caller are configured against
`ingest.port` today and must be repointed in the same window.

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

## Helm chart

The chart wires all of this from `ingest.tls`:

```yaml
ingest:
  tls:
    mode: permissive          # disabled | permissive | required
    port: 9092
    trustDomain: glovebox
    enforceSourceMatch: true
    issuerRef:
      name: glovebox-ingest-ca
      kind: ClusterIssuer
    duration: 24h
    renewBefore: 8h
    producers: []             # names for the `producer` SPIFFE kind
```

Setting a mode other than `disabled` renders, per install:

- a cert-manager `Certificate` for the server (DNS SANs for the ingest
  Service) and **one per producer** — every enabled connector, the Schoology
  connector, and every enabled importer — each carrying its SPIFFE URI SAN;
- the server keypair mounted at `/etc/ingest-tls` on the scanner, and each
  producer's client keypair at the same path in its own pod;
- `GLOVEBOX_INGEST_URL` switched to `https://…:<tls.port>` plus the three
  client-certificate environment variables;
- the mTLS port on the ingest `Service`, the scanner's `containerPort`, and
  the connector NetworkPolicy;
- one `Certificate` per name in `ingest.tls.producers`, carrying
  `spiffe://<trustDomain>/producer/<name>` — see below.

Every producer is wired, deliberately: under `required` the plaintext
listener is never opened, so a producer the chart forgot would fail to
deliver with nothing but a connection error to show for it.

With `mode: disabled` the chart renders **byte-identically** to before, so an
existing install is untouched until it opts in — including the config
checksum, so no pod restarts on upgrade.

**Issue from a dedicated issuer.** `issuerRef` should name a CA used only for
this plane; pointing it at the cluster edge CA would let any certificate that
CA ever signed ingest.

### Callers the chart does not deploy: `ingest.tls.producers`

Connectors and importers get a certificate automatically, because the chart
deploys them and so knows they exist. The `producer` kind is for a caller
that runs somewhere else — the recognizer
(`spiffe://glovebox/producer/recognizer`) is the documented one. Nothing in
the values would otherwise tell the chart it is coming, so it is named
explicitly:

```yaml
ingest:
  tls:
    producers:
      - recognizer
```

Each name renders one `Certificate`,
`<release>-glovebox-<name>-ingest-producer`, from the same issuer, duration
and `renewBefore` as the connector certificates, with its keypair in
`<release>-glovebox-<name>-ingest-producer-tls`. The `-ingest-producer`
suffix keeps it clear of the `-ingest-client` Secrets connectors and
importers use, so a producer and a connector of the same name are two
identities with two Secrets, not one contested Secret.

The list is **empty by default**, so an install that sets nothing renders
exactly as it did before the key existed. Nothing in the chart mounts the
Secret — the producer runs in its own namespace, so copy or reflect it
there. Names are validated (non-empty, DNS-1123 label, no duplicates)
whether or not `mode` is `disabled`, so a typo fails the render at the point
it is written rather than on the day mTLS is turned on.

## Not yet covered
- **`/v1/archives`** still uses spec 10 bearer tokens. A cert SAN is a
  strictly stronger caller identity, so a later spec can retire the tokens or
  keep them as a second factor for archive-scale sources. When that happens
  the recognizer's certificate is already a chart concern:
  `ingest.tls.producers: [recognizer]`. The cross-namespace
  exposure noted above is **closed by default** as of this release:
  `config.ingest.bearerPort` defaults to 9093, so the recognizer's ingress
  rule reaches the archive endpoint and nothing else. This is a breaking
  change for any caller pointed at 9091 for `/v1/archives*` or `/v1/sanitize`
  -- repoint them at 9093. Setting `bearerPort: 0` restores the shared port
  and re-opens the exposure.
