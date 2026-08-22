# Glovebox archive-delivery: recognizer-team handoff

> **Audience:** humans + LLMs on the recognizer team building the
> client that ships large archives to glovebox — mboxes, Google
> Takeout subtrees, health-record exports, and scanned documents.
> **You don't need to read:** spec 13, spec 10, spec 11, spec 15, or
> any of the glovebox source. Everything you need is here — including
> the provenance keys and the media types that live only in spec 15 and
> in the code.
> **Authoritative source:** the protocol is
> [`docs/specs/13-archive-delivery-design.md`](../specs/13-archive-delivery-design.md);
> if this doc disagrees with it on protocol, the spec wins. Everything
> written here about validation rules, media types, error codes and
> finalize behaviour was verified against the running code, which is
> authoritative over any spec where the two have drifted apart. File an
> issue if you spot a discrepancy either way.

This is the document the umbrella bead `glovebox-gdp4` Phase 7 calls
out. Its five jobs:

1. **Token acquisition** — where your bearer token lives and how to
   get it into your pod.
2. **Endpoint address** — the in-cluster URL and the namespace label
   you need on the recognizer namespace, plus the two operator-side
   changes (a port move, an mTLS mode) that can take that address
   away from you mid-flight.
3. **API contract** — `curl` recipes for every state transition, the
   metadata you have to supply, the six accepted media types.
4. **Completion signal** — what to do after a successful upload
   (spoiler: for most media types, nothing).
5. **Version preconditions** — the chart and app versions this
   contract assumes, and what silently breaks below them.

---

## 1. Token acquisition

### 1a. Vault is the source of truth

Your bearer token lives at:

```
secret/glovebox/ingest-tokens/<your-source-id>
```

`<your-source-id>` is a string in `^[a-z][a-z0-9]*(-[a-z0-9]+)*$`,
max 64 chars. The smoke-test source-id is `recognizer-smoke-test`;
your production source-id is whatever you and Steve agreed on (e.g.
`recognizer-v1`). One source-id per consumer; do **not** share tokens
across services.

The KV value at that path is `{"token": "<64-char-lowercase-hex>"}`.
That hex string IS the bearer token glovebox compares against.

### 1b. ESO projects the token into a K8s Secret

You should NOT read Vault directly from the recognizer pod. Use
ExternalSecret to project the token into a K8s Secret in your
namespace, then mount the Secret.

Drop this manifest in your namespace (or copy
`charts/glovebox/templates/archive-tokens-externalsecret.yaml` from
this repo — same shape, just with `recognizer` substituted for your
source-id):

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: recognizer-glovebox-ingest-token
  namespace: recognizer    # your namespace
spec:
  refreshInterval: 1m
  secretStoreRef:
    kind: ClusterSecretStore
    name: vault-default
  target:
    name: recognizer-glovebox-ingest-token
    creationPolicy: Owner
  data:
    - secretKey: token
      remoteRef:
        key: glovebox/ingest-tokens/recognizer-smoke-test  # replace with your source-id
        property: token
```

ESO refreshes every minute, so rotating the token in Vault propagates
to the Secret within 60 seconds.

### 1c. Mount it in your pod

```yaml
spec:
  containers:
    - name: recognizer
      env:
        - name: GLOVEBOX_INGEST_TOKEN
          valueFrom:
            secretKeyRef:
              name: recognizer-glovebox-ingest-token
              key: token
        - name: GLOVEBOX_INGEST_SOURCE_ID
          value: "recognizer-smoke-test"   # your source-id
        - name: GLOVEBOX_INGEST_URL
          value: "http://glovebox-glovebox-ingest.glovebox.svc.cluster.local:9093"
```

**That port is 9093, not 9091.** It moved, it is a breaking change,
and it has to be flipped in the same maintenance window as the
operator's chart upgrade. §2c is the whole story; do not skip it.

Or mount the Secret as a file at `/var/run/recognizer/glovebox-token`
and read it; either pattern works. The token is plain ASCII, not
base64-encoded in transit (ESO base64-decodes between Vault and the
Secret).

### 1d. Token rotation

You don't have to do anything special. When the operator rotates the
Vault entry, ESO syncs within 60s, your pod re-reads the env var or
file on its next read, and the new token goes out on the next request.
**Caveat:** if you cache the token at process start and never re-read
it, a rotation requires a pod restart. Either re-read on every send,
or accept the pod-restart story.

---

## 2. Endpoint address

### 2a. URL

```
http://glovebox-glovebox-ingest.glovebox.svc.cluster.local:9093
```

Service-name shape is `{release}-glovebox-ingest`. The current deploy
uses release name `glovebox` in namespace `glovebox`, so the
double-`glovebox-glovebox-` is correct (verified via `kubectl get svc -n glovebox`).
If the operator ever re-installs under a different release name,
update the host segment.

**The port is 9093 and used to be 9091.** The bearer-authenticated
endpoints — `/v1/archives*` and `/v1/sanitize` — now get a listener of
their own, separate from the connector intake on 9091. §2c is the
migration; read it before you hard-code anything.

Routes mounted on that port:

| Path | Method | Purpose |
|---|---|---|
| `/v1/archives` | OPTIONS | tus.io capability discovery (read `Tus-Version`, `Tus-Max-Size`, etc.) |
| `/v1/archives` | POST | Create an upload; returns `Location: /v1/archives/<upload-id>` |
| `/v1/archives/<upload-id>` | HEAD | Probe upload state for resume |
| `/v1/archives/<upload-id>` | PATCH | Stream bytes (multiple PATCHes allowed) |
| `/v1/archives/<upload-id>` | DELETE | Abandon an in-progress upload |
| `/v1/archives/<archive_id>` | GET | Read the finalize receipt (post-completion) |

**The last row uses a different id from the four above it.** HEAD,
PATCH and DELETE address the *upload*: a server-assigned random id
handed to you in the POST's `Location` header, which lives only until
finalize. GET addresses the *archive*: the `archive_id` you chose and
sent in `Upload-Metadata`, which is what the staged tree is named
after. `GET /v1/archives/<upload-id>` returns `404 archive_not_found`
— it is not a bug, you asked for the wrong noun.

The same listener also carries `POST /v1/sanitize`, the synchronous
scan gate. It is not yours; it shares the port because it shares the
auth model (spec 10 bearer tokens).

`/v1/ingest` is the legacy single-file connector path (spec 08, not
spec 13). Don't use it for archives — and after the port split you
cannot reach it from the recognizer namespace at all, which is the
point of the split.

### 2b. NetworkPolicy expectation

Glovebox renders a NetworkPolicy that **only** accepts traffic on the
bearer port (9093 by default; it follows `config.ingest.bearerPort`)
from namespaces carrying the label:

```
name: openclaw-recognizer
```

That's an operator-set namespace label, NOT the kubelet-managed
`kubernetes.io/metadata.name` auto-label. Confirm with:

```bash
kubectl get ns recognizer -o jsonpath='{.metadata.labels.name}'
```

If that returns empty, ask Steve to:

```bash
kubectl label ns recognizer name=openclaw-recognizer
```

Without that label, **every request** gets dropped at the network
layer — your client will see TCP timeouts, not HTTP 401s.

The rule grants exactly one port. After the split it is 9093, and
9091 is no longer granted to your namespace: a request to the old port
from a correctly-labelled namespace looks *identical* to a request
from an unlabelled one. Both time out. See §2c.

### 2c. The bearer-port move: 9091 → 9093 (BREAKING, coordinate this)

**Status: unreleased.** It is on `main` and in no tagged release — not
even `v0.7.0`, the newest tag. So it arrives the moment the operator
deploys anything newer than that tag, which for a homelab tracking
`main` means "the next time they redeploy". Nothing you can do on your
side triggers it and nothing you can do on your side delays it.

**What changed.** `/v1/archives*` and `/v1/sanitize` used to share the
connector-intake port, 9091. That meant the NetworkPolicy granting the
recognizer namespace the archive endpoint necessarily also granted it
unauthenticated `/v1/ingest` — one port, three route families, three
different auth models, one ingress rule (security review P0-7). A
namespace that should have been able to upload an archive could stage
any item, from any claimed source, to any allowlisted agent. The fix
is to put the bearer endpoints on their own listener, and that cannot
be done without moving a port, so the port moves. `config.ingest.bearerPort`
now defaults to **9093** and the split is on by default; an earlier
revision defaulted it off, which left the hole open for anyone who
didn't read the release notes.

**What it means for you.**

- `GLOVEBOX_INGEST_URL` must become `...:9093`.
- The old port does not fall back, redirect, or answer. Two different
  things happen there depending on the operator's `ingest.tls.mode`,
  and neither is useful to you:
  - the port still listens and serves `/v1/ingest` only, so
    `/v1/archives` gets **404** — *if* you could reach it, which
    the NetworkPolicy no longer lets you (so in practice: timeout);
  - under `ingest.tls.mode: required` nothing binds 9091 in plaintext
    at all, and the chart stops declaring it.
- **Connectors are unaffected.** They are templated off
  `ingest.port` and keep using 9091 for `/v1/ingest`. If someone tells
  you "the ingest port didn't change", they are talking about a
  different endpoint than yours.

**When to do it.** In the same maintenance window as the operator's
chart upgrade, not before and not after. There is no overlap period:
the NetworkPolicy rule that admits your namespace grants **exactly one
port**, and the chart points it at whichever port the bearer endpoints
are on. So 9091 and 9093 are never both reachable from the recognizer
namespace, and there is no window in which both URLs work. Concretely:

1. Agree a window with the operator.
2. Operator upgrades the chart. The archive endpoint is unreachable
   from your namespace from this moment.
3. You roll out the new `GLOVEBOX_INGEST_URL`.
4. Re-run the capability probe (§3e Step 1) to confirm.

In-flight uploads do not survive step 2 — the pod restarts on upgrade
and your PATCHes see `connection refused`. Partial upload state is on
an RWO PVC and does survive the restart, so the recovery is the
ordinary one: HEAD the upload-id **against the new port** and resume
from the returned `Upload-Offset`. See the OOM bullet in §3e for the
same dance.

**The escape hatch, and why not to ask for it.** Setting
`config.ingest.bearerPort: 0` restores the shared-port layout and 9091
starts working again. It also re-opens P0-7. It is a migration aid, not
a supported configuration; if the window slips, the right answer is to
finish the window, not to roll the port back.

**How to tell which layout you are on**, without asking anyone:

```bash
kubectl get svc -n glovebox glovebox-glovebox-ingest -o jsonpath='{.spec.ports[*].name}{"\n"}'
# "ingest ingest-bearer"  -> split; use the ingest-bearer port (9093)
# "ingest"                -> shared; use the ingest port (9091)
```

### 2d. `ingest.tls.mode: required` — a precondition worth checking

This one is a build-version hazard rather than a config you control,
and it has already bitten us once: recognizer's uploads stopped and
nobody could see why.

**The failure.** The mTLS work for `/v1/ingest` (spec 08 §3.10) gave
`ingest.tls.mode` three values — `disabled`, `permissive`, `required`.
On a build carrying that work but predating the fix, all three route
families shared one mux, and that mux was served only by the plaintext
listener — which `required` mode refuses to open. The mTLS listener
mounts `/v1/ingest` alone. So flipping the *connector transport* to
mTLS silently blacked out two endpoints that authenticate themselves
and have nothing to do with that transport. You got `connection
refused` on every `/v1/archives` call, with nothing in the glovebox
logs saying why, because from glovebox's point of view nothing had
gone wrong.

**The current behaviour.** Fixed on `main`: the bearer surface has its
own lifecycle and is opened in **all three** modes. Under `required`
the plaintext listener still comes up for `/v1/archives*` and
`/v1/sanitize`; `/v1/ingest` is simply not registered on it, so that
route answers 404 and there is still no unauthenticated path to the
connector intake. `ingest.tls.mode` is now, correctly, none of your
business.

**Which builds are affected.** The window is bounded on both ends by
commits that are *both* unreleased: the mTLS commit opens it, the
listener-split commit closes it. So:

- **Every tag in the repo, up to and including `v0.7.0`,** predates
  the mTLS work entirely — not affected, and `ingest.tls.mode` does
  not exist there, so `required` cannot be set.
- **A `main` build between those two commits** is affected. That is a
  narrow window, but it is exactly the kind of build a homelab runs.
- **Current `main`** is not affected.

**How to know your precondition holds** — one probe, no operator
required:

```bash
curl -sS -o /dev/null -w '%{http_code}\n' -X OPTIONS "$URL/v1/archives" \
  -H "Tus-Resumable: 1.0.0" -H "Authorization: Bearer $TOKEN"
# 200                -> the bearer listener is up. You are fine.
# connection refused -> the listener is not there.
# timeout            -> NetworkPolicy (§2b), not this.
```

Treat `connection refused` on OPTIONS as a **hard stop**, not as
something to retry into. Retrying is what turned this into a quiet
outage last time: connection-refused is indistinguishable from a pod
restart, so a client with exponential backoff just gets quieter. Alert
on it, name `ingest.tls.mode` in the alert text, and ask the operator
to confirm the glovebox build postdates the listener split. The pod's
boot log states which port answers which endpoints — that line exists
because this failure was so hard to see:

```bash
kubectl logs deploy/glovebox -n glovebox | grep "ingest server listening"
# ingest server listening on :9091 (/v1/ingest)
# ingest server listening on :9093 (/v1/archives*, /v1/sanitize)
```

### 2e. Outside the cluster

There is no ingress for `/v1/archives*` by default; this endpoint is
intentionally cluster-private. The smoke-test script
(`scripts/archive-smoke-test.sh`) runs in-cluster via a `kubectl run`
or with the in-cluster service hostname; it doesn't reach in from a
laptop.

---

## 3. API contract

The endpoint speaks **tus.io v1.0.0** (resumable upload protocol).
This is the same protocol used by `tusd`, `uppy`, etc. — your favorite
language probably has a client library. If you write your own, here
are the recipes that pass glovebox's tests.

### 3a. Required Upload-Metadata keys

Per spec 13 §4.2, plus the spec 15 provenance keys below. Every key is
base64-encoded (`StdEncoding`, NOT URL-safe). The header format is:

```
Upload-Metadata: key1 BASE64(value1),key2 BASE64(value2),...
```

| Key | Required | Format | Notes |
|---|---|---|---|
| `archive_id` | yes | `^[a-zA-Z0-9._-]{1,128}$` | Idempotency key; same `archive_id` + same `sha256` from same `source_id` is a no-op replay. |
| `archive_filename` | yes | `[A-Za-z0-9._-]+`, no `..`, ≤ 256 B | What the final file gets named under `archives/<id>/`. |
| `media_type` | yes | see §3b allow-list | Drives untar-vs-raw dispatch. |
| `matcher_id` | yes | `^[A-Za-z0-9._/-]{1,256}$` | Free-form correlation id you control. |
| `provider` | yes | `^[a-z][a-z0-9-]{0,63}$` | E.g. `recognizer`. |
| `sha256` | yes | `^[0-9a-f]{64}$` | sha256 of the upload body, lowercase hex. **Glovebox computes it during PATCH and verifies at finalize**; a mismatch returns `400 sha256_mismatch`. |
| `size_bytes` | yes | decimal int ≥ 0 | Must equal `Upload-Length` exactly. Mismatch -> 400 at POST. |
| `subtree_relative_path` | only for `archive/google-takeout-subtree` | UTF-8, no NUL, no C0 controls except `\t`, ≤ 1024 B | The relative path within the Takeout export this tarball represents. |
| `acq_provider` | only for `archive/walhelm-export` | `^[a-z][a-z0-9-]{0,63}$` | Acquisition provenance. See §3c. |
| `acq_account_id` | only for `archive/walhelm-export` | ≤ 256 B, no C0 controls except `\t` | Acquisition provenance. See §3c. |
| `acq_auth_method` | only for `archive/walhelm-export` | enum, one value: `browser_session` | Acquisition provenance. See §3c. |
| `data_subject` | only for `archive/walhelm-export` | ≤ 256 B, no C0 controls except `\t` | Opaque subject principal. See §3c. |
| `audience` | no | comma-separated tokens; cross-field rules in §3c | Producer-asserted audience for the archive. Validated for **every** media type when present. |

The required set is **per media type**, not global. The parser looks up
`media_type` in the allow-list first, then applies that type's extra
required keys — so the same header that is complete for `archive/mbox`
is a `400` for `archive/walhelm-export`.

**Reserved keys you cannot set:** `delivered_by`, `delivered_at`.
Glovebox writes those at finalize from the auth-resolved `source_id`
and the wall-clock. Including either in your `Upload-Metadata`
returns `400 metadata_reserved_key`.

The whole `Upload-Metadata` header is capped at **4 KiB**. Exceeding
returns `431 metadata_too_long`.

### 3b. Accepted media types

Six. The list is hard-coded in `mediaAllowList`; operators cannot add
an entry at runtime, and a seventh needs a code change in glovebox.

```
archive/mbox                      (raw)   — one mbox file, ships as-is.
archive/google-takeout-subtree    (tar)   — uncompressed tarball (no .tar.gz / .tar.zst); glovebox untars.
archive/imap-export               (raw)   — one mbox-shaped IMAP dump.
archive/generic-tarball           (tar)   — uncompressed tarball, no Takeout semantics.
archive/walhelm-export            (tar)   — uncompressed tarball of health records; REQUIRES the provenance keys (§3c).
archive/recognizer-scan           (tar)   — uncompressed tarball from recognizer's document scanner (§3d).
```

The shape in parentheses decides where the bytes land: `raw` renames
the upload to `archives/<archive_id>/raw/<archive_filename>`, `tar`
untars it to `archives/<archive_id>/tree/<entry-path>...`. Nothing else
about a media type changes the wire protocol — the extra rules for the
last two are validation and finalize behaviour, not different HTTP.

Two of these are new relative to the four this document used to list.
They were added for real producers (`archive/walhelm-export` for
health-record delivery, `archive/recognizer-scan` for the document
scanner) and both are recognizer's to send, which is why they get
their own sections below rather than a row in a table.

**Importer coverage is a separate question from acceptance.** Only
`archive/mbox` has a downstream importer today (the spec-9 watcher,
`glovebox-c9zt`). The other five are accepted, verified, untarred and
staged under `archives/<id>/` — and then sit there until a consumer for
them lands. That was a deliberate relaxation: glovebox takes the data
so it isn't lost at the boundary while the importer side catches up.
Do not read a 2xx as "this has been ingested"; read it as "this has
been durably staged".

**Tarballs MUST be uncompressed in v1** — spec 13 §"Out of scope and
deferred" §"Server-side compression" is explicit. The finalize path
feeds the body straight into `archive/tar.NewReader` with no gzip
wrapper; a `.tar.gz` upload fails at finalize with
`untar: tar read: archive/tar: invalid tar header` in the log and a
**`500 internal_finalize`** on the wire (see §3f — an unparseable tar
is not one of the recognised rejection reasons, so it does not get a
4xx).
Recognizer must emit plain `.tar` (or expose decompression as a
separate spec-amendment bead with a decompression-bomb defense
budget).

Anything else returns `400 unknown_media_type`. Adding a seventh media
type — or adding gzip/zstd support to existing tarball media types —
requires a code change in glovebox; ask Steve to file a spec
amendment.

### 3c. `archive/walhelm-export` and the provenance keys

`archive/walhelm-export` carries health records (spec 15). It is the
only media type that makes four extra `Upload-Metadata` keys
**mandatory** (plus a fifth, `audience`, that stays optional), and
those keys are documented nowhere else you are expected to read —
hence this section.

The keys exist because glovebox's server-set `delivered_by` answers
"who handed us these bytes" (your ingest token) and nothing answers
"whose credential fetched them, from where, and about whom". For
ordinary mail that gap is tolerable. For health data it is not: the
receipt has to record the acquisition identity separately from the
delivery identity, and only the recognizer knows it. So these are
**client-set and required** — the one place in this contract where
glovebox trusts a producer assertion and writes it into the permanent
record.

| Key | Enforced format | What to put in it |
|---|---|---|
| `acq_provider` | `^[a-z][a-z0-9-]{0,63}$` | The source system the credential authenticated to, e.g. `kp-wa`. Same character class as `provider`, different meaning: `provider` is who is delivering, `acq_provider` is who was logged into. |
| `acq_account_id` | non-empty, ≤ 256 bytes, no C0 control bytes except `\t` | The account/login used to fetch — the member's user id on the source system. |
| `acq_auth_method` | **exact string `browser_session`** | How that credential authenticated. It is an enum with exactly one legal value today; any other string is a `400`, including the empty string and `browser-session`. |
| `data_subject` | non-empty, ≤ 256 bytes, no C0 control bytes except `\t` | The **subject principal**: a connector-scoped opaque id of the form `walhelm:<source-subject-id>`. Never a human name. Glovebox treats it as opaque and resolves it downstream. |
| `audience` | optional; see the token rules below | Producer-asserted audience for every item in the archive. |

All of them are validated at **POST**, before you stream a byte. A missing
one returns `400 metadata_missing`; a malformed one returns
`400 metadata_invalid`. Both bodies are the opaque single-code shape and
**do not name the key that failed** — that opacity is deliberate (a
client could otherwise craft metadata to echo strings back at itself),
so debug against this table rather than against the response.

**`audience` is not walhelm-specific.** If you send it, it is validated
for *any* media type, and the rules are cross-field, not per-token:

- Legal tokens: `subject`, `guardians`, `siblings`, `household`,
  `caregivers`, `public`, `operator`. Anything else is a `400`.
- Comma-separated, at most 16 entries, no duplicates.
- `public` must appear alone. `operator` must appear alone.
- `household` cannot be combined with its own subsets (`subject`,
  `guardians`, `siblings`) — that would be redundant. `caregivers` is
  the exception and *may* sit alongside `household`.
- `subject` and `siblings` require `data_subject` to be set. `guardians`
  and `caregivers` may stand alone (they read as household-scope).
- Omitting the key entirely is legal and is **not** the same as sending
  an empty value: an empty `audience` is a `400`. Omitted means
  "unset", and a downstream reader applies the default `["household"]`.

**The other five media types accept these keys but do not validate
them.** If you set `acq_provider` on an `archive/mbox` upload it is
copied verbatim into the finalize receipt's `acquisition` block with no
regex applied; the same is true of `data_subject`. Glovebox will not
catch a typo there. Only `audience` is validated unconditionally. Treat
provenance on non-walhelm media as your own invariant to hold.

### 3d. `archive/recognizer-scan`

This media type is recognizer's own — a tar of one scanned document
from recognizer's document scanner — and it behaves differently from
the other five in three ways you have to code against. It is the
reason this section exists: the contract for recognizer's own media
type was, until now, readable only in glovebox's source.

**1. It is gated to one authenticated source-id, fail-closed.**

`archive/recognizer-scan` may only be delivered by a source-id the
operator has registered with `kind: recognizer-scanner` in glovebox's
source registry (`/etc/glovebox/sources.json`, shipped by the chart
with the entry `source_id: recognizer-scanner`). The gate reads your
**authenticated** identity — the source-id your bearer token resolves
to — not the `provider` metadata key, so it cannot be asserted around.

- Practically: `GLOVEBOX_INGEST_SOURCE_ID` must be exactly
  `recognizer-scanner`, and the token at
  `secret/glovebox/ingest-tokens/recognizer-scanner` is the one to use.
  Your ordinary archive source-id will not work for this media type.
- Failure is `403 source_not_authorized`, returned from the **final
  PATCH** rather than from POST: the gate runs at the top of finalize.
  You will upload the entire tarball before finding out. Validate your
  source-id at startup, not per-upload.
- Fail-closed means fail-closed: an unset or unreadable registry, an
  unregistered source-id, and a registered source-id of some other
  kind all reject identically. An operator who forgets to configure the
  registry gets a refusal, not a passthrough.

**2. The tarball must carry `ocr.txt` at the tar root.**

Recognizer pre-extracts the OCR text and ships it in the tarball as
UTF-8 plaintext at `ocr.txt` — tar root, no directory prefix, that
exact name. After untar it lands at `archives/<archive_id>/tree/ocr.txt`,
and glovebox renders it into `archives/<archive_id>/content.extracted.md`
so the openclaw operator agent can index and recall the document.

The division of labour is a locked decision (2026-06-15): **recognizer
owns extraction, glovebox owns rendering.** Glovebox will not add a
PDF/A text-extraction dependency to do this for you. Whitespace-only
content counts as missing.

**Watch the direction of the two rules.** The media type requires the
source — `archive/recognizer-scan` from anyone else is a 403. But the
extraction requires the *source*, not the media type: once you are
authenticated as `recognizer-scanner`, **every tar-shaped upload you
make** goes down the extraction path and must carry `ocr.txt`, including
`archive/generic-tarball`. If you need to deliver a non-scan tarball,
deliver it under a different source-id. (Raw-shaped media — `archive/mbox`,
`archive/imap-export` — are unaffected; there is no tree to look in.)

**3. Finalize can now fail for content reasons. This is the part that
changes your client.**

Every other media type's finalize fails only for reasons you can check
before you send: a hash you computed, a size you declared, a tar you
built. This lane adds failures that depend on what the *content* is and
on operator-side state:

| What happened | Wire result | Retryable as-is? |
|---|---|---|
| No `ocr.txt`, or it is empty/whitespace-only | `500 internal_finalize` | **No.** Rebuild the tarball. |
| The content scanner returned an error | `500 internal_finalize` | Yes, after a delay — it is a glovebox-side fault. |
| No scanner configured on the glovebox side | `500 internal_finalize` | **No.** Operator fix. |

Three things follow, and all three are client work:

- **A 5xx from this endpoint is no longer automatically "glovebox is
  having a bad day".** All three collapse to the same opaque
  `{"error":"internal_finalize"}` body — the response cannot tell you
  which. A blind retry loop on 5xx will re-upload a multi-GB tarball
  forever against a missing `ocr.txt`. **Bound your finalize retries**
  and treat a repeated `internal_finalize` on the same `archive_id` as
  a content bug, not as backpressure.
- **Alert on it.** Because it is indistinguishable on the wire, the
  only way to tell the three apart is glovebox's pod log, which names
  the `archive_id`:
  ```bash
  kubectl logs deploy/glovebox -n glovebox | grep "finalize internal error"
  ```
  Page a human on the second occurrence for one `archive_id`.
- **Retrying the same `archive_id` is safe.** A failed finalize
  publishes nothing and cleans up its own temp state, so
  `archives/<archive_id>/` is never created and the idempotency
  pre-flight has nothing to collide with. Fix the tarball, POST again
  with the same `archive_id`, get a `201` — not a `409`.

**"Configured scanner" is an operator-side precondition, not something
you can supply.** The shipped glovebox binary always builds a scanner
at boot (it refuses to start otherwise), so in a normal deployment this
particular failure cannot occur. It exists as a fail-closed guard for
builds that embed the archive listener without one, and it is written
down here because from your side it is indistinguishable from the other
two 500s.

**Success is not the same as "text published."** If the scanner
*quarantines* the extracted text — it came off a physical document
someone could have printed, mailed or posted, so it is treated as
hostile input — finalize still succeeds and returns 2xx. What changes
is `content.extracted.md`: instead of the text it carries a stub
naming the score and the signals that fired, and says the body was
withheld. The raw text is untouched at `tree/ocr.txt` for a human, and
your archive is otherwise complete. If recognizer needs to know whether
the text was published, read `content.extracted.md` — a 2xx will not
tell you.

**What the receipt looks like for this lane.** On success,
`archives/<archive_id>/metadata.json` carries two fields that ordinary
deliveries do not:

- `source: "recognizer-scanner"` — the authenticated source-id, stamped
  by the server, never a producer assertion.
- `audience: ["operator"]` — taken from the registry and applied
  **unconditionally**, overriding any `audience` you sent. A scan that
  escaped the operator marker would be auto-routed by openclaw's
  per-person triage resolver, which is exactly what this lane must not
  do. Do not bother sending `audience` on this media type.

`data_subject` behaves differently again: yours wins if you send one,
and the registry's per-connector default fills in when you don't.

### 3e. Recipes

These assume `$TOKEN`, `$URL`, and `$SID` are set from §1c. The
metadata header is hand-built to keep the recipe legible; in a real
client use a tus.io library.

**Helper for metadata base64-encoding:**

```bash
b64() { printf '%s' "$1" | base64 -w0; }
ARCHIVE_ID=my-archive-001            # yours; also the GET path in Step 6
META="archive_id $(b64 $ARCHIVE_ID)\
,archive_filename $(b64 archive.mbox)\
,media_type $(b64 archive/mbox)\
,matcher_id $(b64 my-correlation-id)\
,provider $(b64 recognizer)\
,sha256 $(b64 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef)\
,size_bytes $(b64 1048576)"
```

**Step 1: OPTIONS (optional capability probe)**

```bash
curl -i -X OPTIONS "$URL/v1/archives" \
  -H "Tus-Resumable: 1.0.0" \
  -H "Authorization: Bearer $TOKEN"
# Returns 200 with Tus-Version, Tus-Max-Size, Tus-Extension headers.
# Use this to discover the per-upload size cap (30 GiB by default).
# OPTIONS is NOT exempt from auth here: without a valid bearer token it
# is a 401 like everything else, even though tus.io exempts it from the
# Tus-Resumable check.
```

**Step 2: POST to create the upload**

```bash
curl -i -X POST "$URL/v1/archives" \
  -H "Tus-Resumable: 1.0.0" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Upload-Length: 1048576" \
  -H "Upload-Metadata: $META"
# 201 Created
# Location: /v1/archives/<server-assigned-id>
# Tus-Resumable: 1.0.0
# Save the Location value as UPLOAD_URL for the PATCH steps.
```

`size_bytes` in `META` MUST equal the `Upload-Length` header.

**Status codes:**

- `201` — created, upload-id in `Location`.
- `303` — replay; an archive with the same `archive_id` **and** the
  same `sha256`, from this source-id, is already **finalized**. The
  `Location` points at `/v1/archives/<archive_id>` — GET it for the
  receipt. Note this only catches *finalized* archives: the check is a
  lookup of `archives/<archive_id>/metadata.json`, so a second POST
  while the first upload is still streaming gets a fresh `201`, not a
  303.
- `400` — metadata validation failed (`metadata_missing`,
  `metadata_invalid`, `unknown_media_type`, `size_mismatch`).
- `401` — bad/missing `Authorization`. The 401 body is byte-identical
  for "no header", "wrong format", "valid format but unknown token";
  don't try to distinguish.
- `409 archive_id_conflict` — an archive with this `archive_id` is
  already finalized and does not match. Two cases, one code: another
  source-id owns it (rare), **or** you own it and the `sha256`
  differs. The second is the one that will actually happen to you —
  re-delivering corrected bytes under a reused `archive_id` is a
  conflict, not an update. `archive_id` is an idempotency key, not a
  mutable name; pick a new one.
- `413` — `Upload-Length` exceeds `Tus-Max-Size`. **You should never
  see this** since glovebox is configured for 30 GiB by default; if
  you do, the operator under-provisioned the cap.
- **OOM mid-upload** (not an HTTP code — the pod restarts, your in-flight
  PATCHes see `connection refused`). The glovebox pod runs with a hard
  memory cap; the current default is still 2 GiB, now at chart 0.7.0.
  The number has not moved, and it is not sized for archive size: the
  PATCH path genuinely streams (32 KiB buffer plus a rolling sha256, Go
  heap flat at ~10 MiB regardless of upload size), so nearly the whole
  footprint is OS page cache from writing the staging file, and that
  plateaus at the kernel's dirty-page ceiling rather than scaling with
  the file. A 12 GiB upload peaks around 3 GiB, the same order as a
  2 GiB one. What 2 GiB does *not* cover is **concurrency** — size it
  for (concurrent uploads × ~3 GiB), so if recognizer runs several
  uploads at once, ask the operator to raise the limit rather than
  assuming multi-GB is the risk. Retry semantics: HEAD the upload-id;
  if it still exists in `.tmp-archives/` (RWO PVC survives pod restart)
  the offset is recoverable and PATCH-resume picks up cleanly. If HEAD
  returns 404 the in-flight state was lost — POST a fresh upload.
  Tracked under `glovebox-5ud9` and the pprof / streaming-audit
  followup bead.
- `429` — either auth rate-limiting (`Retry-After: 60`) or a
  concurrency cap: `concurrent_uploads_per_source` (default 4 in
  flight per source-id) or `concurrent_uploads_global` (default 32),
  both with `Retry-After: 60`. Honor it. DELETE-ing abandoned uploads
  is how you stop hitting the per-source one.
- `503 storage_hard_cap` — the staging PVC is over the hard cap (95%).
  `Retry-After: 600`, and it does not lift until usage drops back
  under 85%. This is the one 503 that is genuinely worth waiting out.
- `503` from the listener fallback — archive listener unavailable
  (Vault load failed at boot, or the st_dev check failed).
  `Retry-After: 60`, but note that a later successful Vault reload does
  **not** promote the route back: it takes an operator-led pod
  restart. If you are still getting this after a few minutes, escalate
  rather than back off further.

**Step 3: PATCH in chunks**

```bash
curl -i -X PATCH "$URL$UPLOAD_URL" \
  -H "Tus-Resumable: 1.0.0" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/offset+octet-stream" \
  -H "Upload-Offset: 0" \
  --data-binary @chunk1.bin
# 204 No Content
# Upload-Offset: <new-server-offset>
# Tus-Resumable: 1.0.0
```

Each PATCH:
- Must include `Upload-Offset` matching the server's current offset.
  Mismatch returns `409 offset_mismatch`, with the correct value in the
  body's `expected` field — the one place in this API where an error
  body carries anything beyond the code.
- One PATCH at a time per upload-id. A second concurrent PATCH gets
  `409 upload_busy` rather than queuing behind the first; the server
  refuses rather than blocks, so a client that fans out chunk writes
  will see this. Serialize per upload.
- `Content-Type` must be `application/offset+octet-stream`; anything
  else is `415 wrong_content_type`.
- Server returns the new offset on success.
- The final PATCH delivers the last byte (`server_offset + body_size
  == Upload-Length`). At that moment glovebox runs finalize: verifies
  sha256, runs untar (if applicable), and atomically renames into
  `archives/<archive_id>/`.

Per-PATCH body has a **5-minute idle timeout** (no bytes received in
5 min -> 408 + tmp cleanup). Keep your chunks flowing; typical chunk
size is 16-64 MiB.

**Step 4: HEAD to probe state (e.g. after a crash)**

```bash
curl -i -X HEAD "$URL$UPLOAD_URL" \
  -H "Tus-Resumable: 1.0.0" \
  -H "Authorization: Bearer $TOKEN"
# 200 OK
# Upload-Length: 1048576
# Upload-Offset: 524288     <-- resume from here
# Tus-Expires: <when the tmp sweep will reclaim this upload>
# Cache-Control: no-store
```

Resume from `Upload-Offset`. `Tus-Expires` tells you how long you have:
it is the upload's creation time plus the tmp expiry (72h by default),
which is deliberately generous so an overnight laptop suspend doesn't
cost you a 12 GiB re-upload. If you get `404`, the upload was either
swept or never existed; POST a new one.

**Step 5: DELETE to abandon**

```bash
curl -i -X DELETE "$URL$UPLOAD_URL" \
  -H "Authorization: Bearer $TOKEN"
# 204 No Content
```

Frees the server's slot in the per-source concurrent cap (default 4
per source, 32 global). If you don't DELETE, the cleanup goroutine
sweeps after 72h.

**Step 6: GET (post-finalize inspection)**

```bash
curl -i "$URL/v1/archives/$ARCHIVE_ID" \
  -H "Authorization: Bearer $TOKEN"
# 200 OK if finalized
# Body: the finalize receipt (the same JSON as metadata.json; see §4)
# 404 archive_not_found otherwise
```

**Address this by `archive_id`, not by the upload-id.** Once finalize
succeeds the upload-id is gone; the archive is named after the
`archive_id` you chose. `curl "$URL$UPLOAD_URL"` here returns 404.

**This is the only way to read the receipt over HTTP.** The successful
final PATCH answers `204 No Content` with `Upload-Offset` and
`Tus-Resumable` and an **empty body** — it does not carry the receipt.
If you need `entries_extracted`, the stamped `audience`, or anything
else the server decided, GET it.

The 404 is deliberately opaque: a malformed `archive_id`, an archive
that never existed, and an archive that exists but belongs to a
different source-id all return the identical
`{"error":"archive_not_found"}`, so a probing client learns nothing.
Don't try to distinguish them.

### 3f. Finalize error codes (in the final PATCH's response body)

The final PATCH may return 4xx or 5xx with a JSON body. The body is a
**single field** — there is no `message`, and it never echoes anything
you sent:

```json
{"error":"<code>"}
```

Codes you'll see:

| Code | Status | What it means | What you should do |
|---|---|---|---|
| `sha256_mismatch` | 400 | Computed sha256 disagrees with the declared `sha256` metadata. | Re-upload from scratch; the bytes corrupted in flight or your computation was wrong. |
| `size_mismatch` | 400 | The staged file's size disagrees with `Upload-Length`. | Same — re-upload. |
| `tar_unsafe_entry` | 400 | The tarball violated the untar allow-list: `..`, absolute paths, symlinks, an entry type other than regular file or directory, a pax `path`/`linkpath` override, a name over 4096 bytes or a component over 255, more than 1,000,000 entries, any single entry declaring more than `Upload-Length` bytes, or extracted bytes totalling more than 2 × `Upload-Length`. | Fix your tarball; spec 13 §4.7 is strict, and **all** of those collapse to this one code. Read the pod log's `reason` field for which. |
| `archive_id_conflict` | 409 | An archive with this `archive_id` already exists and doesn't match. | Pick a new `archive_id`. See §3e Step 2. |
| `source_not_authorized` | 403 | `archive/recognizer-scan` from a source-id not registered as `recognizer-scanner`. | Fix your source-id; see §3d. |
| `internal_finalize` | 500 | Everything else, including the `archive/recognizer-scan` content failures. | See §3d — bound your retries, and don't treat it as pure backpressure. |

**A tarball glovebox cannot parse at all is a 500, not a 400.** The
allow-list violations above are checked per entry, so they need a
readable tar first. A `.tar.gz`, a `.tar.zst`, a truncated tar or any
other unparseable stream fails in `archive/tar` itself, which is not
one of the recognised rejection reasons, so it lands in the
`internal_finalize` bucket. It is your bug and it looks like ours. If
you see `internal_finalize` on a tar-shaped media type, check
compression before you escalate.

Two codes that used to be listed here **do not exist** and never
reach you on this path: there is no `tar_unsupported_entry` (that case
is folded into `tar_unsafe_entry`), and there is no `quota_exhausted`
— the storage cap is enforced at **POST** as `503 storage_hard_cap`,
before you upload anything, which is the friendlier place for it.

Nothing in this table is retry-worthy as-is except `internal_finalize`,
and that one only conditionally (§3d). Everything else is "fix
something and try again". The storage-pressure case you *should* wait
out never appears here, because you hit it at POST.

---

## 4. Completion signal

**You do not need to do anything after a successful final PATCH.**

Once finalize returns 2xx, glovebox has:

1. Written either `archives/<archive_id>/raw/<archive_filename>` (for
   raw media shapes — mbox, imap-export) **or**
   `archives/<archive_id>/tree/<entry-path>...` (for tarballs —
   google-takeout-subtree, generic-tarball, walhelm-export,
   recognizer-scan; one file per tar entry).
2. For the `archive/recognizer-scan` lane only, also written
   `archives/<archive_id>/content.extracted.md` — the rendered (or
   withheld) OCR text. It is written *before* the publish rename, so
   if it can't be produced the whole finalize fails and nothing is
   published. See §3d.
3. Written **`archives/<archive_id>/metadata.json`**, the single
   sidecar carrying every validated `Upload-Metadata` key + the
   server-set `delivered_by` (your source-id), `received_at`,
   `sha256_verified: true`, `staged_path`, `entries_extracted`
   (`0` for raw, count for tarball), and `raw_filename` (raw shapes
   only). This file IS the finalize receipt — there is no separate
   `receipt.json`. The `GET /v1/archives/<archive_id>` endpoint
   returns the same JSON (spec 13 §4.8).

Four receipt fields appear only when they apply, so don't be surprised
by their absence: `matcher_id` (omitted if you didn't send one),
`acquisition` (the `acq_*` block, present when you sent `acq_provider`
— see §3c), `data_subject`, and `audience`. For the scanner lane
`source` and the operator `audience` are added by the server; see §3d.

**The publish is atomic and all-or-nothing.** Everything above is
assembled in a temporary directory and moved into place with a single
rename at the end. A downstream watcher therefore never sees a
half-written `archives/<id>/` — either the whole tree is there or none
of it is. That is also why a finalize failure leaves nothing behind and
why retrying a failed `archive_id` is safe (§3d).

For `archive/mbox`, the spec-9 mbox-importer watcher (bead
`glovebox-c9zt`) picks the `archives/<id>/` directory up from there
and feeds its contents into the scanner pipeline. It learns about new
archives via filesystem watch on `archives/`; no extra signal is
required from you.

**For the other five media types there is no importer yet** (§3b).
Your archive is staged, verified and durable, and that is all that has
happened to it. If you are shipping one of those and expecting
downstream behaviour, confirm with Steve that a consumer exists before
you build on it — a 2xx will not tell you.

**Do NOT** add a "I just finished, please scan" webhook/ping. There
is no such endpoint, and adding one would duplicate the watcher's
job. If the watcher isn't picking up your archives, that's a bug in
spec-9 land — file it as a new bead, don't paper over with a side
channel.

---

## 5. Version preconditions

Everything above describes glovebox as it is on `main` today. Some of
it is not true of older deployments, and the failure modes are quiet
enough to be worth stating rather than discovering.

### 5a. Chart 0.7.0 is the current chart

The Helm chart is at **0.7.0**. If you see a version below that quoted
anywhere as "current", it is stale — this document quoted 0.4.2 for
some time, which was wrong in name only (the memory limit it named was
and is 2 GiB), but wrong is wrong.

### 5b. App 0.6.4 is the floor for multi-GB uploads

Below glovebox **0.6.4**, any single PATCH that takes longer than 60
seconds is force-closed by the server mid-body. The archive endpoint
shared the ingest `http.Server`, which set `ReadTimeout` from
`request_timeout_seconds` (default 60s), and `http.Server.ReadTimeout`
bounds the *entire request including the body* — so it overrode the
handler's own 5-minute idle timeout and made the advertised
`Tus-Max-Size: 30 GiB` undeliverable. 0.6.4 sets only
`ReadHeaderTimeout` and leaves the body unbounded.

**What it looks like from your side:** `curl: (55) Send failure: Broken
pipe`, or your HTTP client reporting a closed connection, at
consistently ~60 seconds into a PATCH regardless of chunk size. It is
reproducible and time-based, which distinguishes it from an OOM (§3e)
— an OOM is `connection refused` and doesn't land on the same clock
every time. Resume works, so a naive client can thrash forever making
60 seconds of progress at a time.

**So: 30 GiB in one upload requires app ≥ 0.6.4.** Below that, the cap
you can actually deliver is however many bytes your link moves in 60
seconds. This document advertises `Tus-Max-Size: 30 GiB` throughout;
that advertisement is only honest at 0.6.4 and up.

### 5c. Check the app version, not just the chart version

The chart and the app version it deploys are separate numbers, and
they can disagree. Chart 0.7.0 pins `appVersion: 0.6.1` — which is
below the 0.6.4 floor in §5b — and the chart uses `appVersion` as the
image tag whenever `image.tag` is unset, which is the default. **A
default install of the current chart deploys an app that has the 60s
bug.** Do not infer the app version from the chart version.

Ask for the image tag, or read it yourself:

```bash
kubectl get deploy glovebox -n glovebox \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
```

If that comes back at `:0.6.1` or lower and you intend to ship
multi-GB archives, ask the operator to set `image.tag` explicitly to a
release at or above `0.6.4` before you start. The `appVersion` lag is
glovebox's to fix; until it is, the image tag is the operator's to
set, and yours to check.

### 5d. The two migrations

`config.ingest.bearerPort` (§2c) and the `ingest.tls.mode: required`
listener fix (§2d) are both **unreleased** — on `main`, in no tagged
release. They land together the next time the operator moves off a
tagged release. §2c tells you what to do and when; §2d tells you how to
check the precondition. Neither is optional reading.

---

## Quick reference

```bash
# In your recognizer pod, with the ExternalSecret mounted:
# NOTE the port: 9093, not 9091. See 2c.
export URL=http://glovebox-glovebox-ingest.glovebox.svc.cluster.local:9093
export TOKEN=$(cat /var/run/recognizer/glovebox-token)
export SID=recognizer-smoke-test
export ARCHIVE_ID=my-archive-001

# Capability probe. 200 = the bearer listener is up (see 2d).
curl -sS -X OPTIONS "$URL/v1/archives" -H "Tus-Resumable: 1.0.0" -H "Authorization: Bearer $TOKEN"

# Create + upload a small file in one go (for >1 MiB, prefer chunks).
LOCATION=$(curl -sS -i -X POST "$URL/v1/archives" \
  -H "Tus-Resumable: 1.0.0" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Upload-Length: $(stat -c%s archive.mbox)" \
  -H "Upload-Metadata: $META" \
  | awk -F': ' '/^Location:/ { gsub(/\r/,"",$2); print $2 }')

# Use --upload-file, not --data-binary @file: the latter reads the whole
# archive into memory and dies on anything multi-GB.
curl -sS -i -X PATCH "$URL$LOCATION" \
  -H "Tus-Resumable: 1.0.0" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/offset+octet-stream" \
  -H "Upload-Offset: 0" \
  --upload-file archive.mbox

# Read the receipt (by archive_id, NOT by the upload-id in $LOCATION).
curl -sS "$URL/v1/archives/$ARCHIVE_ID" -H "Authorization: Bearer $TOKEN"

# That's it. Glovebox handles the rest.
```

## Reaching out

- **Endpoint up but auth rejecting:** check `kubectl logs deploy/glovebox -n glovebox`
  for `glovebox ingest auth rejected` lines. The `remote_addr` and
  `remote_ip_bucket` fields will tell you if it's coming from the
  expected source.
- **Endpoint down (503):** check
  `kubectl logs deploy/glovebox -n glovebox | grep "archive listener"`.
  Most likely failure: Vault load timed out at boot. Operator
  restart fixes.
- **NetworkPolicy dropping (TCP timeout):** confirm
  `kubectl get ns recognizer -o yaml | grep "name:"` shows
  `name: openclaw-recognizer` — and confirm you are dialling the port
  the policy actually grants (§2b, §2c). After the port split, dialling
  9091 from a correctly-labelled namespace times out exactly like a
  missing label.
- **Connection refused (not a timeout):** the listener isn't there.
  Check `ingest.tls.mode` and the glovebox build (§2d) before assuming
  a restart loop.
- **Finalize returning 500 on a tarball:** check compression (§3f) and,
  for the scanner lane, `ocr.txt` (§3d) — both look like glovebox
  faults on the wire and are not.
- **Anything else:** spec 13 sections 4–7, or file a bead under
  `glovebox-gdp4`'s tree.
