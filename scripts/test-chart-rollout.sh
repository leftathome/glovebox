#!/usr/bin/env bash
# Render-level assertions for the Deployment rollout strategy.
#
# Why it matters: the core and every connector are one replica with
# ReadWriteOnce state, usually pinned to one node. With the Kubernetes default
# (maxSurge 25%) each upgrade needs a second pod to fit next to the old one;
# on a node at its CPU requests the surge pods stay Pending, the Helm wait
# times out and the release rolls back. That is how the 0.14.0 upgrade failed
# on 2026-11-11. rolloutStrategy lets a deployment set maxSurge 0 /
# maxUnavailable 1 (or Recreate); left empty it renders no strategy, so the
# default render is unchanged.
#
# Requires helm and python3 with PyYAML.
# Usage: scripts/test-chart-rollout.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART="$ROOT/charts/glovebox"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0

# Enable a spread of Deployments: the core, generic connectors, Schoology.
ENABLE=(--set connectors.rss.enabled=true --set connectors.youtube.enabled=true
        --set schoologyConnector.enabled=true)

render() {
  helm template glovebox "$CHART" -n glovebox "${ENABLE[@]}" "$@"
}

# check NAME PYTHON_EXPR [helm args...]
# The expression sees `deps`: {name: Deployment} for every rendered
# Deployment except the login broker (which is Recreate by design).
check() {
  local name="$1"; shift
  local expr="$1"; shift
  if render "$@" 2>"$TMP/err" | python3 -c "
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
deps = {d['metadata']['name']: d for d in docs
        if d['kind'] == 'Deployment' and 'login-broker' not in d['metadata']['name']}
assert len(deps) >= 4, 'expected core + 3 connectors, got %s' % sorted(deps)
ok = $expr
sys.exit(0 if ok else 'failed: %s' % {n: d['spec'].get('strategy') for n, d in deps.items()})
" 2>"$TMP/py"; then
    echo "ok   $name"
  else
    echo "FAIL $name"; cat "$TMP/err" "$TMP/py" | head -5 | sed 's/^/     /'; FAILED=1
  fi
}

# fails NAME [helm args...]: the render must be refused.
fails() {
  local name="$1"; shift
  if render "$@" >/dev/null 2>&1; then echo "FAIL $name (render succeeded)"; FAILED=1
  else echo "ok   $name"; fi
}

check "default: no strategy rendered (Kubernetes default applies)" \
  "all('strategy' not in d['spec'] for d in deps.values())"

check "no surge: every core/connector Deployment is RollingUpdate maxSurge 0 maxUnavailable 1" \
  "all(d['spec'].get('strategy') == {'type': 'RollingUpdate', 'rollingUpdate': {'maxSurge': 0, 'maxUnavailable': 1}} for d in deps.values())" \
  --set rolloutStrategy.rollingUpdate.maxSurge=0 --set rolloutStrategy.rollingUpdate.maxUnavailable=1

check "Recreate override renders no rollingUpdate" \
  "all(d['spec'].get('strategy') == {'type': 'Recreate'} for d in deps.values())" \
  --set rolloutStrategy.type=Recreate

check "custom surge is passed through" \
  "all(d['spec']['strategy']['rollingUpdate'] == {'maxSurge': 1, 'maxUnavailable': 0} for d in deps.values())" \
  --set rolloutStrategy.rollingUpdate.maxSurge=1 --set rolloutStrategy.rollingUpdate.maxUnavailable=0

fails "unknown strategy type is refused" --set rolloutStrategy.type=BlueGreen

exit $FAILED
