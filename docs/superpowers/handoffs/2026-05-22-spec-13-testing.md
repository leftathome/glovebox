# Spec 13 — Archive Delivery: Testing & Deployment Handoff

> **For the next session.** Implementation is complete and on `main` at `50ae6d4`.
> This document is the playbook for taking it from green-locally to green-in-cluster.

**Status snapshot (2026-05-22):**

| | |
|---|---|
| Spec | `docs/specs/13-archive-delivery-design.md` (v1.0) + `docs/specs/10-external-ingest-auth-design.md` (v1.0) |
| Plan | `docs/superpowers/plans/2026-05-21-spec-13-archive-delivery.md` (all 15 tasks closed) |
| Merge | `50ae6d4` — `git merge --no-ff` from `chore/beads-glovebox-p1zx` |
| Branch backup | `chore/beads-glovebox-p1zx` pushed to gitlab at `9fea5cb` |
| Local gates | `go vet`, full `go test`, `helm lint`, integration_test.go — all green |
| Cluster-side | Untouched. No deployment yet. Acceptance criterion (12 GiB mbox + multi-GiB subtree without 413) NOT yet exercised. |

---

## What the next session should NOT redo

These are done. Skip them.

- The Go implementation. `internal/ingest/auth/` and `internal/ingest/archives/` are complete with integration tests that exercise the full tus.io flow against `httptest.NewServer`.
- The Helm chart wiring. `charts/glovebox/templates/{archive-networkpolicy,archive-tokens-externalsecret,archive-pvc-tmp,archive-pvc-archives,configmap}.yaml` render correctly under `--set ingest.auth.enabled=true --set ingest.archives.enabled=true`.
- The smoke-test script. `scripts/archive-smoke-test.sh` Phase 1 (12 GiB mbox) and Phase 2 (2 GiB tarball, `archive/google-takeout-subtree` media type) are written and ready to run.
- The security review. Final review approved against `main` post-merge; all 18 iteration-1 findings verified implemented.

---

## What still needs doing, in order

### 1. Stand up the deployment preconditions

These are operator tasks Steve owns; they can be done in any order but ALL must be true before flipping `ingest.archives.enabled=true`.

**Vault:**
- [ ] Mount KV v2 at `secret/` (likely already done from spec 06/12).
- [ ] Create per-source paths: `secret/glovebox/ingest-tokens/<source-id>` with `{token: "<64-char-lowercase-hex>"}`. Generate with `openssl rand -hex 32`. Spec 10 §3.2 defines source-id format (`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`, ≤64 chars).
- [ ] Create K8s auth role `glovebox-ingest` bound to ServiceAccount `glovebox` in the glovebox namespace, with a read policy on `secret/data/glovebox/ingest-tokens/*` and list on `secret/metadata/glovebox/ingest-tokens`.

**ESO:**
- [ ] A `ClusterSecretStore` already exists from spec 12 (default name `vault-default`). Re-use it; no new store needed.

**K8s:**
- [ ] Confirm `kubernetes.io/storage-class` for `persistence.tmpArchives` and `persistence.archives` resolves to a backend where `os.Rename` between the two PVCs lands on the same device. **Spec 13 §3.4 + the listener's `st_dev` startup check will 503 the entire archive surface if the two PVCs end up on different devices.** Easiest pattern: pre-provision both PVCs against the same NFS/CSI export, or use one large PVC with sub-paths. The chart defaults to two separate PVCs assuming the operator will pin them to the same storage class.
- [ ] Pick the recognizer namespace's `name:` label value and set `ingest.archives.networkPolicy.recognizerNamespaceLabel` in your values overlay. Default placeholder is `openclaw-recognizer`. The chart's NetworkPolicy matches on `name: <this-value>` per spec 13 §8.1 — NOT the kubelet-managed `kubernetes.io/metadata.name`.

### 2. Render and review the chart

Before deploying:

```bash
helm template charts/glovebox/ \
  --set ingest.auth.enabled=true \
  --set ingest.archives.enabled=true \
  --set ingest.archives.networkPolicy.recognizerNamespaceLabel=<your-value> \
  > /tmp/spec-13-render.yaml
```

Eyeball:
- The `ExternalSecret` resolves to the right Vault path and `refreshInterval: 1m`.
- The `NetworkPolicy` allows ingress on port 9091 (tus) from the recognizer-namespace selector AND egress to Vault.
- The `ConfigMap` for `config.json` carries the `ingest.archives.*` block with `perSourceSoftCapPct: 40`, `globalHardCapPct: 95`, `globalHardCapHysteresisPct: 85`, and no Helm-only `enabled` keys leaking through.
- Both PVCs reference a storage class you trust.

### 3. Deploy to a staging cluster

```bash
helm upgrade --install glovebox charts/glovebox/ \
  -n glovebox --create-namespace \
  -f values-staging.yaml
```

Where `values-staging.yaml` enables `ingest.auth` + `ingest.archives` and sets the recognizer namespace label. **Do NOT enable in prod yet.**

Verify post-deploy:
- [ ] `kubectl logs deploy/glovebox` shows `glovebox ingest token store reloaded` with the expected source-id count, AND `glovebox archive listener st_dev check ok` (NOT the 503-fallback path).
- [ ] `kubectl exec deploy/glovebox -- ls -ld /data/glovebox/tmp-archives /data/glovebox/archives` shows mode `0700` for both, fsGroup matching the runAsUser.
- [ ] `kubectl exec deploy/glovebox -- stat -c %d /data/glovebox/tmp-archives /data/glovebox/archives` returns identical device IDs. If not, fix storage class before continuing — atomic rename won't work.
- [ ] `curl -sS -H "Authorization: Bearer <token>" http://glovebox.glovebox.svc.cluster.local:9091/v1/archives` returns `405` (method not allowed on collection root with GET — expected).
- [ ] `curl -sS http://glovebox.glovebox.svc.cluster.local:9091/v1/archives -X POST` (no auth) returns `401` with empty body. Repeat with a wrong token — assert the 401 body is byte-identical (integration_test.go scenario 9 covers this locally; verify in-cluster too).

### 4. Run the smoke test against the deployed instance

`scripts/archive-smoke-test.sh` is the bead's acceptance test, driven from outside the cluster as the recognizer would. It needs:
- `GLOVEBOX_INGEST_URL` — e.g. `https://glovebox.example.lan` (through ingress).
- `GLOVEBOX_INGEST_TOKEN` — one of the source-id tokens from Vault.
- `GLOVEBOX_INGEST_SOURCE_ID` — the source-id paired with that token.
- `~6 GiB` of free disk on the runner (creates a 12 GiB sparse mbox + 2 GiB sparse tarball).

```bash
GLOVEBOX_INGEST_URL=https://... \
GLOVEBOX_INGEST_TOKEN=... \
GLOVEBOX_INGEST_SOURCE_ID=... \
bash scripts/archive-smoke-test.sh
```

Pass criteria (per `glovebox-p1zx` acceptance):
- Phase 1: 12 GiB mbox uploaded in resumable chunks, finalizes to `/data/glovebox/archives/<id>/archive.mbox` + `metadata.json` + `receipt.json`. No 413 anywhere in the response stream.
- Phase 2: 2 GiB tarball uploaded, untars under `archives/<id>/extracted/`, every entry recognized by `internal/ingest/archives/untar.go`. No 413; tar-safety rejections logged at INFO if test entries hit them.

**If a chunk fails mid-upload, deliberately kill the curl, then re-invoke the script with the same upload-id env var to test the HEAD-then-PATCH resume path. The script supports this via `RESUME_FROM_ID=<id>`.**

### 5. Observe telemetry

While the smoke test runs, watch:

- `kubectl port-forward deploy/glovebox 9090` then `curl localhost:9090/metrics | grep glovebox_archive`.
- The 15 instruments named in `internal/ingest/archives/telemetry.go` should all show non-zero values for the events that fired (upload_created, patch_bytes_total, finalize_success/failure, etc).
- `glovebox_ingest_token_load_errors_total` should be 0 unless a malformed source-id slipped into Vault.
- `glovebox_archive_quota_state` should be 0 (under hysteresis) for the duration; if it goes to 1, you've over-provisioned the test.

### 6. Hand off to the recognizer team

Once smoke tests pass, the recognizer team needs:
- The endpoint URL.
- A source-id and token (rotate to a recognizer-owned token via Vault; don't reuse the smoke-test one).
- A pointer to spec 13 §4 (endpoint contract) — they're the consumer.
- The four media types currently accepted (spec 13 §4.2.1): `archive/mbox`, `archive/google-takeout-subtree`, `archive/imap-export`, `archive/generic-tarball`. If they need a fifth, that's a spec amendment.

---

## Followup beads to schedule alongside testing

These were filed by the final security review and are non-blocking, but the next session should fold them into a "Wave E followups" batch if there's time:

| Bead | What | Priority |
|---|---|---|
| (filed via `bd create` 2026-05-22) | `.semgrep/auth-leakage.yml` rule banning `r.Header.Get("Authorization")` outside `internal/ingest/auth/` | P3 |
| (same) | `hasControlChar` permits `\t` in `subtree_relative_path` — spec 13 §144 contract drift, not exploitable | P3 |
| (same) | `ClassifyReason` is too coarse — thread typed `*TarError` for finer metric labels | P3 |
| (same) | `Tus-Expires` vs `cleanup.TmpAge` config-drift footgun — collapse to one config field or assert invariant | P3 |

Plus the pre-existing carryovers:
- `glovebox-6ou1` — receipt `affected_count` timing, cross-cutting (P3).
- `glovebox-txla` — Browserless cluster service (P2, gated on a second consumer materializing).
- `glovebox-c9zt` — spec 09 mbox-importer watcher mode. Relevant here: the archive endpoint deposits mboxes into `archives/<id>/archive.mbox` and emits a `delivered_by` provenance entry; the watcher mode bead will read those mboxes and feed them into the scanner pipeline. Spec 13 explicitly does NOT handle scanning — that's the watcher's job. Don't ship spec 13 to recognizer without confirming the downstream watcher path is on the roadmap.

---

## Failure modes to deliberately exercise

The integration test covers these locally; in-cluster smoke-testing should also exercise:

- [ ] **Token rotation.** Update the Vault entry for the smoke-test source-id mid-upload (between PATCHes). The upload should complete (in-flight tokens stay valid until the reload swap), and the next POST/HEAD with the OLD token should 401. Spec 10 §4.1 documents that in-flight uploads complete under a revoked token; full containment requires `kubectl rollout restart`.
- [ ] **Storage exhaustion.** Pre-fill `/data/glovebox/archives` to 96% of the PVC. The next POST should 503 with `quota_exhausted`. Then truncate and watch for the 85% hysteresis lift.
- [ ] **Concurrent upload cap.** Open 5 concurrent uploads as one source-id. The 5th POST should 429 with `per_source_concurrent_cap`.
- [ ] **Slowloris.** Open a POST, send the first byte, then idle for >5 min. The PATCH should time out and the upload-id should become reusable.
- [ ] **DELETE race.** Open a PATCH and a DELETE concurrently for the same upload-id. One must win; the other must 409 or 404. Spec 13 §4.6 + handler_test.go cover this locally.
- [ ] **Tar slip.** Submit a tarball with `../etc/passwd` as an entry path. The finalize should return 422 with `tar_unsafe_entry`. (Don't try to actually escape — the allow-list will reject it; this is a positive-control test that the rejection fires.)
- [ ] **Resume across pod restart.** Start an upload, `kubectl rollout restart` glovebox mid-upload, then HEAD the upload-id from outside. The PVC is RWO, so the new pod should see the same `.tmp-archives/<id>/` state; HEAD should return the last-known offset. PATCH-resume should pick up cleanly.

---

## Session-open checklist for whoever picks this up

```bash
# 1. Confirm where main is.
git log --oneline main -5

# 2. Verify nothing has drifted post-merge.
go vet ./...
go test ./... -count=1

# 3. Re-render the chart with the staging values.
helm template charts/glovebox/ -f values-staging.yaml | less

# 4. Read this handoff doc.
# 5. Read scripts/archive-smoke-test.sh and confirm the env vars are set.
# 6. Run the smoke test.
```

If anything in step 2 has gone red since 2026-05-22, **do not skip past it** — investigate. Spec 13 ships with race-detector-clean tests; a sudden race failure means something landed on `main` that needs untangling first.

---

## Quick reference — file paths

| What | Where |
|---|---|
| Spec 13 | `docs/specs/13-archive-delivery-design.md` |
| Spec 10 | `docs/specs/10-external-ingest-auth-design.md` |
| Plan | `docs/superpowers/plans/2026-05-21-spec-13-archive-delivery.md` |
| Auth package | `internal/ingest/auth/` |
| Archive package | `internal/ingest/archives/` |
| Identity helpers | `internal/ingest/audit_provenance.go` |
| Chart additions | `charts/glovebox/templates/archive-*.yaml` + `configmap.yaml` |
| Smoke test | `scripts/archive-smoke-test.sh` |
| Bead memory | `bd memories spec-13` |
