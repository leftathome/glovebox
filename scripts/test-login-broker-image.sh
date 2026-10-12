#!/usr/bin/env bash
#
# test-login-broker-image.sh -- build the login broker's image and check
# that it starts, and that its browser starts, the way the chart runs it.
#
# The counterpart of scripts/test-powerschool-auth-refresher-image.sh for
# connectors/login-broker: the same base, the same browser, another binary.
# The unit tests never start a browser, and the one test that does
# (TestRealLoginAgainstAnUnresolvableHost) is skipped where there is none.
# This script runs that test INSIDE the image, under the container-level
# equivalent of the chart's pod: uid 65532, every capability dropped, no new
# privileges, the runtime's default seccomp profile, a read-only root, and
# tmpfs for /tmp, $HOME and /dev/shm.
#
# It contacts no portal: the test points the browser at a .invalid name
# (RFC 6761), which cannot resolve. No credential is involved.
#
# Usage:
#   bash scripts/test-login-broker-image.sh
# Environment:
#   LOGIN_BROKER_IMAGE       image tag to build/test (default
#                            glovebox-login-broker:test)
#   LOGIN_BROKER_SKIP_BUILD  1 = test an image that already exists
# Requires: docker, go.
#
# CI: .github/workflows/ci.yml job `login-broker-image`, before any image
# is published.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

IMAGE="${LOGIN_BROKER_IMAGE:-glovebox-login-broker:test}"
DOCKERFILE=connectors/login-broker/Dockerfile
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILED=0

ok() { echo "ok   $1"; }
fail() {
  echo "FAIL $1"
  FAILED=1
}

if [ "${LOGIN_BROKER_SKIP_BUILD:-0}" != "1" ]; then
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
  --tmpfs /home/broker:rw,size=64m,uid=65532,gid=65532,mode=0700
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
#    it (exit 2), on a read-only root -- and listens on nothing.
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

# 4. A configuration error never echoes a value: the password, given with
#    an otherwise incomplete configuration, is not in the output.
set +e
docker run "${RUN_AS_POD[@]}" \
  -e LOGIN_BROKER_PASSWORD=image-test-password-value \
  -e LOGIN_BROKER_SITE=image-test-password-value \
  "$IMAGE" >"$WORK/partial.log" 2>&1
code=$?
set -e
if [ "$code" != "2" ] || ! grep -q '"outcome":"config_error"' "$WORK/partial.log"; then
  fail "the binary exited $code on a partial configuration, want 2 (config_error)"
  cat "$WORK/partial.log"
elif grep -q 'image-test-password-value' "$WORK/partial.log"; then
  fail "a configuration error echoed a value"
  cat "$WORK/partial.log"
else
  ok "a configuration error names settings, not values"
fi

# 5. The browser starts inside the image under the pod's restrictions, and
#    the library's "sign-in page unreachable" error is the one the broker
#    treats as "nothing was submitted".
echo "=== building the test binary"
CGO_ENABLED=0 GOOS=linux go test -c -o "$WORK/broker.test" ./connectors/login-broker/
chmod 0755 "$WORK" "$WORK/broker.test"
if docker run "${RUN_AS_POD[@]}" \
  -e LOGIN_BROKER_BROWSER_TEST=1 \
  -v "$WORK:/t:ro" --entrypoint /t/broker.test "$IMAGE" \
  -test.run '^TestRealLoginAgainstAnUnresolvableHost$' -test.v >"$WORK/browser.log" 2>&1 &&
  grep -q -- '--- PASS: TestRealLoginAgainstAnUnresolvableHost' "$WORK/browser.log"; then
  ok "Chromium starts as uid 65532 with dropped capabilities and a read-only root"
else
  fail "the browser test did not pass inside the image"
  cat "$WORK/browser.log"
fi

if [ "$FAILED" -ne 0 ]; then
  echo "test-login-broker-image: FAILED"
  exit 1
fi
echo "test-login-broker-image: all checks passed"
