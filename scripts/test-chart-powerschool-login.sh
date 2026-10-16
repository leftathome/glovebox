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
#   crons     {schedules key: CronJob}
#   env       {schedules key: {env name: value, or (secret, key) for a secretKeyRef}}
#   reqcrons  {onRequest.windows key: CronJob}, reqenv likewise
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
SCHED = 'glovebox.io/powerschool-login-schedule'
WINDOW = 'glovebox.io/powerschool-login-request-window'
crons = {d['metadata']['labels'][SCHED]: d
         for d in mine if d['kind'] == 'CronJob' and SCHED in d['metadata']['labels']}
reqcrons = {d['metadata']['labels'][WINDOW]: d
            for d in mine if d['kind'] == 'CronJob' and WINDOW in d['metadata']['labels']}
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
reqenv = {k: envmap(c) for k, c in reqcrons.items()}
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

check "default schedules: 05:00 Mon-Fri and 21:05 Sun-Thu" \
  'crons["morning"]["spec"]["schedule"] == "0 5 * * 1-5" and crons["evening"]["spec"]["schedule"] == "5 21 * * 0-4"' \
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

check "by default no extra windows and no state path override are rendered" \
  'all("POWERSCHOOL_BLACKOUT_EXTRA" not in e and "VAULT_STATE_PATH" not in e for e in env.values())' \
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

check "read-only root filesystem, seccomp RuntimeDefault" \
  'all(container(c)["securityContext"]["readOnlyRootFilesystem"] is True
       and pod(c)["securityContext"]["seccompProfile"] == {"type": "RuntimeDefault"} for c in crons.values())' \
  "${ON[@]}"

check "seccomp stays RuntimeDefault for this job when the chart-wide profile is switched off" \
  'all(pod(c)["securityContext"]["seccompProfile"] == {"type": "RuntimeDefault"} for c in crons.values())' \
  "${ON[@]}" --set seccompProfile.type=

check "/tmp, HOME and /dev/shm are memory-backed, size-capped emptyDirs" \
  'all([v for v in pod(c)["volumes"] if v["name"] in ("tmp", "home", "dshm")] ==
       [{"name": "tmp", "emptyDir": {"medium": "Memory", "sizeLimit": "256Mi"}},
        {"name": "home", "emptyDir": {"medium": "Memory", "sizeLimit": "64Mi"}},
        {"name": "dshm", "emptyDir": {"medium": "Memory", "sizeLimit": "256Mi"}}]
       and [(m["name"], m["mountPath"]) for m in container(c)["volumeMounts"]] ==
           [("tmp", "/tmp"), ("home", "/home/refresher"), ("dshm", "/dev/shm")]
       and env[k]["TMPDIR"] == "/tmp" and env[k]["HOME"] == "/home/refresher"
       for k, c in crons.items())' \
  "${ON[@]}"

check "no volume is backed by the node's disk" \
  'all(set(v) - {"name"} <= {"emptyDir", "configMap"} and v.get("emptyDir", {"medium": "Memory"}).get("medium") == "Memory"
       for c in crons.values() for v in pod(c)["volumes"])' \
  "${ON[@]}" --set powerschoolAuthRefresher.vault.caConfigMap.name=vault-ca-bundle

check "finished pods are removed after an hour; the termination message is read from the file" \
  'all(c["spec"]["jobTemplate"]["spec"]["ttlSecondsAfterFinished"] == 3600
       and container(c)["terminationMessagePath"] == "/dev/termination-log"
       and container(c)["terminationMessagePolicy"] == "File" for c in crons.values())' \
  "${ON[@]}"

check "the state path can be set" \
  'all(e["VAULT_STATE_PATH"] == "glovebox/powerschool/default/latch" for e in env.values())' \
  "${ON[@]}" --set powerschoolAuthRefresher.vault.statePath=glovebox/powerschool/default/latch

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

check "network policy: Vault egress is the Vault pods in the Vault namespace on the Vault port" \
  "[rule for rule in $NP[\"spec\"][\"egress\"] if rule[\"ports\"][0][\"port\"] == 8200] ==
   [{\"to\": [{\"namespaceSelector\": {\"matchLabels\": {\"kubernetes.io/metadata.name\": \"vault\"}},
              \"podSelector\": {\"matchLabels\": {\"app.kubernetes.io/name\": \"vault\"}}}],
     \"ports\": [{\"port\": 8200, \"protocol\": \"TCP\"}]}]" \
  "${ON[@]}"

check "network policy: the Vault pod labels can be changed" \
  "[rule[\"to\"][0][\"podSelector\"] for rule in $NP[\"spec\"][\"egress\"] if rule[\"ports\"][0][\"port\"] == 8200] ==
   [{\"matchLabels\": {\"app.kubernetes.io/name\": \"vault\", \"component\": \"server\"}}]" \
  "${ON[@]}" --set powerschoolAuthRefresher.networkPolicy.vaultPodLabels.component=server

check "network policy: 443 excludes private, CGNAT and link-local ranges (pod and service CIDRs, the LAN)" \
  "[rule for rule in $NP[\"spec\"][\"egress\"] if rule[\"ports\"][0][\"port\"] == 443] ==
   [{\"to\": [{\"ipBlock\": {\"cidr\": \"0.0.0.0/0\", \"except\":
       [\"10.0.0.0/8\", \"172.16.0.0/12\", \"192.168.0.0/16\", \"100.64.0.0/10\", \"169.254.0.0/16\"]}}],
     \"ports\": [{\"port\": 443, \"protocol\": \"TCP\"}]}]" \
  "${ON[@]}"

check "network policy: nothing but DNS, Vault and 443 is allowed out" \
  "len($NP[\"spec\"][\"egress\"]) == 3" \
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
refuse "no catch-up deadline at all" "must be set (10 or more)" \
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
accept "15:26 with a 30-minute deadline (34 minutes; 33 are needed)" "${ON[@]}" --set-string "$(morning '26 15 * * *')"
refuse "15:27 with a 30-minute deadline (33 minutes)" "before the login blackout" \
  "${ON[@]}" --set-string "$(morning '27 15 * * *')"
refuse "15:55 with a 2-minute deadline (a login needs 3 more)" "before the login blackout" \
  "${ON[@]}" --set-string "$(morning '55 15 * * *')" --set powerschoolAuthRefresher.startingDeadlineSeconds=120
accept "15:54 with a 2-minute deadline"          "${ON[@]}" --set-string "$(morning '54 15 * * *')" \
  --set powerschoolAuthRefresher.startingDeadlineSeconds=120
accept "day-of-week names (not evaluated)"       "${ON[@]}" --set-string "$(morning '0 5 * * MON-FRI')"
accept "several safe hours"                      "${ON[@]}" --set-string "$(morning '0 5\,6\,22 * * *')"

# --- the blackout can be widened, never weakened ------------------------------

WIDEN="may only be widened"

refuse "a later start"              "$WIDEN" "${ON[@]}" --set-string powerschoolAuthRefresher.blackout.start=17:00
refuse "a start one minute late"    "$WIDEN" "${ON[@]}" --set-string powerschoolAuthRefresher.blackout.start=16:01
refuse "an earlier end"             "$WIDEN" "${ON[@]}" --set-string powerschoolAuthRefresher.blackout.end=20:00
refuse "an end one minute early"    "$WIDEN" "${ON[@]}" --set-string powerschoolAuthRefresher.blackout.end=20:59
refuse "a window moved to the night" "$WIDEN" "${ON[@]}" \
  --set-string powerschoolAuthRefresher.blackout.start=22:00 --set-string powerschoolAuthRefresher.blackout.end=02:00
refuse "a window inverted to leave the afternoon out" "$WIDEN" "${ON[@]}" \
  --set-string powerschoolAuthRefresher.blackout.start=21:00 --set-string powerschoolAuthRefresher.blackout.end=16:00
refuse "another blackout zone" "never moved to another zone" \
  "${ON[@]}" --set powerschoolAuthRefresher.blackout.timeZone=UTC --set powerschoolAuthRefresher.timeZone=UTC
refuse "another blackout zone, schedules left alone" "never moved to another zone" \
  "${ON[@]}" --set powerschoolAuthRefresher.blackout.timeZone=America/Denver
refuse "a state path equal to the session path" "must not be the session path" \
  "${ON[@]}" --set powerschoolAuthRefresher.vault.statePath=glovebox/powerschool/default/session

cat >"$TMP/wide.yaml" <<'YAML'
powerschoolAuthRefresher:
  enabled: true
  blackout:
    start: "15:00"
    end: "21:00"
    extraWindows:
      - {start: "07:30", end: "08:30"}
      - {start: "23:00", end: "01:00"}
YAML

check "a widened blackout and its extra windows reach the binary" \
  'all(e["POWERSCHOOL_BLACKOUT_START"] == "15:00" and e["POWERSCHOOL_BLACKOUT_END"] == "21:00"
       and e["POWERSCHOOL_BLACKOUT_EXTRA"] == "07:30-08:30,23:00-01:00" for e in env.values())' \
  -f "$TMP/wide.yaml"
refuse "widened start: 15:20, fine by the floor, is now inside" "$INSIDE" -f "$TMP/wide.yaml" \
  --set-string "$(morning '20 15 * * *')"
refuse "widened start: 14:40 is now too close" "before the login blackout" -f "$TMP/wide.yaml" \
  --set-string "$(morning '40 14 * * *')"
refuse "extra window: 08:00 is inside" "$INSIDE" -f "$TMP/wide.yaml" --set-string "$(morning '0 8 * * *')"
refuse "extra window past midnight: 00:30 is inside" "$INSIDE" -f "$TMP/wide.yaml" --set-string "$(morning '30 0 * * *')"
refuse "extra window: 07:00 is too close to it" "before the login blackout" -f "$TMP/wide.yaml" \
  --set-string "$(morning '0 7 * * *')"
refuse "the floor still applies beside a widened window" "$INSIDE" -f "$TMP/wide.yaml" \
  --set-string "$(morning '0 18 * * *')"
refuse "a later end catches the default evening run" "$INSIDE" \
  "${ON[@]}" --set-string powerschoolAuthRefresher.blackout.end=21:30
accept "a later end with the evening run moved after it" \
  "${ON[@]}" --set-string powerschoolAuthRefresher.blackout.end=21:30 \
  --set-string 'powerschoolAuthRefresher.schedules.evening.schedule=35 21 * * 0-4'
accept "a window widened past midnight" \
  "${ON[@]}" --set-string powerschoolAuthRefresher.blackout.start=15:00 --set-string powerschoolAuthRefresher.blackout.end=02:00 \
  --set powerschoolAuthRefresher.schedules.evening=null
refuse "a malformed extra window" "must be HH:MM" \
  "${ON[@]}" --set-json 'powerschoolAuthRefresher.blackout.extraWindows=[{"start":"7am","end":"08:00"}]'
refuse "an empty extra window" "a window cannot be empty" \
  "${ON[@]}" --set-json 'powerschoolAuthRefresher.blackout.extraWindows=[{"start":"07:00","end":"07:00"}]'

# --- suspended schedules ------------------------------------------------------

check "CronJobs are not suspended by default" \
  'all("suspend" not in c["spec"] for c in crons.values())' \
  "${ON[@]}"

check "suspendSchedules suspends every CronJob and keeps them as templates" \
  'sorted(crons) == ["evening", "morning"] and all(c["spec"]["suspend"] is True for c in crons.values())
   and all(c["spec"]["jobTemplate"]["spec"]["backoffLimit"] == 0 for c in crons.values())' \
  "${ON[@]}" --set powerschoolAuthRefresher.suspendSchedules=true

check "one schedule can be suspended on its own" \
  'crons["evening"]["spec"]["suspend"] is True and "suspend" not in crons["morning"]["spec"]' \
  "${ON[@]}" --set powerschoolAuthRefresher.schedules.evening.suspend=true

check "every login Job carries the login label" \
  'all(c["spec"]["jobTemplate"]["metadata"]["labels"]["glovebox.io/powerschool-login"] == "true"
       and c["spec"]["jobTemplate"]["metadata"]["labels"]["app.kubernetes.io/component"] == "powerschool-auth-refresher"
       for c in crons.values())' \
  "${ON[@]}"

refuse "a suspended schedule is still checked against the blackout" "$INSIDE" \
  "${ON[@]}" --set powerschoolAuthRefresher.suspendSchedules=true --set-string "$(morning '0 17 * * *')"

# --- logins on request ----------------------------------------------------------

cat >"$TMP/ondemand.yaml" <<'YAML'
powerschoolAuthRefresher:
  enabled: true
  suspendSchedules: true
  onRequest:
    enabled: true
  networkPolicy:
    kubernetesAPICIDRs: [192.0.2.10/32, 192.0.2.11/32, 10.96.0.1/32]
connectors:
  powerschool:
    enabled: true
    loginOnDemand:
      enabled: true
    externalSecret:
      enabled: true
      secretStoreRef:
        name: vault-default
YAML

CM='find("ConfigMap", "glovebox-glovebox-powerschool-login-request")'
ROLE='find("Role", "glovebox-glovebox-powerschool-login-on-demand")'
BINDING='find("RoleBinding", "glovebox-glovebox-powerschool-login-on-demand")'
RROLE='find("Role", "glovebox-glovebox-powerschool-login-request-reader")'
RBINDING='find("RoleBinding", "glovebox-glovebox-powerschool-login-request-reader")'
CONNSA='find("ServiceAccount", "glovebox-glovebox-connector-powerschool")'
PSPOD='find("Deployment", "glovebox-glovebox-powerschool")["spec"]["template"]["spec"]'
PSCFG='__import__("json").loads(find("ConfigMap", "glovebox-glovebox-powerschool-config")["data"]["config.json"])'
RBAC='[d["kind"] + "/" + d["metadata"]["name"] for d in docs if d["kind"] in ("Role", "ClusterRole", "RoleBinding", "ClusterRoleBinding")]'

check "everything is off by default: no request ConfigMap, no Role or binding of any kind, no on-request CronJob" \
  "$CM is None and $RBAC == [] and reqcrons == {} and $CONNSA is None" \
  "${ON[@]}"

check "the connector alone, on-demand off: own ServiceAccount, no token, no request config" \
  "$RBAC == [] and $PSPOD[\"serviceAccountName\"] == \"glovebox-glovebox-connector-powerschool\" and $PSPOD[\"automountServiceAccountToken\"] is False
   and \"login_on_demand\" not in $PSCFG and $CM is None" \
  -f "$TMP/both.yaml"

check "the request ConfigMap is rendered without data (the key belongs to the connector)" \
  "$CM is not None and \"data\" not in $CM and \"binaryData\" not in $CM" \
  -f "$TMP/ondemand.yaml"

check "connector Role: exactly get, update, patch on the one request ConfigMap" \
  "$ROLE[\"rules\"] == [
     {\"apiGroups\": [\"\"], \"resources\": [\"configmaps\"],
      \"resourceNames\": [\"glovebox-glovebox-powerschool-login-request\"], \"verbs\": [\"get\", \"update\", \"patch\"]}]" \
  -f "$TMP/ondemand.yaml"

check "login workload Role: exactly get on the one request ConfigMap" \
  "$RROLE[\"rules\"] == [
     {\"apiGroups\": [\"\"], \"resources\": [\"configmaps\"],
      \"resourceNames\": [\"glovebox-glovebox-powerschool-login-request\"], \"verbs\": [\"get\"]}]" \
  -f "$TMP/ondemand.yaml"

check "no Role in the chart grants anything on jobs, cronjobs, pods or secrets, or create/delete of anything" \
  "all(not ({\"jobs\", \"cronjobs\", \"pods\", \"secrets\", \"*\"} & set(rule[\"resources\"]))
       and not ({\"create\", \"delete\", \"deletecollection\", \"*\"} & set(rule[\"verbs\"]))
       and rule.get(\"resourceNames\")
       for d in docs if d[\"kind\"] in (\"Role\", \"ClusterRole\") for rule in d[\"rules\"])" \
  -f "$TMP/ondemand.yaml"

check "those two Roles and their bindings are all the RBAC there is" \
  "$RBAC == [\"Role/glovebox-glovebox-powerschool-login-request-reader\",
            \"RoleBinding/glovebox-glovebox-powerschool-login-request-reader\",
            \"Role/glovebox-glovebox-powerschool-login-on-demand\",
            \"RoleBinding/glovebox-glovebox-powerschool-login-on-demand\"] or sorted($RBAC) == sorted([
            \"Role/glovebox-glovebox-powerschool-login-request-reader\",
            \"RoleBinding/glovebox-glovebox-powerschool-login-request-reader\",
            \"Role/glovebox-glovebox-powerschool-login-on-demand\",
            \"RoleBinding/glovebox-glovebox-powerschool-login-on-demand\"])" \
  -f "$TMP/ondemand.yaml"

check "each Role is bound to its own ServiceAccount only" \
  "$BINDING[\"roleRef\"][\"name\"] == \"glovebox-glovebox-powerschool-login-on-demand\"
   and $BINDING[\"subjects\"] == [{\"kind\": \"ServiceAccount\", \"name\": \"glovebox-glovebox-connector-powerschool\", \"namespace\": \"glovebox\"}]
   and $RBINDING[\"roleRef\"][\"name\"] == \"glovebox-glovebox-powerschool-login-request-reader\"
   and $RBINDING[\"subjects\"] == [{\"kind\": \"ServiceAccount\", \"name\": \"glovebox-glovebox-powerschool-auth-refresher\", \"namespace\": \"glovebox\"}]" \
  -f "$TMP/ondemand.yaml"

check "the connector runs as its own ServiceAccount with its token mounted; other connectors do not" \
  "$CONNSA is not None and $PSPOD[\"serviceAccountName\"] == \"glovebox-glovebox-connector-powerschool\"
   and $PSPOD[\"automountServiceAccountToken\"] is True
   and find(\"Deployment\", \"glovebox-glovebox-rss\")[\"spec\"][\"template\"][\"spec\"][\"serviceAccountName\"] == \"glovebox-glovebox-connector-rss\"
   and find(\"Deployment\", \"glovebox-glovebox-rss\")[\"spec\"][\"template\"][\"spec\"][\"automountServiceAccountToken\"] is False" \
  -f "$TMP/ondemand.yaml" --set connectors.rss.enabled=true

check "the connector is told which ConfigMap, namespace, wait (10 min) and interval" \
  "$PSCFG[\"login_on_demand\"] == {\"enabled\": True, \"configmap_name\": \"glovebox-glovebox-powerschool-login-request\",
     \"namespace\": \"glovebox\", \"wait_seconds\": 600, \"min_interval_minutes\": 20}
   and $PSCFG[\"login_on_demand\"][\"configmap_name\"] == $CM[\"metadata\"][\"name\"]" \
  -f "$TMP/ondemand.yaml"

check "the rest of the connector's config is untouched" \
  "[k[\"name\"] for k in $PSCFG[\"kids\"]] == [\"k1\", \"k2\"] and len($PSCFG[\"rules\"]) == 12
   and $PSCFG[\"poll_schedule\"][\"windows\"][0] == {\"start\": \"06:00\", \"end\": \"06:30\"}" \
  -f "$TMP/ondemand.yaml"

check "wait and interval can be raised" \
  "$PSCFG[\"login_on_demand\"][\"wait_seconds\"] == 900 and $PSCFG[\"login_on_demand\"][\"min_interval_minutes\"] == 45" \
  -f "$TMP/ondemand.yaml" --set connectors.powerschool.loginOnDemand.waitSeconds=900 \
  --set connectors.powerschool.loginOnDemand.minIntervalMinutes=45

check "the session reaches the connector within a minute of ESO's side" \
  'find("ExternalSecret", "glovebox-glovebox-powerschool-files")["spec"]["refreshInterval"] == "1m"' \
  -f "$TMP/ondemand.yaml"

check "the connector still gets no credentials" \
  "all(\"USERNAME\" not in e[\"name\"] and \"PASSWORD\" not in e[\"name\"] for e in $PSPOD[\"containers\"][0][\"env\"])
   and [f[\"secretRef\"][\"name\"] for f in $PSPOD[\"containers\"][0][\"envFrom\"]] == [\"glovebox-glovebox-powerschool-secret\"]" \
  -f "$TMP/ondemand.yaml"

# --- the on-request CronJobs ----------------------------------------------------

check "on-request CronJobs: one per window, every 3 minutes around the connector's poll windows" \
  '{k: c["spec"]["schedule"] for k, c in reqcrons.items()} == {
     "morning": "*/3 6 * * 1-5", "afternoon": "0-45/3 15 * * 1-5",
     "evening": "30-59/3 21 * * 1-5", "late": "0-15/3 22 * * 1-5"}
   and sorted(c["metadata"]["name"] for c in reqcrons.values()) == [
     "glovebox-glovebox-powerschool-login-req-afternoon", "glovebox-glovebox-powerschool-login-req-evening",
     "glovebox-glovebox-powerschool-login-req-late", "glovebox-glovebox-powerschool-login-req-morning"]' \
  -f "$TMP/ondemand.yaml"

check "on-request CronJobs: Pacific time, Forbid, never retried, short catch-up, not suspended" \
  'all(c["spec"]["timeZone"] == "America/Los_Angeles" and c["spec"]["concurrencyPolicy"] == "Forbid"
       and c["spec"]["startingDeadlineSeconds"] == 120 and "suspend" not in c["spec"]
       and c["spec"]["jobTemplate"]["spec"]["backoffLimit"] == 0 and pod(c)["restartPolicy"] == "Never"
       and c["spec"]["jobTemplate"]["spec"]["ttlSecondsAfterFinished"] == 600
       and c["spec"]["successfulJobsHistoryLimit"] == 1 for c in reqcrons.values())' \
  -f "$TMP/ondemand.yaml"

check "on-request CronJobs run in request mode: the ConfigMap, the 20-minute floor, the request age limit" \
  'all(e["POWERSCHOOL_LOGIN_REQUEST_CONFIGMAP"] == "glovebox-glovebox-powerschool-login-request"
       and e["POWERSCHOOL_LOGIN_REQUEST_NAMESPACE"] == "glovebox"
       and e["POWERSCHOOL_LOGIN_REQUEST_MIN_INTERVAL"] == "20m" and e["POWERSCHOOL_LOGIN_REQUEST_MAX_AGE"] == "15m"
       and e["POWERSCHOOL_MIN_SESSION_AGE"] == "5m" and "POWERSCHOOL_LOGIN_REQUEST_BYPASS" not in e
       and e["POWERSCHOOL_BLACKOUT_START"] == "16:00" and e["POWERSCHOOL_BLACKOUT_END"] == "21:00"
       for e in reqenv.values()) and len(reqenv) == 4' \
  -f "$TMP/ondemand.yaml"

check "the scheduled CronJobs are NOT in request mode (they are the manual-run templates) and are suspended" \
  'all("POWERSCHOOL_LOGIN_REQUEST_CONFIGMAP" not in e for e in env.values()) and sorted(env) == ["evening", "morning"]
   and all(c["spec"]["suspend"] is True for c in crons.values())' \
  -f "$TMP/ondemand.yaml"

check "on-request pods are the same hardened pod: read-only root, non-root, the login ServiceAccount" \
  'all(container(c)["securityContext"]["readOnlyRootFilesystem"] is True and pod(c)["securityContext"]["runAsUser"] == 65532
       and pod(c)["serviceAccountName"] == "glovebox-glovebox-powerschool-auth-refresher"
       and c["spec"]["jobTemplate"]["metadata"]["labels"]["glovebox.io/powerschool-login-kind"] == "request"
       for c in reqcrons.values())
   and all(c["spec"]["jobTemplate"]["metadata"]["labels"]["glovebox.io/powerschool-login-kind"] == "schedule" for c in crons.values())' \
  -f "$TMP/ondemand.yaml"

check "the login workload's NetworkPolicy lets it reach the Kubernetes API, and only at the listed addresses" \
  "[rule for rule in $NP[\"spec\"][\"egress\"] if 6443 in [p[\"port\"] for p in rule[\"ports\"]]] == [
     {\"to\": [{\"ipBlock\": {\"cidr\": \"192.0.2.10/32\"}}, {\"ipBlock\": {\"cidr\": \"192.0.2.11/32\"}}, {\"ipBlock\": {\"cidr\": \"10.96.0.1/32\"}}],
      \"ports\": [{\"port\": 443, \"protocol\": \"TCP\"}, {\"port\": 6443, \"protocol\": \"TCP\"}]}]
   and len($NP[\"spec\"][\"egress\"]) == 4" \
  -f "$TMP/ondemand.yaml"

check "onRequest without the connector's side is allowed (nothing asks; nothing happens)" \
  "len(reqcrons) == 4 and $CM is not None and $ROLE is None and $RROLE is not None" \
  "${ON[@]}" --set powerschoolAuthRefresher.onRequest.enabled=true \
  --set 'powerschoolAuthRefresher.networkPolicy.kubernetesAPICIDRs={192.0.2.10/32}'

check "a window can be removed and another added" \
  'sorted(reqcrons) == ["afternoon", "evening", "midday", "morning"]' \
  -f "$TMP/ondemand.yaml" --set powerschoolAuthRefresher.onRequest.windows.late=null \
  --set-string 'powerschoolAuthRefresher.onRequest.windows.midday=*/3 11 * * 1-5'

# The cadence is validated against the blackout like any schedule.
reqwin() { printf '%s' "powerschoolAuthRefresher.onRequest.windows.$1=$2"; }

refuse "an on-request window inside the blackout" "$INSIDE" \
  -f "$TMP/ondemand.yaml" --set-string "$(reqwin afternoon '*/3 17 * * 1-5')"
refuse "an on-request window running up to the blackout (15:57)" "before the login blackout" \
  -f "$TMP/ondemand.yaml" --set-string "$(reqwin afternoon '*/3 15 * * 1-5')"
refuse "an on-request window all day" "the login blackout" \
  -f "$TMP/ondemand.yaml" --set-string "$(reqwin morning '*/3 * * * *')"
refuse "an added on-request window in the evening blackout" "$INSIDE" \
  -f "$TMP/ondemand.yaml" --set-string "$(reqwin extra '*/5 20 * * *')"
accept "an on-request window ending at 15:54 (six minutes clear, five needed)" \
  -f "$TMP/ondemand.yaml" --set-string "$(reqwin afternoon '0-54/3 15 * * 1-5')"
refuse "an on-request window ending at 15:57 (three minutes clear)" "before the login blackout" \
  -f "$TMP/ondemand.yaml" --set-string "$(reqwin afternoon '0-57/3 15 * * 1-5')"
accept "an on-request window from 21:00" \
  -f "$TMP/ondemand.yaml" --set-string "$(reqwin evening '*/3 21 * * 1-5')"
refuse "an on-request window the chart cannot evaluate" "five-field cron expression" \
  -f "$TMP/ondemand.yaml" --set-string "$(reqwin morning '@hourly')"
refuse "the widened blackout applies to on-request windows too" "$INSIDE" \
  -f "$TMP/ondemand.yaml" --set-string powerschoolAuthRefresher.blackout.start=15:00 \
  --set-string "$(reqwin afternoon '0-45/3 15 * * 1-5')"

refuse "on-demand without the login workload" "requires powerschoolAuthRefresher.enabled" \
  -f "$TMP/ondemand.yaml" --set powerschoolAuthRefresher.enabled=false
refuse "on-demand without the on-request runs" "requires powerschoolAuthRefresher.onRequest.enabled" \
  -f "$TMP/ondemand.yaml" --set powerschoolAuthRefresher.onRequest.enabled=false
refuse "on-request with no windows" "onRequest.windows is empty" \
  -f "$TMP/ondemand.yaml" --set powerschoolAuthRefresher.onRequest.windows=null
refuse "an interval between request logins under the floor" "minIntervalMinutes must be at least 20" \
  -f "$TMP/ondemand.yaml" --set powerschoolAuthRefresher.onRequest.minIntervalMinutes=5
refuse "on-request behind the NetworkPolicy without the API server's addresses" "kubernetesAPICIDRs" \
  -f "$TMP/ondemand.yaml" --set powerschoolAuthRefresher.networkPolicy.kubernetesAPICIDRs=null
accept "on-request without the API addresses when the job's NetworkPolicy is off" \
  -f "$TMP/ondemand.yaml" --set powerschoolAuthRefresher.networkPolicy.kubernetesAPICIDRs=null \
  --set powerschoolAuthRefresher.networkPolicy.enabled=false
refuse "a wait too short to be met" "waitSeconds must be between 30 and 1800" \
  -f "$TMP/ondemand.yaml" --set connectors.powerschool.loginOnDemand.waitSeconds=5
refuse "a connector interval under the floor" "minIntervalMinutes must be at least 20" \
  -f "$TMP/ondemand.yaml" --set connectors.powerschool.loginOnDemand.minIntervalMinutes=5
accept "schedules removed entirely, on-request only" \
  -f "$TMP/ondemand.yaml" --set powerschoolAuthRefresher.schedules.morning=null --set powerschoolAuthRefresher.schedules.evening=null

if [ "$FAILED" -ne 0 ]; then
  echo "test-chart-powerschool-login: FAILED"
  exit 1
fi
echo "test-chart-powerschool-login: all checks passed"
