#!/usr/bin/env bash
# Scan the checked-in adversarial corpus through the shipped scan path and
# fail if detection has dropped below, or false positives risen above, the
# thresholds committed in testdata/adversarial-corpus/thresholds.json.
#
# Why a gate and not just a test: the efficacy fixes each landed with a
# regression test proving one bypass closed. Nothing measured the scanner as
# a whole, so a rule edit could close one hole and open three without any
# check going red. Both numbers are printed on every run so CI logs carry
# the actual measurement, not merely a pass/fail.
#
# The thresholds are what the engine measurably achieves, recorded after
# measuring. If this fails, the answer is to fix the scanner or to record a
# genuine gap in the manifest -- not to relax the numbers.
#
# Usage:
#   scripts/corpus-gate.sh          # metrics + gate
#   scripts/corpus-gate.sh -v       # also print every case
set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$repo_root"

exec go run ./cmd/corpus-gate \
	-corpus testdata/adversarial-corpus \
	-rules configs/default-rules.json \
	"$@"
