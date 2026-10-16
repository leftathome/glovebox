#!/usr/bin/env bash
# Render-level assertions for workload identities (glovebox-wekc.19): one
# ServiceAccount per connector Deployment, one per importer Job, one for the
# core, no API token mounted where nothing uses the Kubernetes API, and every
# Role bound to the account of the one workload that needs it.
#
# Why it matters: the record journal authorises writes by ServiceAccount
# (docs/superpowers/specs/2026-10-16-record-journal-design.md, 4.9c). The RSS
# connector parses whatever the open internet sends it; it must not share an
# identity with the connectors that write children's school records.
#
# The last section renders BASE_REF's chart (default origin/main) and this
# one with the same values and asserts the only differences are the intended
# ones. Set GLOVEBOX_EXTRA_VALUES to a colon-separated list of values files
# (production values, kept out of this repository) to compare those renders
# too. When BASE_REF does not resolve the section is skipped with a note --
# except in CI (CI or REQUIRE_BASE_REF set), where that is a failure.
#
# Requires helm, git and python3 with PyYAML.
# Usage: scripts/test-chart-serviceaccounts.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/charts/glovebox"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0
BASE_REF="${BASE_REF:-origin/main}"

render() {
  helm template glovebox "$CHART" -n glovebox "$@"
}

# check NAME PYTHON_EXPR [helm args...]
#
# The expression sees:
#   docs      every rendered object
#   find(kind, name)
#   pod(d)    the pod spec of a Deployment or Job
#   conns     {connector name: its Deployment}
#   sa_of     {connector name: its pod's serviceAccountName}
#   bound     {ServiceAccount name: sorted [Kind/name of every (Cluster)RoleBinding naming it]}
#   projected(d)  the serviceAccountToken sources of d's pod's projected volumes
check() {
  local name="$1"; shift
  local expr="$1"; shift
  if render "$@" 2>"$TMP/err" | python3 -c "
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
keys = [(d['kind'], d['metadata']['name']) for d in docs]
dupes = sorted({k for k in keys if keys.count(k) > 1})
if dupes:
    sys.exit('duplicate objects: %s' % dupes)
def find(kind, name):
    hits = [d for d in docs if d['kind'] == kind and d['metadata']['name'] == name]
    return hits[0] if hits else None
def pod(d):
    return d['spec']['template']['spec']
def labels(d):
    return d['metadata'].get('labels') or {}
conns = {labels(d)['glovebox.io/connector']: d for d in docs
         if d['kind'] == 'Deployment' and labels(d).get('app.kubernetes.io/component') == 'connector'}
sa_of = {n: pod(d).get('serviceAccountName') for n, d in conns.items()}
bound = {}
for d in docs:
    if d['kind'] in ('RoleBinding', 'ClusterRoleBinding'):
        for s in d.get('subjects') or []:
            if s['kind'] == 'ServiceAccount':
                bound.setdefault(s['name'], []).append(d['kind'] + '/' + d['metadata']['name'])
bound = {k: sorted(v) for k, v in bound.items()}
def projected(d):
    return [src['serviceAccountToken'] for v in pod(d).get('volumes') or [] if 'projected' in v
            for src in v['projected']['sources'] if 'serviceAccountToken' in src]
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

# Every connector the chart knows, enabled at once.
ALL=()
for c in $(python3 -c "
import yaml
print(' '.join(yaml.safe_load(open('$CHART/values.yaml'))['connectors']))"); do
  ALL+=(--set "connectors.$c.enabled=true")
done
ALL+=(--set schoologyConnector.enabled=true)

cat >"$TMP/ondemand.yaml" <<'YAML'
powerschoolAuthRefresher:
  enabled: true
  suspendSchedules: true
  onRequest:
    enabled: true
  networkPolicy:
    kubernetesAPICIDRs: [192.0.2.10/32]
connectors:
  rss:
    enabled: true
  powerschool:
    enabled: true
    loginOnDemand:
      enabled: true
    externalSecret:
      enabled: true
      secretStoreRef:
        name: vault-default
schoologyConnector:
  enabled: true
YAML

cat >"$TMP/broker.yaml" <<'YAML'
loginBrokers:
  powerschool:
    enabled: true
    networkPolicy:
      kubernetesAPICIDRs: [192.0.2.10/32]
connectors:
  rss:
    enabled: true
  powerschool:
    enabled: true
    loginBroker:
      enabled: true
    externalSecret:
      enabled: true
      secretStoreRef:
        name: vault-default
schoologyConnector:
  enabled: true
YAML

cat >"$TMP/importers.yaml" <<'YAML'
importers:
  apple:
    enabled: true
    input:
      existingClaim: apple-archive
  mbox:
    enabled: true
    input:
      existingClaim: mbox-archive
YAML

# --- the core ---------------------------------------------------------------

check "the core keeps <fullname> as its account, with no token by default" \
  'pod(find("Deployment", "glovebox-glovebox"))["serviceAccountName"] == "glovebox-glovebox"
   and pod(find("Deployment", "glovebox-glovebox"))["automountServiceAccountToken"] is False
   and find("ServiceAccount", "glovebox-glovebox")["automountServiceAccountToken"] is False
   and [d["metadata"]["name"] for d in docs if d["kind"] == "ServiceAccount"] == ["glovebox-glovebox"]'

check "the core mounts its token only for the archive listener's Vault login (ingest.auth.enabled)" \
  'pod(find("Deployment", "glovebox-glovebox"))["automountServiceAccountToken"] is True
   and find("ServiceAccount", "glovebox-glovebox")["automountServiceAccountToken"] is True
   and all(pod(d)["automountServiceAccountToken"] is False for d in conns.values())' \
  "${ALL[@]}" --set ingest.auth.enabled=true

# --- connectors ---------------------------------------------------------------

check "every connector Deployment has an account of its own, named <fullname>-connector-<name>" \
  'len(conns) >= 20 and len(set(sa_of.values())) == len(conns)
   and all(sa == "glovebox-glovebox-connector-" + n for n, sa in sa_of.items())
   and "glovebox-glovebox" not in sa_of.values()' \
  "${ALL[@]}"

check "each of those accounts is rendered, labelled with its connector, and mounts no token" \
  'all(find("ServiceAccount", sa) is not None
       and find("ServiceAccount", sa)["automountServiceAccountToken"] is False
       and labels(find("ServiceAccount", sa))["glovebox.io/connector"] == n
       for n, sa in sa_of.items())' \
  "${ALL[@]}"

check "no connector pod mounts an API token, and none has a projected token, by default" \
  'all(pod(d)["automountServiceAccountToken"] is False and projected(d) == [] for d in conns.values())' \
  "${ALL[@]}"

check "nothing is bound to any connector's account by default" \
  'not (set(bound) & set(sa_of.values()))' \
  "${ALL[@]}"

check "the account follows the connector: enabling one renders exactly its account and the core's" \
  'sorted(d["metadata"]["name"] for d in docs if d["kind"] == "ServiceAccount")
     == ["glovebox-glovebox", "glovebox-glovebox-connector-rss"]' \
  --set connectors.rss.enabled=true

# --- on-demand login (the mailbox): the one connector that uses the API ------

check "on-demand: RSS and Schoology have distinct accounts, no token, no binding, no projected token" \
  'len({sa_of["rss"], sa_of["schoology"], sa_of["powerschool"], "glovebox-glovebox"}) == 4
   and all(pod(conns[n])["automountServiceAccountToken"] is False and projected(conns[n]) == [] for n in ("rss", "schoology"))
   and sa_of["rss"] not in bound and sa_of["schoology"] not in bound' \
  -f "$TMP/ondemand.yaml"

check "on-demand: the PowerSchool connector mounts its API token and alone among connectors" \
  'pod(conns["powerschool"])["automountServiceAccountToken"] is True
   and [n for n, d in conns.items() if pod(d)["automountServiceAccountToken"]] == ["powerschool"]' \
  -f "$TMP/ondemand.yaml"

check "on-demand: every binding names the account of the one workload it is for" \
  'bound == {
     "glovebox-glovebox-connector-powerschool": ["RoleBinding/glovebox-glovebox-powerschool-login-on-demand"],
     "glovebox-glovebox-powerschool-auth-refresher": ["RoleBinding/glovebox-glovebox-powerschool-login-request-reader"]}
   and find("RoleBinding", "glovebox-glovebox-powerschool-login-on-demand")["roleRef"]["name"] == "glovebox-glovebox-powerschool-login-on-demand"' \
  -f "$TMP/ondemand.yaml"

# --- login broker: a projected token for one audience, no API token ----------

check "broker: the PowerSchool connector has no API token, one token for the broker's audience, and no binding" \
  'pod(conns["powerschool"])["automountServiceAccountToken"] is False
   and projected(conns["powerschool"]) == [{"audience": "glovebox-glovebox-login-broker-powerschool.glovebox", "expirationSeconds": 600, "path": "token"}]
   and sa_of["powerschool"] not in bound' \
  -f "$TMP/broker.yaml"

check "broker: it allows the PowerSchool connector's own account and nobody else" \
  '[e["value"] for e in pod(find("Deployment", "glovebox-glovebox-login-broker-powerschool"))["containers"][0]["env"]
      if e["name"] == "LOGIN_BROKER_ALLOWED_CALLERS"] == ["glovebox/glovebox-glovebox-connector-powerschool"]' \
  -f "$TMP/broker.yaml"

check "broker: the TokenReview binding names the broker's account only; RSS has nothing" \
  'bound == {"glovebox-glovebox-login-broker-powerschool": ["ClusterRoleBinding/glovebox-glovebox-glovebox-login-broker-powerschool-tokenreview"]}
   and pod(conns["rss"])["automountServiceAccountToken"] is False and projected(conns["rss"]) == []' \
  -f "$TMP/broker.yaml"

# --- CronJobs keep their own accounts ----------------------------------------

check "the login and Schoology refresher Jobs keep their own accounts" \
  'all(c["spec"]["jobTemplate"]["spec"]["template"]["spec"]["serviceAccountName"] == "glovebox-glovebox-powerschool-auth-refresher"
       for c in docs if c["kind"] == "CronJob" and labels(c).get("app.kubernetes.io/component") == "powerschool-auth-refresher")
   and all(c["spec"]["jobTemplate"]["spec"]["template"]["spec"]["serviceAccountName"] == "glovebox-glovebox-schoology-auth-refresher"
       for c in docs if c["kind"] == "CronJob" and labels(c).get("app.kubernetes.io/component") == "schoology-auth-refresher")
   and len([c for c in docs if c["kind"] == "CronJob"]) >= 2' \
  -f "$TMP/ondemand.yaml" --set schoologyAuthRefresher.enabled=true

# --- importers -----------------------------------------------------------------

check "every importer Job has an account of its own, with no token" \
  'all(pod(j)["serviceAccountName"] == "glovebox-glovebox-importer-" + labels(j)["glovebox.io/importer"]
       and pod(j)["automountServiceAccountToken"] is False
       and find("ServiceAccount", pod(j)["serviceAccountName"])["automountServiceAccountToken"] is False
       for j in docs if j["kind"] == "Job")
   and len([j for j in docs if j["kind"] == "Job"]) == 2' \
  -f "$TMP/importers.yaml"

# --- overrides -------------------------------------------------------------------

check "connectors.<name>.serviceAccount.name renames the account and the pod follows" \
  'sa_of["rss"] == "feeds" and find("ServiceAccount", "feeds") is not None
   and find("ServiceAccount", "glovebox-glovebox-connector-rss") is None' \
  --set connectors.rss.enabled=true --set connectors.rss.serviceAccount.name=feeds

check "serviceAccount.create=false leaves the object to the operator; annotations are copied" \
  'sa_of["rss"] == "glovebox-glovebox-connector-rss" and find("ServiceAccount", "glovebox-glovebox-connector-rss") is None
   and find("ServiceAccount", "glovebox-glovebox-connector-schoology")["metadata"]["annotations"] == {"example.com/owner": "school"}' \
  --set connectors.rss.enabled=true --set connectors.rss.serviceAccount.create=false \
  --set schoologyConnector.enabled=true --set-string 'schoologyConnector.serviceAccount.annotations.example\.com/owner=school'

check "a renamed PowerSchool account is what the broker allows and what the Role is bound to" \
  'sa_of["powerschool"] == "ps-reader"
   and [e["value"] for e in pod(find("Deployment", "glovebox-glovebox-login-broker-powerschool"))["containers"][0]["env"]
      if e["name"] == "LOGIN_BROKER_ALLOWED_CALLERS"] == ["glovebox/ps-reader"]' \
  -f "$TMP/broker.yaml" --set connectors.powerschool.serviceAccount.name=ps-reader

check "a renamed PowerSchool account is what the on-demand Role is bound to" \
  'bound.get("ps-reader") == ["RoleBinding/glovebox-glovebox-powerschool-login-on-demand"]' \
  -f "$TMP/ondemand.yaml" --set connectors.powerschool.serviceAccount.name=ps-reader

# --- no two workloads on one account ------------------------------------------

refuse "a connector renamed onto the core's account" \
  'ServiceAccount "glovebox-glovebox" would be used by both the core (serviceAccount) and connectors.rss' \
  --set connectors.rss.enabled=true --set connectors.rss.serviceAccount.name=glovebox-glovebox

refuse "the PowerSchool connector (with its ConfigMap-write Role) renamed onto RSS's account" \
  'ServiceAccount "glovebox-glovebox-connector-rss" would be used by both connectors.powerschool and connectors.rss' \
  -f "$TMP/ondemand.yaml" --set connectors.powerschool.serviceAccount.name=glovebox-glovebox-connector-rss

refuse "create: false does not hide a collision" \
  'ServiceAccount "glovebox-glovebox-connector-schoology" would be used by both connectors.rss and schoologyConnector' \
  --set connectors.rss.enabled=true --set connectors.rss.serviceAccount.create=false \
  --set connectors.rss.serviceAccount.name=glovebox-glovebox-connector-schoology --set schoologyConnector.enabled=true

refuse "a connector renamed onto a refresher's account" \
  'would be used by both connectors.powerschool and powerschoolAuthRefresher' \
  -f "$TMP/ondemand.yaml" --set connectors.powerschool.serviceAccount.name=glovebox-glovebox-powerschool-auth-refresher

refuse "an importer renamed onto the login broker's account" \
  'would be used by both importers.apple and loginBrokers.powerschool' \
  -f "$TMP/broker.yaml" --set importers.apple.enabled=true --set importers.apple.input.existingClaim=a \
  --set importers.apple.serviceAccount.name=glovebox-glovebox-login-broker-powerschool

refuse "the core renamed onto a connector's account" \
  'would be used by both the core (serviceAccount) and connectors.rss' \
  --set connectors.rss.enabled=true --set serviceAccount.name=glovebox-glovebox-connector-rss

# --- differences from the base chart: only the intended ones -------------------

# compare NAME [helm args...]: render BASE_REF's chart and this one with the
# same arguments; anything beyond the intended differences fails.
compare() {
  local name="$1"; shift
  helm template glovebox "$TMP/base/charts/glovebox" -n glovebox "$@" >"$TMP/old.yaml" 2>"$TMP/err" || {
    echo "FAIL $name (base render)"; cat "$TMP/err"; FAILED=1; return; }
  render "$@" >"$TMP/new.yaml" 2>"$TMP/err" || {
    echo "FAIL $name (render)"; cat "$TMP/err"; FAILED=1; return; }
  if python3 - "$TMP/old.yaml" "$TMP/new.yaml" >"$TMP/err" 2>&1 <<'PY'
import sys, yaml, re
def load(p):
    out = {}
    for d in yaml.safe_load_all(open(p)):
        if not d:
            continue
        k = (d['kind'], d['metadata']['name'])
        if k in out:
            sys.exit('%s: duplicate object %s/%s' % (p, k[0], k[1]))
        out[k] = d
    return out
old, new = load(sys.argv[1]), load(sys.argv[2])
bad = []
# Objects added: only per-workload ServiceAccounts.
for k in sorted(set(new) - set(old)):
    comp = (new[k]['metadata'].get('labels') or {}).get('app.kubernetes.io/component')
    if not (k[0] == 'ServiceAccount' and comp in ('connector', 'importer')):
        bad.append('added %s/%s' % k)
# Objects removed: only the PowerSchool connector's old account.
for k in sorted(set(old) - set(new)):
    if not (k[0] == 'ServiceAccount' and re.fullmatch(r'.+-powerschool', k[1])):
        bad.append('removed %s/%s' % k)
# Objects in both: equal once the pod identity fields and the broker's
# caller list (which names a connector's account) are set aside.
def scrub(d):
    d = yaml.safe_load(yaml.safe_dump(d))
    spec = d.get('spec') or {}
    pods = []
    if 'template' in spec:
        pods.append(spec['template']['spec'])
    if 'jobTemplate' in spec:
        pods.append(spec['jobTemplate']['spec']['template']['spec'])
    for p in pods:
        p.pop('serviceAccountName', None)
        p.pop('automountServiceAccountToken', None)
        for c in p.get('containers') or []:
            for e in c.get('env') or []:
                if e.get('name') == 'LOGIN_BROKER_ALLOWED_CALLERS':
                    e['value'] = '<callers>'
    if d['kind'] in ('RoleBinding', 'ClusterRoleBinding'):
        for s in d.get('subjects') or []:
            if s['kind'] == 'ServiceAccount':
                s['name'] = '<account>'
    return d
for k in sorted(set(old) & set(new)):
    if scrub(old[k]) != scrub(new[k]):
        bad.append('changed %s/%s beyond its identity' % k)
# The identity fields that did change must say what this change intends.
for k, d in new.items():
    spec = d.get('spec') or {}
    p = (spec.get('template') or {}).get('spec')
    if k[0] in ('Deployment', 'Job') and p is not None:
        if p.get('automountServiceAccountToken') is None:
            bad.append('%s/%s states no automountServiceAccountToken' % k)
print('\n'.join(bad))
sys.exit(1 if bad else 0)
PY
  then
    echo "ok   $name"
  else
    echo "FAIL $name"; cat "$TMP/err"; FAILED=1
  fi
}

if git -C "$ROOT" rev-parse --verify --quiet "$BASE_REF^{commit}" >/dev/null 2>&1; then
  mkdir -p "$TMP/base"
  git -C "$ROOT" archive "$BASE_REF" charts/glovebox | tar -x -C "$TMP/base"
  # Render the base under this chart's version and appVersion: a release bump
  # changes the chart labels, the default image tags and the checksums of
  # every hashed object, none of which is an identity difference.
  grep -E '^(version|appVersion):' "$CHART/Chart.yaml" >"$TMP/chart-version"
  grep -vE '^(version|appVersion):' "$TMP/base/charts/glovebox/Chart.yaml" >"$TMP/chart-rest"
  cat "$TMP/chart-rest" "$TMP/chart-version" >"$TMP/base/charts/glovebox/Chart.yaml"
  compare "vs $BASE_REF, default values: only identity differences"
  compare "vs $BASE_REF, every connector: only identity differences" "${ALL[@]}"
  compare "vs $BASE_REF, on-demand login: only identity differences" -f "$TMP/ondemand.yaml"
  compare "vs $BASE_REF, login broker: only identity differences" -f "$TMP/broker.yaml"
  compare "vs $BASE_REF, importers: only identity differences" -f "$TMP/importers.yaml"
  compare "vs $BASE_REF, ingest.auth on: only identity differences" "${ALL[@]}" --set ingest.auth.enabled=true
  if [ -n "${GLOVEBOX_EXTRA_VALUES:-}" ]; then
    IFS=: read -r -a extra <<<"$GLOVEBOX_EXTRA_VALUES"
    for f in "${extra[@]}"; do
      compare "vs $BASE_REF, $(basename "$f"): only identity differences" -f "$f"
    done
  fi
elif [ -n "${REQUIRE_BASE_REF:-}" ] || [ -n "${CI:-}" ]; then
  echo "FAIL comparison with $BASE_REF: the ref does not resolve (fetch it, e.g. git fetch origin main)"; FAILED=1
else
  echo "skip comparison with $BASE_REF (does not resolve here)"
fi

if [ "$FAILED" -ne 0 ]; then
  echo "test-chart-serviceaccounts: FAILED"
  exit 1
fi
echo "test-chart-serviceaccounts: all checks passed"
