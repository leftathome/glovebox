#!/usr/bin/env bash
# Render-level assertions for the PowerSchool session login job
# (charts/glovebox/templates/powerschool-auth-refresher-*.yaml).
#
# The job logs in to a real parent account that its owner also uses, and a
# login ends the owner's session. What keeps it from doing that at the wrong
# time is partly in the rendered manifests -- the schedule and its zone, the
# catch-up deadline, no retries, the blackout handed to the binary -- and
# partly in the render refusing a schedule that can fire inside the blackout.
# Both are pinned here.
#
# Requires helm and python3 with PyYAML. Usage: scripts/test-chart-powerschool-login.sh
set -euo pipefail

CHART="$(cd "$(dirname "$0")/.." && pwd)/charts/glovebox"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0

render() {
  helm template glovebox "$CHART" -n glovebox "$@"
}

# check NAME PYTHON_EXPR [helm args...]
#
# The expression sees:
#   docs   every rendered object
#   mine   those labelled component=powerschool-auth-refresher
#   crons  {schedules key: CronJob}
#   env    {schedules key: {env name: value, or (secret, key) for a secretKeyRef}}
#   kinds  sorted [kind/name] of `mine`
check() {
  local name="$1"; shift
  local expr="$1"; shift
  if render "$@" 2>"$TMP/err" | python3 -c "
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
def comp(d):
    return (d.get('metadata', {}).get('labels') or {}).get('app.kubernetes.io/component')
mine = [d for d in docs if comp(d) == 'powerschool-auth-refresher']
kinds = sorted(d['kind'] + '/' + d['metadata']['name'] for d in mine)
crons = {d['metadata']['labels']['glovebox.io/powerschool-login-schedule']: d
         for d in mine if d['kind'] == 'CronJob'}
def pod(c):
    return c['spec']['jobTemplate']['spec']['template']['spec']
def container(c):
    return pod(c)['containers'][0]
def envmap(c):
    out = {}
    for e in container(c).get('env', []):
        if 'value' in e:
            out[e['name']] = e['value']
        else:
            ref = e['valueFrom']['secretKeyRef']
            out[e['name']] = (ref['name'], ref['key'])
    return out
env = {k: envmap(c) for k, c in crons.items()}
def find(kind, name):
    hits = [d for d in docs if d['kind'] == kind and d['metadata']['name'] == name]
    return hits[0] if hits else None
sys.exit(0 if ($expr) else 1)
"; then
    echo "ok   $name"
  else
    echo "FAIL $name"; cat "$TMP/err"; FAILED=1
  fi
}

# refuse NAME PATTERN [helm args...]: the render must fail with PATTERN.
refuse() {
  local name="$1"; shift
  local pattern="$1"; shift
  if render "$@" >/dev/null 2>"$TMP/err"; then
    echo "FAIL $name (rendered; expected a refusal)"; FAILED=1
  elif grep -q -- "$pattern" "$TMP/err"; then
    echo "ok   $name"
  else
    echo "FAIL $name (failed for another reason)"; cat "$TMP/err"; FAILED=1
  fi
}

# accept NAME [helm args...]: the render must succeed.
accept() {
  local name="$1"; shift
  if render "$@" >/dev/null 2>"$TMP/err"; then
    echo "ok   $name"
  else
    echo "FAIL $name (refused)"; cat "$TMP/err"; FAILED=1
  fi
}

ON=(--set powerschoolAuthRefresher.enabled=true)
morning() { printf '%s' "powerschoolAuthRefresher.schedules.morning.schedule=$1"; }

# --- off by default ---------------------------------------------------------

check "disabled by default: nothing is rendered" 'mine == []'

# --- what is rendered -------------------------------------------------------

check "enabled: two CronJobs, a ServiceAccount, a credentials ExternalSecret, a NetworkPolicy" \
  'kinds == ["CronJob/glovebox-glovebox-powerschool-login-evening",
             "CronJob/glovebox-glovebox-powerschool-login-morning",
             "ExternalSecret/glovebox-glovebox-powerschool-refresher-credentials",
             "NetworkPolicy/glovebox-glovebox-powerschool-auth-refresher",
             "ServiceAccount/glovebox-glovebox-powerschool-auth-refresher"]' \
  "${ON[@]}"

check "default schedules: 05:00 Mon-Fri and 21:30 Sun-Thu" \
  'crons["morning"]["spec"]["schedule"] == "0 5 * * 1-5" and crons["evening"]["spec"]["schedule"] == "30 21 * * 0-4"' \
  "${ON[@]}"

check "every CronJob carries the Pacific time zone" \
  'all(c["spec"]["timeZone"] == "America/Los_Angeles" for c in crons.values())' \
  "${ON[@]}"

check "concurrencyPolicy is Forbid" \
  'all(c["spec"]["concurrencyPolicy"] == "Forbid" for c in crons.values())' \
  "${ON[@]}"

check "a missed run may start at most 30 minutes late" \
  'all(c["spec"]["startingDeadlineSeconds"] == 1800 for c in crons.values())' \
  "${ON[@]}"

check "a failed run is never retried: backoffLimit 0, restartPolicy Never" \
  'all(c["spec"]["jobTemplate"]["spec"]["backoffLimit"] == 0 and pod(c)["restartPolicy"] == "Never" for c in crons.values())' \
  "${ON[@]}"

check "the blackout is handed to the binary" \
  'all(e["POWERSCHOOL_BLACKOUT_START"] == "16:00" and e["POWERSCHOOL_BLACKOUT_END"] == "21:00"
       and e["POWERSCHOOL_BLACKOUT_TZ"] == "America/Los_Angeles" for e in env.values())' \
  "${ON[@]}"

check "minimum session age: 10h in the morning, 30h in the evening" \
  'env["morning"]["POWERSCHOOL_MIN_SESSION_AGE"] == "10h" and env["evening"]["POWERSCHOOL_MIN_SESSION_AGE"] == "30h"' \
  "${ON[@]}"

check "Vault: https, dedicated role, the one session path" \
  'all(e["VAULT_ADDR"].startswith("https://") and e["VAULT_K8S_ROLE"] == "glovebox-powerschool-refresher"
       and e["VAULT_KV_MOUNT"] == "secret" and e["VAULT_SESSION_PATH"] == "glovebox/powerschool/default/session"
       for e in env.values())' \
  "${ON[@]}"

check "no env var switches certificate verification off" \
  'all("VAULT_SKIP_VERIFY" not in e for e in env.values())' \
  "${ON[@]}"

check "credentials come from the Secret key by key, never as plain values or envFrom" \
  'all(e["POWERSCHOOL_USERNAME"] == ("glovebox-glovebox-powerschool-refresher-credentials", "POWERSCHOOL_USERNAME")
       and e["POWERSCHOOL_PASSWORD"] == ("glovebox-glovebox-powerschool-refresher-credentials", "POWERSCHOOL_PASSWORD")
       and e["POWERSCHOOL_HOSTNAME"] == ("glovebox-glovebox-powerschool-refresher-credentials", "POWERSCHOOL_HOSTNAME")
       for e in env.values())
   and all("envFrom" not in container(c) and "args" not in container(c) and "command" not in container(c) for c in crons.values())' \
  "${ON[@]}"

check "the credentials ExternalSecret reads the three account properties" \
  '[(x["secretKey"], x["remoteRef"]["key"], x["remoteRef"]["property"])
    for x in find("ExternalSecret", "glovebox-glovebox-powerschool-refresher-credentials")["spec"]["data"]] ==
   [("POWERSCHOOL_HOSTNAME", "eso/glovebox-powerschool", "powerschool_hostname"),
    ("POWERSCHOOL_USERNAME", "eso/glovebox-powerschool", "powerschool_username"),
    ("POWERSCHOOL_PASSWORD", "eso/glovebox-powerschool", "powerschool_password")]' \
  "${ON[@]}"

check "an existing credentials Secret replaces the ExternalSecret" \
  'find("ExternalSecret", "glovebox-glovebox-powerschool-refresher-credentials") is None
   and all(e["POWERSCHOOL_PASSWORD"] == ("glovebox-powerschool-secrets", "POWERSCHOOL_PASSWORD")
           and e["POWERSCHOOL_HOSTNAME"] == ("glovebox-powerschool-secrets", "POWERSCHOOL_HOSTNAME") for e in env.values())' \
  "${ON[@]}" --set powerschoolAuthRefresher.credentials.existingSecret=glovebox-powerschool-secrets

check "the pod runs as the dedicated ServiceAccount, non-root, without privileges" \
  'all(pod(c)["serviceAccountName"] == "glovebox-glovebox-powerschool-auth-refresher"
       and pod(c)["securityContext"]["runAsNonRoot"] is True and pod(c)["securityContext"]["runAsUser"] == 65532
       and container(c)["securityContext"]["allowPrivilegeEscalation"] is False
       and container(c)["securityContext"]["capabilities"]["drop"] == ["ALL"] for c in crons.values())' \
  "${ON[@]}"

check "the Vault CA bundle is mounted and named when configured" \
  'all(e["VAULT_CACERT"] == "/etc/vault-ca/tls-ca-bundle.pem" for e in env.values())
   and all({"name": "vault-ca", "configMap": {"name": "vault-ca-bundle"}} in pod(c)["volumes"]
           and {"name": "vault-ca", "mountPath": "/etc/vault-ca", "readOnly": True} in container(c)["volumeMounts"]
           for c in crons.values())' \
  "${ON[@]}" --set powerschoolAuthRefresher.vault.caConfigMap.name=vault-ca-bundle \
  --set powerschoolAuthRefresher.vault.caConfigMap.key=tls-ca-bundle.pem

check "without a CA ConfigMap no VAULT_CACERT is set" \
  'all("VAULT_CACERT" not in e for e in env.values())' \
  "${ON[@]}"

check "the image tag defaults to the chart's appVersion" \
  'all(container(c)["image"].startswith("ghcr.io/leftathome/glovebox-powerschool-auth-refresher:")
       and not container(c)["image"].endswith(":") for c in crons.values())' \
  "${ON[@]}"

check "a schedule set to null is removed; the other stays" \
  'sorted(crons) == ["morning"]' \
  "${ON[@]}" --set powerschoolAuthRefresher.schedules.evening=null

check "a schedule can be added from an overlay" \
  'sorted(crons) == ["evening", "morning", "saturday"] and env["saturday"]["POWERSCHOOL_MIN_SESSION_AGE"] == "12h"' \
  "${ON[@]}" --set-string 'powerschoolAuthRefresher.schedules.saturday.schedule=0 7 * * 6' \
  --set powerschoolAuthRefresher.schedules.saturday.minSessionAge=12h

# --- network policy ---------------------------------------------------------

NP='find("NetworkPolicy", "glovebox-glovebox-powerschool-auth-refresher")'

check "network policy: no ingress; egress to DNS, Vault 8200 and https only" \
  "$NP[\"spec\"][\"ingress\"] == [] and sorted($NP[\"spec\"][\"policyTypes\"]) == [\"Egress\", \"Ingress\"]
   and sorted(p[\"port\"] for rule in $NP[\"spec\"][\"egress\"] for p in rule[\"ports\"]) == [53, 53, 443, 8200]" \
  "${ON[@]}"

check "network policy: Vault is reached in its own namespace only" \
  "[rule[\"to\"] for rule in $NP[\"spec\"][\"egress\"] if rule[\"ports\"][0][\"port\"] == 8200] ==
   [[{\"namespaceSelector\": {\"matchLabels\": {\"kubernetes.io/metadata.name\": \"vault\"}}}]]" \
  "${ON[@]}"

check "network policy: the portal range can be narrowed" \
  "[rule[\"to\"] for rule in $NP[\"spec\"][\"egress\"] if rule[\"ports\"][0][\"port\"] == 443] ==
   [[{\"ipBlock\": {\"cidr\": \"203.0.113.0/24\"}}]]" \
  "${ON[@]}" --set 'powerschoolAuthRefresher.networkPolicy.portalCIDRs={203.0.113.0/24}'

check "network policy: selects the job's pods and nothing else" \
  "$NP[\"spec\"][\"podSelector\"][\"matchLabels\"][\"app.kubernetes.io/component\"] == \"powerschool-auth-refresher\"
   and all(pod_labels[\"app.kubernetes.io/component\"] == \"powerschool-auth-refresher\"
           for pod_labels in [c[\"spec\"][\"jobTemplate\"][\"spec\"][\"template\"][\"metadata\"][\"labels\"] for c in crons.values()])" \
  "${ON[@]}"

check "network policy: can be switched off for the job" \
  "$NP is None and len(crons) == 2" \
  "${ON[@]}" --set powerschoolAuthRefresher.networkPolicy.enabled=false

check "network policy: follows the chart-wide switch" \
  "$NP is None" \
  "${ON[@]}" --set networkPolicy.enabled=false

# --- the connector receives what the job writes -----------------------------

cat >"$TMP/both.yaml" <<'YAML'
powerschoolAuthRefresher:
  enabled: true
connectors:
  powerschool:
    enabled: true
    externalSecret:
      enabled: true
      secretStoreRef:
        name: vault-default
YAML

FILES='find("ExternalSecret", "glovebox-glovebox-powerschool-files")'
DEPLOY='find("Deployment", "glovebox-glovebox-powerschool")["spec"]["template"]["spec"]'

check "the connector's session.json is the property the job writes" \
  "[(x[\"secretKey\"], x[\"remoteRef\"][\"key\"], x[\"remoteRef\"][\"property\"]) for x in $FILES[\"spec\"][\"data\"]] ==
   [(\"session.json\", env[\"morning\"][\"VAULT_SESSION_PATH\"], \"session\")]" \
  -f "$TMP/both.yaml"

check "the connector mounts that Secret where POWERSCHOOL_SESSION_FILE points" \
  "{\"name\": \"secret-files\", \"secret\": {\"secretName\": \"glovebox-glovebox-powerschool-files\", \"defaultMode\": 288}} in $DEPLOY[\"volumes\"]
   and {\"name\": \"secret-files\", \"mountPath\": \"/etc/powerschool\", \"readOnly\": True} in $DEPLOY[\"containers\"][0][\"volumeMounts\"]
   and {\"name\": \"POWERSCHOOL_SESSION_FILE\", \"value\": \"/etc/powerschool/session.json\"} in $DEPLOY[\"containers\"][0][\"env\"]" \
  -f "$TMP/both.yaml"

check "the connector is never given the username or the password" \
  "all(\"USERNAME\" not in x[\"secretKey\"] and \"PASSWORD\" not in x[\"secretKey\"]
       for name in [\"glovebox-glovebox-powerschool-files\", \"glovebox-glovebox-powerschool-secret\"]
       for x in find(\"ExternalSecret\", name)[\"spec\"][\"data\"])" \
  -f "$TMP/both.yaml"

refuse "a connector reading another path than the job writes is refused" \
  "the connector would never receive the session" \
  -f "$TMP/both.yaml" --set powerschoolAuthRefresher.vault.sessionPath=glovebox/powerschool/other/session

refuse "a connector reading another property is refused" \
  "the connector would never receive the session" \
  -f "$TMP/both.yaml" --set 'connectors.powerschool.externalSecret.files.data.session\.json.property=cookies'

# --- schedules the render refuses -------------------------------------------

INSIDE="inside the login blackout"

refuse "16:00 exactly"                    "$INSIDE" "${ON[@]}" --set-string "$(morning '0 16 * * *')"
refuse "18:30"                            "$INSIDE" "${ON[@]}" --set-string "$(morning '30 18 * * 1-5')"
refuse "20:59"                            "$INSIDE" "${ON[@]}" --set-string "$(morning '59 20 * * *')"
refuse "an hour list reaching into it"    "$INSIDE" "${ON[@]}" --set-string "$(morning '0 5\,17 * * *')"
refuse "an hour range reaching into it"   "$INSIDE" "${ON[@]}" --set-string "$(morning '0 9-17 * * *')"
refuse "a stepped hour reaching into it"  "$INSIDE" "${ON[@]}" --set-string "$(morning '0 */4 * * *')"
refuse "every hour"                       "$INSIDE" "${ON[@]}" --set-string "$(morning '15 * * * *')"
refuse "a from-hour step"                 "$INSIDE" "${ON[@]}" --set-string "$(morning '0 6/5 * * *')"
refuse "the evening entry too"            "$INSIDE" "${ON[@]}" --set-string 'powerschoolAuthRefresher.schedules.evening.schedule=30 20 * * 0-4'
refuse "an added entry too"               "$INSIDE" "${ON[@]}" --set-string 'powerschoolAuthRefresher.schedules.extra.schedule=0 17 * * 6' \
  --set powerschoolAuthRefresher.schedules.extra.minSessionAge=10h

refuse "15:45 with a 30-minute catch-up deadline" "before the login blackout" \
  "${ON[@]}" --set-string "$(morning '45 15 * * *')"
refuse "15:30 with a 30-minute catch-up deadline (could start at 16:00:00)" "before the login blackout" \
  "${ON[@]}" --set-string "$(morning '30 15 * * *')"
refuse "a 12-hour catch-up deadline on the 05:00 run" "before the login blackout" \
  "${ON[@]}" --set powerschoolAuthRefresher.startingDeadlineSeconds=43200
refuse "no catch-up deadline at all" "startingDeadlineSeconds must be set" \
  "${ON[@]}" --set powerschoolAuthRefresher.startingDeadlineSeconds=null

refuse "a macro schedule"                 "five-field cron expression" "${ON[@]}" --set-string "$(morning '@daily')"
refuse "a six-field schedule"             "five-field cron expression" "${ON[@]}" --set-string "$(morning '0 0 5 * * 1-5')"
refuse "an hour the chart cannot evaluate" "cannot evaluate"           "${ON[@]}" --set-string "$(morning '0 noon * * *')"
refuse "an hour out of range"             "is outside 0-23"            "${ON[@]}" --set-string "$(morning '0 24 * * *')"
refuse "a zero step"                      "must be at least 1"         "${ON[@]}" --set-string "$(morning '0 */0 * * *')"

refuse "a schedule zone that differs from the blackout's" "must equal powerschoolAuthRefresher.blackout.timeZone" \
  "${ON[@]}" --set powerschoolAuthRefresher.timeZone=UTC
refuse "a malformed blackout bound" "must be HH:MM" \
  "${ON[@]}" --set-string powerschoolAuthRefresher.blackout.start=4pm
refuse "an empty blackout window" "the window cannot be empty" \
  "${ON[@]}" --set-string powerschoolAuthRefresher.blackout.end=16:00
refuse "a plaintext Vault address" "must be an https URL" \
  "${ON[@]}" --set powerschoolAuthRefresher.vault.addr=http://vault.vault.svc.cluster.local:8200
refuse "no schedules at all" "schedules is empty" \
  "${ON[@]}" --set powerschoolAuthRefresher.schedules.morning=null --set powerschoolAuthRefresher.schedules.evening=null
refuse "a schedule without a minimum session age" "minSessionAge is required" \
  "${ON[@]}" --set-string 'powerschoolAuthRefresher.schedules.extra.schedule=0 7 * * 6'
refuse "a name too long for a CronJob" "the limit is 52" \
  "${ON[@]}" --set-string 'powerschoolAuthRefresher.schedules.a-very-long-schedule-name-indeed.schedule=0 7 * * 6' \
  --set powerschoolAuthRefresher.schedules.a-very-long-schedule-name-indeed.minSessionAge=10h

# --- schedules the render accepts -------------------------------------------

accept "21:00 exactly (the window is half-open)" "${ON[@]}" --set-string "$(morning '0 21 * * *')"
accept "15:29 with a 30-minute deadline"         "${ON[@]}" --set-string "$(morning '29 15 * * *')"
accept "15:55 with a 2-minute deadline"          "${ON[@]}" --set-string "$(morning '55 15 * * *')" \
  --set powerschoolAuthRefresher.startingDeadlineSeconds=120
accept "day-of-week names (not evaluated)"       "${ON[@]}" --set-string "$(morning '0 5 * * MON-FRI')"
accept "several safe hours"                      "${ON[@]}" --set-string "$(morning '0 5\,6\,22 * * *')"

# A moved window moves the check with it, and may wrap past midnight.
cat >"$TMP/night-window.yaml" <<'YAML'
powerschoolAuthRefresher:
  enabled: true
  blackout:
    start: "22:00"
    end: "02:00"
YAML
refuse "a wrapped window: 23:30 is inside" "$INSIDE" -f "$TMP/night-window.yaml" \
  --set-string 'powerschoolAuthRefresher.schedules.evening.schedule=30 23 * * *'
refuse "a wrapped window: 01:00 is inside" "$INSIDE" -f "$TMP/night-window.yaml" \
  --set-string 'powerschoolAuthRefresher.schedules.evening.schedule=0 1 * * *'
refuse "a wrapped window: the default 21:30 run is now too close to it" "before the login blackout" \
  -f "$TMP/night-window.yaml"
accept "a wrapped window: 17:00, inside the default window, is now fine" -f "$TMP/night-window.yaml" \
  --set-string 'powerschoolAuthRefresher.schedules.evening.schedule=0 17 * * *'

if [ "$FAILED" -ne 0 ]; then
  echo "test-chart-powerschool-login: FAILED"
  exit 1
fi
echo "test-chart-powerschool-login: all checks passed"
