#!/usr/bin/env bash
# Render-level assertions for connectors that write the record journal
# (docs/superpowers/plans/2026-10-28-record-journal-part-b.md, Tasks 20-22):
# the PowerSchool connector's records block, its projected journal token, the
# refusals of decisions S12 and S13, the connector record types (S14) and the
# spool alerts.
#
# Off by default: with records off the chart must render exactly as
# BASE_REF's (default origin/main). When BASE_REF does not resolve that
# section is skipped with a note -- except in CI (CI or REQUIRE_BASE_REF
# set), where it is a failure.
#
# Requires helm and python3 with PyYAML; promtool, when installed, also runs
# the alert unit tests (scripts/testdata/records-alerts-test.yaml).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/charts/glovebox"
VALUES="$ROOT/scripts/testdata/records-values.yaml"
JVALUES="$ROOT/scripts/testdata/journal-values.yaml"
BASE_REF="${BASE_REF:-origin/main}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0

render() { helm template glovebox "$CHART" -n openclaw "$@"; }

# check NAME PYTHON_EXPR [helm args...]; the expression sees docs,
# find(kind, name), kinds, pod(name) (a connector Deployment's pod spec or
# None), cfg(name) (a connector's parsed config.json or None),
# core_config (the core's parsed config.json), tokens(name) (the
# serviceAccountToken sources of that connector's pod), mounts(name) (its
# container's volumeMounts by name), alerts (the records PrometheusRule's
# rules by alert name, or {}).
check() {
  local name="$1"; shift
  local expr="$1"; shift
  if render "$@" 2>"$TMP/err" | python3 -I -c "
import sys, yaml, json
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
def find(kind, name):
    hits = [d for d in docs if d['kind'] == kind and d['metadata']['name'] == name]
    return hits[0] if hits else None
kinds = sorted(d['kind'] + '/' + d['metadata']['name'] for d in docs)
def pod(name):
    d = find('Deployment', 'glovebox-glovebox-' + name)
    return d['spec']['template']['spec'] if d else None
def cfg(name):
    d = find('ConfigMap', 'glovebox-glovebox-' + name + '-config')
    return json.loads(d['data']['config.json']) if d else None
_core = find('ConfigMap', 'glovebox-glovebox-config')
core_config = json.loads(_core['data']['config.json']) if _core else None
def tokens(name):
    return [s['serviceAccountToken'] for v in pod(name).get('volumes', []) for s in v.get('projected', {}).get('sources', []) if 'serviceAccountToken' in s]
def vols(name):
    return {v['name']: v for v in pod(name).get('volumes', [])}
def mounts(name):
    return {m['name']: m for m in pod(name)['containers'][0].get('volumeMounts', [])}
_pr = find('PrometheusRule', 'glovebox-glovebox-connector-records')
alerts = {r['alert']: r for g in _pr['spec']['groups'] for r in g['rules']} if _pr else {}
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
check "off: no journal token, no records block, no records alerts" \
  "pod('powerschool') is None and not alerts"
check "off: powerschool enabled, records off: no records block, no journal token" \
  "'records' not in cfg('powerschool') and 'journal-token' not in vols('powerschool') and 'journal-token' not in mounts('powerschool') and not alerts" \
  --set connectors.powerschool.enabled=true

# Values that make the login broker and the login mailbox render.
LB=(--set connectors.powerschool.loginBroker.enabled=true --set loginBrokers.powerschool.enabled=true
    --set 'loginBrokers.powerschool.networkPolicy.kubernetesAPICIDRs={192.0.2.10/32}')
LOD=(--set connectors.powerschool.loginOnDemand.enabled=true --set powerschoolAuthRefresher.enabled=true
     --set powerschoolAuthRefresher.onRequest.enabled=true
     --set 'powerschoolAuthRefresher.networkPolicy.kubernetesAPICIDRs={192.0.2.10/32}')

# same NAME [helm args...]: BASE_REF's chart and this one render identically
# (and the render succeeds: two identical errors are not a pass).
same() {
  local name="$1"; shift
  if ! render "$@" >/dev/null 2>"$TMP/err"; then
    echo "FAIL $name (does not render)"; cat "$TMP/err"; FAILED=1
  elif diff -u <(helm template glovebox "$TMP/base/charts/glovebox" -n openclaw "$@" 2>&1) \
              <(render "$@" 2>&1) >"$TMP/diff"; then
    echo "ok   $name"
  else
    echo "FAIL $name"; head -40 "$TMP/diff"; FAILED=1
  fi
}

# same_objects NAME KIND/NAME,... [helm args...]: those objects render
# identically on BASE_REF and here (the rest of the render may differ).
same_objects() {
  local name="$1"; shift
  local objs="$1"; shift
  local pick="
import sys, yaml
want = sys.argv[1].split(',')
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
got = {d['kind'] + '/' + d['metadata']['name']: d for d in docs}
missing = [w for w in want if w not in got]
if missing:
    sys.exit('missing: ' + ', '.join(missing))
yaml.safe_dump([got[w] for w in want], sys.stdout, sort_keys=True)
"
  if diff -u <(helm template glovebox "$TMP/base/charts/glovebox" -n openclaw "$@" 2>&1 | python3 -I -c "$pick" "$objs" 2>&1) \
              <(render "$@" 2>&1 | python3 -I -c "$pick" "$objs" 2>&1) >"$TMP/diff"; then
    echo "ok   $name"
  else
    echo "FAIL $name"; head -40 "$TMP/diff"; FAILED=1
  fi
}

PSOBJS="Deployment/glovebox-glovebox-powerschool,ConfigMap/glovebox-glovebox-powerschool-config,ServiceAccount/glovebox-glovebox-connector-powerschool"
if git -C "$ROOT" rev-parse --verify --quiet "$BASE_REF^{commit}" >/dev/null 2>&1; then
  mkdir -p "$TMP/base"
  git -C "$ROOT" archive "$BASE_REF" charts/glovebox | tar -x -C "$TMP/base"
  same "off: default values render exactly as $BASE_REF"
  same "off: powerschool and rss enabled render exactly as $BASE_REF" \
    --set connectors.powerschool.enabled=true --set connectors.rss.enabled=true
  same "off: powerschool with metrics.prometheusRule renders exactly as $BASE_REF" \
    --set connectors.powerschool.enabled=true --set metrics.prometheusRule.enabled=true
  same "off: powerschool with the login broker renders exactly as $BASE_REF" \
    --set connectors.powerschool.enabled=true "${LB[@]}"
  same_objects "off: journal enabled, records off: powerschool pod as on $BASE_REF" "$PSOBJS" \
    -f "$VALUES" --set connectors.powerschool.records.enabled=false
  same_objects "off: journal values, records off: powerschool pod as on $BASE_REF" "$PSOBJS" \
    -f "$JVALUES" --set connectors.powerschool.enabled=true
else
  if [ -n "${CI:-}${REQUIRE_BASE_REF:-}" ]; then
    echo "FAIL off: $BASE_REF not available in CI"; FAILED=1
  else
    echo "skip off: $BASE_REF not available"
  fi
fi

# --- records on -----------------------------------------------------------
V=(-f "$VALUES")
check "on: records block in config.json" \
  "cfg('powerschool')['records'] == {'enabled': True, 'journal_url': 'http://glovebox-glovebox-journal:9095', 'token_file': '/var/run/secrets/glovebox-journal/token', 'timeout_seconds': 90, 'attendance_day_settle_school_days': 3, 'spool': {'max_age_hours': 168, 'max_bytes': 268435456}}" "${V[@]}"
check "on: records block is the Go fixture (connectors/powerschool TestHelmRenderedRecordsBlock)" \
  "cfg('powerschool')['records'] == json.load(open('$ROOT/connectors/powerschool/testdata/helm-rendered-records.json'))" "${V[@]}"
check "on: journal URL follows journal.port" \
  "cfg('powerschool')['records']['journal_url'] == 'http://glovebox-glovebox-journal:9096'" "${V[@]}" --set journal.port=9096
check "on: values reach the block" \
  "cfg('powerschool')['records']['timeout_seconds'] == 120 and cfg('powerschool')['records']['attendance_day_settle_school_days'] == 5 and cfg('powerschool')['records']['spool'] == {'max_age_hours': 72, 'max_bytes': 4294967296}" \
  "${V[@]}" --set connectors.powerschool.records.timeoutSeconds=120 --set connectors.powerschool.records.attendanceDaySettleSchoolDays=5 \
  --set connectors.powerschool.records.spool.maxAgeHours=72 --set connectors.powerschool.records.spool.maxBytes=4294967296
check "on: exactly one projected token, audience glovebox-journal, no API token" \
  "tokens('powerschool') == [{'audience': 'glovebox-journal', 'expirationSeconds': 600, 'path': 'token'}] and pod('powerschool')['automountServiceAccountToken'] is False and vols('powerschool')['journal-token']['projected']['defaultMode'] == 0o440" "${V[@]}"
check "on: token mounted read-only where token_file points" \
  "mounts('powerschool')['journal-token'] == {'name': 'journal-token', 'mountPath': '/var/run/secrets/glovebox-journal', 'readOnly': True}" "${V[@]}"
check "on: tokenExpirationSeconds reaches the projection" \
  "tokens('powerschool')[0]['expirationSeconds'] == 3600" "${V[@]}" --set connectors.powerschool.records.tokenExpirationSeconds=3600
check "on: with the login broker, two tokens for two audiences" \
  "len(tokens('powerschool')) == 2 and len(set(t['audience'] for t in tokens('powerschool'))) == 2 and 'glovebox-journal' in [t['audience'] for t in tokens('powerschool')] and pod('powerschool')['automountServiceAccountToken'] is False and 'login_on_demand' in cfg('powerschool') and 'records' in cfg('powerschool')" \
  "${V[@]}" "${LB[@]}"
check "on: the RSS pod has no journal token and no records block" \
  "tokens('rss') == [] and 'journal-token' not in vols('rss') and 'records' not in cfg('rss') and not any(k.endswith('connector-rss') for k in core_config['journal']['services'])" "${V[@]}"
# only_records NAME [helm args...]: rendered with records on and with records
# off (same values otherwise), the two differ in exactly the records block of
# the powerschool config.json, the journal-token volume and mount, the
# config checksum annotation and the records alerts.
only_records() {
  local name="$1"; shift
  render "$@" >"$TMP/on.yaml" 2>"$TMP/err" && render "$@" --set connectors.powerschool.records.enabled=false >"$TMP/off.yaml" 2>>"$TMP/err" || {
    echo "FAIL $name"; cat "$TMP/err"; FAILED=1; return; }
  if python3 -I -c "
import sys, yaml, json
def load(p):
    return {d['kind'] + '/' + d['metadata']['name']: d for d in yaml.safe_load_all(open(p)) if d}
on, off = load(sys.argv[1]), load(sys.argv[2])
cm = 'ConfigMap/glovebox-glovebox-powerschool-config'
dep = 'Deployment/glovebox-glovebox-powerschool'
c = json.loads(on[cm]['data']['config.json'])
assert c.pop('records')['enabled'] is True
assert c == json.loads(off[cm]['data']['config.json']), 'config differs beyond records'
on[cm]['data']['config.json'] = off[cm]['data']['config.json']
# checksum/config hashes every connector's ConfigMap (connector-configmap.yaml
# renders them all), so it changes on every connector Deployment.
for side in (on, off):
    for k, d in side.items():
        if k.startswith('Deployment/glovebox-glovebox-'):
            d['spec']['template']['metadata'].get('annotations', {}).pop('checksum/config', None)
t = on[dep]['spec']['template']
vols = [v for v in t['spec']['volumes'] if v['name'] != 'journal-token']
assert len(vols) == len(t['spec']['volumes']) - 1, 'no journal-token volume'
t['spec']['volumes'] = vols
c0 = t['spec']['containers'][0]
mounts = [m for m in c0['volumeMounts'] if m['name'] != 'journal-token']
assert len(mounts) == len(c0['volumeMounts']) - 1, 'no journal-token mount'
c0['volumeMounts'] = mounts
on.pop('PrometheusRule/glovebox-glovebox-connector-records', None)
assert on == off, sorted(k for k in set(on) | set(off) if on.get(k) != off.get(k))
" "$TMP/on.yaml" "$TMP/off.yaml" 2>"$TMP/err"; then echo "ok   $name"; else echo "FAIL $name"; cat "$TMP/err"; FAILED=1; fi
}
only_records "on: records add only the block, the token volume and its mount" "${V[@]}"
only_records "on: same with the login broker" "${V[@]}" "${LB[@]}"
only_records "on: same with filesystem ingest" "${V[@]}" --set connectors.powerschool.ingestMode=filesystem
only_records "on: same with the read-only root off" "${V[@]}" --set connectors.powerschool.readOnlyRootFilesystem=false

# Refusals (S12, S13, ranges).
expect_fail "refused: records without journal.enabled" "needs journal.enabled" "${V[@]}" --set journal.enabled=false
expect_fail "refused: records with loginOnDemand" "loginOnDemand" "${V[@]}" "${LOD[@]}"
expect_fail "refused: records without the writer connector entry" "journal.networkPolicy.writerConnectors" "${V[@]}" \
  --set 'journal.networkPolicy.writerConnectors=null'
expect_fail "refused: records without the services entry" "no write_types" "${V[@]}" \
  --set-json 'journal.services={"openclaw/openclaw-gateway":{"read":true,"readers":"*"}}' --set 'journal.networkPolicy.writerConnectors=null'
expect_fail "refused: services entry without write_types" "no write_types" "${V[@]}" \
  --set-json 'journal.services={"openclaw/openclaw-gateway":{"read":true,"readers":"*"},"openclaw/glovebox-glovebox-connector-powerschool":{"read":true}}' --set 'journal.networkPolicy.writerConnectors=null'
expect_fail "refused: tokenExpirationSeconds 300" "tokenExpirationSeconds must be 600-3600" "${V[@]}" --set connectors.powerschool.records.tokenExpirationSeconds=300
expect_fail "refused: tokenExpirationSeconds 3601" "tokenExpirationSeconds must be 600-3600" "${V[@]}" --set connectors.powerschool.records.tokenExpirationSeconds=3601
expect_fail "refused: settle -1" "attendanceDaySettleSchoolDays must be 1-10" "${V[@]}" --set connectors.powerschool.records.attendanceDaySettleSchoolDays=-1
expect_fail "refused: settle 11" "attendanceDaySettleSchoolDays must be 1-10" "${V[@]}" --set connectors.powerschool.records.attendanceDaySettleSchoolDays=11
expect_fail "refused: timeoutSeconds 9" "timeoutSeconds must be 10-600" "${V[@]}" --set connectors.powerschool.records.timeoutSeconds=9
expect_fail "refused: timeoutSeconds 601" "timeoutSeconds must be 10-600" "${V[@]}" --set connectors.powerschool.records.timeoutSeconds=601
expect_fail "refused: maxAgeHours -1" "spool.maxAgeHours must be 1-2160" "${V[@]}" --set connectors.powerschool.records.spool.maxAgeHours=-1
expect_fail "refused: maxAgeHours 2161" "spool.maxAgeHours must be 1-2160" "${V[@]}" --set connectors.powerschool.records.spool.maxAgeHours=2161
expect_fail "refused: maxBytes below 1 MiB" "spool.maxBytes must be" "${V[@]}" --set connectors.powerschool.records.spool.maxBytes=1048575
expect_fail "refused: maxBytes above 4 GiB" "spool.maxBytes must be" "${V[@]}" --set connectors.powerschool.records.spool.maxBytes=4294967297
expect_fail "refused: records on another connector" "connectors.rss.records.enabled" "${V[@]}" --set connectors.rss.records.enabled=true
check "edge values Go accepts render" \
  "cfg('powerschool')['records']['timeout_seconds'] == 600 and cfg('powerschool')['records']['attendance_day_settle_school_days'] == 10 and cfg('powerschool')['records']['spool'] == {'max_age_hours': 2160, 'max_bytes': 1048576} and tokens('powerschool')[0]['expirationSeconds'] == 600" \
  "${V[@]}" --set connectors.powerschool.records.timeoutSeconds=600 --set connectors.powerschool.records.attendanceDaySettleSchoolDays=10 \
  --set connectors.powerschool.records.spool.maxAgeHours=2160 --set connectors.powerschool.records.spool.maxBytes=1048576

# --- connector record types (S14) ------------------------------------------
TYPES="$CHART/journal-record-types/powerschool.json"
check "types: none without connectorRecordTypes (journal values)" \
  "[t['name'] for t in core_config['journal']['record_types']] == ['assignment']" -f "$JVALUES"
check "types: connectorRecordTypes [powerschool] renders the file's seven types" \
  "core_config['journal']['record_types'] == json.load(open('$TYPES')) and len(core_config['journal']['record_types']) == 7 and all(t['owner'] == 'powerschool' for t in core_config['journal']['record_types'])" \
  "${V[@]}"
check "types: the core config is the Go fixture (internal/config TestJournalHelmRenderedRecordsFixture)" \
  "core_config == json.load(open('$ROOT/internal/config/testdata/helm-rendered-records.json'))" "${V[@]}"
# A write_type the core does not have would stop the core at startup
# (policy.New: unknown record type), and every item with it.
expect_fail "types: records on without connectorRecordTypes" \
  "journal.services \"openclaw/glovebox-glovebox-connector-powerschool\" may write record type \"course_mark\", which is not in the journal's record_types" \
  "${V[@]}" --set 'journal.connectorRecordTypes=null'
expect_fail "types: any services write_type outside record_types" \
  "may write record type \"nosuch\", which is not in the journal's record_types" \
  -f "$JVALUES" --set-json 'journal.services."openclaw/glovebox-glovebox-connector-powerschool".write_types=["assignment","nosuch"]'
check "types: recordTypes come first, then the connector's" \
  "[t['name'] for t in core_config['journal']['record_types']] == ['household_note'] + [t['name'] for t in json.load(open('$TYPES'))]" \
  "${V[@]}" --set 'journal.connectorRecordTypes={powerschool}' \
  --set-json 'journal.recordTypes=[{"name":"household_note","schema_version":1,"owner":"household","fields":{"text":{"type":"text"}},"fixed_audience":["guardians"]}]'
check "types: a later version of a connector type in recordTypes is not a duplicate" \
  "len(core_config['journal']['record_types']) == 8" \
  "${V[@]}" --set 'journal.connectorRecordTypes={powerschool}' \
  --set-json 'journal.recordTypes=[{"name":"assignment","schema_version":2,"owner":"powerschool","fields":{"title":{"type":"string"}}}]'
expect_fail "types: unknown connector name" "journal.connectorRecordTypes names \"nosuch\"" \
  "${V[@]}" --set 'journal.connectorRecordTypes={nosuch}'
expect_fail "types: a name with a path in it" "journal.connectorRecordTypes names \"../values\"" \
  "${V[@]}" --set 'journal.connectorRecordTypes={../values}'
expect_fail "types: listed twice" "journal.connectorRecordTypes lists \"powerschool\" twice" \
  "${V[@]}" --set 'journal.connectorRecordTypes={powerschool,powerschool}'
expect_fail "types: a type version also in recordTypes" "assignment version 1 is defined twice" \
  -f "$JVALUES" --set 'journal.connectorRecordTypes={powerschool}'
check "types: the documented attendance rules and staleness pass through" \
  "[r['name'] for r in core_config['journal']['rules']] == ['full_day_absence', 'partial_day_absence'] and core_config['journal']['staleness_prefixes'] == ['source=powerschool/']" \
  "${V[@]}" --set 'journal.connectorRecordTypes={powerschool}' \
  --set-json 'journal.staleness=[{"prefix":"source=powerschool/","maxAgeHours":96}]' \
  --set-json 'journal.rules=[{"name":"full_day_absence","match":{"record_type":"attendance_day","kind":["created","updated"],"field_becomes":{"classification":"full_day","any_unexcused":true}},"importance":"high"},{"name":"partial_day_absence","match":{"record_type":"attendance_day","field_becomes":{"classification":"partial_day","any_unexcused":true}},"importance":"normal"}]'

# --- spool alerts ---------------------------------------------------------
ALERTS="{'GloveboxJournalSpoolNotEmpty', 'GloveboxJournalSpoolDeploymentFault', 'GloveboxJournalSpoolRejected', 'GloveboxJournalSpoolDropped'}"
check "alerts: the four spool alerts with records on" "set(alerts) == $ALERTS" "${V[@]}"
check "alerts: every alert has a summary and a runbook description" \
  "all(r['annotations'].get('summary') and len(r['annotations'].get('description', '')) > 200 for r in alerts.values())" "${V[@]}"
check "alerts: aggregated by namespace and connector" \
  "all('by (namespace, connector' in r['expr'] for r in alerts.values())" "${V[@]}"
check "alerts: deployment fault counts retry_alert answers only; dropped leaves out erased subjects" \
  "'action=\"retry_alert\"' in alerts['GloveboxJournalSpoolDeploymentFault']['expr'] and 'reason!=\"erased_subject\"' in alerts['GloveboxJournalSpoolDropped']['expr'] and alerts['GloveboxJournalSpoolNotEmpty']['for'] == '5m'" "${V[@]}"
check "alerts: extraSelector reaches every expression" \
  "all(r['expr'].count('glovebox_journal_spool_') == r['expr'].count('namespace=\"openclaw\"') for r in alerts.values())" \
  "${V[@]}" --set 'metrics.prometheusRule.extraSelector=namespace="openclaw"'
check "alerts: severity and labels from metrics.prometheusRule" \
  "all(r['labels'] == {'severity': 'critical'} for r in alerts.values()) and find('PrometheusRule', 'glovebox-glovebox-connector-records')['metadata']['labels']['release'] == 'kps'" \
  "${V[@]}" --set metrics.prometheusRule.severity=critical --set metrics.prometheusRule.labels.release=kps
check "alerts: none without metrics.prometheusRule.enabled" "not alerts" "${V[@]}" --set metrics.prometheusRule.enabled=false
check "alerts: none with records off" "not alerts" "${V[@]}" --set connectors.powerschool.records.enabled=false
check "alerts: none with records on but powerschool disabled" "not alerts" "${V[@]}" --set connectors.powerschool.enabled=false

if command -v promtool >/dev/null 2>&1; then
  for extra in "" 'namespace="openclaw"'; do
    render "${V[@]}" --set "metrics.prometheusRule.extraSelector=$extra" | python3 -I -c "
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d and d['kind'] == 'PrometheusRule' and d['metadata']['name'] == 'glovebox-glovebox-connector-records']
yaml.safe_dump({'groups': docs[0]['spec']['groups']}, sys.stdout)
" >"$TMP/rules.yaml"
    if promtool check rules "$TMP/rules.yaml" >"$TMP/err" 2>&1; then
      echo "ok   promtool: spool rules parse (extraSelector '$extra')"
    else
      echo "FAIL promtool: spool rules (extraSelector '$extra')"; cat "$TMP/err"; FAILED=1
    fi
  done
  render "${V[@]}" | python3 -I -c "
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d and d['kind'] == 'PrometheusRule' and d['metadata']['name'] == 'glovebox-glovebox-connector-records']
groups = docs[0]['spec']['groups']
for g in groups:
    for r in g['rules']:
        r.pop('annotations', None)
yaml.safe_dump({'groups': groups}, sys.stdout)
" >"$TMP/rules.yaml"
  cp "$ROOT/scripts/testdata/records-alerts-test.yaml" "$TMP/records-alerts-test.yaml"
  if (cd "$TMP" && promtool test rules records-alerts-test.yaml) >"$TMP/err" 2>&1; then
    echo "ok   promtool: spool alert unit tests"
  else
    echo "FAIL promtool: spool alert unit tests"; cat "$TMP/err"; FAILED=1
  fi
else
  echo "skip promtool: not installed"
fi

if [ -d "$TMP/base" ]; then
  same_objects "types: journal values without connectorRecordTypes: core config as on $BASE_REF" \
    "ConfigMap/glovebox-glovebox-config" -f "$JVALUES"
fi

exit "$FAILED"
