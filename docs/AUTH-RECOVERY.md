# Auth Recovery: Schoology Session Expiry

Operator procedure for recovering the Schoology connector after a session
cookie expires.

## When this applies

The Schoology connector authenticates using browser-session cookies
captured from a parent's Schoology account. Schoology does not expose
OAuth, API tokens, or service accounts to parent accounts, and the
cookies expire approximately every **14 days**. The library does not
auto-refresh.

When the session expires, the connector pod surfaces a `PermanentError`
with a recovery-instruction message and exits non-zero. Kubernetes then
reports `CrashLoopBackOff`, which should fire whatever external alert
you have wired to that condition (Prometheus alert on
`kube_pod_status_phase`, Alertmanager, etc.).

If you see one of the following in `kubectl logs`, follow this procedure:

```
Schoology session expired. To recover:
  1. On your workstation: schoology-go auth.Login <SCHOOLOGY_HOST>
  ...
  See docs/AUTH-RECOVERY.md for the full procedure.
```

Expected operator wall-clock time: 5-10 minutes (most of it waiting for
ESO to sync).

## Recovery procedure

### 1. Detect

Confirm the failure mode is session expiry, not a different
`PermanentError`:

```bash
kubectl logs -n glovebox deploy/glovebox-schoology --tail=50
```

You should see the `Schoology session expired` message above. If you
see a different `PermanentError` (config validation, schema-drift
escalation, etc.), this document does not apply; investigate
separately.

### 2. Re-authenticate on your workstation

The `schoology-go` library exposes `auth.Login` as a Go function -- there
is no standalone CLI binary. The simplest way to run it is a short
helper program. If your repo doesn't already have one, this is the
minimum viable form:

```go
package main

import (
    "context"
    "log"
    "os"

    "github.com/leftathome/schoology-go/auth"
)

func main() {
    host := os.Args[1] // e.g. "yourschool.schoology.com"
    out := os.Args[2]  // output path for the JSON, e.g. "/tmp/schoology-session.json"

    creds, err := auth.Login(context.Background(), host)
    if err != nil {
        log.Fatal(err)
    }
    if err := auth.SaveCredentials(out, creds); err != nil {
        log.Fatal(err)
    }
}
```

Run it:

```bash
go run ./hack/schoology-login yourschool.schoology.com /tmp/schoology-session.json
```

A visible Chromium window opens. Complete whatever login flow your
district uses (SSO, MFA, native password). The library captures the
session when the browser lands on the post-login home page, then writes
a 5-field JSON file (`host`, `sess_id`, `csrf_token`, `csrf_key`, `uid`)
to the path you specified, mode `0600`. **All five fields are required**
by `auth.LoadCredentials` validation; do not strip any of them when
copying into 1Password.

Notes:
- The first run downloads a Chromium build into go-rod's cache
  (~140 MB). Subsequent runs reuse it.
- If your district has a clean native-password form with no SSO
  redirect, you can use `auth.LoginWithPassword(ctx, host, user, pass)`
  for a headless flow instead. Most districts will not.

### 3. Inspect the credentials file (optional but recommended)

```bash
cat /tmp/schoology-session.json
```

It should be a JSON object with exactly five string fields (`host`,
`sess_id`, `csrf_token`, `csrf_key`, `uid`). If it looks
empty or malformed, the browser flow did not complete successfully
(common cause: closed the window before landing on `/home`); re-run.

### 4. Update the 1Password item

Open the 1Password item named `schoology-session-<household>` (where
`<household>` matches the `household` value in your connector config).
Replace its contents entirely with the JSON you just wrote.

If you store the JSON in a structured 1Password field rather than a
single notes blob, replace each of the four sub-fields. Whichever shape
your ExternalSecret template expects.

### 5. Wait for ESO to sync

External Secrets Operator polls 1Password on its configured refresh
interval (default ~60 seconds for this connector's `ExternalSecret`).
The K8s Secret will update, and because the Deployment template carries
a checksum annotation on the Secret, the pod will be re-rolled
automatically.

Watch:

```bash
kubectl get externalsecret -n glovebox schoology-session -w
```

Look for `STATUS=SecretSynced` and a fresh `LAST SYNC` timestamp. Then:

```bash
kubectl rollout status -n glovebox deploy/glovebox-schoology
```

### 6. Verify

Tail the logs of the freshly-rolled pod:

```bash
kubectl logs -n glovebox deploy/glovebox-schoology --tail=50 -f
```

You should see normal startup (config loaded, scheduler started, first
poll completed) and no `session expired` errors. The connector resumes
from the last checkpoint, so any items posted to Schoology during the
outage are caught up on the next poll cycle.

Securely delete the workstation copy of the JSON:

```bash
shred -u /tmp/schoology-session.json   # Linux
# or: rm -P /tmp/schoology-session.json   # macOS
```

## Troubleshooting

### 1Password item not found

ESO logs an `ItemNotFound` (or vendor-specific equivalent) error. Check
that the item name matches the `household` value in `config.json`
exactly (case-sensitive). The expected name is
`schoology-session-<household>`. Example: `household: "smith"` →
`schoology-session-smith`.

```bash
kubectl describe externalsecret -n glovebox schoology-session
```

### ESO sync stuck

If the `ExternalSecret` status never advances past `SecretSyncedError`
or the `LAST SYNC` timestamp is stale, ESO is not reaching 1Password.
Check the operator and the `ClusterSecretStore`:

```bash
kubectl get clustersecretstore
kubectl describe clustersecretstore <your-1password-store>
kubectl logs -n external-secrets deploy/external-secrets -f
```

Common causes: expired 1Password service-account token, network policy
blocking egress from the ESO pod, 1Password Connect server down.

### Pod doesn't restart after Secret updates

Check that the Deployment carries a checksum annotation on the Secret.
Without it, K8s does not roll the Deployment when a mounted Secret
changes. As a one-time workaround:

```bash
kubectl rollout restart -n glovebox deploy/glovebox-schoology
```

Then fix the chart so future recoveries are automatic.

### Re-auth succeeded but pod still loops on session-expired

Possible causes:
- The JSON in 1Password is wrapped (e.g. base64-encoded, quoted-string)
  in a way the ExternalSecret template doesn't unwrap. Check the
  rendered Secret with `kubectl get secret -n glovebox schoology-session
  -o jsonpath='{.data.credentials\.json}' | base64 -d` and confirm it
  is the raw 5-field JSON the library expects (`host`, `sess_id`,
  `csrf_token`, `csrf_key`, `uid` -- all required).
- You authenticated as a different Schoology account (e.g. one without
  parent visibility into the kids' courses). Check the `uid` in the
  JSON matches the parent account you intend to use.
