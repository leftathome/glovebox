# Upgrading glovebox

`CHANGELOG.md` records every change and why it was made. This document is the
short version an operator needs *before* running `helm upgrade`: the things
that will break if nobody looks at them, and what to do about each one.

If you read nothing else, read [Before you upgrade](#before-you-upgrade). It is
four checks, and each one is a five-minute job that is much less pleasant to do
at 02:00 with uploads failing.

---

## Upgrading to 0.12.0 (from 0.11.0)

- **Helm's wait can be turned back on.** A PowerSchool connector waiting
  for a new session used to be NotReady, which made `helm upgrade --wait`
  and Flux's upgrade and rollback time out (glovebox-wekc.17). It is now
  Ready while it waits; the state is in `powerschool_session_state`,
  `/statusz` and the new `GloveboxPowerSchoolWaitingForSession` alert. If
  you disabled the wait to work around it (Flux HelmRelease
  `spec.upgrade.disableWait` / `spec.rollback.disableWait`), remove those
  after this release's images are running, so the deploy health check
  comes back. Re-enable it only once the connector pod runs the new image:
  an old image still waiting would still be NotReady. Pair it with a sane
  `spec.timeout` -- long enough for your slowest pod to pull and start,
  short enough that a real failure still fails the upgrade promptly.
- **What Helm's wait no longer catches for PowerSchool.** The connector is
  now Ready as soon as it has started (`ReadyWhenServing`), not after its
  first poll. A connector whose catch-up poll fails -- a wrong ingest URL, a
  parser broken by a portal change -- stays Ready, so `helm upgrade --wait`
  passes. Those failures show up in the log, `connector_errors_total`,
  `powerschool_polls_total` and the existing poll alerts, not in the
  rollout. (Most of its polls are skipped by design anyway: three windows
  on weekdays.) Invalid config still exits at start and fails the rollout.
  Schoology keeps its old readiness (ready after the first successful
  poll).
- No chart value has to change. `metrics.prometheusRule.sessionWaitingHours`
  (default 60) and `metrics.prometheusRule.schoologySessionWaitingHours`
  (default 14) are new; with `metrics.prometheusRule.enabled` the
  PrometheusRule gains a group `glovebox-powerschool-connector-session`
  (PowerSchool connector enabled, any login mode) and a group
  `glovebox-schoology-connector-session` (Schoology connector enabled).
- Both connectors keep a small `session-state` file in their state
  directory: the reported session state and when it began.

### One ServiceAccount per workload (glovebox-wekc.19)

No chart value has to be added or changed, and no Vault or gitops change has
to land first. What changes in the render:

- **Every enabled connector runs as a ServiceAccount of its own**,
  `<fullname>-connector-<name>` (`glovebox-glovebox-connector-rss`,
  `-connector-powerschool`, `-connector-schoology` for a release named
  `glovebox`), rendered by the chart with `automountServiceAccountToken:
  false`, and every connector pod states `automountServiceAccountToken:
  false` too. Until now the connectors ran as the core's account,
  `<fullname>`. Each enabled importer Job likewise gets
  `<fullname>-importer-<name>`, with no token.
- **The core keeps `<fullname>`** (or `serviceAccount.name`), now used by
  the core alone. Its pod states `automountServiceAccountToken` explicitly:
  `true` only with `ingest.auth.enabled`, which is the one thing in the core
  that needs the token (the archive listener's Vault Kubernetes login).
- **The PowerSchool connector's account is renamed** from
  `<fullname>-powerschool` (rendered only with `loginOnDemand` or
  `loginBroker`) to `<fullname>-connector-powerschool`. The chart moves the
  on-demand RoleBinding and the login broker's allowed caller with it in the
  same render.
- Per-connector override, same shape as the core's:
  `connectors.<name>.serviceAccount.{create,name,annotations}`,
  `schoologyConnector.serviceAccount`, `importers.<name>.serviceAccount`.
- **The render refuses two workloads on one account**: the core, every
  enabled connector, importer and login broker, and the two refresher
  CronJobs must resolve to distinct names, whether or not the chart creates
  the object. A values file that pointed a connector at the core's account
  (`serviceAccount.name: glovebox-glovebox`) now fails with a message naming
  both workloads.

**What it costs.** Every connector Deployment rolls once (its pod template
changes), and so does the core (the explicit `automountServiceAccountToken`).
With three connectors enabled, as in the `orac` `openclaw` release, that is
four pods. Nothing is lost: connectors keep their state PVCs. The PowerSchool connector
is the one that matters -- let it roll outside its poll windows.

**Who uses an in-cluster token, after the upgrade:**

| Workload | Account | Token | Why |
| --- | --- | --- | --- |
| core | `<fullname>` | API token only with `ingest.auth.enabled` | Vault Kubernetes login (`ingest.auth.vault.k8sRole`) |
| every connector | `<fullname>-connector-<name>` | none | -- |
| PowerSchool connector, `loginOnDemand` | `<fullname>-connector-powerschool` | API token | get/update/patch on the one login-request ConfigMap (Role bound to this account only) |
| PowerSchool connector, `loginBroker` | `<fullname>-connector-powerschool` | projected, audience = the broker's, 600 s; no API token | presented to the broker, which allow-lists this account |
| login broker | `<fullname>-login-broker-<name>` | API token | TokenReview of callers; Vault Kubernetes login |
| PowerSchool login CronJobs | `<fullname>-powerschool-auth-refresher` | API token | Vault Kubernetes login; with `onRequest`, get on the request ConfigMap |
| Schoology refresher CronJob | `<fullname>-schoology-auth-refresher` | API token | Vault Kubernetes login |
| importers | `<fullname>-importer-<name>` | none | -- |

The three CronJob/broker accounts are unchanged by this release.

**Vault.** Every Kubernetes-auth role the chart's workloads use:

| Role | Bound to | Used by | Change |
| --- | --- | --- | --- |
| `glovebox-ingest` (`ingest.auth.vault.k8sRole`) | `glovebox-glovebox` in `glovebox` | the core's archive listener | none: the core keeps the name |
| `glovebox-powerschool-refresher` | `glovebox-glovebox-powerschool-auth-refresher` in `openclaw` | PowerSchool login CronJobs | none |
| `glovebox-schoology-refresher` | `glovebox-glovebox-schoology-auth-refresher` in `openclaw` | Schoology refresher CronJob | none |
| `loginBrokers.<name>.vault.k8sRole` | `<fullname>-login-broker-<name>` | login broker | none |

No connector or importer authenticates to Vault; their secrets reach them
through ESO, under ESO's own role. So nothing bound to the old shared
account breaks, and gitops does not have to merge first. **Check your own
Vault** for any role bound to `<fullname>` that is not `glovebox-ingest` --
anything outside the chart that relied on a connector holding the core's
token stops working with this release; bind it to the connector's own
account instead.

`glovebox-ingest` exists in the `orac` Vault but was created by hand from
`docs/runbooks/vault-spec-13-provisioning.md`; it is not in gitops'
`clusters/orac/platform/vault/config`. Bringing it under the configurator is
optional and independent of this upgrade (same values as the runbook):

- `policies/glovebox-ingest-read.hcl`:

  ```hcl
  path "secret/data/glovebox/ingest-tokens/*" {
    capabilities = ["read"]
  }
  path "secret/metadata/glovebox/ingest-tokens" {
    capabilities = ["list"]
  }
  ```

- `kustomization.yaml`, under the `vault-policies` configMapGenerator:
  `- policies/glovebox-ingest-read.hcl`
- `scripts/configure.sh`, beside the other glovebox roles:

  ```bash
  # Archive/ingest listener of the glovebox-namespace release: the core's own
  # ServiceAccount (no connector runs as it from glovebox-wekc.19 on).
  vault write auth/kubernetes/role/glovebox-ingest \
    bound_service_account_names=glovebox-glovebox \
    bound_service_account_namespaces=glovebox \
    policies=glovebox-ingest-read \
    ttl=24h
  ```

**Check anything outside the chart that names an account:** a Vault role, a
RoleBinding, `loginBrokers.<name>.extraAllowedCallers`, or a policy engine
that selects pods by ServiceAccount (the chart's own NetworkPolicies select
by labels, which are unchanged). If you set `serviceAccount.name`, note that
it now names the core's account only.

**Verify after the rollout** (namespace `openclaw` in the `orac` install):

```bash
kubectl -n openclaw get pods -l app.kubernetes.io/instance=glovebox \
  -o custom-columns=POD:.metadata.name,SA:.spec.serviceAccountName,TOKEN:.spec.automountServiceAccountToken
kubectl auth can-i --list -n openclaw \
  --as=system:serviceaccount:openclaw:glovebox-glovebox-connector-rss
```

Each connector pod should show its own account and `false`; the RSS
connector's account should be allowed nothing beyond what every
authenticated identity gets.

---

## Upgrading to 0.11.0 (from 0.10.3)

No required changes: no chart value has to be added or changed, and a
0.10.3 values file renders with 0.11.0. With nothing new enabled the only
rendered difference besides the chart version label is a set of labels on
the Jobs the PowerSchool login CronJobs create
(`glovebox.io/powerschool-login=true` among them). Everything below
concerns the PowerSchool connector and its login workload; if you run
neither, there is nothing to read.

If your values pin image tags, the behaviour described here arrives with
the 0.11.0 `glovebox-powerschool` and `glovebox-powerschool-auth-refresher`
images, not with the chart. The 0.11.0 chart renders the same workloads for
0.10.3 images.

- **The fix this release is for: a replaced session is picked up, and a
  dead one is recognised.** Up to 0.10.3 the connector read its session
  file once, at start. A session written later by the scheduled login job
  was not used until the pod restarted, and the portal's answer for the
  session it had ended was classified `error_class=parse` ("could not
  select and confirm this kid's student"), not `auth`, so the
  rejected-session path was never taken. Seen in production on 2026-10-12:
  login at 05:52, the poll at 06:05 failed. From 0.11.0 the connector
  re-reads the session file before every poll and adopts a changed session
  without a restart, then makes one GET of the home page
  (`/guardian/home.html`) before any child is read:
  - live: the poll runs;
  - dead (the portal's sign-in page or the access gateway's logout page,
    with status 200): in the default mode the session is marked rejected
    and the process exits with a permanent error (`error_class=auth`); the
    restarted process waits, sending nothing to the portal, until the
    session file holds a different session;
  - anything else is unknown: the poll is skipped and nothing is changed
    (`powerschool_polls_skipped_total{reason}`).

  This is one extra request per poll with the account's session.

- **The scheduled login job behaves differently, with nothing new
  enabled.**
  - It writes an attempt marker (`attempt_started_at`) to its Vault state
    record before each login and clears it when the outcome is recorded.
  - A run that finds a marker under ten minutes old does not log in:
    `login_in_progress`, exit 0.
  - A run that finds an older marker -- the earlier run was killed in the
    middle of a login (node drain, OOM, the Job's deadline) -- counts that
    login as an unconfirmed one. **Two unconfirmed logins in a row set the
    latch** (`blocked_reason=login_unconfirmed`; every later run exits 6
    without starting a browser). Before, a killed run left no trace. The
    count is the one an ordinary `login_unconfirmed` run (exit 5) adds to,
    and only a confirmed login resets it.
  - Every write of the state record is check-and-set. A run that finds the
    record changed under it does not log in and exits 8
    (`login_state_conflict`); the Job shows as failed. If you alert on
    failed login Jobs, this is a new way to get one.
  - Each login is counted into `login_day` / `login_day_count`. The
    scheduled job only counts; it is not stopped by the daily budget, which
    the login broker applies to itself.
  - **Clearing the latch** is done as before. Find out why it was set first
    (`blocked_reason` in the state record; sign in to the portal in a
    browser and see what it asks for). Then, with an operator token (the
    job's policy cannot delete), run
    `vault kv metadata delete -mount="$KV" "$STATE"` on the login-state
    path; the next run starts from an empty record. For
    `credentials_rejected`, changing the credentials is enough and nothing
    needs clearing. See "Clearing the latch" in
    `docs/connectors/powerschool-auth-refresher.md`.
  - Login Jobs are labelled `glovebox.io/powerschool-login=true`.
    `powerschoolAuthRefresher.suspendSchedules` and
    `schedules.<name>.suspend` (new, off) keep a CronJob as a template for
    a manual run.

- **Login on demand through a ConfigMap mailbox (new, off by default; do
  not enable it yet):** `connectors.powerschool.loginOnDemand` with
  `powerschoolAuthRefresher.onRequest`. It was merged with four known
  defects, all consequences of a polled mailbox:
  1. a request made outside the answering CronJobs' windows (a catch-up
     poll) is not answered in time and still counts toward the two
     unanswered requests after which the connector stops asking and exits;
  2. a session whose state stays unknown is never escalated: polls are
     skipped for as long as it lasts and no login is asked for;
  3. a request stays valid for 15 minutes but the connector waits only 10,
     so a login can be made -- ending the account owner's own browser
     session -- for a poll that has already given up;
  4. a login that fails before the credentials are submitted still uses up
     the request and the 20-minute interval.

- **Login broker (new, off by default; do not enable it yet):**
  `loginBrokers.<name>` and `connectors.powerschool.loginBroker`. Nothing
  is rendered while it is disabled. Before enabling an instance
  (`internal/loginbroker/DESIGN.md`, "Deploying an instance"):
  - a Vault policy and Kubernetes auth role bound to that instance's
    ServiceAccount (`<fullname>-login-broker-<name>`) and to no other, with
    `create`/`read`/`update` on the session path and the login-state path;
  - `networkPolicy.kubernetesAPICIDRs` for the instance (the render fails
    without them), and its NetworkPolicy left on;
  - the scheduled login CronJobs for the same session suspended (the chart
    refuses live schedules beside a broker unless
    `acknowledgeScheduledLogins` is set), and neither `loginOnDemand` nor
    `powerschoolAuthRefresher.onRequest` set for it;
  - `metrics.prometheusRule.enabled` on and its alerts routed: with a
    broker nothing restarts when a session cannot be had, so the alert is
    the only signal;
  - the account owner's agreement that a login can end their browser
    session at any time outside the 16:00-21:00 America/Los_Angeles
    blackout, up to 6 times a day;
  - acceptance of the residual that the browser process in the broker's
    pod can reach that account's two Vault paths;
  - the `glovebox-login-broker` image for the release actually published
    (its vulnerability gate passed).

- **Chart default.** `connectors.powerschool.externalSecret.refreshInterval`
  `5m` -> `1m`. Nothing to do; set it explicitly if you relied on the old
  default.

## Upgrading to 0.10.3 (from 0.10.2)

No required changes. If you run the PowerSchool login workload behind an
access gateway that sets cookies over redirects before showing the sign-in
page (an F5 `/my.policy` hop, for example), 0.10.2 refuses every run with
`signin_page_unrecognised: no_password_form` without logging in; 0.10.3
carries the gateway's cookies for that one check.

## Upgrading to 0.10.2 (from 0.10.1)

No required changes. 0.10.1 did not publish the
`glovebox-powerschool-auth-refresher` image (its vulnerability gate failed);
use 0.10.2 or later for the PowerSchool login workload.

## Upgrading to 0.10.1 (from 0.10.0)

No required changes; the new workload is off by default.

- **PowerSchool login workload (new, optional): `powerschoolAuthRefresher`.**
  It signs in to the parent portal on a schedule and stores the session in
  Vault for the PowerSchool connector, which never logs in itself. Before
  enabling it, read `docs/connectors/powerschool-auth-refresher.md` in full
  and follow its first-run checklist: the portal allows one session per
  account, so every login ends the account owner's own browser session.
  The workload refuses to log in between 16:00 and 21:00 America/Los_Angeles
  (compiled in; configuration can only widen the window), submits
  credentials at most once per run, and stops submitting altogether after a
  rejected login or two unconfirmed ones until an operator clears its latch.
  It needs a Vault policy and Kubernetes auth role (commands in the runbook)
  and Kubernetes 1.27 or later for the CronJob `timeZone` field.
- The PowerSchool connector's default session location is now
  `glovebox/powerschool/default/session`, property `session`.

## Upgrading to 0.10.0 (from 0.9.7)

No required changes; everything new is off by default.

- **Schoology session refresher schedule.** The chart default
  (`0 6 */12 * *`) assumes a session lasts about two weeks. On at least one
  tenant a session was valid 6.6 hours after login and a login page 32 hours
  after, so with the default the connector meets a rejected session within a
  day or two and stages nothing until the next refresh. If you run the
  Schoology connector, set `schoologyAuthRefresher.schedule` so a refresh
  runs shortly before each poll window (the CronJob has no `timeZone`; the
  schedule is UTC).
- **Schoology `assignment_detail` surface (new, optional).** Enable it per
  kid with `schoologyConnector.assignmentDetail.kids` and add an exact
  `schoology:<kid>:assignment_detail` rule ahead of any wildcard; the chart
  refuses to render, and the connector to start, for a kid enabled without
  one. It costs one or two extra GETs per listed assignment, at most
  `maxPerPoll` (20) assignments per poll. Grades are not available to parent
  accounts and are not staged.
- **PowerSchool connector (first release, optional).** It never logs in: it
  needs a portal session written by a separate login workload, which is not
  part of this release. Do not enable it until that workload is deployed.
  Per-assignment teacher comments are staged on the `assignment` item and
  therefore follow that rule's audience; term-level teacher comments are a
  separate, guardians-only surface.

## Upgrading to 0.9.7 (from 0.9.6)

No required changes. If you run the Schoology connector, set
`schoologyConnector.state.persistent: true`: without it each pod restart
re-delivers every item Schoology currently lists. On first start after the
upgrade the connector stages the items 0.9.6 wrongly skipped.

## Upgrading to 0.9.6 (from 0.9.5)

No required changes. 0.9.6 is the first release in which the Schoology
connector starts when deployed with HTTP ingest (the chart's default); on
0.9.5 and earlier it exits at startup.

## Upgrading to 0.9.5 (from 0.9.4)

No required changes. The Schoology connector now polls each configured
window exactly once and no longer retries a refused session on restart. While
it waits for a replacement session its pod reports unhealthy; that is
intended.

## Upgrading to 0.9.4 (from 0.9.3)

No required changes. If you run the Schoology connector, upgrade before
anything else: 0.9.3 and earlier poll Schoology every 5 minutes regardless of
`poll_schedule`.

## Upgrading to 0.9.3 (from 0.9.1)

No required changes. 0.9.2 was tagged but its images were never published;
go straight to 0.9.3. Operators running the Schoology refresher against a Vault
secret whose properties are not literally `host`/`username`/`password` can
drop any post-render patches in favour of
`schoologyAuthRefresher.vault.credentialsKeys`, and can verify a private-CA
Vault with `schoologyAuthRefresher.vault.caConfigMap`.

## Upgrading to 0.9.1 (from 0.9.0)

No configuration changes. Two scanner changes can move verdicts:

- [ ] **More quarantines of injections hidden with character references.**
      `ig&#8203;nore all previous instructions` (or `&shy;`, `&zwj;`,
      `&zwnj;`) in an HTML body, or as literal references in plain text, used
      to pass, as did a zero-width-split payload inside base64 or hex; both
      are now matched and quarantined. That is the fix, not a false
      positive.
- [ ] **A few more `suspicious_encoding` (0.7) signals.** The detector now
      counts every default-ignorable character except variation selectors,
      the soft hyphen and the Tags block. The one likely to appear in real
      mail is the Arabic letter mark (U+061C): English content carrying one is
      flagged but still passes; Arabic prose carrying one is boosted x1.5 to
      1.05 and quarantined, as Arabic and Hebrew prose with U+200E/U+200F
      already was (glovebox-5ukb). Soft hyphens -- including HTML `&shy;` in
      German and Dutch newsletters -- are stripped and not scored, so they do
      not change verdicts.

---

## Upgrading to 0.9.0 (from 0.8.0)

No breaking changes. One thing to check if your CNI enforces NetworkPolicy
(Cilium, Calico; Flannel does not):

- [ ] **Bearer callers outside the glovebox namespace** -- every client of
      `/v1/sanitize` or `/v1/archives*` -- are now admitted by namespace name:
      list them in `networkPolicy.bearerCallerNamespaces` (e.g.
      `[recognizer, nagus]`). Before this release only a namespace carrying the
      hand-applied label in
      `ingest.archives.networkPolicy.recognizerNamespaceLabel` was admitted, so
      a sanitize caller anywhere else was silently blocked. That label still
      works but is deprecated; set it to `""` once the namespace is in the list.

The scanner's egress also changes from deny-all to DNS plus, with
`ingest.auth.enabled`, Vault (`networkPolicy.vault`, default `vault`/`8200`).
If your Vault is outside the cluster, set `networkPolicy.vault.namespace: ""`
and allow it through `networkPolicy.extraEgress`.

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
