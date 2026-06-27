#!/usr/bin/env bash
#
# archive-smoke-test.sh -- resumable tus.io upload smoke test for the
# spec 13 archive-delivery endpoint, runnable on a single node/container
# WITHOUT a Vault-backed cluster (glovebox-grbi / glovebox-4ypk).
#
# Drives resumable tus.io uploads against a glovebox container built from
# this repo's top-level Dockerfile. The bead acceptance criterion is
# "12 GiB mbox + at least one multi-GiB subtree delivers without 413";
# this script exercises exactly that (shrink via env vars for a quick
# sanity run -- see ARCHIVE_SIZE / SUBTREE_SIZE below).
#
# How auth works here (the spec 13 listener only mounts /v1/archives
# when ingest.auth is enabled): we run with ingest.auth.source=env, the
# DEV-ONLY token source added in glovebox-4ypk. A single static token is
# handed to the container via GLOVEBOX_INGEST_TOKEN / _SOURCE_ID, so no
# Vault is required. Production uses source=vault; this path is dev-only.
#
# Assumptions:
#   - docker (or podman aliased as docker) is on PATH (script skips with
#     exit 0 if not, so it is CI-safe).
#   - Host networking is available. We use --network=host instead of -p
#     because the podman CNI bridge is broken on some WSL2 hosts
#     (iptables nf_tables incompatibility); host networking sidesteps it
#     and works on plain Docker/Linux too. The container's ingest
#     (9091) and metrics (9090) ports bind directly on the host.
#   - ~ARCHIVE_SIZE + SUBTREE_SIZE of free space in $TMPDIR for the
#     sparse files (sparse, so real disk use is near-zero until the
#     sha256 read materializes them).
#
# What this script does NOT do:
#   - Verify on-disk bytes match the source (would need a second full
#     read). We verify the upload finalized and archives/<id>/ exists.
#   - Use chunked PATCH. A single PATCH delivers the whole body; the
#     listener's job here is just to accept it without a 413.

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
INGEST_PORT="${INGEST_PORT:-9091}"
METRICS_PORT="${METRICS_PORT:-9090}"
CONTAINER_NAME="${CONTAINER_NAME:-glovebox-archive-smoke-$$}"
ARCHIVE_SIZE="${ARCHIVE_SIZE:-12884901888}"  # 12 GiB
ARCHIVE_ID="${ARCHIVE_ID:-smoke-$$}"
TOKEN="${TOKEN:-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}"
SOURCE_ID="${SOURCE_ID:-recognizer}"
READY_TIMEOUT_SEC="${READY_TIMEOUT_SEC:-30}"

BASE="http://127.0.0.1:$INGEST_PORT"

# Always rebuild so the image reflects current source. Docker layer
# caching makes this a fast no-op when nothing changed; when source
# changed, "build if absent" would silently test a stale binary.
echo "Building image $IMAGE_TAG..."
docker build -t "$IMAGE_TAG" -f Dockerfile .

# Scratch dir for the sparse archives, the mounted /data tree, the
# dev config, and curl response files.
TMP_DIR="$(mktemp -d)"
DATA_DIR="$TMP_DIR/data"
ARCHIVE_FILE="$TMP_DIR/smoke.mbox"
HDR_FILE="$TMP_DIR/post-headers"
BODY_FILE="$TMP_DIR/post-body"

# Cleanup container + tmp files on exit (success OR failure).
cleanup() {
    docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
    rm -rf "$TMP_DIR"
}
trap cleanup EXIT

# Build the /data tree the container expects. The distroless image runs
# as nonroot (uid 65532) and does NOT mkdir these, so they must exist
# and be writable before boot. ingest.archives.staging_root is
# /data/glovebox, so finalized archives land at
# /data/glovebox/archives/<id>/ (spec 13 section 3); .tmp-archives/ is
# its same-filesystem sibling (st_dev identity is enforced at boot).
mkdir -p \
    "$DATA_DIR"/glovebox/{staging,quarantine,audit,failed,archives,.tmp-archives} \
    "$DATA_DIR"/agents \
    "$DATA_DIR"/shared
chmod -R 777 "$DATA_DIR"

# Dev config: enable ingest auth (source=env) + archive delivery. All
# other fields fall back to LoadConfig defaults (rate limits, etc.).
cat > "$DATA_DIR/config.json" <<EOF
{
  "metrics_port": ${METRICS_PORT},
  "staging_dir": "/data/glovebox/staging",
  "quarantine_dir": "/data/glovebox/quarantine",
  "audit_dir": "/data/glovebox/audit",
  "failed_dir": "/data/glovebox/failed",
  "agents_dir": "/data/agents",
  "shared_dir": "/data/shared",
  "rules_file": "/etc/glovebox/rules.json",
  "ingest": {
    "enabled": true,
    "auth": { "enabled": true, "source": "env" },
    "archives": { "enabled": true, "staging_root": "/data/glovebox" }
  }
}
EOF

# Create a sparse mbox file. We do not have a real multi-GiB mbox lying
# around; a sparse file reads back as NUL bytes, fine for the upload
# contract (the server hashes whatever we send).
echo "Creating $ARCHIVE_SIZE-byte sparse file at $ARCHIVE_FILE..."
truncate -s "$ARCHIVE_SIZE" "$ARCHIVE_FILE"
echo "Computing sha256 (about a minute for 12 GiB of zeros)..."
ARCHIVE_SHA256="$(sha256sum "$ARCHIVE_FILE" | awk '{print $1}')"
echo "sha256 = $ARCHIVE_SHA256"

# Start the container. The static token + source-id populate the env
# TokenStore without contacting Vault (glovebox-4ypk). Host networking
# binds 9091/9090 directly on the host.
echo "Starting container $CONTAINER_NAME (host networking)..."
docker run -d --rm \
    --name "$CONTAINER_NAME" \
    --network=host \
    -v "$DATA_DIR":/data \
    -e GLOVEBOX_INGEST_TOKEN="$TOKEN" \
    -e GLOVEBOX_INGEST_SOURCE_ID="$SOURCE_ID" \
    "$IMAGE_TAG" --config /data/config.json >/dev/null

# Wait for readiness. There is no /healthz route; the metrics endpoint
# (:9090/metrics) returns 200 once the process is serving, which is
# after bootstrapArchives has mounted /v1/archives.
echo "Waiting for :$METRICS_PORT/metrics (timeout ${READY_TIMEOUT_SEC}s)..."
ready=0
for i in $(seq 1 "$READY_TIMEOUT_SEC"); do
    if curl -sf "http://127.0.0.1:$METRICS_PORT/metrics" >/dev/null 2>&1; then
        echo "ready after ${i}s"
        ready=1
        break
    fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    echo "FAIL: metrics endpoint never came up after ${READY_TIMEOUT_SEC}s"
    docker logs "$CONTAINER_NAME" || true
    exit 1
fi

# Confirm the archive listener mounted (and did NOT fall back to 503). The
# metrics server starts a beat BEFORE the archive listener mounts (separate
# bootstrap steps), so on a fast boot (host networking) the :metrics readiness
# probe above can pass before the "listener mounted" log line is written. Poll
# the logs briefly rather than grepping once, or this races to a false FAIL.
mounted=0
for i in $(seq 1 15); do
    if docker logs "$CONTAINER_NAME" 2>&1 | grep -q "archive listener mounted on /v1/archives"; then
        mounted=1
        break
    fi
    sleep 1
done
if [ "$mounted" -ne 1 ]; then
    echo "FAIL: archive listener did not mount (503 fallback or auth disabled)"
    docker logs "$CONTAINER_NAME" || true
    exit 1
fi

# Build the Upload-Metadata header (base64-encoded k=v pairs per tus.io).
b64() { printf '%s' "$1" | base64 -w0; }
UPLOAD_METADATA="archive_id $(b64 "$ARCHIVE_ID"),archive_filename $(b64 "smoke.mbox"),subtree_relative_path $(b64 "."),media_type $(b64 "archive/mbox"),matcher_id $(b64 "smoke/mbox"),provider $(b64 "smoke"),sha256 $(b64 "$ARCHIVE_SHA256"),size_bytes $(b64 "$ARCHIVE_SIZE")"

# POST without a token first: the listener must reject it (spec 10 auth).
echo "POST /v1/archives without token (expect 401)..."
NOAUTH_STATUS="$(curl -sS -o /dev/null -w '%{http_code}' \
    -X POST "$BASE/v1/archives" \
    -H "Tus-Resumable: 1.0.0" \
    -H "Upload-Length: $ARCHIVE_SIZE" \
    -H "Upload-Metadata: $UPLOAD_METADATA")"
if [ "$NOAUTH_STATUS" != "401" ]; then
    echo "FAIL: unauthenticated POST returned $NOAUTH_STATUS (expected 401)"
    docker logs "$CONTAINER_NAME"
    exit 1
fi
echo "  -> 401 as expected (auth enforced)"

# POST /v1/archives to create the upload (authenticated).
echo "POST /v1/archives..."
HTTP_STATUS="$(curl -sS -D "$HDR_FILE" -o "$BODY_FILE" -w '%{http_code}' \
    -X POST "$BASE/v1/archives" \
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

# Stream the entire body via a single PATCH. The listener is expected to
# accept this without imposing a 413; that is the bead's criterion.
echo "PATCH $LOCATION (streaming $ARCHIVE_SIZE bytes)..."
# Stream the body with --upload-file (-T), NOT --data-binary @file:
# --data-binary loads the whole file into memory ("curl: option
# --data-binary: out of memory" on a 12 GiB archive). -T streams the file
# incrementally. -H "Expect:" disables 100-continue so the PATCH starts
# immediately. The single streamed PATCH is the bead's "no 413" criterion.
HTTP_STATUS="$(curl -sS -o "$BODY_FILE" -w '%{http_code}' \
    -X PATCH "$BASE$LOCATION" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Tus-Resumable: 1.0.0" \
    -H "Upload-Offset: 0" \
    -H "Content-Type: application/offset+octet-stream" \
    -H "Expect:" \
    --upload-file "$ARCHIVE_FILE")"
if [ "$HTTP_STATUS" != "204" ]; then
    echo "FAIL: PATCH returned $HTTP_STATUS (expected 204). Body:"
    cat "$BODY_FILE"
    docker logs "$CONTAINER_NAME"
    exit 1
fi

# Verify the archive is staged. staging_root=/data/glovebox, so the
# container writes to /data/glovebox/archives/<archive_id>/ at finalize.
# We check the host bind-mount directly rather than `docker exec ls`:
# the image is distroless (no ls / no shell), so exec-based inspection
# is impossible. /data is bind-mounted from $DATA_DIR, so the host view
# is authoritative.
echo "Verifying staged archive at archives/$ARCHIVE_ID/..."
if [ ! -d "$DATA_DIR/glovebox/archives/$ARCHIVE_ID" ]; then
    echo "FAIL: archives/$ARCHIVE_ID/ not found on the /data mount"
    docker logs "$CONTAINER_NAME"
    exit 1
fi

echo "SUCCESS (raw mbox): $ARCHIVE_SIZE-byte upload completed and staged at archives/$ARCHIVE_ID/"

# ----------------------------------------------------------------------
# Phase 2: multi-GiB tarball subtree per the bead acceptance criterion.
# ----------------------------------------------------------------------
# media_type archive/google-takeout-subtree -> Untar dispatch; the
# server walks tar.Reader and entries land under archives/<id>/tree/.
SUBTREE_SIZE="${SUBTREE_SIZE:-2147483648}"  # 2 GiB
SUBTREE_ARCHIVE_ID="${SUBTREE_ARCHIVE_ID:-smoke-subtree-$$}"
TARBALL_FILE="$TMP_DIR/subtree.tar"
SUBTREE_DIR="$TMP_DIR/subtree"
mkdir -p "$SUBTREE_DIR"
echo "Phase 2: creating $SUBTREE_SIZE-byte sparse subtree file..."
truncate -s "$SUBTREE_SIZE" "$SUBTREE_DIR/data.bin"
( cd "$TMP_DIR" && tar cf "$TARBALL_FILE" subtree )
TARBALL_SIZE=$(stat -c '%s' "$TARBALL_FILE" 2>/dev/null || stat -f '%z' "$TARBALL_FILE")
TARBALL_SHA256="$(sha256sum "$TARBALL_FILE" | awk '{print $1}')"
echo "Phase 2: tarball size=$TARBALL_SIZE bytes, sha256=$TARBALL_SHA256"

SUBTREE_METADATA="archive_id $(b64 "$SUBTREE_ARCHIVE_ID"),archive_filename $(b64 "subtree.tar"),subtree_relative_path $(b64 "subtree"),media_type $(b64 "archive/google-takeout-subtree"),matcher_id $(b64 "smoke/subtree"),provider $(b64 "smoke"),sha256 $(b64 "$TARBALL_SHA256"),size_bytes $(b64 "$TARBALL_SIZE")"

echo "Phase 2: POST /v1/archives..."
HDR_FILE2="$(mktemp)"
BODY_FILE2="$(mktemp)"
HTTP_STATUS="$(curl -sS -D "$HDR_FILE2" -o "$BODY_FILE2" -w '%{http_code}' \
    -X POST "$BASE/v1/archives" \
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
# Streamed upload (-T), not --data-binary @file -- see phase 1 note.
HTTP_STATUS="$(curl -sS -o /dev/null -w '%{http_code}' \
    -X PATCH "$BASE$LOCATION2" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Tus-Resumable: 1.0.0" \
    -H "Upload-Offset: 0" \
    -H "Content-Type: application/offset+octet-stream" \
    -H "Expect:" \
    --upload-file "$TARBALL_FILE")"
if [ "$HTTP_STATUS" != "204" ]; then
    echo "FAIL: Phase 2 PATCH returned $HTTP_STATUS"
    docker logs "$CONTAINER_NAME" || true
    exit 1
fi

echo "Phase 2: verifying untarred tree at archives/$SUBTREE_ARCHIVE_ID/tree/..."
if [ ! -f "$DATA_DIR/glovebox/archives/$SUBTREE_ARCHIVE_ID/tree/subtree/data.bin" ]; then
    echo "FAIL: archives/$SUBTREE_ARCHIVE_ID/tree/subtree/data.bin not found on the /data mount"
    docker logs "$CONTAINER_NAME" || true
    exit 1
fi

echo "SUCCESS (tarball subtree): ~$((SUBTREE_SIZE / 1024 / 1024)) MiB tarball completed and untarred at archives/$SUBTREE_ARCHIVE_ID/tree/"
echo
echo "SUCCESS: both bead acceptance phases passed (mbox + multi-GiB subtree)."
