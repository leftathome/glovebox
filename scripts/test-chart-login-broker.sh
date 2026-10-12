#!/usr/bin/env bash
# Render-level assertions for the login brokers
# (charts/glovebox/templates/login-broker-*.yaml) and the PowerSchool
# connector's wiring to one (connectors.powerschool.loginBroker).
#
# A broker holds a real account's password and speaks plain HTTP inside the
# cluster. What makes that acceptable is partly in the binary (the token
# check, the login policy) and partly in the rendered manifests: one
# instance per site with nothing shared, an ingress policy that admits only
# the connector, an egress policy, a single replica that is never doubled,
# and a render that refuses the configurations that would undo any of it.
# Those are pinned here.
#
# Requires helm and python3 with PyYAML. Usage: scripts/test-chart-login-broker.sh
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
#   docs    every rendered object
#   mine    those labelled component=login-broker
#   kinds   sorted [kind/name] of `mine`
#   find(kind, name)
#   env(deployment)   {env name: value, or (secret, key) for a secretKeyRef}
check() {
  local name="$1"; shift
  local expr="$1"; shift
  if render "$@" 2>"$TMP/err" | python3 -c "
import sys, json, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
def comp(d):
    return (d.get('metadata', {}).get('labels') or {}).get('app.kubernetes.io/component')
mine = [d for d in docs if comp(d) == 'login-broker']
kinds = sorted(d['kind'] + '/' + d['metadata']['name'] for d in mine)
def find(kind, name):
    hits = [d for d in docs if d['kind'] == kind and d['metadata']['name'] == name]
    return hits[0] if hits else None
def env(dep):
    out = {}
    for e in dep['spec']['template']['spec']['containers'][0].get('env', []):
        if 'value' in e:
            out[e['name']] = e['value']
        else:
            ref = e['valueFrom']['secretKeyRef']
            out[e['name']] = (ref['name'], ref['key'])
    return out
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

accept() {
  local name="$1"; shift
  if render "$@" >/dev/null 2>"$TMP/err"; then
    echo "ok   $name"
  else
    echo "FAIL $name (refused)"; cat "$TMP/err"; FAILED=1
  fi
}

cat >"$TMP/broker.yaml" <<'YAML'
loginBrokers:
  powerschool:
    enabled: true
    networkPolicy:
      kubernetesAPICIDRs: [192.0.2.10/32, 10.96.0.1/32]
YAML

cat >"$TMP/wired.yaml" <<'YAML'
loginBrokers:
  powerschool:
    enabled: true
    networkPolicy:
      kubernetesAPICIDRs: [192.0.2.10/32, 10.96.0.1/32]
connectors:
  powerschool:
    enabled: true
    loginBroker:
      enabled: true
    externalSecret:
      enabled: true
      secretStoreRef:
        name: vault-default
YAML

# A second site, to prove instances share nothing. The site has no driver in
# the binary; the chart does not know or care.
cat >"$TMP/two.yaml" <<'YAML'
loginBrokers:
  powerschool:
    enabled: true
    networkPolicy:
      kubernetesAPICIDRs: [192.0.2.10/32]
  schoology:
    enabled: true
    site: schoology
    connector: schoology
    image:
      repository: ghcr.io/leftathome/glovebox-login-broker
    credentials:
      env:
        LOGIN_BROKER_USERNAME: username
        LOGIN_BROKER_PASSWORD: password
        SCHOOLOGY_HOST: host
    vault:
      addr: "https://vault.vault.svc.cluster.local:8200"
      k8sRole: glovebox-login-broker-schoology
      sessionPath: glovebox/schoology/default/session
      credentialsPath: eso/glovebox-schoology
      credentialsProperties:
        username: schoology_username
        password: schoology_password
        host: schoology_host
      secretStoreRef:
        name: vault-default
    networkPolicy:
      enabled: true
      kubernetesAPICIDRs: [192.0.2.10/32]
      siteCIDRs: [203.0.113.0/24]
    resources: {}
YAML

DEP='find("Deployment", "glovebox-glovebox-login-broker-powerschool")'
POD="$DEP[\"spec\"][\"template\"][\"spec\"]"
NP='find("NetworkPolicy", "glovebox-glovebox-login-broker-powerschool")["spec"]'
PSDEP='find("Deployment", "glovebox-glovebox-powerschool")'
PSPOD="$PSDEP[\"spec\"][\"template\"][\"spec\"]"
PSCFG='json.loads(find("ConfigMap", "glovebox-glovebox-powerschool-config")["data"]["config.json"])'

# --- off by default ---------------------------------------------------------

check "disabled by default: nothing is rendered, and no cluster-scoped object" \
  'mine == [] and [d for d in docs if d["kind"].startswith("Cluster")] == []'

# --- one instance -----------------------------------------------------------

check "enabled: Deployment, Service, ServiceAccount, credentials ExternalSecret, NetworkPolicy, TokenReview RBAC" \
  'kinds == ["ClusterRole/glovebox-glovebox-glovebox-login-broker-powerschool-tokenreview",
             "ClusterRoleBinding/glovebox-glovebox-glovebox-login-broker-powerschool-tokenreview",
             "Deployment/glovebox-glovebox-login-broker-powerschool",
             "ExternalSecret/glovebox-glovebox-login-broker-powerschool-credentials",
             "NetworkPolicy/glovebox-glovebox-login-broker-powerschool",
             "Service/glovebox-glovebox-login-broker-powerschool",
             "ServiceAccount/glovebox-glovebox-login-broker-powerschool"]' \
  -f "$TMP/broker.yaml"

check "one replica, Recreate: never two pods logging in to one account" \
  "$DEP[\"spec\"][\"replicas\"] == 1 and $DEP[\"spec\"][\"strategy\"] == {\"type\": \"Recreate\"}" \
  -f "$TMP/broker.yaml"

check "the pod: own ServiceAccount, non-root, read-only root, no capabilities, seccomp, memory-backed scratch" \
  "$POD[\"serviceAccountName\"] == \"glovebox-glovebox-login-broker-powerschool\"
   and $POD[\"securityContext\"][\"runAsNonRoot\"] is True and $POD[\"securityContext\"][\"runAsUser\"] == 65532
   and $POD[\"securityContext\"][\"seccompProfile\"][\"type\"] == \"RuntimeDefault\"
   and $POD[\"containers\"][0][\"securityContext\"] == {\"readOnlyRootFilesystem\": True, \"allowPrivilegeEscalation\": False,
        \"runAsNonRoot\": True, \"runAsUser\": 65532, \"capabilities\": {\"drop\": [\"ALL\"]}}
   and sorted(v[\"name\"] for v in $POD[\"volumes\"]) == [\"dshm\", \"home\", \"tmp\"]
   and all(v[\"emptyDir\"][\"medium\"] == \"Memory\" and v[\"emptyDir\"][\"sizeLimit\"] for v in $POD[\"volumes\"])" \
  -f "$TMP/broker.yaml"

check "a login in flight gets time to finish on shutdown" \
  "$POD[\"terminationGracePeriodSeconds\"] == 420" \
  -f "$TMP/broker.yaml"

check "probes ask the broker, never the site: /healthz only" \
  "$POD[\"containers\"][0][\"livenessProbe\"][\"httpGet\"][\"path\"] == \"/healthz\"
   and $POD[\"containers\"][0][\"readinessProbe\"][\"httpGet\"][\"path\"] == \"/healthz\"" \
  -f "$TMP/broker.yaml"

check "policy settings reach the binary: site, interval, failure limit, blackout with its zone" \
  "env($DEP)[\"LOGIN_BROKER_SITE\"] == \"powerschool\"
   and env($DEP)[\"LOGIN_BROKER_MIN_INTERVAL\"] == \"20m\" and env($DEP)[\"LOGIN_BROKER_MAX_FAILURES\"] == \"2\"
   and env($DEP)[\"LOGIN_BROKER_BLACKOUT_WINDOWS\"] == \"16:00-21:00\"
   and env($DEP)[\"LOGIN_BROKER_BLACKOUT_TZ\"] == \"America/Los_Angeles\"
   and env($DEP)[\"LOGIN_BROKER_LISTEN\"] == \":8080\" and env($DEP)[\"LOGIN_BROKER_METRICS_LISTEN\"] == \":9090\"" \
  -f "$TMP/broker.yaml"

check "callers: a dedicated audience, and exactly the connector's ServiceAccount" \
  "env($DEP)[\"LOGIN_BROKER_AUDIENCE\"] == \"glovebox-glovebox-login-broker-powerschool.glovebox\"
   and env($DEP)[\"LOGIN_BROKER_ALLOWED_CALLERS\"] == \"glovebox/glovebox-glovebox-powerschool\"" \
  -f "$TMP/broker.yaml"

check "credentials come key by key from the broker's own Secret, never envFrom" \
  "env($DEP)[\"LOGIN_BROKER_USERNAME\"] == (\"glovebox-glovebox-login-broker-powerschool-credentials\", \"username\")
   and env($DEP)[\"LOGIN_BROKER_PASSWORD\"] == (\"glovebox-glovebox-login-broker-powerschool-credentials\", \"password\")
   and env($DEP)[\"POWERSCHOOL_HOSTNAME\"] == (\"glovebox-glovebox-login-broker-powerschool-credentials\", \"hostname\")
   and \"envFrom\" not in $POD[\"containers\"][0]" \
  -f "$TMP/broker.yaml"

check "Vault: its own role, the session path the scheduled job writes, https" \
  "env($DEP)[\"VAULT_K8S_ROLE\"] == \"glovebox-login-broker-powerschool\"
   and env($DEP)[\"VAULT_SESSION_PATH\"] == \"glovebox/powerschool/default/session\"
   and env($DEP)[\"VAULT_ADDR\"].startswith(\"https://\") and \"VAULT_STATE_PATH\" not in env($DEP)" \
  -f "$TMP/broker.yaml"

check "the ClusterRole grants TokenReview and nothing else" \
  'find("ClusterRole", "glovebox-glovebox-glovebox-login-broker-powerschool-tokenreview")["rules"]
     == [{"apiGroups": ["authentication.k8s.io"], "resources": ["tokenreviews"], "verbs": ["create"]}]
   and find("ClusterRoleBinding", "glovebox-glovebox-glovebox-login-broker-powerschool-tokenreview")["subjects"]
     == [{"kind": "ServiceAccount", "name": "glovebox-glovebox-login-broker-powerschool", "namespace": "glovebox"}]
   and [d for d in docs if d["kind"] in ("Role", "RoleBinding")] == []' \
  -f "$TMP/broker.yaml"

check "rbac.create=false leaves the cluster-scoped objects to the operator" \
  '[d for d in docs if d["kind"].startswith("Cluster")] == []' \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.rbac.create=false

# --- NetworkPolicy ----------------------------------------------------------

check "ingress to the API port: the PowerSchool connector's pods and nobody else" \
  "$NP[\"policyTypes\"] == [\"Ingress\", \"Egress\"]
   and [r for r in $NP[\"ingress\"] if {\"port\": 8080, \"protocol\": \"TCP\"} in r[\"ports\"]] == [{
        \"from\": [{\"podSelector\": {\"matchLabels\": {
            \"app.kubernetes.io/name\": \"glovebox\", \"app.kubernetes.io/instance\": \"glovebox\",
            \"app.kubernetes.io/component\": \"connector\", \"glovebox.io/connector\": \"powerschool\"}}}],
        \"ports\": [{\"port\": 8080, \"protocol\": \"TCP\"}]}]" \
  -f "$TMP/broker.yaml"

check "no ingress rule without a source reaches the API port" \
  "all(\"from\" in r for r in $NP[\"ingress\"] if any(p[\"port\"] == 8080 for p in r[\"ports\"]))" \
  -f "$TMP/broker.yaml"

check "metrics ingress is limited to the monitoring namespace when one is named" \
  "[r for r in $NP[\"ingress\"] if {\"port\": 9090, \"protocol\": \"TCP\"} in r[\"ports\"]]
     == [{\"from\": [{\"namespaceSelector\": {\"matchLabels\": {\"name\": \"monitoring\"}}}], \"ports\": [{\"port\": 9090, \"protocol\": \"TCP\"}]}]" \
  -f "$TMP/broker.yaml" --set metrics.allowedNamespaceLabel=monitoring

check "egress: DNS, the Vault pods, the API server's addresses, and the site on 443 minus private ranges" \
  "len($NP[\"egress\"]) == 4
   and $NP[\"egress\"][1][\"to\"][0][\"podSelector\"][\"matchLabels\"] == {\"app.kubernetes.io/name\": \"vault\"}
   and $NP[\"egress\"][2] == {\"to\": [{\"ipBlock\": {\"cidr\": \"192.0.2.10/32\"}}, {\"ipBlock\": {\"cidr\": \"10.96.0.1/32\"}}],
        \"ports\": [{\"port\": 443, \"protocol\": \"TCP\"}, {\"port\": 6443, \"protocol\": \"TCP\"}]}
   and $NP[\"egress\"][3][\"to\"][0][\"ipBlock\"][\"cidr\"] == \"0.0.0.0/0\"
   and \"10.0.0.0/8\" in $NP[\"egress\"][3][\"to\"][0][\"ipBlock\"][\"except\"]
   and \"169.254.0.0/16\" in $NP[\"egress\"][3][\"to\"][0][\"ipBlock\"][\"except\"]
   and $NP[\"egress\"][3][\"ports\"] == [{\"port\": 443, \"protocol\": \"TCP\"}]" \
  -f "$TMP/broker.yaml"

refuse "the policy without the API server's addresses (TokenReview would be cut off)" "kubernetesAPICIDRs is required" \
  --set loginBrokers.powerschool.enabled=true

# A broker without its NetworkPolicy is reachable, over plain HTTP, by every
# pod in the cluster. Neither switch produces that by accident.
refuse "the chart-wide NetworkPolicy switch off (the broker would be open to every pod)" "WITHOUT its NetworkPolicy" \
  -f "$TMP/broker.yaml" --set networkPolicy.enabled=false
refuse "the instance's own NetworkPolicy switch off" "WITHOUT its NetworkPolicy" \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.networkPolicy.enabled=false
check "an acknowledged external policy: the instance renders, with no policy of the chart's" \
  'find("NetworkPolicy", "glovebox-glovebox-login-broker-powerschool") is None
   and find("Deployment", "glovebox-glovebox-login-broker-powerschool") is not None' \
  -f "$TMP/broker.yaml" --set networkPolicy.enabled=false --set loginBrokers.powerschool.acknowledgeNoNetworkPolicy=true
check "a disabled broker is not held to it" 'mine == []' \
  --set networkPolicy.enabled=false

# --- one instance per site, nothing shared ----------------------------------

SDEP='find("Deployment", "glovebox-glovebox-login-broker-schoology")'
SNP='find("NetworkPolicy", "glovebox-glovebox-login-broker-schoology")["spec"]'

check "two sites: two of everything" \
  'len([k for k in kinds if k.startswith("Deployment/")]) == 2 and len([k for k in kinds if k.startswith("ServiceAccount/")]) == 2
   and len([k for k in kinds if k.startswith("NetworkPolicy/")]) == 2 and len([k for k in kinds if k.startswith("ExternalSecret/")]) == 2
   and len([k for k in kinds if k.startswith("ClusterRoleBinding/")]) == 2' \
  -f "$TMP/two.yaml"

check "two sites: same image; separate ServiceAccount, Vault role, credentials Secret, audience, caller" \
  "$DEP[\"spec\"][\"template\"][\"spec\"][\"containers\"][0][\"image\"] == $SDEP[\"spec\"][\"template\"][\"spec\"][\"containers\"][0][\"image\"]
   and $SDEP[\"spec\"][\"template\"][\"spec\"][\"serviceAccountName\"] == \"glovebox-glovebox-login-broker-schoology\"
   and env($SDEP)[\"VAULT_K8S_ROLE\"] == \"glovebox-login-broker-schoology\" != env($DEP)[\"VAULT_K8S_ROLE\"]
   and env($SDEP)[\"LOGIN_BROKER_PASSWORD\"][0] == \"glovebox-glovebox-login-broker-schoology-credentials\" != env($DEP)[\"LOGIN_BROKER_PASSWORD\"][0]
   and env($SDEP)[\"LOGIN_BROKER_AUDIENCE\"] != env($DEP)[\"LOGIN_BROKER_AUDIENCE\"]
   and env($SDEP)[\"LOGIN_BROKER_ALLOWED_CALLERS\"] == \"glovebox/glovebox-glovebox-schoology\"
   and \"POWERSCHOOL_HOSTNAME\" not in env($SDEP) and \"SCHOOLOGY_HOST\" not in env($DEP)" \
  -f "$TMP/two.yaml"

check "two sites: each policy selects its own pods, admits its own connector, and has its own site egress" \
  "$SNP[\"podSelector\"][\"matchLabels\"][\"glovebox.io/login-broker\"] == \"schoology\"
   and $NP[\"podSelector\"][\"matchLabels\"][\"glovebox.io/login-broker\"] == \"powerschool\"
   and $SNP[\"ingress\"][0][\"from\"][0][\"podSelector\"][\"matchLabels\"][\"glovebox.io/connector\"] == \"schoology\"
   and $SNP[\"egress\"][-1][\"to\"] == [{\"ipBlock\": {\"cidr\": \"203.0.113.0/24\"}}]
   and $NP[\"egress\"][-1][\"to\"][0][\"ipBlock\"][\"cidr\"] == \"0.0.0.0/0\"" \
  -f "$TMP/two.yaml"

check "two sites: each ExternalSecret reads its own Vault path" \
  '[d["remoteRef"]["key"] for d in find("ExternalSecret", "glovebox-glovebox-login-broker-schoology-credentials")["spec"]["data"]] == ["eso/glovebox-schoology"] * 3
   and [d["remoteRef"]["key"] for d in find("ExternalSecret", "glovebox-glovebox-login-broker-powerschool-credentials")["spec"]["data"]] == ["eso/glovebox-powerschool"] * 3' \
  -f "$TMP/two.yaml"

refuse "two brokers sharing a Vault role" "use the same vault.k8sRole" \
  -f "$TMP/two.yaml" --set loginBrokers.schoology.vault.k8sRole=glovebox-login-broker-powerschool
refuse "two brokers for one account" "write the same vault.sessionPath" \
  -f "$TMP/two.yaml" --set loginBrokers.schoology.vault.sessionPath=glovebox/powerschool/default/session

# --- refusals ---------------------------------------------------------------

refuse "a plaintext Vault address" "vault.addr must be an https URL" \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.vault.addr=http://vault:8200
refuse "no Vault role" "vault.k8sRole is required" \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.vault.k8sRole=
refuse "the state stored in the session secret" "statePath must not be the session path" \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.vault.statePath=glovebox/powerschool/default/session
refuse "blackout windows without a zone" "blackout.timeZone is required" \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.blackout.timeZone=
refuse "nobody allowed to call" "nobody may call it" \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.connector=
refuse "the API and metrics on one port" "port and metricsPort must differ" \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.metricsPort=8080
refuse "no password mapping" "must map LOGIN_BROKER_USERNAME and LOGIN_BROKER_PASSWORD" \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.credentials.env.LOGIN_BROKER_PASSWORD=null
refuse "the request mailbox and a broker for the same session" "choose one way to log in on demand" \
  -f "$TMP/broker.yaml" --set powerschoolAuthRefresher.enabled=true --set powerschoolAuthRefresher.onRequest.enabled=true \
  --set powerschoolAuthRefresher.suspendSchedules=true \
  --set "powerschoolAuthRefresher.networkPolicy.kubernetesAPICIDRs={192.0.2.10/32}"
refuse "LIVE scheduled logins beside a broker for the same session" "is a live schedule for the same session" \
  -f "$TMP/broker.yaml" --set powerschoolAuthRefresher.enabled=true
refuse "one live schedule left among suspended ones" "schedules.evening is a live schedule" \
  -f "$TMP/broker.yaml" --set powerschoolAuthRefresher.enabled=true --set powerschoolAuthRefresher.schedules.morning.suspend=true
accept "SUSPENDED scheduled login CronJobs beside a broker (templates for a manual run)" \
  -f "$TMP/broker.yaml" --set powerschoolAuthRefresher.enabled=true --set powerschoolAuthRefresher.suspendSchedules=true
accept "each schedule suspended on its own" \
  -f "$TMP/broker.yaml" --set powerschoolAuthRefresher.enabled=true \
  --set powerschoolAuthRefresher.schedules.morning.suspend=true --set powerschoolAuthRefresher.schedules.evening.suspend=true
accept "live schedules beside a broker, explicitly acknowledged" \
  -f "$TMP/broker.yaml" --set powerschoolAuthRefresher.enabled=true --set loginBrokers.powerschool.acknowledgeScheduledLogins=true
accept "live schedules for ANOTHER session are nobody's business" \
  -f "$TMP/broker.yaml" --set powerschoolAuthRefresher.enabled=true \
  --set powerschoolAuthRefresher.vault.sessionPath=glovebox/powerschool/other/session
check "the daily login budget reaches the binary" \
  'env(find("Deployment", "glovebox-glovebox-login-broker-powerschool"))["LOGIN_BROKER_DAILY_LOGIN_BUDGET"] == "6"' \
  -f "$TMP/broker.yaml"
check "the budget can be lowered" \
  'env(find("Deployment", "glovebox-glovebox-login-broker-powerschool"))["LOGIN_BROKER_DAILY_LOGIN_BUDGET"] == "3"' \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.dailyLoginBudget=3
refuse "the budget raised above the ceiling" "dailyLoginBudget must be between 1 and 6" \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.dailyLoginBudget=12
accept "an existing credentials Secret instead of the ExternalSecret" \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.credentials.existingSecret=my-secret \
  --set loginBrokers.powerschool.vault.secretStoreRef.name=
check "an existing credentials Secret: no ExternalSecret, and the Deployment reads that Secret" \
  'find("ExternalSecret", "glovebox-glovebox-login-broker-powerschool-credentials") is None
   and env(find("Deployment", "glovebox-glovebox-login-broker-powerschool"))["LOGIN_BROKER_PASSWORD"] == ("my-secret", "password")' \
  -f "$TMP/broker.yaml" --set loginBrokers.powerschool.credentials.existingSecret=my-secret

# --- the connector's side ---------------------------------------------------

check "the connector is told where its broker is and which token to present" \
  "$PSCFG[\"login_on_demand\"] == {\"enabled\": True, \"broker\": {
        \"url\": \"http://glovebox-glovebox-login-broker-powerschool.glovebox.svc:8080\",
        \"token_file\": \"/var/run/secrets/login-broker/token\", \"timeout_seconds\": 420}}" \
  -f "$TMP/wired.yaml"

check "the connector's pod: own ServiceAccount, NO API token, one token projected for the broker's audience" \
  "$PSPOD[\"serviceAccountName\"] == \"glovebox-glovebox-powerschool\" and $PSPOD[\"automountServiceAccountToken\"] is False
   and [v[\"projected\"][\"sources\"] for v in $PSPOD[\"volumes\"] if v[\"name\"] == \"login-broker-token\"] == [[{\"serviceAccountToken\": {
        \"audience\": \"glovebox-glovebox-login-broker-powerschool.glovebox\", \"expirationSeconds\": 600, \"path\": \"token\"}}]]
   and [m for m in $PSPOD[\"containers\"][0][\"volumeMounts\"] if m[\"name\"] == \"login-broker-token\"]
        == [{\"name\": \"login-broker-token\", \"mountPath\": \"/var/run/secrets/login-broker\", \"readOnly\": True}]" \
  -f "$TMP/wired.yaml"

check "the audience the connector's token is minted for is the one the broker demands" \
  "env($DEP)[\"LOGIN_BROKER_AUDIENCE\"] == [v for v in $PSPOD[\"volumes\"] if v[\"name\"] == \"login-broker-token\"][0][\"projected\"][\"sources\"][0][\"serviceAccountToken\"][\"audience\"]" \
  -f "$TMP/wired.yaml"

check "the connector's ServiceAccount exists, mounts nothing by default, and has no Role" \
  'find("ServiceAccount", "glovebox-glovebox-powerschool")["automountServiceAccountToken"] is False
   and [d for d in docs if d["kind"] in ("Role", "RoleBinding")] == []
   and find("ConfigMap", "glovebox-glovebox-powerschool-login-request") is None' \
  -f "$TMP/wired.yaml"

check "the broker is off for the connector by default: shared ServiceAccount, no token volume, no login_on_demand" \
  "$PSPOD[\"serviceAccountName\"] == \"glovebox-glovebox\" and \"login_on_demand\" not in $PSCFG
   and [v for v in $PSPOD[\"volumes\"] if v[\"name\"] == \"login-broker-token\"] == []" \
  -f "$TMP/broker.yaml" --set connectors.powerschool.enabled=true

refuse "both ways of asking for the same connector" "are both enabled" \
  -f "$TMP/wired.yaml" --set connectors.powerschool.loginOnDemand.enabled=true \
  --set powerschoolAuthRefresher.enabled=true --set powerschoolAuthRefresher.onRequest.enabled=true \
  --set "powerschoolAuthRefresher.networkPolicy.kubernetesAPICIDRs={192.0.2.10/32}"
refuse "the connector asking a broker that is not enabled" "requires loginBrokers.powerschool.enabled" \
  -f "$TMP/wired.yaml" --set loginBrokers.powerschool.enabled=false
refuse "the connector asking a broker that serves another connector" "would refuse this connector" \
  -f "$TMP/wired.yaml" --set loginBrokers.powerschool.connector=schoology
refuse "the connector reading another session than the broker writes" "would start from a session nobody refreshes" \
  -f "$TMP/wired.yaml" --set loginBrokers.powerschool.vault.sessionPath=glovebox/powerschool/other/session
refuse "a call timeout too short for a login" "timeoutSeconds must be between 60 and 900" \
  -f "$TMP/wired.yaml" --set connectors.powerschool.loginBroker.timeoutSeconds=10

if [ "$FAILED" -ne 0 ]; then
  echo "test-chart-login-broker: FAILED"
  exit 1
fi
echo "test-chart-login-broker: all checks passed"
