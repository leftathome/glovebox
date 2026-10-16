#!/usr/bin/env bash
# Every chart check in one place: helm lint, then each scripts/test-chart-*.sh.
# Run by the chart-test CI jobs (.gitlab-ci.yml, .github/workflows/ci.yml) and
# by hand before a chart change is pushed. Runs every script even after a
# failure, so one run reports all of them, and exits non-zero if any failed.
#
# Requires helm, git and python3 with PyYAML. scripts/test-chart-serviceaccounts.sh
# compares against BASE_REF (default origin/main); CI fetches it first.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FAILED=()

echo "== helm lint"
if ! helm lint "$ROOT/charts/glovebox"; then
  FAILED+=("helm lint")
fi

for t in "$ROOT"/scripts/test-chart-*.sh; do
  echo "== $(basename "$t")"
  if ! bash "$t"; then
    FAILED+=("$(basename "$t")")
  fi
done

if [ "${#FAILED[@]}" -ne 0 ]; then
  echo "chart checks FAILED: ${FAILED[*]}"
  exit 1
fi
echo "chart checks: all passed"
