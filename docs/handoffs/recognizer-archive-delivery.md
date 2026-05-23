# Glovebox archive-delivery: recognizer-team handoff

> **Audience:** humans + LLMs on the recognizer team building the
> client that ships large archives (mboxes, Google Takeout subtrees)
> to glovebox.
> **You don't need to read:** spec 13, spec 10, or any of the glovebox
> source. Everything you need is here.
> **Authoritative source:** if anything in this doc disagrees with
> [`docs/specs/13-archive-delivery-design.md`](../specs/13-archive-delivery-design.md),
> the spec wins. File an issue if you spot a discrepancy.

This is the document the umbrella bead `glovebox-gdp4` Phase 7 calls
out. Its four jobs:

1. **Token acquisition** — where your bearer token lives and how to
   get it into your pod.
2. **Endpoint address** — the in-cluster URL and the namespace label
   you need on the recognizer namespace.
3. **API contract** — `curl` recipes for every state transition, the
   metadata you have to supply, the four accepted media types.
4. **Completion signal** — what to do after a successful upload
   (spoiler: nothing).

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
          value: "http://glovebox-glovebox-ingest.glovebox.svc.cluster.local:9091"
```

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
http://glovebox-glovebox-ingest.glovebox.svc.cluster.local:9091
```

Service-name shape is `{release}-glovebox-ingest`. The current deploy
uses release name `glovebox` in namespace `glovebox`, so the
double-`glovebox-glovebox-` is correct (verified via `kubectl get svc -n glovebox`).
If the operator ever re-installs under a different release name,
update the host segment.

Routes mounted on that port:

| Path | Method | Purpose |
|---|---|---|
| `/v1/archives` | OPTIONS | tus.io capability discovery (read `Tus-Version`, `Tus-Max-Size`, etc.) |
| `/v1/archives` | POST | Create an upload; returns `Location: /v1/archives/<id>` |
| `/v1/archives/<id>` | HEAD | Probe upload state for resume |
| `/v1/archives/<id>` | PATCH | Stream bytes (multiple PATCHes allowed) |
| `/v1/archives/<id>` | GET | Inspect finalize state (post-completion) |
| `/v1/archives/<id>` | DELETE | Abandon an in-progress upload |

`/v1/ingest` is the legacy single-file connector path (spec 08, not
spec 13). Don't use it for archives.

### 2b. NetworkPolicy expectation

Glovebox renders a NetworkPolicy that **only** accepts traffic on port
9091 from namespaces carrying the label:

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

### 2c. Outside the cluster

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

Per spec 13 §4.2. Every key is base64-encoded (`StdEncoding`, NOT
URL-safe). The header format is:

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

**Reserved keys you cannot set:** `delivered_by`, `delivered_at`.
Glovebox writes those at finalize from the auth-resolved `source_id`
and the wall-clock. Including either in your `Upload-Metadata`
returns `400 metadata_reserved_key`.

The whole `Upload-Metadata` header is capped at **4 KiB**. Exceeding
returns `431 metadata_too_long`.

### 3b. Accepted media types

```
archive/mbox                      (raw)   — one mbox file, ships as-is.
archive/google-takeout-subtree    (tar)   — uncompressed tarball (no .tar.gz / .tar.zst); glovebox untars.
archive/imap-export               (raw)   — one mbox-shaped IMAP dump.
archive/generic-tarball           (tar)   — uncompressed tarball, no Takeout semantics.
```

**Tarballs MUST be uncompressed in v1** — spec 13 §"Out of scope and
deferred" §"Server-side compression" is explicit. The finalize path
feeds the body straight into `archive/tar.NewReader` with no gzip
wrapper; a `.tar.gz` upload fails with
`untar: tar read: archive/tar: invalid tar header` at finalize.
Recognizer must emit plain `.tar` (or expose decompression as a
separate spec-amendment bead with a decompression-bomb defense
budget).

Anything else returns `400 unknown_media_type`. Adding a fifth media
type — or adding gzip/zstd support to existing tarball media types —
requires a code change in glovebox; ask Steve to file a spec
amendment.

### 3c. Recipes

These assume `$TOKEN`, `$URL`, and `$SID` are set from §1c. The
metadata header is hand-built to keep the recipe legible; in a real
client use a tus.io library.

**Helper for metadata base64-encoding:**

```bash
b64() { printf '%s' "$1" | base64 -w0; }
META="archive_id $(b64 my-archive-001)\
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
# Returns 204 with Tus-Version, Tus-Max-Size, Tus-Extension headers.
# Use this to discover the per-upload size cap (30 GiB by default).
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
- `303` — replay; an upload with the same `archive_id`+`sha256` from
  this source-id is already in progress or finalized. Use the
  `Location` to inspect.
- `400` — metadata validation failed (`metadata_missing`,
  `metadata_invalid`, `unknown_media_type`, `size_mismatch`).
- `401` — bad/missing `Authorization`. The 401 body is byte-identical
  for "no header", "wrong format", "valid format but unknown token";
  don't try to distinguish.
- `409` — `archive_id` is in use by another source-id (rare;
  collision).
- `413` — `Upload-Length` exceeds `Tus-Max-Size`. **You should never
  see this** since glovebox is configured for 30 GiB by default; if
  you do, the operator under-provisioned the cap.
- **OOM mid-upload** (not an HTTP code — the pod restarts, your in-flight
  PATCHes see `connection refused`). The glovebox pod runs with a hard
  memory cap; current default is 2 GiB (chart 0.4.2). Multi-GiB uploads
  pass cleanly within that envelope, but a node-level memory squeeze or
  an unrelated runaway can still get the pod evicted. Retry semantics:
  HEAD the upload-id; if it still exists in `.tmp-archives/` (RWO PVC
  survives pod restart) the offset is recoverable and PATCH-resume picks
  up cleanly. If HEAD returns 404 the in-flight state was lost — POST a
  fresh upload. Tracked under `glovebox-5ud9` (chart bump) and the
  pprof / streaming-audit followup bead.
- `429` — rate-limited (your IP or a global backstop). Honor
  `Retry-After`.
- `503` — archive listener unavailable (Vault load failed, st_dev
  check failed). Honor `Retry-After: 60`; rotate via operator-led
  pod restart.

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
  Mismatch returns `409 offset_mismatch`; HEAD to get the right value.
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
# Cache-Control: no-store
```

Resume from `Upload-Offset`. If you get `404`, the upload was either
cleaned up (72h tmp expiry) or never existed; POST a new one.

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
curl -i "$URL$UPLOAD_URL" \
  -H "Authorization: Bearer $TOKEN"
# 200 OK if finalized
# Body: {"archive_id":"...", "state":"finalized", "receipt":{...}}
# 404 if cleaned up / never existed
```

You typically don't need this; the final PATCH's response carries the
same info.

### 3d. Finalize error codes (in the final PATCH's response body)

The final PATCH may return 4xx with a JSON body:

```json
{"error":"<code>","message":"<human-readable>"}
```

Codes you'll see:

| Code | Status | What it means | What you should do |
|---|---|---|---|
| `sha256_mismatch` | 400 | Computed sha256 disagrees with declared `sha256` metadata. | Re-upload from scratch; the bytes corrupted in flight or your computation was wrong. |
| `tar_unsafe_entry` | 422 | Tarball contained `..`, abs paths, symlinks, etc. | Fix your tarball; spec 13 §4.7 allow-list is strict. |
| `tar_unsupported_entry` | 422 | Entry type other than regular file or dir. | Same. |
| `quota_exhausted` | 503 | PVC is at the hard cap. | Back off; honor `Retry-After: 60`. |

A 503 mid-upload is the only retry-worthy code; everything else is
"fix something and try again."

---

## 4. Completion signal

**You do not need to do anything after a successful final PATCH.**

Once finalize returns 2xx, glovebox has:

1. Written `archives/<archive_id>/<archive_filename>` (or
   `archives/<archive_id>/extracted/` for tarballs).
2. Written `archives/<archive_id>/metadata.json` carrying every
   `Upload-Metadata` key + the server-set `delivered_by` (your
   source-id) and `delivered_at`.
3. Written `archives/<archive_id>/receipt.json` carrying the final
   sha256, byte counts, finalize timestamp.

The spec-9 mbox-importer watcher (bead `glovebox-c9zt`) picks the
`archives/<id>/` directory up from there and feeds its contents into
the scanner pipeline. It learns about new archives via filesystem
watch on `archives/`; no extra signal is required from you.

**Do NOT** add a "I just finished, please scan" webhook/ping. There
is no such endpoint, and adding one would duplicate the watcher's
job. If the watcher isn't picking up your archives, that's a bug in
spec-9 land — file it as a new bead, don't paper over with a side
channel.

---

## Quick reference

```bash
# In your recognizer pod, with the ExternalSecret mounted:
export URL=http://glovebox-glovebox-ingest.glovebox.svc.cluster.local:9091
export TOKEN=$(cat /var/run/recognizer/glovebox-token)
export SID=recognizer-smoke-test

# Capability probe.
curl -sS -X OPTIONS "$URL/v1/archives" -H "Tus-Resumable: 1.0.0" -H "Authorization: Bearer $TOKEN"

# Create + upload a small file in one go (for >1 MiB, prefer chunks).
LOCATION=$(curl -sS -i -X POST "$URL/v1/archives" \
  -H "Tus-Resumable: 1.0.0" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Upload-Length: $(stat -c%s archive.mbox)" \
  -H "Upload-Metadata: $META" \
  | awk -F': ' '/^Location:/ { gsub(/\r/,"",$2); print $2 }')

curl -sS -i -X PATCH "$URL$LOCATION" \
  -H "Tus-Resumable: 1.0.0" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/offset+octet-stream" \
  -H "Upload-Offset: 0" \
  --data-binary @archive.mbox

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
- **NetworkPolicy dropping:** confirm `kubectl get ns recognizer -o yaml | grep "name:"`
  shows `name: openclaw-recognizer`.
- **Anything else:** spec 13 sections 4–7, or file a bead under
  `glovebox-gdp4`'s tree.
