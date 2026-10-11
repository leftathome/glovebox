# PowerSchool session login job (`powerschool-auth-refresher`)

Operator runbook. Source: `connectors/powerschool-auth-refresher/`. Chart:
`powerschoolAuthRefresher` in `charts/glovebox/values.yaml`. Design
background: spec 16 section 4 and [powerschool.md](powerschool.md).

## What it is

The PowerSchool connector never logs in. This job does: it signs in to the
parent portal with the account's username and password through a headless
Chromium, checks the result, and writes the session to Vault KV v2. ESO
copies it into the Secret the connector mounts as
`/etc/powerschool/session.json`; the kubelet updates the file in place and a
waiting connector picks it up without a restart.

It is the PowerSchool counterpart of `schoology-auth-refresher` and keeps
that binary's layout, Vault wiring and exit codes. Two things differ:

- **The image bundles Chromium.** `powerschool-go` v0.4.0 can only start a
  local browser (chromedp's exec allocator); it has no remote-browser option,
  so the cluster's Browserless cannot be used. The image is
  `debian:bookworm-slim` plus the apt `chromium`, run as uid 65532, which is
  what the Schoology refresher's image was before it moved to Browserless.
  Chromium is started with `--no-sandbox` (the image sets
  `POWERSCHOOL_CHROME_NO_SANDBOX=true`): its own sandbox needs user
  namespaces or a setuid helper, and the pod has neither (all capabilities
  dropped, no privilege escalation, default seccomp).
- **The account is shared with a person.** See the next section. Everything
  unusual about this job follows from it.

## One account, one session: the rules the job keeps

The connector uses the owner's own parent account, and the portal keeps one
live session per account: **every login ends the session that existed
before**, whoever held it. A login by this job ends the connector's current
session (harmless: it gets the new one) or the owner's, if he is signed in
(not harmless: he is thrown back to the sign-in page mid-use).

| Rule | Where it is enforced |
|------|----------------------|
| No login between 16:00 and 21:00 `America/Los_Angeles`, any day. | In the binary, against the real clock in that zone, checked when the run starts and again immediately before each login. Such a run logs `refusing to log in` (`outcome=blackout_refused`) and exits 0. Also at render time: the chart refuses a schedule that can fire inside the window, or close enough before it that a late start could. |
| No login while the stored session is young and still works. | The binary reads the session from Vault first. If it is younger than the run's minimum age **and** one GET of the home page with it returns the signed-in page, the run logs `session still good` and exits 0. |
| One login per run. | At most one attempt, plus one more only if the first failed before the credentials were submitted (the browser did not start; the sign-in page could not be reached: DNS, connect). Never after a submit, never after rejected credentials. Nothing re-runs a failed run: `backoffLimit: 0`, `restartPolicy: Never`. |
| After the login, only read. | One GET of `/guardian/home.html` to confirm the session, through the connector's own transport pinned to the portal host and refusing every method but GET and HEAD. The login form is the only POST. |
| No secrets outside Vault. | Username, password, cookies and tokens never appear in arguments, log lines, error strings or events. Portal-side errors are logged by class, not text. Nothing about a student is logged. |

The window is `[16:00, 21:00)`: 16:00:00 is refused, 21:00:00 proceeds. It
follows the wall clock, so it is the same local hours on both sides of a
daylight-saving change. It is configurable (`powerschoolAuthRefresher.blackout`)
but that is the owner's decision to change, not an operator's convenience.

## Schedules

Two CronJobs, both in `America/Los_Angeles` (`spec.timeZone`, Kubernetes
1.27+), from `powerschoolAuthRefresher.schedules`:

| CronJob | Schedule | Minimum session age | What it does on a normal day |
|---------|----------|---------------------|------------------------------|
| `<fullname>-powerschool-login-morning` | `0 5 * * 1-5` | 10h | Yesterday's 05:00 session is 24h old: **logs in**. One login per school day, an hour before the connector's 06:00 poll. |
| `<fullname>-powerschool-login-evening` | `30 21 * * 0-4` | 30h | Finds the morning's session 16.5h old and still accepted: **does nothing**. |

The evening run logs in only when the day's session is gone: the owner
signed in during the afternoon (which ended it), or it is Sunday and the
last login was Friday's. So a day on which the owner uses the portal costs
the connector that afternoon's poll, not the next morning's too. If the
evening run did log in, the next morning's run finds that session 7.5h old
and does nothing; the pattern returns to morning logins the day after.

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

## Vault: what to create

One KV v2 path, one policy, one Kubernetes auth role. Defaults shown; if
you change a name, change the matching chart value.

The session is written to `secret/glovebox/powerschool/default/session`
(mount `secret`, path `glovebox/powerschool/default/session`). The policy
needs `read` as well as `create` and `update`: the job reads the current
session before deciding whether to log in.

```hcl
# glovebox-powerschool-refresher.hcl
path "secret/data/glovebox/powerschool/default/session" {
  capabilities = ["create", "read", "update"]
}
```

```bash
vault policy write glovebox-powerschool-refresher glovebox-powerschool-refresher.hcl

# The ServiceAccount is <fullname>-powerschool-auth-refresher; check with
#   kubectl -n glovebox get sa -l app.kubernetes.io/component=powerschool-auth-refresher
vault write auth/kubernetes/role/glovebox-powerschool-refresher \
    bound_service_account_names=glovebox-glovebox-powerschool-auth-refresher \
    bound_service_account_namespaces=glovebox \
    token_policies=glovebox-powerschool-refresher \
    token_ttl=10m \
    token_max_ttl=15m
```

Nothing else is granted: not the credentials path (the job gets the
credentials from a Kubernetes Secret, below), not `delete`, not the
metadata path. Do not set `token_no_default_policy`: before logging in the
job asks Vault (`sys/capabilities-self`, allowed by the `default` policy)
whether it can write the path, so that a login is never spent on a session
it cannot store. If it cannot ask, it logs a warning and carries on.

Two more things must already be true:

- **ESO can read the session path.** The SecretStore that
  `connectors.powerschool.externalSecret.secretStoreRef` names needs `read`
  on `secret/data/glovebox/powerschool/default/session`. If its policy lists
  paths one by one, add this one.
- **Vault's certificate can be verified.** The job only talks to an `https`
  Vault and refuses `VAULT_SKIP_VERIFY`. Give it the CA bundle with
  `powerschoolAuthRefresher.vault.caConfigMap` (a ConfigMap in the release
  namespace; mounted read-only and exported as `VAULT_CACERT`), the same
  mechanism as `schoologyAuthRefresher.vault.caConfigMap`.

## Credentials

The job reads three environment variables, each from one key of one Secret:
`POWERSCHOOL_HOSTNAME` (a bare host or an https URL), `POWERSCHOOL_USERNAME`,
`POWERSCHOOL_PASSWORD`.

- If a Secret with them already exists (for example `glovebox-powerschool-secrets`,
  synced from `secret/eso/glovebox-powerschool`), name it in
  `powerschoolAuthRefresher.credentials.existingSecret`.
- Otherwise the chart renders an ExternalSecret,
  `<fullname>-powerschool-refresher-credentials`, from
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
| `timeZone` | `America/Los_Angeles` | Zone of the schedules. Must equal `blackout.timeZone`. |
| `schedules.<name>.schedule` | `0 5 * * 1-5`, `30 21 * * 0-4` | Five-field cron expression. One CronJob per entry; `null` removes one. |
| `schedules.<name>.minSessionAge` | `10h`, `30h` | Log in only if the stored session is at least this old or no longer accepted. |
| `startingDeadlineSeconds` | `1800` | How late a missed run may start. Required. |
| `blackout.start` / `.end` / `.timeZone` | `16:00` / `21:00` / `America/Los_Angeles` | The no-login window, `[start, end)`, daily. |
| `sessionLifetime` | `36h` | `expires_at` = login time + this. |
| `credentials.existingSecret` | `""` | Existing Secret with the account; empty renders an ExternalSecret. |
| `credentials.keys.*` | `POWERSCHOOL_HOSTNAME` / `_USERNAME` / `_PASSWORD` | Key names in that Secret. |
| `vault.addr` | `https://vault.vault.svc.cluster.local:8200` | https only. |
| `vault.k8sRole` | `glovebox-powerschool-refresher` | Kubernetes auth role. |
| `vault.kvMount` / `vault.sessionPath` | `secret` / `glovebox/powerschool/default/session` | Where the session is written. |
| `vault.caConfigMap.name` / `.key` | `""` / `ca.crt` | CA bundle for Vault's certificate. |
| `vault.credentialsPath`, `vault.credentialsKeys.*`, `vault.refreshInterval`, `vault.secretStoreRef` | see `values.yaml` | The chart's own credentials ExternalSecret. |
| `networkPolicy.enabled` | `true` | Restrict the pods' egress (needs the top-level `networkPolicy.enabled`). |
| `networkPolicy.portalCIDRs` | `[0.0.0.0/0]` | Where port 443 may be reached. |
| `networkPolicy.extraEgress` | `[]` | Further rules, verbatim. |
| `shmSizeLimit` | `256Mi` | Memory-backed `/dev/shm` for Chromium. |
| `resources` | 200m / 384Mi requests, 1 CPU / 1Gi limits | |

The render fails, naming the value, when: a schedule can fire inside the
blackout, or within `startingDeadlineSeconds` before it; a schedule is not
five fields or its minute/hour fields use anything but numbers, lists,
ranges and steps; `timeZone` differs from `blackout.timeZone`;
`startingDeadlineSeconds` is unset; `vault.addr` is not https; the connector
is enabled with an ExternalSecret whose `session.json` does not read
`vault.sessionPath` property `session`. `scripts/test-chart-powerschool-login.sh`
pins all of this.

### What is in Vault

| Field | Content |
|-------|---------|
| `session` | The session JSON (`{"cookies":[...],"expires_at":"..."}`) as one string. This property becomes the connector's `session.json`. |
| `logged_in_at` | When the job logged in (RFC 3339, UTC). The next run computes the session's age from it; a secret written by hand without it uses the KV version's creation time. |
| `expires_at` | The same instant as inside `session`. |
| `selected_student_id` | The student the portal had selected right after login, or empty. Recorded here only; not logged, not synced to the connector. |

## First run

Do it outside 16:00-21:00 Pacific. Inside it the job refuses, which is
correct and proves nothing else.

1. Create the Vault policy and role (above). Confirm the ESO store can read
   the session path and that the credentials Secret has the three keys:

   ```bash
   kubectl -n glovebox get secret glovebox-powerschool-secrets \
     -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}'
   ```

2. Enable `powerschoolAuthRefresher` (and the connector) and let Flux apply
   it. Check what was rendered:

   ```bash
   kubectl -n glovebox get cronjob -l app.kubernetes.io/component=powerschool-auth-refresher \
     -o custom-columns=NAME:.metadata.name,SCHEDULE:.spec.schedule,TZ:.spec.timeZone,SUSPEND:.spec.suspend
   ```

   Until the first session is written the connector's
   `<fullname>-powerschool-files` ExternalSecret reports an error and the
   connector pod waits in `ContainerCreating` for its Secret. That is
   expected.

3. Tell the owner you are about to sign in to his account, then run the job
   once by hand (next section) and follow its log.

4. Expect, in order: `no session is stored yet; logging in`,
   `logging in to the portal` (`attempt=1`), `refresh complete`
   (`outcome=success`, `version=1`). Exit code 0.

5. Confirm the round trip without printing the session:

   ```bash
   vault kv get -field=logged_in_at secret/glovebox/powerschool/default/session
   kubectl -n glovebox get externalsecret -l glovebox.io/connector=powerschool
   kubectl -n glovebox get pods -l glovebox.io/connector=powerschool
   ```

   The ExternalSecret becomes `SecretSynced` within its refresh interval
   (5m) and the connector pod starts.

6. Run the job by hand a second time. It must log `session still good` and
   exit 0 without logging in. If it logs in again, stop and look: the
   home-page check is not recognising a good session.

7. Continue with the connector's own supervised first run
   ([powerschool.md](powerschool.md)).

## Running it once by hand

```bash
JOB=ps-login-manual-$(date +%s)
kubectl -n glovebox create job "$JOB" \
  --from=cronjob/glovebox-glovebox-powerschool-login-morning
kubectl -n glovebox logs -f "job/$JOB"
kubectl -n glovebox get job "$JOB"
```

The CronJob name is `<fullname>-powerschool-login-<schedules key>`;
`kubectl -n glovebox get cronjob` lists them.

A manual run is bound by the same rules as a scheduled one. Inside the
blackout it refuses. If the stored session is younger than 10h and still
works it does nothing -- which is the answer to "is the session all right?".

To make it log in although the session is young and working (after
replacing the password, say), the minimum age has to be zero for that one
run. Outside the blackout only:

```bash
kubectl -n glovebox create job ps-login-forced-$(date +%s) \
  --from=cronjob/glovebox-glovebox-powerschool-login-morning --dry-run=client -o yaml \
  | sed 's/value: 10h/value: 0s/' \
  | kubectl apply -f -
```

To stop the job logging in at all (wrong password, account trouble):

```bash
kubectl -n glovebox patch cronjob -l app.kubernetes.io/component=powerschool-auth-refresher \
  --type merge -p '{"spec":{"suspend":true}}'
```

Flux will revert that at its next reconcile unless the HelmRelease is
suspended or the value is changed in git.

## Outcomes

One JSON line per event on stderr. The last line of a run carries `outcome`.

| Exit | `outcome` | Meaning | What to do |
|------|-----------|---------|------------|
| 0 | `success` | Logged in, confirmed, written. | Nothing. |
| 0 | `session_still_good` | Stored session is young and accepted; no login. | Nothing. |
| 0 | `blackout_refused` | The clock was inside the window; nothing was contacted. | Find out what started it (below). |
| 1 | `transient_failure` | Browser, network or Vault unavailable; or the portal could not be asked about a young session. | Nothing; the next scheduled run retries. Look if it repeats. |
| 2 | `config_error` | A setting is missing or malformed; `problems` names it. | Fix the values. |
| 3 | `bad_credentials` | The portal rejected the username/password. | Suspend the CronJobs, correct the password in Vault, resume. Each further run is another bad submission. |
| 4 | `secret_read_failed` / `secret_write_failed` | Vault refused (policy, role binding, path). No login happens when this is detected beforehand. | Fix the policy or role. |
| 5 | `login_unconfirmed` | The browser login returned but its session does not open the home page. Nothing was written; the previous session stays in Vault. | See below. |

`login_unconfirmed` usually means the sign-in page changed (a new field, a
notice to accept, a captcha) and the library did not notice the login had
not happened. The owner signing in himself will show what the portal now
asks for.

No run is retried by Kubernetes. A failed Job stays visible for three days
(`ttlSecondsAfterFinished`).

## If it ran inside the blackout

**If the job refused** (`blackout_refused`, exit 0): nothing happened to
anyone. Nothing was contacted, nobody was signed out, the connector's
session is untouched. Find what started the run: a hand-made Job, a changed
`schedules` entry (the chart would have refused it, unless `blackout` was
changed too), or a `timeZone` problem (`kubectl get cronjob -o yaml`,
`.spec.timeZone`; a cluster older than 1.27 ignores the field and reads the
schedule in UTC).

**If a login did happen inside the window** (it cannot with the default
settings; it can if `blackout` was changed in values so that the binary's
window no longer covers the owner's hours):

- What the owner sees: if he was signed in to the portal at that moment,
  his next click lands on the sign-in page. Nothing is lost or changed in
  the account; he signs in again.
- His signing in again ends the job's new session. The connector's next
  poll is refused; the pod goes `Running`, `0/1 Ready`, logging `this
  session was already rejected upstream; waiting for a new one`. It contacts
  nothing while it waits.
- The evening run (21:30) finds the session gone and logs in; the connector
  resumes by itself and stages whatever changed in between.
- Then put `blackout` back, and tell the owner what happened.

## Not verified against the live portal

Everything here is tested without the portal: the blackout on both
daylight-saving changes, the young-session skip, the attempt ceiling, the
Vault read/write against an HTTP test server with TLS verification on, the
home-page read against a local TLS server, and that the connector's own
loader accepts what the job writes. What only the first supervised run can
show:

- that `powerschool-go`'s browser login works from the image (Chromium from
  apt, non-root, `--no-sandbox`, default seccomp) -- the image has not been
  run;
- that the portal's sign-in page is still the one the library drives;
- that the home page fetched by plain GET with the new cookies is the
  signed-in page (the connector relies on the same thing, verified live);
- that a second login really ends the first session every time (observed
  once), and how long a session lasts when nobody ends it.
