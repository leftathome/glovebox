# Glovebox Session Handoff — 2026-06-22

Context for a fresh session. Canonical task state is in beads (`bd list`,
`bd show <id>`); this doc adds narrative + the one live blocker.

## TL;DR

- **Everything is landed except `glovebox-npsj` (the gitlab-first CI pipeline)**,
  which is **blocked on a Docker Hub pull rate limit**, not on code.
- The pipeline + shared template are **built, validated, and correct**. The only
  thing standing between it and a green run is the homelab's Docker Hub auth
  tier. **Do not re-run the pipeline / warm images until that's resolved** —
  hammering on-demand syncs burns the rate limit further (it already happened).

## The one live blocker: `glovebox-npsj` (Docker Hub rate limit)

**Chain (fully diagnosed via kubectl):** gitlab job "pod-start timeout" is really
an image-pull failure. Node image mesh = **spegel** (P2P, serves only
already-cached images) + **zot** (registry-system, on-demand pull-through that
authenticates upstream). zot's Docker Hub credential **is live** (authenticates
as Docker Hub user `leftathome`); small bases synced fine (alpine, debian,
python, busybox, nginx, postgres, redis). But `golang:1.26` is a large
**multi-arch index** (~8 platforms, several GB), and repeatedly re-requesting it
**exhausted `leftathome`'s free-tier authenticated pull limit** → zot now gets
`429 ... pull rate limit as 'leftathome'` even on the golang manifest, so the
sync can't complete. (My own warmer/driver retry churn over ~2h contributed to
burning the limit — lesson: never loop-request a large on-demand sync.)

**Fix is an infra/account decision (pick one):**
1. **Best:** upgrade Docker Hub to **Pro/Team** (raises/removes the authenticated
   pull limit). Free-tier limits make CI base pulls chronically fragile.
2. Wait ~6h for `leftathome`'s limit to reset, then do **one clean pull** (no
   churn). Consider configuring zot to sync **only amd64** for big multi-arch
   images (cuts request volume ~8x; the CI runners only use amd64 nodes anyway).
3. **Most robust:** permanently mirror CI base images (`golang`, `debian`, the
   distroless/kaniko bases) into `registry.orac.local` (like `aether-fm/*`
   already are) so CI never pulls them from Docker Hub.

**Once unblocked, the next-session steps are:**
1. Run ONE fresh pipeline on branch `feat/glovebox-npsj-gitlab-pipeline`
   (`glab api --method POST projects/steve%2Fglovebox/pipeline -f ref=feat/glovebox-npsj-gitlab-pipeline`).
   Watch via `glab` (auth'd). It should flow `test → build-base → build (28 imgs)
   → chart`. The `release` job is `allow_failure: true` (release-cli + private CA).
2. If green: **strip the TEMP feature-branch triggers** from
   `.gitlab-ci.yml` (the `test` job rules + the `.build_rules` anchor each have a
   clearly-commented `feat/glovebox-npsj-gitlab-pipeline` line — remove both),
   then merge the branch to `main` (gitlab first, then github).
3. Verify images + the OCI chart landed in `registry.orac.local`.

**Build/validation already proven:** `glab ci lint` passes; the cross-project
include of the private `homelab/ci-templates` resolves; the johnny `nodeSelector`
applies (pod scheduled to johnny, helper cached). The earlier config bug
(`build-base needs:[test]` while `test` excluded the branch) is fixed.

## Landed this session

- **v0.6.0 released** (gitlab+github), tag on `f7a9f31`. Consolidated everything
  since v0.5.0; cleared the v0.4.x/v0.5.0 tag inversion. CHANGELOG `[0.6.0]`
  finalized.
- **v0.6.0 Helm chart published**: `oci://ghcr.io/leftathome/charts/glovebox:0.6.0`.
  Root cause of the missing chart was `Chart.yaml` pinned at 0.4.4; fixed + the
  CI helm job now derives the chart version from the release tag.
- **Apple export consumer (`glovebox-5lud`)** — full feature on main (merge
  `9ae87dc`), CI green, e2e against the real export (2687 iTunes purchases
  normalized, `data_subject=e_111111`). Caught + fixed an Apple backslash-quote
  CSV bug. `glovebox-mbox-importer:0.6.0` and `glovebox-apple-importer` images
  published to ghcr. All 7 children closed.
- **Schoology batch** (your queue m2sg → mrtl → vkrb → 6ou1; I swapped mrtl
  before vkrb for the hard dependency):
  - `m2sg`: schoology **connector** chart deployment (secret-volume session at
    `/etc/schoology/credentials.json`, checksum auto-roll, per-kid `data_subject`
    rules). Chart bumped to 0.6.1, then 0.6.2 (vkrb).
  - `mrtl`: **schoology-go v0.2.0** published (gitlab+github) — `auth.WithBrowserURL`
    for remote CDP/Browserless. Tag `v0.2.0`.
  - `vkrb`: schoology-auth-refresher → **Browserless** via `WithBrowserURL`,
    Dockerfile back to distroless (−Chromium), chart `BROWSERLESS_URL/TOKEN`,
    spec-06 §12.10 addendum.
  - `6ou1`: schoology row-parse receipt now emits once per poll with the **final
    `affected_count`** (observe-all-then-emit across assignments/feed/messages).
- **Schoology CI images** wired into github CI (`glovebox-schoology`,
  `glovebox-schoology-auth-refresher`) — both published to ghcr.
- **`hyvp`** (mbox per-person `data_subject`) shipped in v0.6.0.
- **Dependabot HIGH fixed**: `go-jose/v4` → 4.1.4.
- **Windows cross-compile fixed**: build-tagged `syscall.Stat_t`/`Statfs`.
- **Private `homelab/ci-templates` project created** (internal/private), tagged
  **v0.1.0**: `.kaniko-build` (kaniko Zot mirror, johnny pin, internal registry,
  CA/ingress workarounds) + `.helm-push`. Holds the homelab infra so public
  mirrors don't leak it.

## Beads status

- **Closed this session:** glovebox-4ypk (prior), -9s60 (prior), -hyvp, -do3z,
  -fslv? (NO — still open), -5lud + its children (ugfx/507b/x2xf/yea7/ot7v/gchj/uj7y),
  -m2sg, -mrtl, -vkrb, -6ou1. schoology-go: WithBrowserURL shipped (v0.2.0).
- **Open / queued (priority order):**
  - `glovebox-npsj` (P1, in_progress) — gitlab-first pipeline; **blocked on Docker
    Hub rate limit** (see above). Branch parked; temp triggers present.
  - `glovebox-i8nd` (P2) — CI image-coverage gap (~14 connectors/importers with no
    image). **Folded into npsj** (its 28-image matrix builds them all). Close when
    npsj merges.
  - `glovebox-fslv` (P2) — govulncheck v1.4.0 segfaults on our generics under
    go1.26; security-scan made non-gating (`continue-on-error`). Re-enable gating
    when x/vuln ships a fix or a working toolchain combo exists.
  - `glovebox-nabc` (P3, deferred) — spec-15 attachment-layout doc; gated on
    walhelm-go's export format (`walhelm-go-ns3`, not yet implemented).
  - `glovebox-0nzk` (P1) — PII/identity scrub: opaque household eids baked as
    DEFAULTS in PUBLIC charts. **NOTE:** my m2sg/apple work added
    `data_subject_default: e_111111` (+ kid placeholders `k1`/`k2`) to the
    schoology-connector and apple-importer chart values. eids are opaque (not
    names), but per 0nzk these should be operator-supplied values, not public
    defaults. Reconcile when addressing 0nzk.
  - Other pre-existing open beads (not this session): -gdp4/-grbi/-3d4m (spec-13
    wireup), -rbpt (scanner fsnotify), -afq4.* (spec-14 follow-ups), -txla/-5o6v
    (Browserless cluster service), -544/-c9zt/-glwf (mbox), assorted P3 ingest
    hardening. Leave for their own efforts.

## Branches, tags, artifacts

- **glovebox `main`**: has v0.6.0 + apple + schoology batch + schoology CI images
  + chart fixes. In sync gitlab+github.
- **`feat/glovebox-npsj-gitlab-pipeline`** (gitlab only): the gitlab-first pipeline
  + 8 ARG-parameterized Dockerfiles (enricher base via `ARG ENRICHER_BASE`,
  ghcr default kept so github is unaffected) + include of `homelab/ci-templates@v0.1.0`.
  **Has TEMP branch triggers to strip before merge.** NOT on github.
- **`homelab/ci-templates`** (gitlab, private): `kaniko.yml` (`.kaniko-build`,
  `.helm-push`, global infra vars). Tag **v0.1.0**.
- **schoology-go** `main` (gitlab+github converged) + tag **v0.2.0**.
- Remotes: glovebox `origin`=gitlab.orac.local, `github`=github (both work now —
  github HTTPS push is fixed). schoology-go: `gitlab`=gitlab.orac.local,
  `origin`=github. **Push gitlab FIRST, then github** (operator directive).

## Cluster / infra notes (kubectl context `admin@orac`)

- Nodes: **johnny + venus = amd64**; orac01-04 = arm64 (4 of 6). The
  gitlab-runner-helper is **amd64-only**, so CI jobs must land on amd64 → the
  template pins `KUBERNETES_NODE_SELECTOR_HOSTNAME=kubernetes.io/hostname=johnny`
  (runner allows node_selector overwrite). arm64 CI capacity would need a second
  arm64-manager runner (gitops; optional, `glovebox-txla`-adjacent).
- Image mesh: **spegel** (P2P node mirror) + **zot** (registry-system, on-demand
  pull-through, authenticates to docker.io/ghcr/quay/k8s/registry.orac.local).
  `KUBERNETES_POLL_TIMEOUT` is NOT a CI-variable override (config.toml only).
- kaniko bypasses host containerd, so it must be told to use zot explicitly →
  `--registry-mirror`/`--insecure-registry` (in the template, gitops-recommended).
- The cluster CA is mounted into job containers (SSL_CERT_FILE etc. via the
  runner config), but kaniko/helm still use the internal HTTP registry Service to
  dodge the Traefik ingress timeout on large blobs.

## Conventions / gotchas for the next session

- **Don't put homelab infra in public-mirrored files** — node names, `.svc`
  URLs, CA flags, registry endpoints go in the private `homelab/ci-templates`.
- **Don't loop-request large zot on-demand syncs** — it burns the Docker Hub
  rate limit. One demand, wait.
- **`.beads/` is untracked** (gitignored) — it carried PII; canonical state is
  Dolt. `bd` auto-export shows a harmless "git add failed" warning; ignore it.
- gitlab-first push order; github push works again.

## Cleanup / security TODOs (flagged, not done)

- **Rotate `.beads/.beads-credential-key`** — a 32-byte key was tracked in git
  history (public github) before `.beads/` was untracked.
- `.gitignore` has bare `trello`/`hackernews` entries that shadow
  `connectors/trello` — tidy when convenient.
- `glovebox-0nzk` PII reconcile (see beads note above; includes my e_111111
  chart defaults).
- Dolt: `bd dolt push` if you sync beads to a Dolt remote (separate from git).
