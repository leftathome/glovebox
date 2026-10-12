# Login broker: design note

A login broker is a small always-on service that logs in to one site, for
one account, when the connector for that site reports its session dead. It
holds the account's credentials so that the connector -- which parses pages
from the site and must be assumed compromisable -- never does.

Code: `internal/loginbroker` (the generic core), `internal/loginbroker/brokerapi`
(wire types and the connector's client), `internal/pslogin` (the PowerSchool
driver), `connectors/login-broker` (the binary and its image),
`charts/glovebox/templates/login-broker-*.yaml`.

## Why a broker

The PowerSchool portal keeps ONE live session per account and ends an idle
one in well under an hour. A scheduled login is therefore either too early
(dead by the time the poll runs) or has to be timed minutes before every
poll. The first on-demand design (the ConfigMap "request mailbox", glovebox
!97, still available as `connectors.powerschool.loginOnDemand`) has the
connector leave a note that a CronJob reads every three minutes. Its review
found four defects, all artefacts of polling a mailbox. The broker answers
synchronously and has none of them:

| Mailbox defect (bead glovebox-wekc.14) | Here |
|---|---|
| A request made outside the CronJob's windows is never read, and counts as a strike toward the connector's two-strike stop. | No windows. Every request is answered; "not now" is an answer (`deferred`), not a strike. |
| A session whose state cannot be established is skipped for ever, silently. | From the third poll in a row it is reported as a failed poll: an ERROR line and the skip metric (broker mode only). |
| A request stays valid (15 min) longer than the connector waits (10 min), so a late login can end the owner's session for nobody. | There is no pending request. A login that outlives its caller is stored and returned to the next one without a second login. |
| A login that fails before the credentials are submitted still spends the request and the 20-minute interval. | It costs a two-minute cooldown: no interval, no strike. |

## The request

```
POST /v1/session/refresh
Authorization: Bearer <ServiceAccount token minted for this broker's audience>
Content-Type: application/json

{"reason": "session_dead", "fingerprint": "5f2a9c0e11d24b7a"}
```

`reason` is one of `session_dead`, `no_session`, `session_expired`,
`auth_failure`. `fingerprint` is the first 16 hex digits of the caller's
fingerprint of the session it found dead, or `none`. Nothing else is
accepted: unknown fields, free text and bodies over 1 KiB are refused.

Every authenticated, well-formed request is answered with HTTP 200 and an
outcome:

| Outcome | Meaning | Session in the answer |
|---|---|---|
| `refreshed` | The broker logged in. | yes |
| `current` | The stored session is alive; no login. | yes |
| `deferred` | No login may be made now; `retry_after` says when. Code `blackout`, `min_interval`, `cooldown`, `login_in_progress` (another workload's login is in flight) or `state_conflict` (the record changed under this request). | no |
| `budget_exhausted` | Dead, and the day's logins are spent; `retry_after` is the next local midnight. | no |
| `latched` | Logins are stopped until a person clears the latch. Code `credentials_rejected` or `consecutive_failures`. | no |
| `inconclusive` | Could not tell whether the stored session is alive; no login on a guess. | no |
| `failed` | A login was attempted and produced no session, or could not be attempted. | no |

Anything else is 401 (no valid token for this audience), 403 (not an
allow-listed ServiceAccount), 400, 429 or 503 (the API server could not be
asked), with a one-word error code. `GET /healthz` is on both listeners;
`GET /metrics` is on a listener of its own so that a NetworkPolicy can admit
a scraper without admitting it to the endpoint above.

The session is returned as the fields the broker writes to Vault -- for
PowerSchool, `session` is the JSON document the connector already parses --
and is written to the same Vault path the scheduled login job uses, so ESO,
a restarted connector and any other reader stay correct.

## The driver contract

Everything about one site is behind `loginbroker.Driver`:

| Method | What it must do |
|---|---|
| `Site()` | The driver's name (`powerschool`). |
| `Login(ctx, creds)` | Log in and return the session. Submits the credentials AT MOST ONCE per call; never retries after a submission. |
| `Probe(ctx, session)` | One read-only look: `alive`, `dead` or `unknown`, with a reason from a fixed list. |
| `Encode` / `Decode` | The session as string fields for Vault and the answer, and back. |
| `Fingerprint(session)` | The same digest the site's connector computes. |

and, optionally, `Constraints()`: blackout windows, a minimum interval and a
login headroom that configuration can tighten but not loosen.

Rules the core relies on:

- A failed `Login` returns a `*LoginError` with one of three kinds.
  `LoginNotSubmitted` only when it is certain nothing was submitted;
  `LoginRejected` when the site refused the credentials; `LoginUnknown` for
  everything else. When in doubt, `LoginUnknown`: it is the kind that is
  never retried.
- `Probe` cannot write: its transport refuses every method but GET and HEAD.
  Apart from the credential submission inside `Login`, a driver sends the
  site nothing that changes state.
- A driver makes no request the core did not ask for. No keepalive.
- Nothing is logged by a driver, and no error, detail or reason quotes a
  credential, a cookie, a token or page content. The core logs a driver's
  classified `Detail` and never its error text.

The PowerSchool driver is the scheduled login job's own flow behind this
interface, shared rather than copied (`internal/pslogin`): the plain-GET
check that the sign-in page is the portal's own password form, one browser
login through powerschool-go, and the connector's own dead-session probe
(`powerschool.NewSessionProbe`), so the broker and the connector cannot
disagree about what "dead" means.

## The policy, and why

Enforced in the broker, in this order, for every request:

1. **Single flight.** Concurrent requests share one refresh and get the
   same answer. The refresh runs on its own deadline (6 min), not the
   caller's: a caller that gives up does not abort a login halfway.
2. **The record is read from Vault** (below), with its version. If it holds
   a FRESH attempt marker, somebody else -- a second pod during a restart,
   the scheduled login job -- has a login in flight: the answer is
   `deferred` / `login_in_progress`, and nothing is counted, written or
   sent to the site. A marker older than ten minutes is a login that died
   with its process: it counts as a failed login made at that time.
3. **The latch.** If it is set for the configured credentials: stop.
   Nothing is sent to the site.
4. **Probe before login.** The stored session is checked with one GET. If
   the site still accepts it, it is returned: nobody ever logs in over a
   live session, whatever the caller claims. If the question cannot be
   answered, stop (`inconclusive`): a login would end the session if it is
   alive. The caller's fingerprint is never by itself a reason to log in;
   it only lets a request about a session replaced within the last two
   minutes be answered without asking the site again.
5. **The gate**, on a clock read AFTER the probe, and again immediately
   before every call of the driver's login (the store and the probe take
   time; a first attempt can use most of its allowance):
   - **Blackout.** No login starts unless the whole login headroom from
     now is clear of every window. The headroom covers ONE WHOLE login
     attempt -- for PowerSchool the sign-in page check (30 s), the browser
     (90 s) and a margin (60 s): 180 s, so the last instant a login may
     start before 16:00 is 15:56:59 -- and each attempt is cancelled when
     it has run that long, so a submission cannot land inside a window.
     Windows are daily, on the wall clock of a named time zone, across
     daylight-saving changes. The account's owner uses the portal
     16:00-21:00 America/Los_Angeles and a login then signs them out; that
     window is compiled into the PowerSchool driver and cannot be
     configured away. The answer is `deferred` with the time the window
     ends. A live stored session is still returned during a blackout: the
     blackout forbids logins, not answers.
   - **Minimum interval** (default and PowerSchool floor: 20 min), measured
     from the last login by ANYBODY -- the record's `request_login_at` or
     the `logged_in_at` of the stored session. A session that died minutes
     after it was made was most likely ended by the owner signing in;
     logging in again at once would sign them out.
   - **Cooldown** (2 min) after a login that failed before the credentials
     were submitted. Kept in the record, so a restart does not shorten it.
   - **Daily budget.** At most 6 logins per local calendar day for the
     site (three poll windows, one retry each), counted in the record
     across restarts and across the broker and the scheduled job. Six is a
     compiled-in ceiling; configuration can lower it and cannot raise it.
     Over budget the answer is `budget_exhausted` with the next local
     midnight. This is the bound that still holds when the others have
     been talked round -- a caller that restarts, an owner and a connector
     signing each other out all day.
6. **One submission per attempt.** The attempt marker is written BEFORE the
   login, check-and-set against the version read in step 2. If that write
   is refused there is no login: either the outcome could not be recorded
   afterwards, or somebody changed the record since it was read and
   everything above was decided on a stale read (`deferred` /
   `state_conflict`). `Login` is called once; once more only after
   `LoginNotSubmitted`. The new session is confirmed with the probe and
   only then stored.
7. **The failure latch.** ANY rejection of the credentials sets it at once;
   so do `maxFailures` (default 2) logins in a row that did not end with a
   confirmed session. Repeating a wrong password is how an account gets
   locked, and a sign-in page that now wants a CAPTCHA or a new password
   looks like a login that "worked" without a session. Only a person clears
   it.
8. **No traffic without a request.** No warm-up at startup, no periodic
   check, no keepalive: `/healthz` asks the broker, never the site.

### What is kept, and where

In Vault, beside the session, at `vault.statePath` (default: `login-state`
next to the session). It is read at every request, so a restarted pod
forgets nothing and a person's change takes effect without a restart.

The scheduled PowerSchool login job reads and writes the SAME record. Every
write of it, by either workload, is **check-and-set** in Vault against the
version the writer read: two writers can never both act on the same read.
The second is refused and reads again, and a refused write before a login
means no login. Both write the attempt marker before a login and neither
logs in while the other's marker is fresh. A latch set by either stops
both; a login by either counts for the other's interval and for the day.

| Field | Content |
|---|---|
| `login_blocked`, `blocked_reason`, `blocked_at` | The latch. Reasons: `credentials_rejected`, `consecutive_failures` (and the scheduled job's `login_unconfirmed`). |
| `credential_fingerprint`, `salt` | Salted SHA-256 of the credentials last submitted; never the values. A latch applies only while the configured credentials still match it. |
| `unconfirmed_count` | Failed logins in a row. |
| `request_login_at` | When a login was last made on request; the interval is measured from it. |
| `attempt_started_at` | Set immediately before a login, cleared when its outcome is recorded. Fresh (under 10 min): a login is in flight. |
| `cooldown_until` | No login before this time (after a failure before the submit). |
| `login_day`, `login_day_count` | The local calendar day and how many logins were made on it. |

**Clearing the latch.** First find out why it was set (sign in to the site
yourself). Then either change the credentials (the pod must restart to read
them; the latch no longer matches and lifts), or delete the record with an
operator token: `vault kv metadata delete -mount=secret glovebox/powerschool/default/login-state`.
Deleting the record also resets the day's count. Do not clear it to retry
the same rejected password.

### The connector's side

With a broker the PowerSchool connector never exits over a login that
cannot be had. An exit is a restart, and a restart loop is a loop of polls:
it would put a request to the portal with a dead session on a timer, round
the clock. Instead:

- A session found dead is recorded as dead in the connector's checkpoint.
  While that session is still the one in place, later polls -- and the
  process after a restart -- do not ask the portal about it again.
- Each poll asks the broker at most once. After `deferred` or
  `budget_exhausted` it does not ask again before `retry_after` (believed
  for at most a day). After `latched` it asks once per poll window and
  sends the portal nothing.
- Two sessions in a row that the broker delivered and the portal then
  refused stop the asking altogether. That count is in the checkpoint and
  is not tied to which session is in place, so a restart does not undo it;
  a session the portal accepts, or `POWERSCHOOL_LOGIN_BACKOFF_RESET`,
  clears it.

What tells an operator is the ERROR line at every poll,
`powerschool_polls_skipped_total{reason}`, and the broker's
`login_broker_latched` and `login_broker_refresh_total{outcome}`.

## Threat model

**Who can trigger a login.** A request is acted on only if all of these hold:

- it comes from a pod the broker's NetworkPolicy admits (the connector's
  pods; the broker speaks plain HTTP and that policy is what makes it
  acceptable);
- it carries a ServiceAccount token that the Kubernetes API server
  (TokenReview) vouches for, minted for THIS broker's audience. The
  connector's pod holds only such a token, projected with a ten-minute
  lifetime; its ordinary API token is not mounted. A token for the API
  server, or for another broker, fails here;
- the token's ServiceAccount is on the broker's allow-list. Any pod can ask
  for a token with any audience for its OWN ServiceAccount; it cannot be a
  ServiceAccount it is not.

Even then the request does not cause a login unless the site itself refuses
the stored session, outside the blackout, outside the interval, with the
latch clear.

**What a compromised connector can do.** Ask. That is all: it has no
credentials, no Role in the Kubernetes API, no Vault access. Asking while
the session is alive returns the session it already has. By making its
session die (it cannot, short of the owner signing in) or waiting for it to
idle out, it can cause at most one login per minimum interval, never in the
blackout, and never after a rejection. It learns the session cookies -- which
it is given anyway -- and never the password. It cannot make the broker
contact another host: the request carries no address. It can cause one
probe GET per request; an authenticated caller is rate limited (one request
per second sustained), and whatever it does, the day's logins are bounded
by the daily budget.

**What a compromised broker exposes.** One account: its password, and that
account's session and login record in Vault. Not another site's: each
instance has its own ServiceAccount, Vault role and credentials Secret, and
the chart refuses two instances sharing a role or a session path. Its egress
is DNS, Vault, the API server and the site on 443, with private ranges
carved out.

**The browser inside the broker.** The PowerSchool driver starts a Chromium
without Chromium's own sandbox, as the same user as the broker, and that
browser parses whatever the site sends. What it can and cannot reach:

- *The password.* It arrives in the process environment. `os.Unsetenv`
  keeps a child from inheriting it but does not change
  `/proc/<pid>/environ`, so the broker also marks itself non-dumpable
  (`prctl(PR_SET_DUMPABLE, 0)`) before it reads its configuration: the
  kernel then refuses a same-user process access to the broker's
  `/proc/<pid>/environ` and memory. Reading the credentials from a mounted
  file instead was considered and is no better: with one user in the pod, a
  file the broker can read the browser can read too, for as long as it is
  mounted.
- *The ServiceAccount token* -- NOT protected. It is a file the pod's user
  can read. Code running in the browser could read it, log in to Vault as
  this instance (Vault is within the pod's egress) and read or overwrite
  this account's session and login record: clear the latch, reset the
  day's count. It could also present the token to the Kubernetes API,
  where the instance may only create TokenReviews. This is a real residual
  exposure of running the browser in the broker's pod. The login record's
  guards protect against mistakes and against a compromised CONNECTOR; they
  do not survive code execution in the BROWSER. Closing it needs the
  browser under another identity -- its own container and user, or a remote
  browser -- which is not built. The Vault role should in any case carry
  no `delete` and nothing outside the two paths.
- *Leftovers.* A browser profile holds session cookies. The driver removes
  its profile on every way out of a login, and removes any browser profile
  or scratch directory found in the temporary directory at startup and
  after every attempt.

**Other pods; the network.** Another pod in the namespace is refused by the
NetworkPolicy before the token is looked at. Should one reach the port
anyway, its junk tokens are limited per source address BEFORE
authentication -- so it cannot use up an allowance the connector needs --
and the shared limit sits behind authentication, where only an allow-listed
caller can spend it. A token read off the wire is
valid for ten minutes, against this broker only, from an admitted pod only;
what it buys is the list above under "compromised connector". The answer
carries the session in the clear over the pod network: if that is not
acceptable for a site, put the instances behind a mesh or add TLS at the
listener (not built).

**What is never logged.** Credentials, cookies, tokens, page content, a
driver's error text. Outcomes, codes and reasons are from fixed lists. The
one free text is a Vault error, with the username and password removed and
bounded in length. `connectors/login-broker/main_test.go` runs a whole life
of the service over real HTTP and scans every log line, the metrics page
and every refusal for each secret it handled.

## Deploying an instance

`loginBrokers.<name>` in the chart: a Deployment (1 replica, `Recreate`),
Service, ServiceAccount, credentials ExternalSecret, NetworkPolicy and a
ClusterRole that allows TokenReview and nothing else. Disabled by default.
The connector is wired with `connectors.powerschool.loginBroker.enabled`.

The chart refuses to render:

- `loginOnDemand` and `loginBroker` together for the connector, and a
  broker together with `powerschoolAuthRefresher.onRequest` for the same
  session;
- a broker beside LIVE `powerschoolAuthRefresher.schedules` for the same
  session, unless `acknowledgeScheduledLogins: true` (suspended schedules,
  kept as templates for a manual run, are fine);
- a broker WITHOUT its NetworkPolicy -- the chart-wide switch or the
  instance's own turned off -- unless `acknowledgeNoNetworkPolicy: true`
  says an equivalent policy exists elsewhere;
- two brokers sharing a Vault role or a session path;
- a daily budget above 6.

Not in this chart, and required before enabling an instance: a Vault policy
and Kubernetes auth role bound to that instance's ServiceAccount
(`<fullname>-login-broker-<name>`) and to no other:

```hcl
# glovebox-login-broker-powerschool.hcl
path "secret/data/glovebox/powerschool/default/session"     { capabilities = ["create", "read", "update"] }
path "secret/data/glovebox/powerschool/default/login-state" { capabilities = ["create", "read", "update"] }
```

and `networkPolicy.kubernetesAPICIDRs` for the instance (the API server's
addresses; the render fails without them).

## Adding a second site

1. Write a package that implements `Driver` for the site, with tests
   against an `httptest` server. Decide, per failure the site's login can
   produce, which of the three `LoginError` kinds it is -- conservatively.
2. Register it in `connectors/login-broker/drivers.go`.
3. Add a `loginBrokers.<site>` entry: its own `site`, `connector`, Vault
   role, session path, credentials and egress. Nothing is shared with the
   other instance but the image.
4. Teach that site's connector to call `brokerapi.Client` when it finds its
   session dead, and to compute the same session fingerprint as the driver.

For Schoology specifically (`connectors/schoology-auth-refresher` today;
NOT ported in this change), a driver would need:

- **Login** through schoology-go's `auth.LoginWithPassword`, which drives a
  REMOTE browser (Browserless) rather than a local Chromium: the instance
  needs `BROWSERLESS_URL` / `BROWSERLESS_TOKEN` and an egress rule to it
  (`networkPolicy.extraEgress`); the Chromium in the image goes unused.
- **A failure classification.** The existing job maps the library's errors
  onto exit codes (bad credentials = 3) and retries the whole Job three
  times; a driver must instead say which failures are certainly before the
  submit. That has not been worked out for the Drupal login form.
- **A probe.** The existing job has none: it logs in on every run. A
  read-only "is this session accepted" check has to be written, on a
  GET-only transport.
- **Encode/Decode for a session that is several fields**, not one document:
  the job writes five fields (`credentialsToMap`). The core and
  `loginvault.ReadFields` already carry arbitrary string fields.
- **Its own constraints.** No owner-shared blackout is known for that
  account, and its session lifetime is different; neither is assumed here.

## Known limits

- One replica, by design, and no leader election. What keeps two writers
  apart is in Vault, not in the Deployment: every write of the login record
  is check-and-set, the attempt marker makes a login in flight visible, and
  the session write is check-and-set too. `Recreate` only keeps the overlap
  short. A second writer that ignores the record entirely -- a person
  logging in by hand, a workload that does not use `internal/loginvault` --
  is not guarded against.
- The browser shares the broker's pod and user: see "The browser inside the
  broker". The password is kept out of its reach; the ServiceAccount token,
  and with it this account's Vault paths, is not.
- The attempt marker is honoured for ten minutes. A broker pod killed in
  the middle of a login therefore answers `login_in_progress` for up to ten
  minutes after it comes back, and then counts the login as failed.
- `selected_student_id` is written empty by the broker (the scheduled job
  records it). Nothing reads it.
- The sign-in page is checked with a GET before the browser opens it again;
  what the browser gets could differ. Closing that needs the check inside
  the browser session, which is the library's to add (same as the scheduled
  job).
- If the login succeeds and Vault cannot be written, the caller still gets
  the session (code `session_not_stored`) and the broker offers it to Vault
  again at the next request; a pod restart in between loses it, and the
  next login waits out the interval.
- The scheduled job counts its logins into the daily count but is not
  itself stopped by the budget: its schedule is the operator's own.
