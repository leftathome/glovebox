# PowerSchool session login job (`powerschool-auth-refresher`)

Operator runbook. Source: `connectors/powerschool-auth-refresher/`. Chart:
`powerschoolAuthRefresher` in `charts/glovebox/values.yaml`. Design
background: spec 16 section 4 and [powerschool.md](powerschool.md).

Commands below use these variables. The worked values are this cluster's:
Helm release `glovebox` in namespace `openclaw`, so the chart's fullname is
`glovebox-glovebox`.

```bash
NS=openclaw                        # namespace of the Helm release
FULLNAME=glovebox-glovebox         # <release>-glovebox, or fullnameOverride
SA=$FULLNAME-powerschool-auth-refresher
KV=secret                          # powerschoolAuthRefresher.vault.kvMount
SESSION=glovebox/powerschool/default/session
STATE=glovebox/powerschool/default/login-state
```

## What it is

The PowerSchool connector never logs in. This job does: it signs in to the
parent portal with the account's username and password through a headless
Chromium, checks the result, and writes the session to Vault KV v2. ESO
copies it into the Secret the connector mounts as
`/etc/powerschool/session.json`; the kubelet updates the file in place and a
waiting connector picks it up without a restart.

It is the PowerSchool counterpart of `schoology-auth-refresher` and keeps
that binary's layout, Vault wiring and exit codes 0-4. Two things differ:

- **The image bundles Chromium.** `powerschool-go` v0.4.0 can only start a
  local browser (chromedp's exec allocator); it has no remote-browser option,
  so the cluster's Browserless cannot be used. See "The image".
- **The account is shared with a person.** Everything unusual about this job
  follows from that.

## One account, one session: the rules the job keeps

The connector uses the owner's own parent account, and the portal keeps one
live session per account: **every login ends the session that existed
before**, whoever held it. A login by this job ends the connector's current
session (harmless: it gets the new one) or the owner's, if he is signed in
(not harmless: he is thrown back to the sign-in page mid-use). And the
portal locks an account after repeated bad submissions.

| Rule | Where it is enforced |
|------|----------------------|
| No login between 16:00 and 21:00 `America/Los_Angeles`, any day, nor in the 150 seconds before 16:00. | In the binary, against the real clock, when the run starts and again immediately before each login. A login may take up to 90s, so one is started only if the next 150s (90s + 60s margin) are clear: the last moment is 15:57:29. Such a run logs `refusing to log in` (`outcome=blackout_refused`), writes a termination message, contacts nothing and exits 0. Also at render time: the chart refuses a schedule that can fire inside the window, or close enough before it that a late start could. |
| The blackout cannot be weakened. | 16:00-21:00 Pacific is compiled into the binary. Configuration can only widen it (earlier start, later end, extra windows); anything else is a config error in the binary and a failed render in the chart, and the binary enforces the floor regardless. The zone cannot be changed. |
| No login while the stored session is young and still works. | The binary reads the session from Vault first. If it is younger than the run's minimum age **and** one GET of the home page with it returns the signed-in page, the run logs `session still good` and exits 0. |
| One login per run. | At most one attempt, plus one more only if the first failed before the credentials were submitted (the browser did not start; the sign-in page could not be reached: DNS, connect). Never after a submit. Nothing re-runs a failed run: `backoffLimit: 0`, `restartPolicy: Never`. |
| Rejected credentials are submitted once. | A login the portal rejects sets a **latch** in Vault (`login_blocked`, reason `credentials_rejected`, with a salted SHA-256 fingerprint of the credentials used). Every later run refuses to start a browser (`outcome=login_blocked`, exit 6) until the credentials in Vault change or an operator clears the latch. |
| A login that silently fails is not repeated forever. | If the login returns but its session does not open the home page (a CAPTCHA, a second factor, an expired password: the library sees no error notice), nothing is written and the run ends `login_unconfirmed` (exit 5). Two of those in a row set the latch (reason `login_unconfirmed`). Only a confirmed login resets the count. |
| The password is typed only into the portal's own form. | Before any browser starts, the binary GETs the sign-in page through its pinned read-only transport and requires that every redirect stayed on the configured host over https (at most five), that the page finally served is under `/public/`, and that it holds a form with a password field and a text field posting to that same host. Otherwise `signin_page_unrecognised` (exit 7), no browser, no retry within the run. See "The access gateway and the sign-in check". |
| After the login, only read. | One GET of `/guardian/home.html` to confirm the session, through the connector's own transport pinned to the portal host and refusing every method but GET and HEAD. The login form is the only POST. |
| No secrets outside Vault. | Username, password, cookies and tokens never appear in arguments, log lines, error strings, the termination message or events. Portal-side errors are logged by class, not text. Nothing about a student is logged. |

## Schedules

Two CronJobs, both in `America/Los_Angeles` (`spec.timeZone`, Kubernetes
1.27+), from `powerschoolAuthRefresher.schedules`:

| CronJob | Schedule | Minimum session age | What it does on a normal day |
|---------|----------|---------------------|------------------------------|
| `$FULLNAME-powerschool-login-morning` | `0 5 * * 1-5` | 10h | Yesterday's 05:00 session is 24h old: **logs in**. One login per school day, an hour before the connector's 06:00 poll. |
| `$FULLNAME-powerschool-login-evening` | `5 21 * * 0-4` | 30h | Finds the morning's session 16h old and still accepted: **does nothing**. |

The evening run is at 21:05 because it must come after the blackout ends
(21:00) and before the connector's evening poll window opens (21:30), with
room for the login and for ESO to sync the new session (5m): that poll then
runs on the new session. It logs in only when the day's session is gone: the
owner signed in during the afternoon (which ended it), or it is Sunday and
the last login was Friday's. So a day on which the owner uses the portal
costs the connector that afternoon's poll, not the evening's and the next
morning's too. If the evening run did log in, the next morning's run finds
that session 8h old and does nothing; the pattern returns to morning logins
the day after.

Why two CronJobs and not one schedule with two hours: the two runs need
different minimum ages, and a CronJob has one pod template.

`startingDeadlineSeconds: 1800`: a run missed because the control plane was
down may start at most 30 minutes late. Without it a missed 05:00 run could
be started hours later.

`expires_at` in the stored session is login time + `sessionLifetime` (36h).
It is not the portal's lifetime, which is unknown; it is how long the
connector will go on presenting a session. The portal ends a session by
refusing it, and the connector reports the age at which that happens
(`powerschool_rejected_session_age_seconds`).

## Logins on request (no schedule)

**Finding, 2026-10-11:** the portal ends an idle session in well under an
hour (stored 19:23:51Z, never used, refused at 20:27Z; measured dead at 30
and 45 minutes idle, alive at 4). A login at 05:00 is dead by the 06:00
poll, so logins scheduled well ahead of a poll cannot serve this portal,
and `sessionLifetime` (36h) is only an upper bound.

**No keepalive, by decision.** Sessions are not kept alive artificially,
and this workload does not look at the stored session unless the connector
is waiting for the answer.

Instead the connector asks for a login when a poll finds its session dead
(`connectors.powerschool.loginOnDemand`; described in
[powerschool.md](powerschool.md#idle-sessions-the-pre-poll-check-and-login-on-demand)),
and this workload answers (`powerschoolAuthRefresher.onRequest`):

- One CronJob per entry of `onRequest.windows`,
  `$FULLNAME-powerschool-login-req-<key>`, every 3 minutes inside windows
  that cover the connector's poll windows: by default 06:00-06:57,
  15:00-15:45, 21:30-21:57 and 22:00-22:15 on weekdays. The chart checks
  each against the blackout. **If the connector's `poll_schedule` changes,
  change these to match.**
- Each run is in **request mode**. Its first step -- after the blackout
  check, before Vault, before anything is sent to the portal -- is to read
  the connector's request from the ConfigMap
  `$FULLNAME-powerschool-login-request`. With no pending request it logs
  `no login requested` (`outcome=no_login_requested`), exits 0, and has
  made **no request to the portal at all**. That is nearly every run:
  about 60 a day, of which about 3 do anything.
- A request is *pending* when the field parses as a request (a time and a
  fingerprint prefix; anything else is ignored and never echoed), is not
  dated in the future, is at most 15 minutes old
  (`onRequest.maxRequestAgeMinutes`), has not been acted on already, and no
  login newer than it is in Vault.
- With a pending request the ordinary flow runs: latch, the stored session
  (if it is the one the connector reported dead it is not checked again; a
  different, younger one is checked first, since it may be a login the
  connector has not received yet), sign-in page check, **one** login,
  confirmation, Vault.
- A request is acted on **once**: it is recorded as served in this
  workload's Vault state before the login starts, whatever comes of the
  login. A failed login is not repeated three minutes later.
- Logins made for requests are at least **20 minutes** apart
  (`onRequest.minIntervalMinutes`; the binary refuses less). A request that
  arrives sooner waits: `outcome=login_request_too_soon`, exit 0.

**What this bounds.** The connector, which parses the portal's pages, can
write that ConfigMap and do nothing else in the cluster. Through it, a
connector that has been taken over can cause at most one login per 20
minutes, only inside the windows above, never in the blackout, never with
the latch set, and with no say in which credentials are used or where the
session goes. Those limits are all enforced here, from this workload's own
record in Vault and its compiled-in blackout.

Set `powerschoolAuthRefresher.suspendSchedules: true` so that nothing else
logs in on a clock. The suspended `morning` CronJob stays as the template
for a manual run. A manual run is not in request mode: it ignores the
request and the 20-minute interval and follows the scheduled rules. To run
an on-request CronJob's template by hand without a pending request, set
`POWERSCHOOL_LOGIN_REQUEST_BYPASS=true` on the Job (the connector cannot:
it has no permission to create or change Jobs).

```bash
kubectl -n "$NS" get configmap "$FULLNAME-powerschool-login-request" -o jsonpath='{.data.request}{"\n"}'
kubectl -n "$NS" get jobs -l glovebox.io/powerschool-login-kind=request --sort-by=.metadata.creationTimestamp | tail
vault kv get -mount="$KV" -field=served_request "$STATE"
```

RBAC for this workload: `get` on that one ConfigMap, and nothing else in
the Kubernetes API. Its NetworkPolicy needs the API server's addresses in
`networkPolicy.kubernetesAPICIDRs` (the render fails without them).

## Vault: what to create

Two KV v2 paths, one policy, one Kubernetes auth role. If you change a name,
change the matching chart value.

| Path | What | Read by |
|------|------|---------|
| `secret/glovebox/powerschool/default/session` | The session (below). | The job; ESO, for the connector. |
| `secret/glovebox/powerschool/default/login-state` | The job's own record: the latch, the unconfirmed-login count, a salt and the credential fingerprint. | The job only. Not ESO, not the connector. |

The state is a separate path because the session secret is what ESO copies
to the connector, and KV v2 replaces all fields on a write.

The policy needs `read` as well as `create` and `update`: the job reads the
session before deciding whether to log in, and the state before anything.

```hcl
# glovebox-powerschool-refresher.hcl
path "secret/data/glovebox/powerschool/default/session" {
  capabilities = ["create", "read", "update"]
}

path "secret/data/glovebox/powerschool/default/login-state" {
  capabilities = ["create", "read", "update"]
}
```

```bash
vault policy write glovebox-powerschool-refresher glovebox-powerschool-refresher.hcl

# Confirm the ServiceAccount name first:
#   kubectl -n "$NS" get sa -l app.kubernetes.io/component=powerschool-auth-refresher
vault write auth/kubernetes/role/glovebox-powerschool-refresher \
    bound_service_account_names="$SA" \
    bound_service_account_namespaces="$NS" \
    token_policies=glovebox-powerschool-refresher \
    token_ttl=10m \
    token_max_ttl=15m
```

With this cluster's values that is
`bound_service_account_names=glovebox-glovebox-powerschool-auth-refresher`
and `bound_service_account_namespaces=openclaw`.

Nothing else is granted: not the credentials path (the job gets the
credentials from a Kubernetes Secret, below), not `delete`, not the
metadata paths. Do not set `token_no_default_policy`: the `default` policy
is what lets the job ask `sys/capabilities-self` whether it can write the
session (so a login is never spent on a session it cannot store; if it
cannot ask, it warns and carries on) and revoke its own token when it
finishes. The state path needs no such question: the job writes it before
every login, and does not log in if that write fails.

Two more things must already be true:

- **ESO can read the session path.** The SecretStore that
  `connectors.powerschool.externalSecret.secretStoreRef` names needs `read`
  on `secret/data/glovebox/powerschool/default/session`. If its policy lists
  paths one by one, add this one. It does not need the state path.
- **Vault's certificate can be verified.** The job only talks to an `https`
  Vault and refuses `VAULT_SKIP_VERIFY`. Give it the CA bundle with
  `powerschoolAuthRefresher.vault.caConfigMap` (a ConfigMap in the release
  namespace; mounted read-only and exported as `VAULT_CACERT`), the same
  mechanism as `schoologyAuthRefresher.vault.caConfigMap`.

### What is in Vault

Session path:

| Field | Content |
|-------|---------|
| `session` | The session JSON (`{"cookies":[...],"expires_at":"..."}`) as one string. This property becomes the connector's `session.json`. |
| `logged_in_at` | When the job logged in (RFC 3339, UTC). The next run computes the session's age from it; a secret written by hand without it uses the KV version's creation time. |
| `expires_at` | The same instant as inside `session`. |
| `selected_student_id` | The student the portal had selected right after login, or empty. Recorded here only; not logged, not synced to the connector. |

The session is written check-and-set against the version the run read
before logging in. If another run (a manual Job beside a scheduled one)
wrote in between, the job asks the portal about its own session once more:
if it is still accepted it is the live one and replaces the other; if not,
it is dropped (`outcome=superseded`).

State path:

| Field | Content |
|-------|---------|
| `login_blocked` | `"true"` while the latch is set. |
| `blocked_reason` | `credentials_rejected` or `login_unconfirmed`; `consecutive_failures` when the login broker set it. |
| `blocked_at` | When it was set. |
| `credential_fingerprint` | SHA-256 over `salt`, username and password of the credentials last submitted. Never the values. |
| `salt` | 16 random bytes, generated on first use and kept for the install. |
| `served_request` | The last connector request acted on, verbatim (a time and a fingerprint prefix). A request is acted on once. |
| `request_login_at` | When a login was last started for a request; the 20-minute interval is measured from it. |
| `unconfirmed_count` | Logins in a row that returned without a working session. |
| `attempt_started_at` | Set immediately before a login, cleared when its outcome is recorded. While it is under ten minutes old a login is in flight: no other run, and not the login broker, logs in (`outcome=login_in_progress`, exit 0). Older than that, it is a run that died in the middle of a login, and is counted as an unconfirmed one. |
| `cooldown_until` | Written by the login broker: no broker login before this time. This job carries it through unchanged. |
| `login_day`, `login_day_count` | The Pacific calendar day and the number of logins made on it, by this job and the login broker together. The broker stops at six a day; this job counts and is not stopped. |

Every write of this record is check-and-set against the version the run
read. If something else wrote it in between, the write before the login is
refused and the run does not log in (`outcome=login_state_conflict`, exit
8: the Job shows as failed, because no login was made for its window). A
write Vault applied but answered with an error is recognised as the run's
own when the retry is refused, and is not mistaken for a conflict.

**Changes to the scheduled path that apply even without a login broker:**

- A run that is killed in the middle of a login (node drain, OOM, the
  Job's deadline) leaves its attempt marker. The next run, if it starts
  within ten minutes, does nothing (`login_in_progress`, exit 0). A later
  run counts the dead one as an unconfirmed login: **two such deaths in a
  row set the latch** (`login_unconfirmed`), where before they left no
  trace.
- A run writes the state record once more than before (the marker is
  cleared on every way out).
- Two runs at once (a manual Job beside a scheduled one): the second to
  write is refused and exits 8 without logging in, where before both could
  log in and the check-and-set on the SESSION sorted it out afterwards.

The login broker (`loginBrokers.powerschool`, see
`internal/loginbroker/DESIGN.md`) reads and writes this same record when it
is deployed for the same account: a latch set by either workload stops
both, a login made by either counts for the other's interval, and neither
logs in while the other's login is in flight. The chart refuses live
schedules of this job beside a broker for the same session unless
`loginBrokers.<name>.acknowledgeScheduledLogins` is set; suspended ones,
kept as templates for a manual run, are fine.

### Clearing the latch

A blocked job logs `login blocked` with `outcome=login_blocked`, exits 6,
and its pod's termination message names the reason and the path. First
find out why (`blocked_reason`; sign in to the portal yourself in a browser
and see what it asks for). Then either:

- **Change the credentials** in the credentials source. The next run sees a
  different fingerprint and proceeds; nothing to clear.
- **Clear the latch**, when the credentials were right all along (the portal
  wanted a notice accepted, say, and the owner has dealt with it):

  ```bash
  vault kv get -mount="$KV" "$STATE"            # look first
  vault kv metadata delete -mount="$KV" "$STATE"
  ```

  This needs an operator token; the job's own policy cannot delete. The
  next run starts from an empty state (and a new salt).

Do not clear the latch to "try again" with the same rejected password: that
is the lockout the latch exists to prevent.

## Credentials

The job reads three environment variables, each from one key of one Secret:
`POWERSCHOOL_HOSTNAME` (a bare host or an https URL), `POWERSCHOOL_USERNAME`,
`POWERSCHOOL_PASSWORD`.

- If a Secret with them already exists (here: `glovebox-powerschool-secrets`,
  synced from `secret/eso/glovebox-powerschool`), name it in
  `powerschoolAuthRefresher.credentials.existingSecret`.
- Otherwise the chart renders an ExternalSecret,
  `$FULLNAME-powerschool-refresher-credentials`, from
  `vault.credentialsPath` (`eso/glovebox-powerschool`; properties
  `powerschool_hostname`, `powerschool_username`, `powerschool_password`).

The connector is never given the username or the password.

## Values

```yaml
powerschoolAuthRefresher:
  enabled: true
  image:
    tag: ""                       # empty = the chart's appVersion
  credentials:
    existingSecret: glovebox-powerschool-secrets
  vault:
    addr: https://vault.vault.svc.cluster.local:8200
    k8sRole: glovebox-powerschool-refresher
    kvMount: secret
    sessionPath: glovebox/powerschool/default/session
    caConfigMap:
      name: <configmap holding the CA bundle>
      key: tls-ca-bundle.pem

connectors:
  powerschool:
    enabled: true
    # image, config.kids, config.rules: see powerschool.md
    externalSecret:
      enabled: true
      secretStoreRef: { kind: ClusterSecretStore, name: <store> }
      # Defaults already in the chart:
      #   data.POWERSCHOOL_HOST          <- eso/glovebox-powerschool : powerschool_hostname
      #   files.data."session.json"      <- glovebox/powerschool/default/session : session
```

| Value | Default | Meaning |
|-------|---------|---------|
| `enabled` | `false` | Render the job. |
| `timeZone` | `America/Los_Angeles` | Zone of the schedules. Fixed. |
| `schedules.<name>.schedule` | `0 5 * * 1-5`, `5 21 * * 0-4` | Five-field cron expression. One CronJob per entry; `null` removes one. |
| `schedules.<name>.minSessionAge` | `10h`, `30h` | Log in only if the stored session is at least this old or no longer accepted. |
| `startingDeadlineSeconds` | `1800` | How late a missed run may start. Required. |
| `blackout.start` / `.end` | `16:00` / `21:00` | The main no-login window. `start` may be earlier, `end` later; never the reverse. |
| `blackout.extraWindows` | `[]` | Further daily windows, `{start, end}`. |
| `blackout.timeZone` | `America/Los_Angeles` | Fixed. |
| `suspendSchedules` | `false` | Suspend every CronJob of `schedules`; they remain as templates for a manual run. |
| `onRequest.enabled` | `false` | Render the on-request CronJobs, the request ConfigMap and this workload's Role (`get` on it). |
| `onRequest.windows.<name>` | every 3 min around the default poll windows | Five-field cron expressions, checked against the blackout. |
| `onRequest.startingDeadlineSeconds` | `120` | |
| `onRequest.minIntervalMinutes` | `20` | Least time between logins made for requests; 20 is the floor. |
| `onRequest.maxRequestAgeMinutes` | `15` | Older requests are ignored. |
| `onRequest.minSessionAge` | `5m` | With a pending request, a stored session at least this old is replaced without being checked. |
| `networkPolicy.kubernetesAPICIDRs` / `kubernetesAPIPorts` | `[]` / `[443, 6443]` | The API server, for on-request runs. Required with `onRequest` when this policy is on. |
| `schedules.<name>.suspend` | unset | Suspend one CronJob. |
| `sessionLifetime` | `36h` | `expires_at` = login time + this. An upper bound only: the portal ends an idle session in under an hour. |
| `credentials.existingSecret` | `""` | Existing Secret with the account; empty renders an ExternalSecret. |
| `credentials.keys.*` | `POWERSCHOOL_HOSTNAME` / `_USERNAME` / `_PASSWORD` | Key names in that Secret. |
| `vault.addr` | `https://vault.vault.svc.cluster.local:8200` | https only. |
| `vault.k8sRole` | `glovebox-powerschool-refresher` | Kubernetes auth role. |
| `vault.kvMount` / `vault.sessionPath` | `secret` / `glovebox/powerschool/default/session` | Where the session is written. |
| `vault.statePath` | `""` (`login-state` beside the session) | The job's state record. Never the session path. |
| `vault.caConfigMap.name` / `.key` | `""` / `ca.crt` | CA bundle for Vault's certificate. |
| `vault.credentialsPath`, `vault.credentialsKeys.*`, `vault.refreshInterval`, `vault.secretStoreRef` | see `values.yaml` | The chart's own credentials ExternalSecret. |
| `networkPolicy.enabled` | `true` | Restrict the pods' egress (needs the top-level `networkPolicy.enabled`). |
| `networkPolicy.vaultPodLabels` | `app.kubernetes.io/name: vault` | Vault pods egress is limited to, in `networkPolicy.vault.namespace` on `networkPolicy.vault.port`. |
| `networkPolicy.portalCIDRs` | `[0.0.0.0/0]` | Where port 443 may be reached. |
| `networkPolicy.portalExceptCIDRs` | RFC 1918, `100.64.0.0/10`, `169.254.0.0/16` | Carved out of `0.0.0.0/0`. Add the cluster's pod and service CIDRs if they lie outside these. |
| `networkPolicy.extraEgress` | `[]` | Further rules, verbatim. |
| `tmpSizeLimit` / `homeSizeLimit` / `shmSizeLimit` | `256Mi` / `64Mi` / `256Mi` | Memory-backed emptyDirs for `/tmp`, `$HOME`, `/dev/shm`. |
| `resources` | 200m / 384Mi requests, 1 CPU / 1536Mi limits | The emptyDirs count against the memory limit. |

The render fails, naming the value, when: a schedule can fire inside a
blackout window, or within `startingDeadlineSeconds` + 3 minutes before
one; a schedule is not five fields or its minute/hour fields use anything
but numbers, lists, ranges and steps; the blackout does not contain
16:00-21:00 or its zone (or `timeZone`) is not `America/Los_Angeles`;
`startingDeadlineSeconds` is unset; `vault.addr` is not https;
`vault.statePath` equals `vault.sessionPath`; the connector is enabled with
an ExternalSecret whose `session.json` does not read `vault.sessionPath`
property `session`. `scripts/test-chart-powerschool-login.sh` pins all of
this.

## The pod

- Runs as uid 65532, all capabilities dropped, no privilege escalation,
  seccomp `RuntimeDefault` (also when the chart-wide profile is switched
  off), **read-only root filesystem**.
- `/tmp`, `/home/refresher` and `/dev/shm` are memory-backed, size-capped
  emptyDirs. The browser profile, which holds the session cookies after a
  login, lives under `/tmp` in a directory the binary creates per login and
  removes on every exit path; nothing of it reaches a node's disk, and all
  of it is gone with the pod.
- Finished pods are removed after an hour (`ttlSecondsAfterFinished`).
- NetworkPolicy: no ingress; egress to cluster DNS, to the Vault pods on
  the Vault port, and to port 443 outside private address space.
- The binary removes `POWERSCHOOL_PASSWORD`, `POWERSCHOOL_USERNAME` and
  `VAULT_TOKEN` from its environment before it starts Chromium, so the
  browser does not inherit them, and revokes its Vault token on exit.

### Residual risks, stated plainly

- **The ServiceAccount token.** Vault's Kubernetes auth needs the projected
  token, so it is mounted, and Chromium runs as the same uid in the same
  container: a compromised browser could read it and log in to Vault as
  this role. What that yields is bounded by the policy -- the session it
  would be producing anyway and the state record -- and by a 10-minute
  token TTL. It cannot read the account's password from Vault.
- **The password in the parent process.** The variables are removed from
  the environment Chromium inherits, but the kernel keeps a process's
  initial environment readable to the same uid (`/proc/1/environ`), and the
  password is in the binary's memory. A compromised browser in this
  container could reach it. The browser only ever loads the portal's
  sign-in page, checked beforehand; egress is limited to public 443.
- **The sign-in check is not atomic.** The page is checked with a GET and
  then fetched again by the browser. A page that differs between the two is
  not caught. Closing that needs the check inside the browser session, in
  `powerschool-go`.
- **`--no-sandbox`.** Chromium's own sandbox cannot start under the pod's
  restrictions; the container is the sandbox.

## The image

`ghcr.io/leftathome/glovebox-powerschool-auth-refresher`:
`debian:bookworm-slim` **pinned by digest**, plus the apt `chromium`, uid
65532. Renovate raises a PR when the base tag moves.

Chromium is not pinned to a package version, on purpose: bookworm-security
carries one chromium version at a time, so a version pin breaks the build
at the next browser security update, and freezing it through a dated
snapshot would freeze its known vulnerabilities in the one component that
parses a remote site. The version is whatever bookworm-security holds when
the image is built, recorded in the image at `/etc/chromium-version`; it
changes only when the image is rebuilt (a release tag, or a base digest
bump).

On GitHub, the job `powerschool-auth-refresher-image` builds the image,
runs `scripts/test-powerschool-auth-refresher-image.sh` (Chromium started
inside it as uid 65532, capabilities dropped, read-only root) and scans it
with Trivy (gate: fixed CRITICALs; report: HIGH and CRITICAL) before the
image matrix may publish anything. The internal GitLab pipeline builds and
publishes the image on main and tags but has no image-scanning stage for
any image; run the script by hand there (first-run checklist, step 1).

## First-run checklist

Do all of it outside 15:55-21:00 Pacific except step 3, which is done
inside. Tell the owner before step 4: it signs in to his account.

1. **The image.** Build it and run the browser test inside it as uid 65532
   with dropped capabilities:

   ```bash
   bash scripts/test-powerschool-auth-refresher-image.sh
   ```

   It must end `all checks passed`. This has not been run on the cluster's
   runtime; if Chromium does not start under containerd with the pod's
   settings, this is where it shows, with no portal involved.

2. **Render and Vault.** Render with the real namespace and check the
   names, then the role binding and the capabilities the role yields:

   ```bash
   helm template glovebox charts/glovebox -n "$NS" -f <values> \
     | grep -E 'name: .*powerschool-(login|auth-refresher)'
   vault read auth/kubernetes/role/glovebox-powerschool-refresher   # names $SA, namespace $NS
   vault token create -policy=glovebox-powerschool-refresher -ttl=5m -field=token > /tmp/t
   VAULT_TOKEN=$(cat /tmp/t) vault token capabilities "$KV/data/$SESSION"   # create, read, update
   VAULT_TOKEN=$(cat /tmp/t) vault token capabilities "$KV/data/$STATE"     # create, read, update
   VAULT_TOKEN=$(cat /tmp/t) vault token capabilities "$KV/data/eso/glovebox-powerschool"   # deny
   rm /tmp/t
   kubectl -n "$NS" get secret glovebox-powerschool-secrets \
     -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}'       # the three keys
   ```

   Enable `powerschoolAuthRefresher` (and the connector) and let Flux apply
   it. Until the first session is written the connector's
   `$FULLNAME-powerschool-files` ExternalSecret reports an error and the
   connector pod waits in `ContainerCreating`. That is expected.

   ```bash
   kubectl -n "$NS" get cronjob -l app.kubernetes.io/component=powerschool-auth-refresher \
     -o custom-columns=NAME:.metadata.name,SCHEDULE:.spec.schedule,TZ:.spec.timeZone,SUSPEND:.spec.suspend
   ```

3. **The blackout, at 17:00 Pacific.** Run the job by hand (next section).
   It must log `refusing to log in` with `outcome=blackout_refused`, exit 0,
   and its pod's termination message must say so. There must be no Vault
   traffic and no portal traffic: the log has no `logging in` line and no
   Vault line, and Vault's audit log shows no login for the role at that
   time.

4. **First login**, outside the blackout. Expect, in order: `no session is
   stored yet; logging in`, `logging in to the portal` (`attempt=1`),
   `refresh complete` (`outcome=success`, `version=1`). Exit code 0. Then
   look for leaks -- there must be none:

   ```bash
   read -r PS_USER      # type the account's username; it is not saved
   kubectl -n "$NS" logs "job/$JOB" | grep -c -i -e "$PS_USER" -e JSESSIONID     # 0
   kubectl -n "$NS" get events --field-selector involvedObject.kind=Pod | grep -c -i "$PS_USER"   # 0
   kubectl -n "$NS" get pod -l job-name="$JOB" -o jsonpath='{.items[0].spec.containers[0].args}'  # empty
   ```

   and, while a run is in progress, that no process carries a credential in
   its arguments: `kubectl -n "$NS" exec <pod> -- sh -c 'cat /proc/[0-9]*/cmdline | tr "\0" " "'`
   shows Chromium's flags and nothing of the account.

5. **One POST.** In the portal's access log, or failing that the egress
   firewall's, the run made exactly one POST to the portal (the sign-in
   form). Sign in as the owner afterwards and confirm that the student the
   portal shows first is the one it showed before: the job selects nothing.

6. **Second run.** Run the job by hand again. It must log `session still
   good` and exit 0 without logging in. If it logs in again, stop: the
   home-page check is not recognising a good session.

7. **The owner signs in** (which ends the job's session). Run the job by
   hand: it must log `the stored session is young but the portal no longer
   accepts it; logging in`, log in exactly once, and end `refresh complete`.

8. **The connector.** The ExternalSecret becomes `SecretSynced` within its
   refresh interval (5m) and the connector picks the file up **without a
   pod restart**: same pod, restart count unchanged, log line `a new
   session is in place; starting`.

   ```bash
   kubectl -n "$NS" get externalsecret -l glovebox.io/connector=powerschool
   kubectl -n "$NS" get pods -l glovebox.io/connector=powerschool
   vault kv get -mount="$KV" -field=logged_in_at "$SESSION"
   ```

9. **Session lifetime.** Over the following days read
   `powerschool_rejected_session_age_seconds` and the connector's
   `session_age_seconds` log line. The schedules assume a session survives
   from 05:00 to the 21:30 poll and `sessionLifetime` assumes 36h is an
   upper bound; adjust both to what is measured.

10. **Never test a wrong password live.** The rejected-credentials path
    (latch, exit 3 then 6) is covered by tests with a fake login. Trying it
    against the portal is a bad submission on the owner's account.

Then continue with the connector's own supervised first run
([powerschool.md](powerschool.md)).

## Running it once by hand

```bash
JOB=ps-login-manual-$(date +%s)
kubectl -n "$NS" create job "$JOB" \
  --from="cronjob/$FULLNAME-powerschool-login-morning"
kubectl -n "$NS" logs -f "job/$JOB"
kubectl -n "$NS" get pod -l job-name="$JOB" \
  -o jsonpath='{.items[0].status.containerStatuses[0].state.terminated.message}{"\n"}'
```

With this cluster's values the CronJob is
`glovebox-glovebox-powerschool-login-morning` in namespace `openclaw`.

A manual run is bound by the same rules as a scheduled one. Inside the
blackout it refuses. With the latch set it refuses. If the stored session
is younger than 10h and still works it does nothing -- which is the answer
to "is the session all right?".

To make it log in although the session is young and working (after
replacing the password, say), the minimum age has to be zero for that one
run. Outside the blackout only:

```bash
kubectl -n "$NS" create job "ps-login-forced-$(date +%s)" \
  --from="cronjob/$FULLNAME-powerschool-login-morning" --dry-run=client -o yaml \
  | sed 's/value: 10h/value: 0s/' \
  | kubectl -n "$NS" apply -f -
```

To stop the job logging in at all:

```bash
kubectl -n "$NS" patch cronjob -l app.kubernetes.io/component=powerschool-auth-refresher \
  --type merge -p '{"spec":{"suspend":true}}'
```

Flux will revert that at its next reconcile unless the HelmRelease is
suspended or the value is changed in git.

## Outcomes

One JSON line per event on stderr; `grep '"outcome"'` finds how a run
ended. The same outcome is in the pod's termination message
(`kubectl describe pod`), as
`powerschool-auth-refresher: outcome=<outcome> exit=<code>: <reason>`.

| Exit | `outcome` | Meaning | What to do |
|------|-----------|---------|------------|
| 0 | `success` | Logged in, confirmed, written. | Nothing. |
| 0 | `session_still_good` | Stored session is young and accepted; no login. | Nothing. |
| 0 | `blackout_refused` | The clock was inside the blackout or within 150s of it; nothing was contacted. Exit 0 so that the CronJob is not marked failed for obeying its rule. | Find out what started it (below). |
| 1 | `transient_failure` | Browser, network or Vault unavailable; or the portal could not be asked about a young session. | Nothing; the next scheduled run retries. Look if it repeats. |
| 1 | `blackout_refused` | The blackout came within reach during the pause before the one retry. | Nothing. |
| 1 | `superseded` | Another run stored a newer session while this one was logging in. | Nothing; do not run two at once. |
| 2 | `config_error` | A setting is missing or malformed; `problems` names it. | Fix the values. |
| 3 | `bad_credentials` | The portal rejected the username/password. The latch is now set. | Correct the password in Vault. Later runs exit 6 until then. |
| 4 | `secret_read_failed` / `secret_write_failed` | Vault refused (policy, role binding, path). No login happens when this is detected beforehand. | Fix the policy or role. |
| 5 | `login_unconfirmed` | The browser login returned but its session does not open the home page. Nothing was written; the previous session stays in Vault. The second in a row sets the latch. | Sign in as the owner and see what the portal now asks for. |
| 6 | `login_blocked` | The latch is set. No browser was started, nothing was submitted. | "Clearing the latch" above. |
| 0 | `no_login_requested` | Request mode: the connector has not asked (or its request is stale, malformed, already served, or older than the last login). Nothing was sent to the portal. | Nothing. This is what nearly every on-request run logs. |
| 0 | `login_request_too_soon` | Request mode: a login was made for a request less than 20 minutes ago. Nothing was sent to the portal. | Nothing, unless it repeats: then find out why the connector keeps asking. |
| 7 | `signin_page_unrecognised` | The sign-in page is not a password form on the configured host (`reason` says which check failed). No browser was started. | The table of reasons below. |
| 8 | `login_state_conflict` | The login state record was changed by another login workload (the login broker, another run) between this run reading it and writing its attempt marker. **No login was made by this run.** Non-zero so that the Job shows as failed; safe because a failed login Job is never re-run (backoffLimit 0, restartPolicy Never). | See what the other workload did (its log; `vault kv get` of the state). If no session was made for this window, run the job once by hand. |

No run is retried by Kubernetes.

## The access gateway and the sign-in check

The portal sits behind an access gateway (an F5 APM on this instance).
Seen from the public sign-in page, without credentials:

- A client with no gateway cookie that asks for `/public/` is redirected to
  `/my.policy` and handed cookies on the way (`MRHSession`,
  `LastMRH_Session`, `F5_ST`, ...). Returning them at `/my.policy` gets it
  redirected back to `/public/`, which now serves the real page: one
  `form#LoginForm` posting to `/guardian/home.html`, a text input
  `account`, password inputs `pw` and `translatorpw`, and hidden inputs.
- A client that does **not** return the cookies is redirected to
  `/my.logout.php3?errorcode=19`: a small page with no form.

So the sign-in check keeps cookies -- in a jar created empty for that one
check, used for nothing else, never logged and thrown away when the check
returns. (0.10.2 did not; its first live run ended `no_password_form` on the
gateway's error page. Nothing was submitted.)

The stored session is the browser's whole cookie set for the portal host,
so it holds the gateway's cookies beside the portal's own. They lapse
independently. When the home-page read made with a stored session is sent
through `/my.policy` because the gateway's cookie has lapsed, the job
carries the renewed cookie for the rest of that one read (and does not
write it back); the portal then answers for its own session. The
connector's client does the same (the code is shared,
`internal/psportal`). Before it did -- 0.10.3 -- a dead session behind the
gateway ended on the gateway's error page and was logged by the connector
as `kid skipped ... error_class=parse` instead of as a refused session.

`reason` values of `signin_page_unrecognised`:

| `reason` | What the check saw | What to do |
|----------|--------------------|------------|
| `gateway_session_refused` | Redirects ended on the gateway's own pages (`/my.logout.php3`, `/my.policy`): the gateway would not give even a cookie-keeping client a session. | Open the portal in a private browser window. If that works, the gateway is treating this client differently (source address, user agent, a new policy step); if it does not, the gateway is down or changed. |
| `signin_redirect_off_host` | A redirect pointed at another host, another port, or plain http. Not followed. | The portal may have moved or been put behind an identity provider. Check `POWERSCHOOL_HOSTNAME` against the address a browser ends up at. |
| `signin_redirect_loop` | More than five redirects. | As for `gateway_session_refused`. |
| `final_page_not_under_public` | The page finally served is on the right host but not under `/public/`. | Look at where a browser lands; the sign-in page has moved. |
| `no_password_form` | No form with a password field. A maintenance or error page. | Wait, or look at the portal in a browser. |
| `no_username_field` | A password form, but nowhere to type an account name. A second-factor or password-change step, or a different sign-in design. | Sign in as the owner and see what the portal asks for. |
| `form_posts_off_host` | A form with a password field posts to another host or over http. | Do not work around this. Find out why before anything else. |
| `form_action_unparseable`, `unparseable`, `http_<status>` | The page or its form could not be read. | Look at the portal in a browser. |

None of these is retried within the run, none starts a browser, and none
counts toward the latch. The next scheduled run checks again.

## If it ran inside the blackout

**If the job refused** (`blackout_refused`, exit 0): nothing happened to
anyone. Nothing was contacted, nobody was signed out, the connector's
session is untouched. Find what started the run: a hand-made Job, or a
`timeZone` problem (`kubectl -n "$NS" get cronjob -o yaml`, `.spec.timeZone`;
a cluster older than 1.27 ignores the field and reads the schedule in UTC).

**A login inside 16:00-21:00 Pacific** cannot be produced by configuration:
the window is compiled in. It would take a wrong node clock or a changed
binary. If it happened anyway:

- What the owner sees: if he was signed in to the portal at that moment,
  his next click lands on the sign-in page. Nothing is lost or changed in
  the account; he signs in again.
- His signing in again ends the job's new session. The connector's next
  poll is refused; the pod goes `Running`, `0/1 Ready`, logging `this
  session was already rejected upstream; waiting for a new one`. It contacts
  nothing while it waits.
- The evening run (21:05) finds the session gone and logs in; the connector
  resumes by itself and stages whatever changed in between.
- Tell the owner what happened, and find out how the clock or the image
  came to be wrong before the next run.

## Not verified against the live portal

Everything here is tested without the portal: the blackout and its headroom
on both daylight-saving changes, that it cannot be weakened, the
young-session skip, the attempt ceiling, the latch sequences, the sign-in
page check and the home-page read against a local TLS server, the Vault
read/write (check-and-set, state record, token revocation) against an HTTP
test server with TLS verification on, and that the connector's own loader
accepts what the job writes. What only the checklist above can show:

- that Chromium starts from the image under the pod's restrictions --
  **the image has not been built or run**; read-only root with memory
  volumes is the least certain part (step 1);
- that the portal's sign-in page passes the sign-in check and is still the
  one the library drives (step 4);
- that the home page fetched by plain GET with the new cookies is the
  signed-in page (the connector relies on the same thing, verified live);
- that a second login really ends the first session every time (observed
  once), and how long a session lasts when nobody ends it (steps 7 and 9).
