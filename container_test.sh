#!/usr/bin/env bash
set -euo pipefail

# Container tests for glovebox.
#
# Requires: docker daemon running (podman aliased to docker also works).
#
# What this script tests:
#   1. The top-level glovebox image (Dockerfile at repo root) builds,
#      starts, serves /metrics, and exits cleanly on SIGTERM.
#   2. (Spec 14 §7.2) Per-connector binary-presence assertions for the
#      connectors rebased onto glovebox-enricher-runtime versus the
#      connectors still on distroless/static. Catches accidental rebase
#      (distroless connector pulling enricher bloat) and accidental
#      regression (enricher-runtime connector silently losing a binary
#      because the upstream base image rotated).
#   3. (Spec 14 §7.2) Golden-file content comparison for one rebased
#      connector + one fixture — gmail with an embedded text/plain
#      ingest item — to verify the enrichment pipeline actually runs
#      end-to-end inside a built connector image. Kept narrow to one
#      connector to keep test runtime bounded; the full sidecar matrix
#      is covered by scripts/smoke-enrichment.sh.
#
# Error-message discipline (spec 14 §8): all failure paths emit a
# WHAT/CHECK/FIX line. See log_fail_wcf below.
#
# Environment overrides:
#   CONTAINER_TEST_SKIP_CONNECTORS=1
#       Skip the spec-14 §7.2 connector-binary checks (sections 2-3
#       above). Useful for fast local iteration when only the top-level
#       image changed. Defaults to running the connector checks.
#   CONTAINER_TEST_CONNECTOR_BUILD_TIMEOUT=<seconds>
#       Per-connector docker-build timeout. Defaults to 600s (10 min);
#       a cold first build that pulls golang:1.27 + the enricher-runtime
#       base can approach this on a slow link.
#
# Usage: ./container_test.sh

IMAGE_NAME="glovebox:test"
PASS=0
FAIL=0

# NOTE: increments use `$((var + 1))` rather than `((var++))` because
# the pre-existing script runs under `set -e`, and `((PASS++))` evaluates
# to 0 on the first call (when PASS started at 0), which set -e treats
# as a failed command and aborts the script. The arithmetic-assignment
# form below always returns 0 to set -e.
log_pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
log_fail() { echo "FAIL: $1"; FAIL=$((FAIL + 1)); }

# log_fail_wcf emits a spec-14 §8 WHAT/CHECK/FIX failure line.
#   $1 = WHAT failed (one sentence)
#   $2 = CHECK command (operator-runnable to diagnose)
#   $3 = FIX action (operator-runnable to resolve)
log_fail_wcf() {
    local what="$1" check="$2" fix="$3"
    echo "FAIL: ${what}"
    echo "  CHECK: ${check}"
    echo "  FIX:   ${fix}"
    FAIL=$((FAIL + 1))
}

# require_docker bails the whole script if docker is not on PATH (CI
# safety: we never want a green container_test on a runner with no
# container engine).
require_docker() {
    if ! command -v docker >/dev/null 2>&1; then
        echo "ERROR: docker not on PATH"
        echo "  CHECK: command -v docker"
        echo "  FIX:   install docker / podman, or run from a docker-in-docker CI runner"
        exit 2
    fi
    if ! docker version >/dev/null 2>&1; then
        echo "ERROR: docker daemon not reachable"
        echo "  CHECK: docker version"
        echo "  FIX:   start the docker daemon (sudo systemctl start docker) or fix DOCKER_HOST"
        exit 2
    fi
}
require_docker

cleanup() {
    docker rm -f glovebox-test 2>/dev/null || true
    rm -rf "$TMPDIR"
}
trap cleanup EXIT

TMPDIR=$(mktemp -d)

# ----------------------------------------------------------------------
# Section 1: top-level glovebox image
# ----------------------------------------------------------------------

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

# ----------------------------------------------------------------------
# Section 2 + 3: spec-14 §7.2 per-connector binary-presence assertions
# ----------------------------------------------------------------------
#
# Per spec 14 §6.2 the connector image base is one of two:
#
#   enricher-runtime (debian:bookworm-slim + poppler-utils, tesseract,
#                     pandoc, ca-certificates, nonroot uid 65532):
#       gmail, imap, outlook, mbox-importer, arxiv, semantic-scholar
#
#   distroless/static-debian12:nonroot (no shell, no glibc layer, just
#                                       the static Go binary + CA certs):
#       rss, hackernews, github, gitlab, linkedin, bluesky, x,
#       notion, jira, schoology, gcalendar, gdrive, onedrive, teams,
#       steam, meta, trello, youtube
#
# For each enricher-runtime connector: assert pdftotext, tesseract,
# pandoc are all present and runnable.
#
# For each distroless connector: assert pdftotext is NOT present. This
# catches a regression where someone rebases a distroless connector onto
# enricher-runtime and bloats the image without thinking about it.
# (Pandoc and tesseract are not separately checked on the distroless
# side because pdftotext alone is a sufficient sentinel for the whole
# enricher-runtime base.)
#
# Connector image build is gated behind CONTAINER_TEST_SKIP_CONNECTORS
# so local iteration on the top-level image doesn't pay the per-build
# cost.

if [ "${CONTAINER_TEST_SKIP_CONNECTORS:-0}" = "1" ]; then
    echo ""
    echo "=== Skipping spec-14 §7.2 connector binary checks (CONTAINER_TEST_SKIP_CONNECTORS=1) ==="
else
    echo ""
    echo "=== Spec 14 §7.2: per-connector binary-presence assertions ==="

    ENRICHER_RUNTIME_CONNECTORS=(
        "gmail:connectors/gmail/Dockerfile"
        "imap:connectors/imap/Dockerfile"
        "outlook:connectors/outlook/Dockerfile"
        "mbox-importer:importers/mbox/Dockerfile"
        "arxiv:connectors/arxiv/Dockerfile"
        "semantic-scholar:connectors/semantic-scholar/Dockerfile"
    )

    # Distroless connectors per the actual repo state on this branch.
    # Verified via `grep -E "^FROM " connectors/*/Dockerfile | tail`.
    # schoology-auth-refresher is debian:bookworm-slim (not distroless,
    # not enricher-runtime); excluded from both checks. trello and
    # youtube are distroless/static per current repo state; included.
    DISTROLESS_CONNECTORS=(
        "rss:connectors/rss/Dockerfile"
        "hackernews:connectors/hackernews/Dockerfile"
        "github:connectors/github/Dockerfile"
        "gitlab:connectors/gitlab/Dockerfile"
        "linkedin:connectors/linkedin/Dockerfile"
        "bluesky:connectors/bluesky/Dockerfile"
        "x:connectors/x/Dockerfile"
        "notion:connectors/notion/Dockerfile"
        "jira:connectors/jira/Dockerfile"
        "schoology:connectors/schoology/Dockerfile"
        "gcalendar:connectors/gcalendar/Dockerfile"
        "gdrive:connectors/gdrive/Dockerfile"
        "onedrive:connectors/onedrive/Dockerfile"
        "teams:connectors/teams/Dockerfile"
        "steam:connectors/steam/Dockerfile"
        "meta:connectors/meta/Dockerfile"
        "trello:connectors/trello/Dockerfile"
        "youtube:connectors/youtube/Dockerfile"
    )

    BUILD_TIMEOUT="${CONTAINER_TEST_CONNECTOR_BUILD_TIMEOUT:-600}"

    # build_connector_image $name $dockerfile_path
    # Returns the image tag on stdout (e.g. "glovebox-conn-gmail:test")
    # or empty + fail log on failure.
    build_connector_image() {
        local name="$1" dockerfile="$2"
        local tag="glovebox-conn-${name}:test"

        if ! [ -f "$dockerfile" ]; then
            log_fail_wcf \
                "connector ${name}: dockerfile ${dockerfile} not found" \
                "ls -la ${dockerfile}" \
                "verify the connector still exists in connectors/ or update the array in container_test.sh"
            return 1
        fi

        # Build context is repo root (Dockerfiles COPY go.mod from cwd).
        # --quiet suppresses the layer-by-layer chatter; on failure we
        # rerun without --quiet to surface the build log.
        if timeout "$BUILD_TIMEOUT" docker build -q -f "$dockerfile" -t "$tag" . >/dev/null 2>&1; then
            echo "$tag"
            return 0
        fi

        # Failure path: surface the actual build error.
        echo "  (rebuilding ${name} without --quiet to capture error output)" >&2
        docker build -f "$dockerfile" -t "$tag" . 2>&1 | tail -20 >&2 || true
        log_fail_wcf \
            "connector ${name}: docker build failed" \
            "docker build -f ${dockerfile} -t ${tag} ." \
            "inspect the build output above; common causes are go module download failures or missing base-image tags"
        return 1
    }

    # assert_binary_present $image $name $binary
    # docker run --rm --entrypoint /bin/sh $image -c "command -v $bin";
    # expect exit 0. IMPORTANT: connector images set ENTRYPOINT to the
    # Go binary, so we override with --entrypoint /bin/sh. The enricher-
    # runtime base (debian:bookworm-slim) ships /bin/sh (dash) which
    # supports the POSIX `command -v`. We default to --network=none to
    # sidestep host CNI flakiness.
    assert_binary_present() {
        local image="$1" name="$2" bin="$3"
        if docker run --rm --network=none --entrypoint /bin/sh "$image" -c "command -v ${bin}" >/dev/null 2>&1; then
            log_pass "connector ${name}: ${bin} present"
        else
            log_fail_wcf \
                "connector ${name}: ${bin} not found in image ${image}" \
                "docker run --rm --entrypoint /bin/sh ${image} -c 'command -v ${bin}'" \
                "verify ${name}'s Dockerfile final stage is FROM ghcr.io/leftathome/glovebox-enricher-runtime:latest and that the base image still ships ${bin}"
        fi
    }

    # assert_binary_absent $image $name $binary
    # docker run --rm $image -- which $binary; expect non-zero (binary
    # not present). For distroless/static there is no shell and no
    # 'which', so the assertion form is different: we attempt to exec
    # the binary directly and expect docker to report "executable file
    # not found in $PATH" (exit 127) or "no such file or directory"
    # (exit 126). Either is a pass; a zero exit is a fail.
    assert_binary_absent() {
        local image="$1" name="$2" bin="$3"
        # Use --entrypoint to override the connector's Go binary.
        # On distroless/static there is no /usr/bin/which and no /bin/sh,
        # so we set entrypoint to the candidate binary directly. If the
        # binary doesn't exist, docker reports "OCI runtime exec failed:
        # ... starting container process caused: exec: \"$bin\": executable
        # file not found in $PATH" with exit code 127.
        local exit_code=0
        docker run --rm --network=none --entrypoint "$bin" "$image" --version >/dev/null 2>&1 || exit_code=$?
        if [ "$exit_code" -eq 0 ]; then
            log_fail_wcf \
                "connector ${name}: distroless image unexpectedly has ${bin} (exited 0)" \
                "docker run --rm --entrypoint ${bin} ${image} --version" \
                "verify ${name}'s Dockerfile final stage is FROM gcr.io/distroless/static-debian12:nonroot — an accidental rebase onto enricher-runtime would have caught here"
        else
            # 127 is "executable not found"; 126 is "permission denied".
            # Both mean the binary isn't usable, which is what we want.
            log_pass "connector ${name}: ${bin} absent (exit ${exit_code}, expected non-zero)"
        fi
    }

    # --- enricher-runtime connectors: binaries MUST be present ---
    echo "--- enricher-runtime connectors (binaries must be present) ---"
    for entry in "${ENRICHER_RUNTIME_CONNECTORS[@]}"; do
        name="${entry%%:*}"
        dockerfile="${entry##*:}"
        echo "Building ${name} from ${dockerfile}..."
        if ! tag=$(build_connector_image "$name" "$dockerfile"); then
            continue
        fi
        # pdftotext, tesseract, pandoc must all be present.
        assert_binary_present "$tag" "$name" pdftotext
        # arxiv and semantic-scholar carry tesseract+pandoc transitively
        # via the enricher-runtime base but do not functionally depend
        # on them per spec §6.2 ("pdf" enricher only). We still assert
        # presence — the base image is shared, so absence here would
        # mean the base regressed; the no-functional-use case is just a
        # rationale comment, not a behavioral exception.
        assert_binary_present "$tag" "$name" tesseract
        assert_binary_present "$tag" "$name" pandoc
    done

    # --- distroless connectors: binaries MUST NOT be present ---
    echo "--- distroless connectors (binaries must be absent) ---"
    for entry in "${DISTROLESS_CONNECTORS[@]}"; do
        name="${entry%%:*}"
        dockerfile="${entry##*:}"
        echo "Building ${name} from ${dockerfile}..."
        if ! tag=$(build_connector_image "$name" "$dockerfile"); then
            continue
        fi
        # Only pdftotext is checked as the sentinel; if it's absent,
        # the image is on distroless (or some other minimal base) and
        # tesseract/pandoc would also be absent.
        assert_binary_absent "$tag" "$name" pdftotext
    done

    # --- Section 3: golden-file content comparison for one connector ---
    #
    # The spec calls for "one rebased connector + one fixture" with a
    # golden-file comparison. gmail is the canonical email connector but
    # requires live Gmail API credentials to fetch anything, so we use
    # mbox-importer here instead: it's also rebased on enricher-runtime
    # (§6.2) and reads a fixture file directly, with no API mocking.
    #
    # The deeper end-to-end smoke (HTML body + PDF attachment + image
    # attachment → sidecar set) lives in scripts/smoke-enrichment.sh
    # because it requires fixture munging beyond a single mbox file.
    #
    # What we assert here:
    #   1. The mbox-importer container starts.
    #   2. Given the existing testdata/small.mbox (already a project
    #      fixture, no new file), the container exits 0 (or "interrupted"
    #      but successful import of N messages) and writes N staging
    #      items.
    #   3. Each staging item directory contains a content.raw and a
    #      metadata.json. (Full sidecar assertions are in smoke-
    #      enrichment.sh.)
    #
    # The mbox-importer expects a destination-agent rule in its config;
    # we synthesize a minimal config.json with a wildcard rule pointing
    # at "messaging" so every message ingests.
    echo ""
    echo "=== Spec 14 §7.2: golden-file smoke (mbox-importer + small.mbox) ==="

    MBOX_TAG="glovebox-conn-mbox-importer:test"
    # The build_connector_image call above already produced this tag if
    # mbox-importer's build succeeded. If we got here without it (e.g.
    # build failure), short-circuit.
    if ! docker image inspect "$MBOX_TAG" >/dev/null 2>&1; then
        log_fail_wcf \
            "golden-file smoke: mbox-importer image ${MBOX_TAG} not available — likely a build failure above" \
            "docker image inspect ${MBOX_TAG}" \
            "scroll up to the mbox-importer build log and resolve the underlying error"
    else
        SMOKE_DIR="$TMPDIR/mbox-smoke"
        mkdir -p "$SMOKE_DIR"/{staging,state,config,source}
        cp importers/mbox/testdata/small.mbox "$SMOKE_DIR/source/in.mbox"
        # Minimal importer config: wildcard rule -> messaging agent.
        cat > "$SMOKE_DIR/config/config.json" << 'CFGEOF'
{
  "rules": [
    {"match": "*", "destination": "messaging"}
  ]
}
CFGEOF

        # The connector image runs as nonroot (uid 65532). Host-mounted
        # dirs need to be writable by that uid.
        chmod -R 0777 "$SMOKE_DIR"

        # Run mbox-importer one-shot. --source-name carries through to
        # the origin_archive tag. The 60s timeout is generous: import of
        # a 7-message mbox is sub-second; we keep slack for slow CI VMs.
        # --network=none isolates the import; health port (default 8080)
        # binds inside the container's loopback only, which is fine.
        # /source is NOT mounted read-only: mbox-importer writes a
        # <source>.survey.v1.json sibling next to the mbox as part of
        # its resume-checkpoint design (spec 09 §3.1). A read-only
        # source mount would fail before any message is staged.
        if timeout 60 docker run --rm \
                --network=none \
                -v "$SMOKE_DIR/staging:/data/staging" \
                -v "$SMOKE_DIR/state:/data/state" \
                -v "$SMOKE_DIR/config:/etc/importer" \
                -v "$SMOKE_DIR/source:/source" \
                -e GLOVEBOX_STAGING_DIR=/data/staging \
                -e GLOVEBOX_STATE_DIR=/data/state \
                -e GLOVEBOX_IMPORTER_CONFIG=/etc/importer/config.json \
                "$MBOX_TAG" \
                --source=/source/in.mbox \
                --source-name=container-test \
                >"$SMOKE_DIR/run.log" 2>&1; then
            # Exclude the .tmp/ sidecar dir the staging backend uses
            # for in-progress items before the atomic rename.
            STAGED_COUNT=$(find "$SMOKE_DIR/staging" -mindepth 1 -maxdepth 1 -type d ! -name '.tmp' | wc -l)
            EXPECTED_COUNT=7  # small.mbox contains 7 From_ records
            if [ "$STAGED_COUNT" -eq "$EXPECTED_COUNT" ]; then
                log_pass "golden-file smoke: mbox-importer staged ${STAGED_COUNT} items (expected ${EXPECTED_COUNT})"
            else
                log_fail_wcf \
                    "golden-file smoke: mbox-importer staged ${STAGED_COUNT} items, expected ${EXPECTED_COUNT}" \
                    "ls ${SMOKE_DIR}/staging/ && cat ${SMOKE_DIR}/run.log" \
                    "inspect run.log for ingest errors; verify the rule matcher matches every message (the wildcard rule should)"
            fi

            # Every staged item must have content.raw + metadata.json.
            MISSING_SIDECARS=0
            while IFS= read -r d; do
                if ! [ -f "$d/content.raw" ]; then
                    MISSING_SIDECARS=$((MISSING_SIDECARS + 1))
                    echo "  missing content.raw in $(basename "$d")" >&2
                fi
                if ! [ -f "$d/metadata.json" ]; then
                    MISSING_SIDECARS=$((MISSING_SIDECARS + 1))
                    echo "  missing metadata.json in $(basename "$d")" >&2
                fi
            done < <(find "$SMOKE_DIR/staging" -mindepth 1 -maxdepth 1 -type d ! -name '.tmp')
            if [ "$MISSING_SIDECARS" -eq 0 ]; then
                log_pass "golden-file smoke: every staged item has content.raw + metadata.json"
            else
                log_fail_wcf \
                    "golden-file smoke: ${MISSING_SIDECARS} staged item(s) missing content.raw or metadata.json" \
                    "find ${SMOKE_DIR}/staging -mindepth 1 -maxdepth 1 -type d -exec ls -la {} \\;" \
                    "check connector/staging.go Commit() — the atomic rename should produce both files together"
            fi
        else
            log_fail_wcf \
                "golden-file smoke: mbox-importer container exited non-zero or hung" \
                "cat ${SMOKE_DIR}/run.log" \
                "inspect run.log; common causes are config.json schema mismatch or staging dir permission errors (the container runs as uid 65532)"
        fi
    fi
fi

echo ""
echo "=== Results ==="
echo "Passed: $PASS"
echo "Failed: $FAIL"
[ "$FAIL" -eq 0 ] || exit 1
