#!/usr/bin/env bash
set -euo pipefail

# Container tests for glovebox
# Requires: docker daemon running
# Usage: ./container_test.sh

IMAGE_NAME="glovebox:test"
PASS=0
FAIL=0

log_pass() { echo "PASS: $1"; ((PASS++)); }
log_fail() { echo "FAIL: $1"; ((FAIL++)); }

cleanup() {
    docker rm -f glovebox-test 2>/dev/null || true
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

TMPDIR=$(mktemp -d)

echo "=== Building image ==="
docker build -t "$IMAGE_NAME" . || { log_fail "docker build"; exit 1; }
log_pass "docker build succeeds"

# Check image size
SIZE=$(docker images "$IMAGE_NAME" --format '{{.Size}}')
echo "Image size: $SIZE"
# Parse size - accept MB or MiB under 50
SIZE_NUM=$(echo "$SIZE" | grep -oP '[\d.]+')
SIZE_UNIT=$(echo "$SIZE" | grep -oP '[A-Za-z]+')
if [[ "$SIZE_UNIT" == "MB" || "$SIZE_UNIT" == "MiB" ]] && (( $(echo "$SIZE_NUM < 50" | bc -l) )); then
    log_pass "image size under 50MB ($SIZE)"
else
    log_fail "image size should be under 50MB, got $SIZE"
fi

echo "=== Setting up test directories ==="
mkdir -p "$TMPDIR"/{staging,quarantine,audit,failed,agents/messaging/workspace/inbox,shared}

# Write a test item
ITEM_DIR="$TMPDIR/staging/20260328-container-test"
mkdir -p "$ITEM_DIR"
echo "Hello from container test" > "$ITEM_DIR/content.raw"
cat > "$ITEM_DIR/metadata.json" << 'METAEOF'
{
  "source": "email",
  "sender": "test@example.com",
  "timestamp": "2026-03-28T12:00:00Z",
  "destination_agent": "messaging",
  "content_type": "text/plain"
}
METAEOF

echo "=== Testing container starts and responds ==="
docker run -d --name glovebox-test \
    -v "$TMPDIR/staging:/data/glovebox/staging" \
    -v "$TMPDIR/quarantine:/data/glovebox/quarantine" \
    -v "$TMPDIR/audit:/data/glovebox/audit" \
    -v "$TMPDIR/failed:/data/glovebox/failed" \
    -v "$TMPDIR/agents:/data/agents" \
    -v "$TMPDIR/shared:/data/shared" \
    -p 9090:9090 \
    "$IMAGE_NAME"

# Wait for startup
sleep 2

# Check container is running
if docker ps --filter "name=glovebox-test" --format '{{.Status}}' | grep -q "Up"; then
    log_pass "container starts and stays running"
else
    log_fail "container is not running"
    docker logs glovebox-test
fi

# Check metrics endpoint
if curl -sf http://localhost:9090/metrics > /dev/null 2>&1; then
    log_pass "/metrics endpoint responds with HTTP 200"
else
    log_fail "/metrics endpoint not responding"
fi

echo "=== Testing graceful shutdown ==="
docker stop -t 5 glovebox-test
EXIT_CODE=$(docker inspect glovebox-test --format '{{.State.ExitCode}}')
if [ "$EXIT_CODE" = "0" ]; then
    log_pass "container exits cleanly on SIGTERM (exit code 0)"
else
    log_fail "container exit code $EXIT_CODE, expected 0"
fi

echo ""
echo "=== Results ==="
echo "Passed: $PASS"
echo "Failed: $FAIL"
[ "$FAIL" -eq 0 ] || exit 1
