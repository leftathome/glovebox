#!/usr/bin/env bash
# Render-level assertions for the Schoology session refresher
# (charts/glovebox/templates/schoology-auth-refresher-cronjob.yaml) and its
# alert (GloveboxSchoologyLoginJobFailed in login-prometheusrule.yaml).
#
# What is pinned: the suspend value (rendered only when true), the exit
# codes the Job never retries (a run that submitted the credentials is not
# repeated, and a held latch is not retried), the state path handed to the
# binary and the refusal of a state path equal to the session path, and the
# alert: present only with metrics.prometheusRule and the refresher both on,
# its expression, and its configurable severity.
#
# Requires helm and python3 with PyYAML.
# Usage: scripts/test-chart-schoology-refresher.sh
set -euo pipefail

CHART="$(cd "$(dirname "$0")/.." && pwd)/charts/glovebox"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
FAILED=0
ON=(--set schoologyAuthRefresher.enabled=true)

render() {
  helm template glovebox "$CHART" -n glovebox "$@"
}

# check NAME PYTHON_EXPR [helm args...]
#
# The expression sees:
#   docs    every rendered object
#   cron    the refresher CronJob, or None
#   env     its container env {name: value}
#   codes   its podFailurePolicy FailJob exit codes
#   alerts  {alert name: rule} of the login PrometheusRule ({} when absent)
check() {
  local name="$1"; shift
  local expr="$1"; shift
  if render "$@" 2>"$TMP/err" | python3 -I -c "
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
def find(kind, name):
    hits = [d for d in docs if d['kind'] == kind and d['metadata']['name'] == name]
    return hits[0] if hits else None
cron = find('CronJob', 'glovebox-glovebox-schoology-auth-refresher')
env = {}
codes = []
if cron:
    job = cron['spec']['jobTemplate']['spec']
    env = {e['name']: e.get('value') for e in job['template']['spec']['containers'][0].get('env', [])}
    codes = sorted(c for r in job.get('podFailurePolicy', {}).get('rules', []) if r['action'] == 'FailJob'
                   for c in r['onExitCodes']['values'])
pr = find('PrometheusRule', 'glovebox-glovebox-login')
alerts = {r['alert']: r for g in pr['spec']['groups'] for r in g['rules']} if pr else {}
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

# --- the CronJob ------------------------------------------------------------
check "off by default: no refresher CronJob" "cron is None"
check "suspend off by default: spec.suspend not rendered" \
  "'suspend' not in cron['spec']" "${ON[@]}"
check "suspend=true renders spec.suspend: true" \
  "cron['spec']['suspend'] is True" "${ON[@]}" --set schoologyAuthRefresher.suspend=true
check "suspend=false renders nothing" \
  "'suspend' not in cron['spec']" "${ON[@]}" --set schoologyAuthRefresher.suspend=false
check "never retried: config, not confirmed, vault refused, unconfirmed, latch held" \
  "codes == [2, 3, 4, 5, 6]" "${ON[@]}"
check "no VAULT_STATE_PATH by default (the binary puts it beside the session)" \
  "'VAULT_STATE_PATH' not in env and env['VAULT_SESSION_PATH'] == 'glovebox/schoology/default/session'" "${ON[@]}"
check "vault.statePath reaches VAULT_STATE_PATH" \
  "env['VAULT_STATE_PATH'] == 'glovebox/schoology/default/latch'" \
  "${ON[@]}" --set schoologyAuthRefresher.vault.statePath=glovebox/schoology/default/latch
expect_fail "vault.statePath equal to the session path is refused" \
  "schoologyAuthRefresher.vault.statePath must not be the session path" \
  "${ON[@]}" --set schoologyAuthRefresher.vault.statePath=/glovebox/schoology/default/session/

# --- the alert --------------------------------------------------------------
check "no alert without metrics.prometheusRule" \
  "'GloveboxSchoologyLoginJobFailed' not in alerts" "${ON[@]}"
check "no alert without the refresher" \
  "'GloveboxSchoologyLoginJobFailed' not in alerts" --set metrics.prometheusRule.enabled=true
check "alert: failed refresher Jobs in the release namespace, no for" \
  "(lambda r: r['expr'] == 'max by (job_name) (kube_job_status_failed{namespace=\"glovebox\",job_name=~\"glovebox-glovebox-schoology-auth-refresher-.*\"}) > 0' and 'for' not in r)(alerts['GloveboxSchoologyLoginJobFailed'])" \
  "${ON[@]}" --set metrics.prometheusRule.enabled=true
check "alert severity defaults to metrics.prometheusRule.severity" \
  "alerts['GloveboxSchoologyLoginJobFailed']['labels']['severity'] == 'warning'" \
  "${ON[@]}" --set metrics.prometheusRule.enabled=true
check "alert severity follows a changed metrics.prometheusRule.severity" \
  "alerts['GloveboxSchoologyLoginJobFailed']['labels']['severity'] == 'page'" \
  "${ON[@]}" --set metrics.prometheusRule.enabled=true --set metrics.prometheusRule.severity=page
check "schoologyLoginJobSeverity=critical sets only this alert's severity" \
  "alerts['GloveboxSchoologyLoginJobFailed']['labels']['severity'] == 'critical' and all(r['labels']['severity'] == 'warning' for n, r in alerts.items() if n != 'GloveboxSchoologyLoginJobFailed')" \
  "${ON[@]}" --set metrics.prometheusRule.enabled=true --set metrics.prometheusRule.schoologyLoginJobSeverity=critical \
  --set schoologyConnector.enabled=true
check "alert description names exit 3 (not confirmed), exit 6 (no login) and how to clear the latch" \
  "(lambda d: 'Exit 3' in d and 'NOT CONFIRMED' in d and 'possibly rejected' in d and 'Exit 6' in d and 'NO LOGIN WAS MADE' in d and 'statePath' in d and 'AUTH-RECOVERY' in d)(alerts['GloveboxSchoologyLoginJobFailed']['annotations']['description'])" \
  "${ON[@]}" --set metrics.prometheusRule.enabled=true

if [ "$FAILED" -ne 0 ]; then
  echo "schoology refresher chart checks FAILED"
  exit 1
fi
echo "schoology refresher chart checks: all passed"
