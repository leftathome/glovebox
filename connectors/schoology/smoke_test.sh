#!/usr/bin/env bash
# smoke_test.sh: container smoke test for the schoology connector.
#
# Builds the connector image and verifies that the resulting container
# starts long enough to expose /healthz on port 8080. We feed it a mock
# credentials file so it gets past credential loading; the first poll
# will fail against the bogus host, but the framework binds the health
# server BEFORE the initial poll runs, so /healthz responds even though
# the connector will eventually exit on a permanent error.
#
# Skips silently (exit 0) if docker is not on PATH so CI without docker
# does not fail this test.

set -euo pipefail

if ! command -v docker >/dev/null 2>&1; then
    echo "skip: docker not available"
    exit 0
fi

# Run from the repo root so the Dockerfile's relative COPY paths resolve.
cd "$(git rev-parse --show-toplevel)"

IMAGE=glovebox-schoology:smoke

echo "building $IMAGE..."
docker build -f connectors/schoology/Dockerfile -t "$IMAGE" .

# Mock credentials JSON. The values are syntactically valid (so
# schoologyauth.LoadCredentials succeeds) but the host will not respond,
# so the eventual poll will fail. That is fine: /healthz comes up before
# the first poll runs.
TMP_CREDS=$(mktemp)
cat > "$TMP_CREDS" <<'EOF'
{"host":"example.schoology.com","sess_id":"smoke","csrf_token":"smoke","csrf_key":"smoke","uid":"0"}
EOF

CONTAINER=$(docker run -d --rm \
    -e SCHOOLOGY_CREDENTIALS_FILE=/creds.json \
    -e SCHOOLOGY_HOST=example.schoology.com \
    -e SCHOOLOGY_TRIGGER_TOKEN=smoke \
    -e GLOVEBOX_STAGING_DIR=/tmp/staging \
    -e GLOVEBOX_STATE_DIR=/tmp/state \
    -v "$TMP_CREDS:/creds.json:ro" \
    -p 18080:8080 \
    "$IMAGE")

cleanup() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    rm -f "$TMP_CREDS"
}
trap cleanup EXIT

# Wait for /healthz to come up. The framework binds the health server
# inside NewFramework, before the initial poll, so this should respond
# within a couple of seconds even though credentials are bogus.
for i in $(seq 1 10); do
    if curl -sf http://127.0.0.1:18080/healthz >/dev/null 2>&1; then
        echo "healthz: OK"
        exit 0
    fi
    sleep 1
done

echo "healthz: never responded after 10s"
echo "--- container logs ---"
docker logs "$CONTAINER" 2>&1 || true
exit 1
