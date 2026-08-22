#!/usr/bin/env bash
# The shipped ruleset exists twice: configs/default-rules.json, which the
# corpus gate measures, and charts/glovebox/rules.json, which is what a
# Helm install actually mounts into the pod. Nothing kept them in sync.
#
# They have never diverged -- every edit so far touched both in the same
# commit -- but that is discipline, not a guarantee. A divergence would be
# silent and would invert the meaning of the gate: CI would keep reporting
# a detection rate for a ruleset that no deployment runs. The failure mode
# is a chart shipping weaker rules than the ones measured, which is exactly
# the case where nobody goes looking.
#
# Byte-identical is the right test rather than semantically-equal: the two
# files have no reason to differ at all, so any difference is a mistake,
# and comparing bytes needs no JSON parser to stay correct as the schema
# grows.
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

canonical=configs/default-rules.json
chart_copy=charts/glovebox/rules.json

for f in "$canonical" "$chart_copy"; do
	if [[ ! -f $f ]]; then
		echo "ERROR: $f is missing; the ruleset must exist in both places." >&2
		exit 1
	fi
done

if ! diff -u "$canonical" "$chart_copy" >/dev/null; then
	echo "ERROR: $chart_copy has drifted from $canonical." >&2
	echo "The corpus gate measures $canonical; a Helm install mounts $chart_copy." >&2
	echo "Copy the canonical file over the chart copy and commit both:" >&2
	echo "    cp $canonical $chart_copy" >&2
	echo >&2
	diff -u "$canonical" "$chart_copy" >&2 || true
	exit 1
fi

echo "ruleset in sync: $canonical == $chart_copy"
