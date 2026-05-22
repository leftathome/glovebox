#!/usr/bin/env bash
#
# archive-smoke-test.sh -- 12 GiB tus.io upload smoke test for glovebox-p1zx
#
# Drives a single resumable tus.io upload against a glovebox container
# built from this repo's top-level Dockerfile. The bead acceptance
# criterion is "12 GiB mbox delivers without 413"; this script exercises
# exactly that.
#
# Assumptions:
#   - docker is on PATH (script skips with exit 0 if not).
#   - The glovebox image can be given a static ingest token via a
#     GLOVEBOX_INGEST_TOKEN env var, OR the operator has built the image
#     with a dev --static-token flag baked in. The production wire-up
#     uses Vault per spec 10; for a self-contained smoke test we need a
#     non-Vault path. If the image's TokenStore requires Vault, this
#     script will fail at the /healthz wait.
#   - The host has ~12 GiB of free space in $TMPDIR for the sparse file
#     (sparse, so actual disk usage on most filesystems is near-zero
#     until the sha256sum read materializes it; some filesystems will
#     still reserve space).
#
# What this script does NOT do:
#   - Verify the on-disk file's bytes match the source. That would
#     require a second 12 GiB read. We verify the upload finalized and
#     archives/<archive_id>/ exists.
#   - Exercise the tarball / untar path. This is the raw-mbox path only.
#   - Use chunked PATCH. A single PATCH delivers all 12 GiB. The tus.io
#     protocol allows this; the recognizer's real-world client may chunk
#     for resumability, but the listener's job here is just to accept.

set -euo pipefail

# Skip if docker not on PATH (CI-safe).
if ! command -v docker >/dev/null 2>&1; then
    echo "skip: docker not available"
    exit 0
fi

# Work from repo root.
cd "$(git rev-parse --show-toplevel)"

# Configuration. Override via env vars.
IMAGE_TAG="${IMAGE_TAG:-glovebox-archive:smoke}"
HOST_PORT="${HOST_PORT:-19091}"
CONTAINER_NAME="${CONTAINER_NAME:-glovebox-archive-smoke-$$}"
ARCHIVE_SIZE="${ARCHIVE_SIZE:-12884901888}"  # 12 GiB
ARCHIVE_ID="${ARCHIVE_ID:-smoke-$(date +%s)}"
TOKEN="${TOKEN:-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}"
SOURCE_ID="${SOURCE_ID:-recognizer}"
HEALTHZ_TIMEOUT_SEC="${HEALTHZ_TIMEOUT_SEC:-30}"

# Build the glovebox image if not already present.
if ! docker image inspect "$IMAGE_TAG" >/dev/null 2>&1; then
    echo "Building image $IMAGE_TAG..."
    docker build -t "$IMAGE_TAG" -f Dockerfile .
fi

# Scratch dir for the sparse mbox + curl response files.
TMP_DIR="$(mktemp -d)"
ARCHIVE_FILE="$TMP_DIR/smoke.mbox"
HDR_FILE="$TMP_DIR/post-headers"
BODY_FILE="$TMP_DIR/post-body"

# Cleanup container + tmp files on exit (success OR failure).
cleanup() {
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
    rm -rf "$TMP_DIR"
}
trap cleanup EXIT

# Create a sparse 12 GiB mbox file. We do not have 12 GiB of real mbox
# content lying around; a sparse file reads back as NUL bytes which is
# fine for the upload's contract (the server just hashes whatever we
# send). The well-known sha256 of N NUL bytes is deterministic per N.
echo "Creating $ARCHIVE_SIZE-byte sparse file at $ARCHIVE_FILE..."
truncate -s "$ARCHIVE_SIZE" "$ARCHIVE_FILE"
echo "Computing sha256 (this takes about a minute for 12 GiB of zeros)..."
ARCHIVE_SHA256="$(sha256sum "$ARCHIVE_FILE" | awk '{print $1}')"
echo "sha256 = $ARCHIVE_SHA256"

# Start the container. The static token + source-id are passed via env
# so the container can populate its TokenStore without contacting Vault.
echo "Starting container $CONTAINER_NAME on host port $HOST_PORT..."
docker run -d --rm \
    --name "$CONTAINER_NAME" \
    -e GLOVEBOX_INGEST_TOKEN="$TOKEN" \
    -e GLOVEBOX_INGEST_SOURCE_ID="$SOURCE_ID" \
    -p "$HOST_PORT:9091" \
    "$IMAGE_TAG" >/dev/null

# Wait for the listener to come up.
echo "Waiting for /healthz (timeout ${HEALTHZ_TIMEOUT_SEC}s)..."
ready=0
for i in $(seq 1 "$HEALTHZ_TIMEOUT_SEC"); do
    if curl -sf "http://127.0.0.1:$HOST_PORT/healthz" >/dev/null 2>&1; then
        echo "ready after ${i}s"
        ready=1
        break
    fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    echo "FAIL: /healthz never came up after ${HEALTHZ_TIMEOUT_SEC}s"
    docker logs "$CONTAINER_NAME" || true
    exit 1
fi

# Build the Upload-Metadata header (base64-encoded k=v pairs per tus.io).
b64() { printf '%s' "$1" | base64 -w0; }
UPLOAD_METADATA="archive_id $(b64 "$ARCHIVE_ID"),archive_filename $(b64 "smoke.mbox"),subtree_relative_path $(b64 "."),media_type $(b64 "archive/mbox"),matcher_id $(b64 "smoke/mbox"),provider $(b64 "smoke"),sha256 $(b64 "$ARCHIVE_SHA256"),size_bytes $(b64 "$ARCHIVE_SIZE")"

# POST /v1/archives to create the upload. We capture headers (for the
# Location response) and body (for failure diagnostics) in a single call.
echo "POST /v1/archives..."
HTTP_STATUS="$(curl -sS -D "$HDR_FILE" -o "$BODY_FILE" -w '%{http_code}' \
    -X POST "http://127.0.0.1:$HOST_PORT/v1/archives" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Tus-Resumable: 1.0.0" \
    -H "Upload-Length: $ARCHIVE_SIZE" \
    -H "Upload-Metadata: $UPLOAD_METADATA")"
if [ "$HTTP_STATUS" != "201" ]; then
    echo "FAIL: POST returned $HTTP_STATUS (expected 201). Body:"
    cat "$BODY_FILE"
    docker logs "$CONTAINER_NAME"
    exit 1
fi
LOCATION="$(grep -i '^Location:' "$HDR_FILE" | tr -d '\r' | awk '{print $2}')"
if [ -z "$LOCATION" ]; then
    echo "FAIL: POST 201 but no Location header. Headers:"
    cat "$HDR_FILE"
    exit 1
fi
UPLOAD_ID="$(basename "$LOCATION")"
echo "Got upload-id: $UPLOAD_ID (Location: $LOCATION)"

# Stream the entire 12 GiB body via a single PATCH. The listener is
# expected to accept this without imposing a 413; that is the bead's
# acceptance criterion.
echo "PATCH $LOCATION (streaming $ARCHIVE_SIZE bytes)..."
HTTP_STATUS="$(curl -sS -o "$BODY_FILE" -w '%{http_code}' \
    -X PATCH "http://127.0.0.1:$HOST_PORT$LOCATION" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Tus-Resumable: 1.0.0" \
    -H "Upload-Offset: 0" \
    -H "Content-Type: application/offset+octet-stream" \
    --data-binary "@$ARCHIVE_FILE")"
if [ "$HTTP_STATUS" != "204" ]; then
    echo "FAIL: PATCH returned $HTTP_STATUS (expected 204). Body:"
    cat "$BODY_FILE"
    docker logs "$CONTAINER_NAME"
    exit 1
fi

# Verify the archive is staged. The container writes to
# /data/glovebox/archives/<archive_id>/ at finalize per spec 13 §3.
echo "Verifying staged archive at /data/glovebox/archives/$ARCHIVE_ID/..."
if ! docker exec "$CONTAINER_NAME" ls -la "/data/glovebox/archives/$ARCHIVE_ID/" >/dev/null 2>&1; then
    echo "FAIL: archives/$ARCHIVE_ID/ not found in container"
    docker logs "$CONTAINER_NAME"
    exit 1
fi

echo "SUCCESS (raw mbox): 12 GiB upload completed and staged at archives/$ARCHIVE_ID/"

# ----------------------------------------------------------------------
# Phase 2: multi-GB tarball subtree per the bead acceptance criterion.
# ----------------------------------------------------------------------
# The bead glovebox-p1zx's acceptance criterion is "12 GB mbox + at
# least one multi-GB subtree". Phase 1 above covered the mbox; this
# phase exercises the tarball path:
#   - media_type: archive/google-takeout-subtree -> Untar dispatch
#   - server walks tar.Reader; entries land under archives/<id>/tree/
# We build a small 2 GiB sparse-file inside a one-entry tar so the
# total upload is ~2 GiB (above the "multi-GB" floor) and the test
# adds ~30 seconds vs Phase 1's ~3 minutes.

SUBTREE_SIZE="${SUBTREE_SIZE:-2147483648}"  # 2 GiB
SUBTREE_ARCHIVE_ID="${SUBTREE_ARCHIVE_ID:-smoke-subtree-$(date +%s)-$$}"
TARBALL_FILE="$TMP_DIR/subtree.tar"
SUBTREE_DIR="$TMP_DIR/subtree"
mkdir -p "$SUBTREE_DIR"
echo "Phase 2: creating $SUBTREE_SIZE-byte sparse subtree file..."
truncate -s "$SUBTREE_SIZE" "$SUBTREE_DIR/data.bin"
# Build an uncompressed tarball with a single entry. The server walks
# tar.Reader and enforces spec §4.7 safety rules; sparse-NUL content
# survives the type-flag + name + size checks.
( cd "$TMP_DIR" && tar cf "$TARBALL_FILE" subtree )
TARBALL_SIZE=$(stat -c '%s' "$TARBALL_FILE" 2>/dev/null || stat -f '%z' "$TARBALL_FILE")
TARBALL_SHA256="$(sha256sum "$TARBALL_FILE" | awk '{print $1}')"
echo "Phase 2: tarball size=$TARBALL_SIZE bytes, sha256=$TARBALL_SHA256"

SUBTREE_METADATA="archive_id $(b64 "$SUBTREE_ARCHIVE_ID"),archive_filename $(b64 "subtree.tar"),subtree_relative_path $(b64 "subtree"),media_type $(b64 "archive/google-takeout-subtree"),matcher_id $(b64 "smoke/subtree"),provider $(b64 "smoke"),sha256 $(b64 "$TARBALL_SHA256"),size_bytes $(b64 "$TARBALL_SIZE")"

echo "Phase 2: POST /v1/archives..."
HDR_FILE2="$(mktemp)"
BODY_FILE2="$(mktemp)"
HTTP_STATUS="$(curl -sS -D "$HDR_FILE2" -o "$BODY_FILE2" -w '%{http_code}' \
    -X POST "http://127.0.0.1:$HOST_PORT/v1/archives" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Tus-Resumable: 1.0.0" \
    -H "Upload-Length: $TARBALL_SIZE" \
    -H "Upload-Metadata: $SUBTREE_METADATA")"
if [ "$HTTP_STATUS" != "201" ]; then
    echo "FAIL: Phase 2 POST returned $HTTP_STATUS"
    echo "  body:"
    cat "$BODY_FILE2"
    docker logs "$CONTAINER_NAME" || true
    exit 1
fi
LOCATION2="$(awk -v IGNORECASE=1 '/^Location:/ {print $2}' "$HDR_FILE2" | tr -d '\r' | tail -1)"
echo "Phase 2: upload-id $(basename "$LOCATION2")"

echo "Phase 2: PATCH (streaming $TARBALL_SIZE bytes)..."
HTTP_STATUS="$(curl -sS -o /dev/null -w '%{http_code}' \
    -X PATCH "http://127.0.0.1:$HOST_PORT$LOCATION2" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Tus-Resumable: 1.0.0" \
    -H "Upload-Offset: 0" \
    -H "Content-Type: application/offset+octet-stream" \
    --data-binary "@$TARBALL_FILE")"
if [ "$HTTP_STATUS" != "204" ]; then
    echo "FAIL: Phase 2 PATCH returned $HTTP_STATUS"
    docker logs "$CONTAINER_NAME" || true
    exit 1
fi

echo "Phase 2: verifying untarred tree at archives/$SUBTREE_ARCHIVE_ID/tree/..."
if ! docker exec "$CONTAINER_NAME" ls -la "/data/glovebox/archives/$SUBTREE_ARCHIVE_ID/tree/subtree/data.bin" >/dev/null 2>&1; then
    echo "FAIL: archives/$SUBTREE_ARCHIVE_ID/tree/subtree/data.bin not found"
    docker logs "$CONTAINER_NAME" || true
    exit 1
fi

echo "SUCCESS (tarball subtree): ~$((SUBTREE_SIZE / 1024 / 1024)) MiB tarball completed and untarred at archives/$SUBTREE_ARCHIVE_ID/tree/"
echo
echo "SUCCESS: both bead acceptance phases passed (12 GiB mbox + multi-GB subtree)."
