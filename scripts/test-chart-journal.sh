#!/usr/bin/env bash
# Render-level assertions for the record journal's chart objects
# (docs/superpowers/plans/2026-10-16-record-journal-core.md, Task 29).
#
# Off by default: with journal.enabled false the chart must render exactly as
# BASE_REF's (default origin/main) for the same values. When BASE_REF does not
# resolve that section is skipped with a note -- except in CI (CI or
# REQUIRE_BASE_REF set), where it is a failure.
#
# Requires helm and python3 with PyYAML; promtool, when installed, also
# checks the rendered alert expressions.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/charts/glovebox"
VALUES="$ROOT/scripts/testdata/journal-values.yaml"
BASE_REF="${BASE_REF:-origin/main}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0

render() { helm template glovebox "$CHART" -n openclaw "$@"; }

# check NAME PYTHON_EXPR [helm args...]; the expression sees docs,
# find(kind, name), core (the core Deployment's pod spec), config (its
# parsed config.json), kinds, mounts and vols (the core container's
# volumeMounts and the pod's volumes, by name).
check() {
  local name="$1"; shift
  local expr="$1"; shift
  if render "$@" 2>"$TMP/err" | python3 -I -c "
import sys, yaml, json
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
def find(kind, name):
    hits = [d for d in docs if d['kind'] == kind and d['metadata']['name'] == name]
    return hits[0] if hits else None
core = find('Deployment', 'glovebox-glovebox')['spec']['template']['spec']
config = json.loads(find('ConfigMap', 'glovebox-glovebox-config')['data']['config.json'])
kinds = sorted(d['kind'] + '/' + d['metadata']['name'] for d in docs)
mounts = {m['name']: m for m in core['containers'][0].get('volumeMounts', [])}
vols = {v['name']: v for v in core.get('volumes', [])}
rbac_rules = [r for d in docs if d['kind'] in ('Role', 'ClusterRole') for r in d.get('rules', [])]
sys.exit(0 if ($expr) else 1)
"; then echo "ok   $name"; else echo "FAIL $name"; cat "$TMP/err"; FAILED=1; fi
}

# expect_fail NAME MESSAGE_FRAGMENT [helm args...]
expect_fail() {
  local name="$1"; shift
  local frag="$1"; shift
  if render "$@" >/dev/null 2>"$TMP/err"; then echo "FAIL $name (rendered)"; FAILED=1
  elif grep -qF -- "$frag" "$TMP/err"; then echo "ok   $name"
  else echo "FAIL $name"; cat "$TMP/err"; FAILED=1; fi
}

# --- off by default -------------------------------------------------------
check "disabled: no journal objects" \
  "not [k for k in kinds if 'journal' in k] and 'journal' not in config"
check "disabled: no journal port, volumes or token" \
  "all(p['name'] != 'journal' for p in core['containers'][0]['ports']) and 'journal-roster' not in vols and 'journal-db' not in vols and core['automountServiceAccountToken'] is False"
check "disabled even with every other journal value set" \
  "not [k for k in kinds if 'journal' in k] and 'journal' not in config and 'journal-db' not in vols" \
  -f "$VALUES" --set journal.enabled=false

# same NAME [helm args...]: BASE_REF's chart and this one render identically.
same() {
  local name="$1"; shift
  if diff -u <(helm template glovebox "$TMP/base/charts/glovebox" -n openclaw "$@" 2>&1) \
              <(render "$@" 2>&1) >"$TMP/diff"; then
    echo "ok   $name"
  else
    echo "FAIL $name"; head -40 "$TMP/diff"; FAILED=1
  fi
}

if git -C "$ROOT" rev-parse --verify --quiet "$BASE_REF^{commit}" >/dev/null 2>&1; then
  mkdir -p "$TMP/base"
  git -C "$ROOT" archive "$BASE_REF" charts/glovebox | tar -x -C "$TMP/base"
  same "disabled: default values render exactly as $BASE_REF"
  same "disabled: ingest.auth.enabled renders exactly as $BASE_REF" --set ingest.auth.enabled=true
  same "disabled: journal values present but off render exactly as $BASE_REF" \
    -f "$VALUES" --set journal.enabled=false
elif [ -n "${CI:-}${REQUIRE_BASE_REF:-}" ]; then
  echo "FAIL disabled: $BASE_REF not available in CI"; FAILED=1
else
  echo "skip disabled: $BASE_REF not available"
fi

# --- enabled --------------------------------------------------------------
V=(-f "$VALUES")
check "enabled: config block" \
  "config['journal']['enabled'] and config['journal']['port'] == 9095 and config['journal']['roster']['path'] == '/etc/glovebox-roster/household.json' and config['journal']['database']['password_file'] == '/etc/glovebox-journal-db/password' and config['journal']['database']['user_file'] == '/etc/glovebox-journal-db/username' and config['journal']['database']['sslmode'] == 'require' and config['journal']['services']['openclaw/openclaw-gateway']['readers'] == '*' and config['journal']['staleness_prefixes'] == ['source=powerschool/'] and config['journal']['removal_guard'] == {'max_fraction': 0.3, 'min_keys': 3} and config['journal']['scanner_version'] == 'core-scan-1' and config['journal']['request_timeout_seconds'] == 60 and config['journal']['scan_timeout_seconds'] == 10" "${V[@]}"
check "enabled: config holds no credential" \
  "not any(k in json.dumps(config['journal']) for k in ('\"password\"', '\"user\"', 'sk-'))" "${V[@]}"
check "enabled: port, token, mounts" \
  "any(p['name'] == 'journal' and p['containerPort'] == 9095 for p in core['containers'][0]['ports']) and core['automountServiceAccountToken'] is True and mounts['journal-roster']['readOnly'] is True and 'subPath' not in mounts['journal-roster'] and mounts['journal-db']['readOnly'] is True and 'subPath' not in mounts['journal-db']" "${V[@]}"
check "enabled: core ServiceAccount mounts its token too" \
  "find('ServiceAccount', 'glovebox-glovebox')['automountServiceAccountToken'] is True" "${V[@]}"
check "enabled: roster is the ConfigMap, whole and read-only" \
  "vols['journal-roster']['configMap']['name'] == 'openclaw-household' and vols['journal-roster']['configMap']['items'] == [{'key': 'household.json', 'path': 'household.json'}] and mounts['journal-roster']['mountPath'] == '/etc/glovebox-roster'" "${V[@]}"
check "enabled: database Secret, two keys only, mode 0400, pod has an fsGroup" \
  "vols['journal-db']['secret']['secretName'] == 'glovebox-journal-db' and vols['journal-db']['secret']['defaultMode'] == 0o400 and vols['journal-db']['secret']['items'] == [{'key': 'username', 'path': 'username'}, {'key': 'password', 'path': 'password'}] and core['securityContext'].get('fsGroup')" "${V[@]}"
check "enabled: no ExternalSecret with existingSecret" \
  "find('ExternalSecret', 'glovebox-glovebox-journal-db') is None" "${V[@]}"
check "enabled: ExternalSecret creates the Secret the pod mounts" \
  "(lambda es: es['spec']['target']['name'] == vols['journal-db']['secret']['secretName'] == 'glovebox-glovebox-journal-db' and [(d['secretKey'], d['remoteRef']['key'], d['remoteRef']['property']) for d in es['spec']['data']] == [('username', 'glovebox/journal-db', 'username'), ('password', 'glovebox/journal-db', 'password')] and es['spec']['secretStoreRef'] == {'kind': 'ClusterSecretStore', 'name': 'vault-default'})(find('ExternalSecret', 'glovebox-glovebox-journal-db'))" \
  "${V[@]}" --set journal.database.existingSecret= --set journal.database.externalSecret.enabled=true --set journal.database.externalSecret.path=glovebox/journal-db
check "enabled: service" "find('Service', 'glovebox-glovebox-journal')['spec']['ports'][0]['port'] == 9095 and find('Service', 'glovebox-glovebox-journal')['spec']['selector']['app.kubernetes.io/component'] == 'scanner'" "${V[@]}"
check "enabled: tokenreview RBAC bound to the core only" \
  "find('ClusterRole', 'openclaw-glovebox-glovebox-journal-tokenreview')['rules'] == [{'apiGroups': ['authentication.k8s.io'], 'resources': ['tokenreviews'], 'verbs': ['create']}] and [(s['kind'], s['name'], s['namespace']) for s in find('ClusterRoleBinding', 'openclaw-glovebox-glovebox-journal-tokenreview')['subjects']] == [('ServiceAccount', 'glovebox-glovebox', 'openclaw')] and find('ClusterRoleBinding', 'openclaw-glovebox-glovebox-journal-tokenreview')['roleRef']['name'] == 'openclaw-glovebox-glovebox-journal-tokenreview'" "${V[@]}"
check "enabled: tokenreview binding follows a renamed core ServiceAccount" \
  "[s['name'] for s in find('ClusterRoleBinding', 'openclaw-glovebox-glovebox-journal-tokenreview')['subjects']] == ['glovebox-core'] == [core['serviceAccountName']]" \
  "${V[@]}" --set serviceAccount.name=glovebox-core
check "enabled: no RBAC on configmaps (the roster is mounted, never read)" \
  "not any('configmaps' in r.get('resources', []) or '*' in r.get('resources', []) for r in rbac_rules) and not [d for d in docs if d['kind'] in ('Role', 'RoleBinding')]" "${V[@]}"
check "enabled: rbac.create=false renders no RBAC" \
  "find('ClusterRole', 'openclaw-glovebox-glovebox-journal-tokenreview') is None and find('ClusterRoleBinding', 'openclaw-glovebox-glovebox-journal-tokenreview') is None" \
  "${V[@]}" --set journal.rbac.create=false
check "enabled: networkpolicy ingress only from named peers on 9095" \
  "(lambda np: len(np['spec']['ingress']) == 1 and np['spec']['ingress'][0]['ports'] == [{'port': 9095, 'protocol': 'TCP'}] and len(np['spec']['ingress'][0]['from']) == 2 and np['spec']['ingress'][0]['from'][0]['podSelector']['matchLabels'] == {'app.kubernetes.io/name': 'glovebox', 'app.kubernetes.io/instance': 'glovebox', 'app.kubernetes.io/component': 'connector', 'glovebox.io/connector': 'powerschool'} and np['spec']['ingress'][0]['from'][1] == {'podSelector': {'matchLabels': {'app.kubernetes.io/name': 'openclaw-gateway'}}} and np['spec']['podSelector']['matchLabels']['app.kubernetes.io/component'] == 'scanner' and np['spec']['policyTypes'] == ['Ingress', 'Egress'])(find('NetworkPolicy', 'glovebox-glovebox-journal'))" "${V[@]}"
check "enabled: networkpolicy egress to the database and the API server only" \
  "(lambda e: len(e) == 2 and e[0]['to'] == [{'namespaceSelector': {'matchLabels': {'kubernetes.io/metadata.name': 'databases-app'}}, 'podSelector': {'matchLabels': {'cnpg.io/cluster': 'postgres'}}}] and e[0]['ports'] == [{'port': 5432, 'protocol': 'TCP'}] and e[1]['to'] == [{'ipBlock': {'cidr': '192.0.2.10/32'}}] and [p['port'] for p in e[1]['ports']] == [443, 6443])(find('NetworkPolicy', 'glovebox-glovebox-journal')['spec']['egress'])" "${V[@]}"
check "enabled: the core's own policy is unchanged (journal port not in it)" \
  "all(p.get('port') != 9095 for r in find('NetworkPolicy', 'glovebox-glovebox')['spec']['ingress'] for p in r.get('ports', []))" "${V[@]}"
check "enabled: no ingress peers means no ingress rule" \
  "'ingress' not in find('NetworkPolicy', 'glovebox-glovebox-journal')['spec'] or not find('NetworkPolicy', 'glovebox-glovebox-journal')['spec']['ingress']" \
  "${V[@]}" --set 'journal.networkPolicy.writerConnectors=null' --set 'journal.networkPolicy.from=null'
check "enabled: alerts" \
  "set(r['alert'] for g in find('PrometheusRule', 'glovebox-glovebox-journal')['spec']['groups'] for r in g['rules']) == {'GloveboxJournalSnapshotHeld', 'GloveboxJournalNotReady', 'GloveboxJournalScanWithheld', 'GloveboxJournalRosterReloadFailing', 'GloveboxJournalVisibilityMismatch', 'GloveboxJournalNoSnapshots'}" "${V[@]}"
check "enabled: every alert has a summary and a description" \
  "all(r['annotations'].get('summary') and len(r['annotations'].get('description', '')) > 80 for g in find('PrometheusRule', 'glovebox-glovebox-journal')['spec']['groups'] for r in g['rules'])" "${V[@]}"
check "enabled: staleness alert also fires on absence, per prefix" \
  "(lambda r: r['labels']['prefix'] == 'source=powerschool/' and 'absent_over_time(glovebox_journal_last_snapshot_timestamp_seconds{prefix=\"source=powerschool/\"}[96h])' in r['expr'] and '> 345600' in r['expr'])([r for g in find('PrometheusRule', 'glovebox-glovebox-journal')['spec']['groups'] for r in g['rules'] if r['alert'] == 'GloveboxJournalNoSnapshots'][0])" "${V[@]}"
check "enabled: visibility mismatch is critical and fires on any increase" \
  "(lambda r: r['labels']['severity'] == 'critical' and 'increase(glovebox_journal_visibility_mismatch_total' in r['expr'] and 'unless' in r['expr'] and 'for' not in r)([r for g in find('PrometheusRule', 'glovebox-glovebox-journal')['spec']['groups'] for r in g['rules'] if r['alert'] == 'GloveboxJournalVisibilityMismatch'][0])" "${V[@]}"
check "enabled: held and not-ready alerts" \
  "(lambda rs: 'glovebox_journal_held_snapshots' in rs['GloveboxJournalSnapshotHeld']['expr'] and '> 0' in rs['GloveboxJournalSnapshotHeld']['expr'] and 'absent(glovebox_journal_ready' in rs['GloveboxJournalNotReady']['expr'])({r['alert']: r for g in find('PrometheusRule', 'glovebox-glovebox-journal')['spec']['groups'] for r in g['rules']})" "${V[@]}"
check "enabled: extraSelector reaches every expression" \
  "all('namespace=\"openclaw\"' in r['expr'] for g in find('PrometheusRule', 'glovebox-glovebox-journal')['spec']['groups'] for r in g['rules'])" \
  "${V[@]}" --set 'metrics.prometheusRule.extraSelector=namespace="openclaw"'
check "enabled: no PrometheusRule without metrics.prometheusRule.enabled" \
  "find('PrometheusRule', 'glovebox-glovebox-journal') is None" "${V[@]}" --set metrics.prometheusRule.enabled=false

# promtool parses every expression (when installed; CI's image has none).
if command -v promtool >/dev/null 2>&1; then
  for extra in "" 'namespace="openclaw"'; do
    render "${V[@]}" --set "metrics.prometheusRule.extraSelector=$extra" | python3 -I -c "
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d and d['kind'] == 'PrometheusRule' and d['metadata']['name'] == 'glovebox-glovebox-journal']
yaml.safe_dump({'groups': docs[0]['spec']['groups']}, sys.stdout)
" >"$TMP/rules.yaml"
    if promtool check rules "$TMP/rules.yaml" >"$TMP/err" 2>&1; then
      echo "ok   promtool: journal rules parse (extraSelector '$extra')"
    else
      echo "FAIL promtool: journal rules (extraSelector '$extra')"; cat "$TMP/err"; FAILED=1
    fi
  done
  # Behaviour over time (scripts/testdata/journal-rules-test.yaml), on the
  # rules rendered without an extraSelector. Annotations are dropped so the
  # tests pin labels and timing, not runbook prose.
  render "${V[@]}" | python3 -I -c "
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d and d['kind'] == 'PrometheusRule' and d['metadata']['name'] == 'glovebox-glovebox-journal']
groups = docs[0]['spec']['groups']
for g in groups:
    for r in g['rules']:
        r.pop('annotations', None)
yaml.safe_dump({'groups': groups}, sys.stdout)
" >"$TMP/rules.yaml"
  cp "$ROOT/scripts/testdata/journal-rules-test.yaml" "$TMP/journal-rules-test.yaml"
  if (cd "$TMP" && promtool test rules journal-rules-test.yaml) >"$TMP/err" 2>&1; then
    echo "ok   promtool: journal alert unit tests"
  else
    echo "FAIL promtool: journal alert unit tests"; cat "$TMP/err"; FAILED=1
  fi
else
  echo "skip promtool: not installed"
fi

API=(--set 'journal.networkPolicy.kubernetesAPICIDRs={192.0.2.10/32}')
expect_fail "enabled without roster" "journal.roster.configMap" "${API[@]}" --set journal.enabled=true --set journal.database.host=x --set journal.database.existingSecret=y
expect_fail "enabled without database" "journal.database" "${API[@]}" --set journal.enabled=true --set journal.roster.configMap=x
expect_fail "externalSecret without a Vault path" "journal.database.externalSecret.path" \
  "${V[@]}" --set journal.database.existingSecret= --set journal.database.externalSecret.enabled=true
expect_fail "sslmode other than require" "journal.database.sslmode" "${V[@]}" --set journal.database.sslmode=disable
expect_fail "enabled with no services" "journal.services to name at least one caller" \
  "${V[@]}" --set journal.services=null --set journal.networkPolicy.writerConnectors=null
expect_fail "verify-full before the CA mount exists" "glovebox-wekc.18.2" "${V[@]}" --set journal.database.sslmode=verify-full
expect_fail "pod without fsGroup" "podSecurityContext.fsGroup" "${V[@]}" --set podSecurityContext.fsGroup=null
expect_fail "empty scanner version" "journal.scannerVersion" "${V[@]}" --set journal.scannerVersion=
expect_fail "staleness prefix below the surface" "journal.staleness" "${V[@]}" --set 'journal.staleness[0].prefix=source=powerschool/surface=grades/data_subject=e_x/' --set 'journal.staleness[0].maxAgeHours=96'
expect_fail "roster key with a path in it" "journal.roster.key" "${V[@]}" --set journal.roster.key=../x
expect_fail "networkpolicy without API CIDRs" "kubernetesAPICIDRs" "${V[@]}" --set 'journal.networkPolicy.kubernetesAPICIDRs=null'
expect_fail "API CIDR wider than one address" "not a single address" "${V[@]}" --set 'journal.networkPolicy.kubernetesAPICIDRs={0.0.0.0/0}'
expect_fail "peer with only an empty pod selector" "journal.networkPolicy.from[0] selects nothing" "${V[@]}" --set-json 'journal.networkPolicy.from=[{"podSelector":{}}]'
expect_fail "peer with only an empty namespace selector" "journal.networkPolicy.from[0] selects nothing" "${V[@]}" --set-json 'journal.networkPolicy.from=[{"namespaceSelector":{}}]'
expect_fail "peer pairing a namespace with every pod in it" "from[0].podSelector is empty" "${V[@]}" --set-json 'journal.networkPolicy.from=[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"openclaw"}},"podSelector":{}}]'
expect_fail "peer pairing pods with every namespace" "from[0].namespaceSelector is empty" "${V[@]}" --set-json 'journal.networkPolicy.from=[{"podSelector":{"matchLabels":{"app":"x"}},"namespaceSelector":{}}]'
expect_fail "journal policy without the core's policy" "networkPolicy.enabled" "${V[@]}" --set networkPolicy.enabled=false

# ipBlock peers and CIDR syntax.
expect_fail "ipBlock peer 0.0.0.0/0" "from[0].ipBlock.cidr" "${V[@]}" --set-json 'journal.networkPolicy.from=[{"ipBlock":{"cidr":"0.0.0.0/0"}}]'
expect_fail "ipBlock peer /24" "from[0].ipBlock.cidr" "${V[@]}" --set-json 'journal.networkPolicy.from=[{"ipBlock":{"cidr":"192.0.2.0/24"}}]'
check "ipBlock peer of one address renders" \
  "find('NetworkPolicy', 'glovebox-glovebox-journal')['spec']['ingress'][0]['from'][-1] == {'ipBlock': {'cidr': '192.0.2.7/32'}}" \
  "${V[@]}" --set-json 'journal.networkPolicy.from=[{"ipBlock":{"cidr":"192.0.2.7/32"}}]'
expect_fail "API CIDR 999.1.1.1/32" "not a single address" "${V[@]}" --set 'journal.networkPolicy.kubernetesAPICIDRs={999.1.1.1/32}'
expect_fail "API CIDR :::/128" "not a single address" "${V[@]}" --set 'journal.networkPolicy.kubernetesAPICIDRs={:::/128}'
expect_fail "API CIDR 1::2::3/128" "not a single address" "${V[@]}" --set 'journal.networkPolicy.kubernetesAPICIDRs={1::2::3/128}'
expect_fail "API CIDR with nine IPv6 groups" "not a single address" "${V[@]}" --set 'journal.networkPolicy.kubernetesAPICIDRs={1:2:3:4:5:6:7:8:9/128}'
expect_fail "API CIDR without a prefix length" "not a single address" "${V[@]}" --set 'journal.networkPolicy.kubernetesAPICIDRs={192.0.2.10}'
check "API CIDRs: IPv6 forms that are single addresses render" \
  "[c['ipBlock']['cidr'] for c in find('NetworkPolicy', 'glovebox-glovebox-journal')['spec']['egress'][1]['to']] == ['2001:db8::1/128', 'fd00:0:0:0:0:0:0:1/128', '::1/128', '255.255.255.255/32']" \
  "${V[@]}" --set 'journal.networkPolicy.kubernetesAPICIDRs={2001:db8::1/128,fd00:0:0:0:0:0:0:1/128,::1/128,255.255.255.255/32}'
check "Cilium: kube-apiserver entity egress, CIDRs optional" \
  "(lambda c: c['spec']['egress'] == [{'toEntities': ['kube-apiserver'], 'toPorts': [{'ports': [{'port': '443', 'protocol': 'TCP'}, {'port': '6443', 'protocol': 'TCP'}]}]}] and c['spec']['endpointSelector']['matchLabels']['app.kubernetes.io/component'] == 'scanner' and len(find('NetworkPolicy', 'glovebox-glovebox-journal')['spec']['egress']) == 1)(find('CiliumNetworkPolicy', 'glovebox-glovebox-journal-apiserver'))" \
  "${V[@]}" --set 'journal.networkPolicy.kubernetesAPICIDRs=null' --set journal.networkPolicy.ciliumAPIServer.enabled=true
check "Cilium: off by default" "find('CiliumNetworkPolicy', 'glovebox-glovebox-journal-apiserver') is None" "${V[@]}"

# The Go validator's ranges, mirrored (internal/config/journal.go).
expect_fail "port = config.metricsPort" "is already used by config.metricsPort" "${V[@]}" --set journal.port=9090
expect_fail "port = ingest port" "is already used by config.ingest.port" "${V[@]}" --set journal.port=9091
expect_fail "port = split bearer port" "is already used by config.ingest.bearerPort" "${V[@]}" --set journal.port=9093 --set config.ingest.bearerPort=9093 --set ingest.auth.enabled=true
expect_fail "port = ingest TLS port" "is already used by ingest.tls.port" "${V[@]}" --set journal.port=9443 --set ingest.tls.mode=permissive --set ingest.tls.port=9443
expect_fail "port = ingest TLS default (ingest port + 1)" "is already used by ingest.tls.port" "${V[@]}" --set journal.port=9092 --set ingest.tls.mode=permissive --set ingest.tls.port=0
expect_fail "port 0" "journal.port must be 1-65535" "${V[@]}" --set journal.port=0
expect_fail "port 70000" "journal.port must be 1-65535" "${V[@]}" --set journal.port=70000
expect_fail "roster format" "journal.roster.format" "${V[@]}" --set journal.roster.format=bogus
expect_fail "reloadSeconds 0" "journal.roster.reloadSeconds must be 1-300" "${V[@]}" --set journal.roster.reloadSeconds=0
expect_fail "reloadSeconds 301" "journal.roster.reloadSeconds must be 1-300" "${V[@]}" --set journal.roster.reloadSeconds=301
expect_fail "requestTimeoutSeconds 0" "journal.requestTimeoutSeconds must be 1-600" "${V[@]}" --set journal.requestTimeoutSeconds=0
expect_fail "requestTimeoutSeconds 601" "journal.requestTimeoutSeconds must be 1-600" "${V[@]}" --set journal.requestTimeoutSeconds=601
expect_fail "scanTimeoutSeconds 0" "journal.scanTimeoutSeconds must be 1-300" "${V[@]}" --set journal.scanTimeoutSeconds=0
expect_fail "scanTimeoutSeconds 301" "journal.scanTimeoutSeconds must be 1-300" "${V[@]}" --set journal.scanTimeoutSeconds=301
expect_fail "maxFraction 1" "journal.removalGuard.maxFraction" "${V[@]}" --set journal.removalGuard.maxFraction=1
expect_fail "maxFraction 0" "journal.removalGuard.maxFraction" "${V[@]}" --set journal.removalGuard.maxFraction=0
expect_fail "minKeys -1" "journal.removalGuard.minKeys must be 0-1000" "${V[@]}" --set journal.removalGuard.minKeys=-1
expect_fail "minKeys 1001" "journal.removalGuard.minKeys must be 0-1000" "${V[@]}" --set journal.removalGuard.minKeys=1001
expect_fail "database name empty" "journal.database.name must not be empty" "${V[@]}" --set journal.database.name=
expect_fail "database host with a slash" "journal.database.host must be a single host name" "${V[@]}" --set journal.database.host=a/b
expect_fail "database host with a comma" "journal.database.host must be a single host name" "${V[@]}" --set 'journal.database.host=a\,b'
expect_fail "database host with a question mark" "journal.database.host must be a single host name" "${V[@]}" --set 'journal.database.host=a?b'
expect_fail "database port 0" "journal.database.port must be 1-65535" "${V[@]}" --set journal.database.port=0
expect_fail "database port 70000" "journal.database.port must be 1-65535" "${V[@]}" --set journal.database.port=70000
expect_fail "staleness prefix twice" "is listed twice" "${V[@]}" --set-json 'journal.staleness=[{"prefix":"source=a/","maxAgeHours":1},{"prefix":"source=a/","maxAgeHours":2}]'
check "edge values Go accepts render" \
  "config['journal']['roster']['reload_seconds'] == 300 and config['journal']['request_timeout_seconds'] == 600 and config['journal']['scan_timeout_seconds'] == 300 and config['journal']['removal_guard'] == {'max_fraction': 0.99, 'min_keys': 1000}" \
  "${V[@]}" --set journal.roster.reloadSeconds=300 --set journal.requestTimeoutSeconds=600 --set journal.scanTimeoutSeconds=300 --set journal.removalGuard.maxFraction=0.99 --set journal.removalGuard.minKeys=1000

# Identity cross-checks.
expect_fail "core ServiceAccount named default" "serviceAccount.name=default" "${V[@]}" --set serviceAccount.name=default --set serviceAccount.create=false
expect_fail "services entry for the core itself" "must not list the core's own ServiceAccount" "${V[@]}" --set-json 'journal.services={"openclaw/glovebox-glovebox":{"read":true,"readers":"*"}}' --set 'journal.networkPolicy.writerConnectors=null'
expect_fail "writer connector without write rights" "gives its ServiceAccount \"openclaw/glovebox-glovebox-connector-rss\" no write_types" "${V[@]}" --set 'journal.networkPolicy.writerConnectors={powerschool,rss}'
expect_fail "roster named like the login-request ConfigMap" "is a name this chart uses" "${V[@]}" --set journal.roster.configMap=glovebox-glovebox-powerschool-login-request
expect_fail "roster named like the core's config" "is a name this chart uses" "${V[@]}" --set journal.roster.configMap=glovebox-glovebox-config
check "journal policy managed outside the chart" \
  "find('NetworkPolicy', 'glovebox-glovebox-journal') is None and find('NetworkPolicy', 'glovebox-glovebox') is None" \
  "${V[@]}" --set networkPolicy.enabled=false --set journal.networkPolicy.enabled=false

exit "$FAILED"
