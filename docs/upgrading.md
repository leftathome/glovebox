# Upgrading glovebox

`CHANGELOG.md` records every change and why it was made. This document is the
short version an operator needs *before* running `helm upgrade`: the things
that will break if nobody looks at them, and what to do about each one.

If you read nothing else, read [Before you upgrade](#before-you-upgrade). It is
four checks, and each one is a five-minute job that is much less pleasant to do
at 02:00 with uploads failing.

---

## Upgrading to 0.8.0 (from 0.7.0)

This release carries the security work from the 2026-08 review. Three changes
alter behaviour an existing install depends on, and one fixes a chart defect
that was actively breaking repository installs.

### Before you upgrade

- [ ] **Vault:** if your Vault uses a self-signed or private CA, set
      `ingest.auth.vault.caSecret` **now**, before upgrading. See
      [Vault TLS verification](#1-vault-tls-verification-is-on-by-default).
- [ ] **Bearer callers:** find everything pointed at port **9091** for
      `/v1/archives*` or `/v1/sanitize` and plan to repoint it at **9093** in
      the same window. See [The bearer port split](#2-breaking-the-bearer-endpoints-move-to-port-9093).
- [ ] **Image tag:** if you install the chart from a git checkout rather than
      the published OCI artifact, see [the appVersion fix](#4-chart-installs-from-the-repository-were-deploying-the-wrong-image).
- [ ] **Archive producers:** if anything sends `archive/recognizer-scan`,
      see [fail-closed finalize](#3-archiverecognizer-scan-finalize-can-now-fail-for-content-reasons).

---

### 1. Vault TLS verification is on by default

**What changed.** `ingest.auth.vault.tlsSkipVerify` defaulted to `true`. It now
defaults to `false`.

**Why.** That is the path that fetches the ingest and archive bearer tokens, and
those tokens are how glovebox decides which callers to trust. A pod able to
spoof or relay the in-cluster Vault address could hand glovebox attacker-chosen
tokens, undermining every check that depends on them. "Pod network only" bounds
who can attempt that; it does not make the connection authenticated.

**Who this breaks.** An install whose Vault presents a certificate your cluster's
system roots do not trust — i.e. most self-hosted Vaults — and which relied on
the old default.

**What it looks like if you skip this.** Not a silent failure, but not an obvious
one either. The pod starts. Uploads get **503**, not 401, and the reason is in
the pod log at startup:

```
glovebox vault k8s login failed: <x509 error> (archive listener will mount 503 fallback)
```

The 503 fallback is deliberate — glovebox would rather serve nothing than serve
tokens it could not authenticate. It does not retry into a good state: fix the
config and restart the pod.

**Fix, preferred.** Point it at your CA:

```yaml
ingest:
  auth:
    vault:
      caSecret: vault-ca          # Secret holding the bundle under `ca.crt`
```

The chart mounts it read-only and sets `VAULT_CACERT`. The connection stays
authenticated, against a CA you chose.

**Fix, escape hatch.** `tlsSkipVerify: true` still works. It is now an explicit
decision rather than a default nobody looked at, which is the entire point of
the change.

> Note that setting **neither** value emits neither `VAULT_CACERT` nor
> `VAULT_SKIP_VERIFY`, so the client falls back to the system roots. That is
> correct for a Vault with a publicly-trusted certificate and wrong for
> everything else. The chart cannot tell which you have, so it cannot warn you
> at render time — this document is the warning.

---

### 2. BREAKING: the bearer endpoints move to port 9093

**What changed.** `config.ingest.bearerPort` now defaults to **9093**.
`/v1/archives*` and `/v1/sanitize` move onto a listener of their own.
`/v1/ingest` keeps port 9091 to itself.

**Why.** The archive endpoint and the connector intake endpoint shared a port,
so granting a namespace ingress for archive uploads also granted it connector
intake. A namespace that should be able to upload an archive could stage any
item, from any claimed source, to any allowlisted agent. The NetworkPolicy,
Service port and `containerPort` all follow the split, so an archive caller now
reaches the archive endpoint and nothing else.

This defaulted to `0` (share the port) in an earlier revision, to avoid moving a
port under existing callers. That was the wrong call: a vulnerability that ships
closed only if someone reads the release notes is not closed.

**Who this breaks.** Anything configured against 9091 for `/v1/archives*` or
`/v1/sanitize`. Concretely: the recognizer namespace and any sanitize-gate
client.

**Connectors are unaffected.** They are templated off `ingest.port` and keep
using 9091 for `/v1/ingest`.

**Fix.** Repoint bearer callers at 9093 in the same maintenance window as the
upgrade. For the recognizer that is its `gloveboxIngest.url` value; the port is
operator-configurable there, so this is a values change, not a code change.

**If you need to stage the migration**, set `bearerPort: 0` to restore the old
shared-port behaviour, upgrade, repoint callers, then remove the override:

```yaml
config:
  ingest:
    bearerPort: 0     # legacy: share ingest.port. Restores the exposure above.
```

Treat that as a short-lived bridge. It leaves the exposure this change closes
wide open for as long as it is set.

---

### 3. `archive/recognizer-scan` finalize can now fail for content reasons

**What changed.** Text extracted from an `archive/recognizer-scan` upload is
scanned before it is published, and finalize now fails rather than publishing
text nothing has looked at.

**Why.** The extracted text goes to an operator agent. It came off a physical
document someone could have printed, mailed or posted, so it is hostile input.
Publishing it unscanned would route around the entire point of the product.

**Who this affects.** Producers sending `archive/recognizer-scan`. A finalize
that used to fail only for reasons a producer can check before sending — a hash
they computed, a size they declared, a tar they built — can now fail because of
what the content is.

**The failure you will actually hit is a missing `ocr.txt`.** Recognizer
pre-extracts the OCR text and ships it at the tar root; a missing or
whitespace-only `ocr.txt` fails finalize with `ErrScanMissingOCR`. The
sibling `ErrExtractUnscanned` ("no scanner configured") exists as a
fail-closed guard but is unreachable in the shipped binary, which refuses to
start without a scanner (`main.go:157`) — it is there for builds that embed the
archive listener without one.

**The ergonomics are poor and you should know that up front.** Both collapse to
the same opaque `500 internal_finalize`, so the response body cannot tell a
producer which happened. Two consequences:

- **Bound your finalize retries.** A blind retry loop on 5xx will re-upload a
  multi-GB tarball forever against a tarball that will never have an `ocr.txt`.
  Treat a repeated `internal_finalize` on the same `archive_id` as a content
  bug, not backpressure.
- **Retrying the same `archive_id` is safe.** A failed finalize publishes
  nothing and cleans up after itself, so a corrected re-POST gets a `201`, not
  a `409`.

**A 2xx does not mean the text was published.** If the scanner *quarantines* the
extracted text, finalize still succeeds — but `content.extracted.md` carries a
stub naming the score and signals instead of the text. The raw text stays at
`tree/ocr.txt` for a human. Read `content.extracted.md` if you need to know.

---

### 4. Chart installs from the repository were deploying the wrong image

**What changed.** `charts/glovebox/Chart.yaml` had `appVersion: "0.6.1"` while
the chart was at `version: 0.7.0`. `image.tag` defaults to `.Chart.AppVersion`,
and `values.yaml` ships `tag: ""` — so `helm install ./charts/glovebox` from a
checkout deployed the **0.6.1 image** against a chart whose probes are
`httpGet /healthz` and `/readyz`. Those endpoints did not exist until 0.7.0, so
the pod never passed its probes.

`appVersion` had not been bumped since the v0.6.1 release; 0.6.2, 0.6.3, 0.6.4
and 0.7.0 all left it behind.

It is worse than probe failure. 0.6.1 also predates the **0.6.4** `ReadTimeout`
fix, so that same default install advertised `Tus-Max-Size: 30 GiB` while
running an image that force-closes any PATCH taking over 60 seconds — the chart
promised an upload ceiling its own image could not honour.

**Who this affected.** Anyone installing from a git checkout without pinning
`image.tag`. **Installs from the published OCI chart were fine** — CI stamps
both `version` and `appVersion` from the release tag, so the packaged artifact
always carried the right pair.

**Fix.** Already fixed in this release; no action required. If you worked around
it by pinning `image.tag` explicitly, that pin still wins and still works — but
you can now drop it and let the chart default track the release again.

---

## Version floors worth knowing

- **App 0.6.4 is the floor for multi-GB archive uploads.** Earlier builds carry a
  60-second `ReadTimeout` that kills any PATCH taking longer than a minute; a
  large upload dies with a broken pipe (`curl (55)`). The chart advertises
  `Tus-Max-Size: 30 GiB`, which is only true from 0.6.4.
- **Chart 0.7.0 requires app 0.7.0 or later**, because the main-daemon probes
  are `httpGet` against `/healthz` and `/readyz`, which no earlier image serves.

## Peak memory scales with concurrent uploads, not archive size

Worth stating because the obvious guess is wrong and leads to over-provisioning
the wrong axis. The upload path genuinely streams: the Go heap stays flat at
~4 MiB regardless of upload size. Almost all of the pod's footprint is OS page
cache from writing the staging file, and it plateaus at the kernel's dirty-page
ceiling — a 2 GiB upload peaks around 2.1 GiB, a 12 GiB upload around 3.0 GiB.

So size the pod against how many uploads you expect *at once*. `GOMEMLIMIT` does
not help here: it caps the 10 MiB heap, not the page cache.

---

## Where to look when something goes wrong

| Symptom | Look at |
|---|---|
| Uploads return 503, pod is running | Vault TLS — [§1](#1-vault-tls-verification-is-on-by-default). Check the pod log for `vault k8s login failed`. |
| Uploads return connection-refused | Bearer port — [§2](#2-breaking-the-bearer-endpoints-move-to-port-9093). The caller is probably still on 9091. |
| Pod never becomes ready, no obvious error | Image/chart version skew — [§4](#4-chart-installs-from-the-repository-were-deploying-the-wrong-image). Check the deployed tag against the chart version. |
| A large upload dies partway with a broken pipe | App older than 0.6.4 — see [version floors](#version-floors-worth-knowing). |
| Finalize fails on `archive/recognizer-scan` with `500 internal_finalize` | Almost always a missing or whitespace-only `ocr.txt` at the tar root — [§3](#3-archiverecognizer-scan-finalize-can-now-fail-for-content-reasons). Do not retry-loop it. |
