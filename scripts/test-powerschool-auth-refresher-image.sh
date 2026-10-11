#!/usr/bin/env bash
#
# test-powerschool-auth-refresher-image.sh -- build the PowerSchool login
# job's image and check that its browser starts the way the chart runs it.
#
# The unit tests never start a browser, and the one test that does
# (TestRealLoginAgainstAnUnresolvableHost) is skipped where there is none.
# This script runs that test INSIDE the image, under the container-level
# equivalent of the chart's pod: uid 65532, every capability dropped, no new
# privileges, the runtime's default seccomp profile, a read-only root, and
# tmpfs for /tmp, $HOME and /dev/shm. It is step 1 of the runbook's
# first-run checklist (docs/connectors/powerschool-auth-refresher.md).
#
# It contacts no portal: the test points the browser at a .invalid name
# (RFC 6761), which cannot resolve. No credential is involved.
#
# Usage:
#   bash scripts/test-powerschool-auth-refresher-image.sh
# Environment:
#   PS_REFRESHER_IMAGE       image tag to build/test (default
#                            glovebox-powerschool-auth-refresher:test)
#   PS_REFRESHER_SKIP_BUILD  1 = test an image that already exists
# Requires: docker, go.
#
# CI: .github/workflows/ci.yml job `powerschool-auth-refresher-image`,
# before any image is published.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

IMAGE="${PS_REFRESHER_IMAGE:-glovebox-powerschool-auth-refresher:test}"
DOCKERFILE=connectors/powerschool-auth-refresher/Dockerfile
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILED=0

ok() { echo "ok   $1"; }
fail() {
  echo "FAIL $1"
  FAILED=1
}

if [ "${PS_REFRESHER_SKIP_BUILD:-0}" != "1" ]; then
  echo "=== building $IMAGE"
  docker build -q -f "$DOCKERFILE" -t "$IMAGE" . >/dev/null
fi

# The pod's security context, as docker flags.
RUN_AS_POD=(
  --rm
  --user 65532:65532
  --cap-drop ALL
  --security-opt no-new-privileges
  --read-only
  --tmpfs /tmp:rw,size=256m,mode=1777
  --tmpfs /home/refresher:rw,size=64m,uid=65532,gid=65532,mode=0700
  --shm-size 256m
)

# 1. The image runs as the non-root user the chart expects.
uid=$(docker run --rm --entrypoint /usr/bin/id "$IMAGE" -u)
if [ "$uid" = "65532" ]; then ok "the image's default user is uid 65532"; else fail "default uid is $uid, want 65532"; fi

# 2. A browser is on PATH under a name chromedp looks for, and its version
#    was recorded at build time.
if docker run "${RUN_AS_POD[@]}" --entrypoint /bin/sh "$IMAGE" -c 'command -v chromium >/dev/null && cat /etc/chromium-version'; then
  ok "chromium is on PATH; version recorded"
else
  fail "chromium is not on PATH or /etc/chromium-version is missing"
fi

# 3. The binary starts, reads its configuration and refuses to run without
#    it (exit 2), on a read-only root.
set +e
docker run "${RUN_AS_POD[@]}" "$IMAGE" >"$WORK/noconfig.log" 2>&1
code=$?
set -e
if [ "$code" = "2" ] && grep -q '"outcome":"config_error"' "$WORK/noconfig.log"; then
  ok "the binary exits 2 (config_error) without configuration"
else
  fail "the binary exited $code without configuration"
  cat "$WORK/noconfig.log"
fi

# 4. The browser starts inside the image under the pod's restrictions, and
#    the library's "sign-in page unreachable" error is the one the binary
#    treats as safe to retry.
echo "=== building the test binary"
CGO_ENABLED=0 GOOS=linux go test -c -o "$WORK/refresher.test" ./connectors/powerschool-auth-refresher/
chmod 0755 "$WORK" "$WORK/refresher.test"
if docker run "${RUN_AS_POD[@]}" \
  -e POWERSCHOOL_REFRESHER_BROWSER_TEST=1 \
  -v "$WORK:/t:ro" --entrypoint /t/refresher.test "$IMAGE" \
  -test.run '^TestRealLoginAgainstAnUnresolvableHost$' -test.v >"$WORK/browser.log" 2>&1 &&
  grep -q -- '--- PASS: TestRealLoginAgainstAnUnresolvableHost' "$WORK/browser.log"; then
  ok "Chromium starts as uid 65532 with dropped capabilities and a read-only root"
else
  fail "the browser test did not pass inside the image"
  cat "$WORK/browser.log"
fi

# 5. The test left nothing behind in the image's writable layers: the root
#    was read-only, so anything written went to the tmpfs mounts, which are
#    gone. (Asserted by the run above succeeding with --read-only.)

if [ "$FAILED" -ne 0 ]; then
  echo "test-powerschool-auth-refresher-image: FAILED"
  exit 1
fi
echo "test-powerschool-auth-refresher-image: all checks passed"
