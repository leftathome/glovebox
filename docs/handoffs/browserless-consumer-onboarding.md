# Browserless consumer onboarding (cluster-local headless browser)

> **Audience:** humans + LLMs building services that need a headless
> browser (page rendering, screenshots, automated logins, PDF, etc.).
> Targets: OpenClaw agent tools, browser-session connectors (LinkedIn,
> Meta web flows), and future render-PDF capability.
> **Bead:** [`glovebox-txla`](../../.beads/issues.jsonl).
> **Status of this doc (2026-05-22):** the Browserless service IS
> running in-cluster (verified via `kubectl get deploy -n browserless`,
> 1/1 ready, image `ghcr.io/browserless/chromium:v2.38.1`). What's
> below is current as of the kubectl reads in this session; verify
> with the cluster before depending on specifics.

---

## TL;DR

```yaml
# In your Deployment / Job / CronJob in the openclaw namespace:
env:
  - name: BROWSERLESS_TOKEN
    valueFrom:
      secretKeyRef:
        name: openclaw-secrets
        key: BROWSERLESS_TOKEN
```

Then call either:

- **REST APIs** at `http://browserless.browserless.svc.cluster.local:3000/<path>?token=${BROWSERLESS_TOKEN}`
  (e.g. `/content`, `/screenshot`, `/pdf`).
- **CDP WebSocket** at `ws://browserless.browserless.svc.cluster.local:3000?token=${BROWSERLESS_TOKEN}`
  for full Chrome DevTools Protocol via go-rod, playwright, puppeteer, etc.

Note: **plain `http://` / `ws://`** — the service is ClusterIP-only,
no TLS (`Service.ports.cdp` is port 3000, name `cdp`, no separate
TLS port — verified). The NetworkPolicy at the receiving end is the
isolation boundary; in-cluster traffic stays on the pod network.

---

## What's running

Verified live via `kubectl describe deploy browserless -n browserless`
on 2026-05-22:

| | |
|---|---|
| Image | `ghcr.io/browserless/chromium:v2.38.1` |
| Replicas | 1 (`StrategyType: Recreate`) |
| Resource limits | 2 CPU, 2 GiB memory |
| `MAX_CONCURRENT_SESSIONS` | `8` |
| `MAX_QUEUE_LENGTH` | `16` |
| `CONNECTION_TIMEOUT` | `60000` ms (60s per CDP session) |
| `PREBOOT_CHROME` | `true` (first connect doesn't pay cold-start) |
| Service tier label | `homelab.orac.local/service-tier=2` |

Practical implications:

- **8 concurrent sessions across ALL consumers.** If you plan to open
  4 sessions at once, you've used half the budget. Coordinate with
  other openclaw consumers if you're going to bulk-process.
- **16 queued.** Past that, connections start failing fast rather
  than queueing forever. Build retry-with-backoff into clients that
  burst.
- **60s per session.** Don't open a CDP session and idle on it —
  Browserless will drop it. Open, do work, close.
- **Replicas=1, Recreate strategy.** A node drain or pod restart
  causes a brief outage (no rolling). All in-flight sessions die.
  Consumers should be retry-safe for transient `connection refused`.

---

## Token acquisition

The token is provisioned **once** for the whole cluster, not per
consumer. Verified state:

- **Vault path:** `eso/browserless` (key `BROWSERLESS_TOKEN`).
  Confirmed via `kubectl get externalsecret browserless -n browserless -o yaml`.
- **Two ExternalSecrets project it into K8s Secrets:**
  - `browserless/browserless-secrets` — used by the Browserless pod
    itself to enforce the token on incoming requests.
  - `openclaw/openclaw-secrets` (key `BROWSERLESS_TOKEN`) — used by
    consumers in the `openclaw` namespace. Confirmed via
    `kubectl get externalsecret openclaw-secrets -n openclaw` and
    `kubectl get secret openclaw-secrets -n openclaw` (Opaque,
    9 keys, 57 days old).

**You don't need to provision your own ExternalSecret.** The
`openclaw-secrets` Secret already projects the token into your
namespace — just consume it via `secretKeyRef` or `envFrom`.

If you're in a non-openclaw namespace (see the next section), you'll
need a same-namespace ExternalSecret of your own. Pattern:

```yaml
apiVersion: external-secrets.io/v1beta1
kind: ExternalSecret
metadata:
  name: browserless-token
  namespace: <your-namespace>
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: vault-backend
  target:
    name: browserless-token
    creationPolicy: Owner
  data:
    - secretKey: BROWSERLESS_TOKEN
      remoteRef:
        key: eso/browserless
        property: BROWSERLESS_TOKEN
```

---

## NetworkPolicy gate

**This is the access-control boundary.** Verified via
`kubectl get netpol browserless-allow-consumers -n browserless -o yaml`:

```yaml
ingress:
  - from:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: openclaw
    ports:
      - port: 3000
        protocol: TCP
```

Plus a `browserless-default-deny` policy that drops everything else.

**Practical translation:**

- If your pod runs in `openclaw` namespace → reachable. No extra
  config.
- If your pod runs anywhere else → **TCP connect will hang/timeout**
  (NetworkPolicies drop, they don't reset). Two options:
  - Move your pod to `openclaw` namespace.
  - Open a bead asking to extend `browserless-allow-consumers` to
    your namespace. The policy is Flux-managed (verified labels
    `kustomize.toolkit.fluxcd.io/name=apps`,
    `kustomize.toolkit.fluxcd.io/namespace=flux-system`); change
    goes through whatever repo backs that Flux Kustomization, not
    this glovebox repo.

The `kubernetes.io/metadata.name` label is the kubelet-managed auto-label,
NOT the operator-set `name:` label. So no manual labeling is needed
for any new namespace — the label is always present and equals the
namespace name.

---

## Connecting — concrete recipes

### REST API (simplest path; mirrors the in-cluster probe)

The `openclaw-browserless-probe` CronJob is the reference. Verified
shell extracted from `kubectl get cronjob openclaw-browserless-probe -n openclaw`:

```bash
ENDPOINT="http://browserless.browserless.svc.cluster.local:3000/content?token=${BROWSERLESS_TOKEN}"
curl -fsS -m 60 \
  -H "Content-Type: application/json" \
  -d '{"url":"https://example.com"}' \
  "${ENDPOINT}"
# Returns the rendered HTML of example.com.
```

Other REST endpoints (per Browserless v2 docs — see
[browserless.io/docs](https://docs.browserless.io); not verified
against this specific deployment):

- `POST /content` — rendered HTML.
- `POST /screenshot` — PNG.
- `POST /pdf` — PDF.
- `POST /function` — run arbitrary JS in a page context.
- `POST /scrape` — selector-based extraction.

### Go (go-rod via CDP WebSocket)

This is the pattern the `glovebox-txla` bead description targets for
the future schoology-auth-refresher refactor. **Untested code skeleton;**
verify against go-rod's current docs before shipping. The library
APIs may evolve.

```go
import (
    "github.com/go-rod/rod"
    "github.com/go-rod/rod/lib/launcher"
)

// token from os.Getenv("BROWSERLESS_TOKEN"), set via secretKeyRef.
wsURL := fmt.Sprintf("ws://browserless.browserless.svc.cluster.local:3000?token=%s",
    os.Getenv("BROWSERLESS_TOKEN"))
// Use launcher.MustNewManaged or similar to point at a remote CDP endpoint
// rather than launching a local Chromium. Confirm exact API against the
// version of go-rod in your go.mod.
browser := rod.New().ControlURL(wsURL).MustConnect()
defer browser.MustClose()
page := browser.MustPage("https://example.com")
defer page.MustClose()
html := page.MustHTML()
```

**Open dep:** the existing `connectors/schoology-auth-refresher`
uses upstream `leftathome/schoology-go` (verified in `go.mod`)
which currently exposes `auth.WithBrowserBinary(path)` for a local
Chromium but does **not** expose an equivalent `WithBrowserURL(url)`
for remote CDP. Adding that option is gated on a PR to
`leftathome/schoology-go`. Tracked as a sub-bead of
`glovebox-txla`; not blocking new consumers that don't use schoology-go.

### TypeScript (playwright)

Same caveat — pattern from playwright docs, not verified against
this deployment:

```ts
import { chromium } from 'playwright';
const wsURL = `ws://browserless.browserless.svc.cluster.local:3000?token=${process.env.BROWSERLESS_TOKEN}`;
const browser = await chromium.connect(wsURL);
const page = await browser.newPage();
await page.goto('https://example.com');
const html = await page.content();
await browser.close();
```

### Python (playwright)

```py
from playwright.sync_api import sync_playwright
import os
ws_url = f"ws://browserless.browserless.svc.cluster.local:3000?token={os.environ['BROWSERLESS_TOKEN']}"
with sync_playwright() as p:
    browser = p.chromium.connect(ws_url)
    page = browser.new_page()
    page.goto("https://example.com")
    html = page.content()
    browser.close()
```

---

## Capacity, monitoring, and growth

- **Today:** 8 concurrent sessions, 16 queued, 1 replica. Built for
  light steady-state load + occasional bursts.
- **What happens when you saturate:** new CDP connects sit in the
  queue up to 16; past that, the service returns an error
  immediately. Sessions older than 60s get killed.
- **If your use case needs more:** the bead description says
  "replicas=1 initially; can scale horizontally if contention shows
  up." Two practical paths:
  - **Scale up `MAX_CONCURRENT_SESSIONS`** to 16 or 32 — single
    pod, more parallelism, more memory needed.
  - **Scale replicas** — needs sticky-session handling in clients
    (a CDP session is bound to one pod). Browserless v2 supports a
    `--single-run` mode that pairs well with this; ask in the bead.
- **Monitoring:** the `browserless-allow-monitoring` NetworkPolicy
  is present (verified), so a Prometheus scrape is allowed. Metrics
  endpoint paths weren't verified — check `browserless` v2 docs.

---

## What this doc does NOT cover

- **Promoting glovebox-txla to "done."** That requires the
  schoology-go upstream PR + refresher refactor + spec 06 §12
  addendum. Tracked separately.
- **Stealth / fingerprint workarounds.** If you're hitting an IdP
  that blocks vanilla CDP (LinkedIn, Meta, Cloudflare), you'll need
  to layer rebrowser-style patches OR run Browserless's
  fingerprint-stealth flavour. Out of scope here; file a bead.
- **Cross-namespace access.** If your consumer isn't in `openclaw`
  AND you can't move it there, you need a Flux-side NetworkPolicy
  amendment. File a bead pointing at the cluster-config repo Steve
  manages.
- **Schoology-specific authentication flows.** Wait for the
  `WithBrowserURL` upstream change before refactoring connectors
  that depend on `schoology-go`.

---

## Reaching out

- **Connection refused:** likely outside `openclaw` ns. Check
  `kubectl get pod <yourpod> -o jsonpath='{.metadata.namespace}'`.
- **401 / Unauthorized:** token mismatch. Verify
  `kubectl get secret openclaw-secrets -n openclaw -o jsonpath='{.data.BROWSERLESS_TOKEN}' | base64 -d`
  matches what your pod is reading.
- **Hangs at ~60s:** CDP session exceeded `CONNECTION_TIMEOUT`. Make
  your work faster or open a follow-up bead to bump the timeout.
- **Anything else:** file a bead under `glovebox-txla`.
