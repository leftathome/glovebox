#!/usr/bin/env bash
# Render-level assertions for the glovebox chart's NetworkPolicies.
#
# NetworkPolicy mistakes are invisible on a CNI that does not enforce policy
# (Flannel): the scanner ran for months under a deny-all egress it could not
# actually live with, and a sanitize caller's namespace was never admitted.
# Both surface only at an enforcing CNI cutover, as timeouts. These checks
# pin the rendered shape instead.
#
# Requires helm and python3 with PyYAML. Usage: scripts/test-chart-networkpolicy.sh
set -euo pipefail

CHART="$(cd "$(dirname "$0")/.." && pwd)/charts/glovebox"

render() { helm template glovebox "$CHART" -n glovebox "$@"; }

check() {
  local name="$1"; shift
  local expr="$1"; shift
  if render "$@" | python3 -c "
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d and d.get('kind') == 'NetworkPolicy']
np = {d['metadata']['name']: d['spec'] for d in docs}
scanner = np.get('glovebox-glovebox')
def ports(rules):
    return sorted({p['port'] for r in rules or [] for p in r.get('ports', [])})
def callers(spec):
    out = []
    for r in spec.get('ingress') or []:
        for f in r.get('from', []):
            for e in f.get('namespaceSelector', {}).get('matchExpressions', []):
                if e['key'] == 'kubernetes.io/metadata.name':
                    out.append((sorted(e['values']), ports([r])))
    return out
sys.exit(0 if ($expr) else 1)
"; then
    echo "ok   $name"
  else
    echo "FAIL $name"; FAILED=1
  fi
}

FAILED=0
AUTH=(--set ingest.auth.enabled=true)

check "default: scanner egress allows DNS only" \
  "ports(scanner['egress']) == [53]"
check "auth: scanner egress adds Vault 8200" \
  "ports(scanner['egress']) == [53, 8200]" "${AUTH[@]}"
check "auth + vault.namespace empty: no Vault rule" \
  "ports(scanner['egress']) == [53]" "${AUTH[@]}" --set networkPolicy.vault.namespace=
check "extraEgress is appended verbatim" \
  "ports(scanner['egress']) == [53, 443]" \
  --set 'networkPolicy.extraEgress[0].ports[0].port=443'
check "dns disabled, no auth: egress fully denied" \
  "not scanner['egress'] and 'Egress' in scanner['policyTypes']" \
  --set networkPolicy.dns.enabled=false
check "default: no out-of-namespace bearer callers" \
  "callers(scanner) == []"
check "bearerCallerNamespaces admitted to the bearer port (default 9093)" \
  "callers(scanner) == [(['nagus', 'recognizer'], [9093])]" \
  --set 'networkPolicy.bearerCallerNamespaces={recognizer,nagus}'
check "bearerCallerNamespaces follow a bearer port shared with ingest (9091)" \
  "callers(scanner) == [(['nagus'], [9091])]" "${AUTH[@]}" \
  --set config.ingest.bearerPort=0 --set 'networkPolicy.bearerCallerNamespaces={nagus}'
check "legacy archive policy still renders with a label" \
  "'glovebox-glovebox-archive-ingress' in np" --set ingest.archives.enabled=true
check "legacy archive policy dropped when its label is empty" \
  "'glovebox-glovebox-archive-ingress' not in np" --set ingest.archives.enabled=true \
  --set ingest.archives.networkPolicy.recognizerNamespaceLabel=
check "metrics restricted when allowedNamespaceLabel is set" \
  "any(f.get('namespaceSelector', {}).get('matchLabels') == {'name': 'monitoring'} for r in scanner['ingress'] for f in r.get('from', []))" \
  --set metrics.allowedNamespaceLabel=monitoring
check "networkPolicy.enabled=false renders nothing" \
  "np == {}" --set networkPolicy.enabled=false

exit "$FAILED"
