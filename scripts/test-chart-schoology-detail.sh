#!/usr/bin/env bash
# Render-level assertions for the Schoology connector's assignment_detail
# config block (charts/glovebox/templates/schoology-connector-configmap.yaml).
#
# The block decides whether the connector reads assignment pages at all and
# under which child's rule the result is staged, so its rendered shape is
# pinned here: off by default, on only with a per-kid rule ahead of "*", and
# never clobbering a block the operator wrote directly in config.
#
# Requires helm and python3 with PyYAML. Usage: scripts/test-chart-schoology-detail.sh
set -euo pipefail

CHART="$(cd "$(dirname "$0")/.." && pwd)/charts/glovebox"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0

render() {
  helm template glovebox "$CHART" -n glovebox --set schoologyConnector.enabled=true \
    -s templates/schoology-connector-configmap.yaml "$@"
}

# check NAME PYTHON_EXPR [helm args...]: cfg is the rendered config.json.
check() {
  local name="$1"; shift
  local expr="$1"; shift
  if render "$@" 2>"$TMP/err" | python3 -c "
import sys, json, yaml
doc = [d for d in yaml.safe_load_all(sys.stdin) if d and d.get('kind') == 'ConfigMap'][0]
cfg = json.loads(doc['data']['config.json'])
ad = cfg.get('assignment_detail')
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
  elif grep -q "$pattern" "$TMP/err"; then
    echo "ok   $name"
  else
    echo "FAIL $name (failed for another reason)"; cat "$TMP/err"; FAILED=1
  fi
}

cat >"$TMP/enabled.yaml" <<'YAML'
schoologyConnector:
  assignmentDetail:
    kids: [k1]
    maxPerPoll: 12
    refreshHours: 18
    requestSpacingMs: 2000
  config:
    rules:
      - {match: "schoology:k1:assignment", destination: school, data_subject: k1}
      - {match: "schoology:k1:assignment_detail", destination: school, data_subject: k1}
      - {match: "*", destination: school}
YAML

cat >"$TMP/wildcard-first.yaml" <<'YAML'
schoologyConnector:
  assignmentDetail:
    kids: [k1]
  config:
    rules:
      - {match: "*", destination: school}
      - {match: "schoology:k1:assignment_detail", destination: school, data_subject: k1}
YAML

cat >"$TMP/no-rule.yaml" <<'YAML'
schoologyConnector:
  assignmentDetail:
    kids: [k1]
  config:
    rules:
      - {match: "schoology:k1:assignment", destination: school, data_subject: k1}
YAML

cat >"$TMP/user-block.yaml" <<'YAML'
schoologyConnector:
  config:
    assignment_detail:
      kids: [k2]
      max_per_poll: 5
    rules:
      - {match: "schoology:k2:assignment_detail", destination: school, data_subject: k2}
      - {match: "*", destination: school}
YAML

cat >"$TMP/user-block-no-rule.yaml" <<'YAML'
schoologyConnector:
  config:
    assignment_detail:
      kids: [k2]
    rules:
      - {match: "*", destination: school}
YAML

check "off by default: no assignment_detail block" \
  "ad is None"

check "enabled: kids and every tunable are rendered" \
  "ad == {'kids': ['k1'], 'max_per_poll': 12, 'refresh_hours': 18, 'request_spacing_ms': 2000}" \
  -f "$TMP/enabled.yaml"

check "enabled with defaults: 20 per poll, 20 h refresh, 1500 ms spacing" \
  "ad == {'kids': ['k1'], 'max_per_poll': 20, 'refresh_hours': 20, 'request_spacing_ms': 1500}" \
  -f "$TMP/enabled.yaml" \
  --set schoologyConnector.assignmentDetail.maxPerPoll=20 \
  --set schoologyConnector.assignmentDetail.refreshHours=20 \
  --set schoologyConnector.assignmentDetail.requestSpacingMs=1500

check "a block written in config is kept and wins; chart values fill the rest" \
  "ad == {'kids': ['k2'], 'max_per_poll': 5, 'refresh_hours': 20, 'request_spacing_ms': 1500}" \
  -f "$TMP/user-block.yaml"

check "the rest of the config is untouched" \
  "cfg['kids'][0]['name'] == 'k1' and cfg['rules'][-1]['match'] == '*'" \
  -f "$TMP/enabled.yaml"

refuse "kid enabled with only the default rules (wildcard would catch it)" "wildcard rule comes before" \
  --set 'schoologyConnector.assignmentDetail.kids={k1}'

refuse "kid enabled with no matching rule at all" "has no rule matching" \
  -f "$TMP/no-rule.yaml"

refuse "kid enabled with the rule behind the wildcard" "wildcard rule comes before" \
  -f "$TMP/wildcard-first.yaml"

refuse "kid enabled in a config block without a rule" "wildcard rule comes before" \
  -f "$TMP/user-block-no-rule.yaml"

if [ "$FAILED" -ne 0 ]; then
  echo "chart assignment_detail checks FAILED"; exit 1
fi
echo "chart assignment_detail checks passed"
