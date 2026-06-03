#!/usr/bin/env bash
#
# smoke-enrichment.sh -- spec 14 §7.3 end-to-end enrichment smoke.
#
# What this script does:
#
#   1. Builds (or pulls) a container image whose runtime base is
#      ghcr.io/leftathome/glovebox-enricher-runtime (same base as the
#      gmail / imap / outlook / mbox-importer / arxiv / semantic-scholar
#      connector images).
#   2. Runs that image against four fixture-driven scenarios — html
#      email, pdf "attachment", image-with-text, image-with-no-text —
#      and writes the resulting staging items to a host-mounted scratch
#      dir.
#   3. Reads back the scratch dir from the host and asserts the smoke
#      run produced PASS lines and zero FAIL lines.
#   4. Cleans up (removes container, image tag, and scratch dir on
#      success; on failure leaves the scratch dir for forensics and
#      prints its path).
#
# The Go harness inside the container (scripts/smoke-enrichment/main.go)
# is what actually exercises the enrichment pipeline; this shell script
# is the host-side orchestrator. Every failure path emits a WHAT/CHECK/
# FIX line per spec 14 §8.
#
# Why not gmail? — see the design note at the top of
# scripts/smoke-enrichment/main.go. tl;dr: gmail is API-only and
# mbox-importer doesn't decompose MIME. The harness stages directly.
#
# Modes:
#
#   --use-local-images        Build the smoke image from this repo's
#                             source. This is the default; it is the
#                             safe choice for dev hosts that have not
#                             pulled ghcr.io/leftathome/...
#                             Note: the smoke image's FROM line points
#                             at ghcr.io/leftathome/glovebox-enricher-
#                             runtime:latest. If that image is not on
#                             the daemon, the BUILD will pull it. If
#                             you cannot reach ghcr.io, build the
#                             enricher-runtime base first via
#                             scripts/test-enricher-runtime.sh and
#                             retag it (see workaround block below).
#
#   --use-registry-images <T> Pull (or assume present) the smoke image
#                             with tag T from a registry. Useful when
#                             CI pre-publishes a pinned smoke image
#                             alongside the connector images.
#                             NOTE: this mode is wired but UNTESTED in
#                             v1 — no CI job builds or publishes
#                             ghcr.io/leftathome/glovebox-smoke-enrichment
#                             yet. Tracked in bd issue glovebox-afq4.16.
#                             Default (--use-local-images) is the only
#                             supported path today.
#
# Usage:
#
#   bash scripts/smoke-enrichment.sh                      # default
#   bash scripts/smoke-enrichment.sh --use-local-images
#   bash scripts/smoke-enrichment.sh --use-registry-images v1.2.3
#
# Exit codes:
#
#   0   all scenarios passed
#   1   one or more scenarios failed; scratch dir preserved
#   2   precondition failure (docker missing, fixtures missing, etc.)

set -euo pipefail

SCOPE="smoke-enrichment"

# fail() emits a WHAT/CHECK/FIX failure line and exits non-zero.
fail() {
    local what="$1" check="$2" fix="$3"
    echo "${SCOPE}: ${what}" >&2
    echo "  CHECK: ${check}" >&2
    echo "  FIX:   ${fix}" >&2
    exit 1
}

# precondition() is like fail() but exits 2 (setup error vs assertion
# failure). Useful when docker isn't installed, etc.
precondition() {
    local what="$1" check="$2" fix="$3"
    echo "${SCOPE}: ${what}" >&2
    echo "  CHECK: ${check}" >&2
    echo "  FIX:   ${fix}" >&2
    exit 2
}

# ---- argument parsing ------------------------------------------------

MODE="local"
REGISTRY_TAG=""

while [ $# -gt 0 ]; do
    case "$1" in
        --use-local-images)
            MODE="local"
            shift
            ;;
        --use-registry-images)
            MODE="registry"
            REGISTRY_TAG="${2:-}"
            if [ -z "$REGISTRY_TAG" ]; then
                precondition \
                    "--use-registry-images requires a tag argument" \
                    "echo \$@" \
                    "invoke as: bash scripts/smoke-enrichment.sh --use-registry-images v1.2.3"
            fi
            shift 2
            ;;
        -h|--help)
            sed -n '2,/^set -e/p' "$0" | sed -n '/^# Usage:/,/^# Exit codes/p'
            exit 0
            ;;
        *)
            precondition \
                "unrecognized argument: $1" \
                "bash scripts/smoke-enrichment.sh --help" \
                "use --use-local-images or --use-registry-images <tag>"
            ;;
    esac
done

# ---- preflight: docker on PATH and reachable -------------------------

if ! command -v docker >/dev/null 2>&1; then
    precondition \
        "docker not on PATH" \
        "command -v docker" \
        "install docker (or podman aliased as docker), or run from a docker-in-docker CI runner"
fi

if ! docker version >/dev/null 2>&1; then
    precondition \
        "docker daemon not reachable" \
        "docker version" \
        "start the docker daemon (sudo systemctl start docker) or fix DOCKER_HOST"
fi

# ---- preflight: repo layout ------------------------------------------

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FIXTURES_DIR="${REPO_ROOT}/scripts/smoke-enrichment/testdata"
HARNESS_DOCKERFILE="${REPO_ROOT}/scripts/smoke-enrichment/Dockerfile"
HARNESS_MAIN="${REPO_ROOT}/scripts/smoke-enrichment/main.go"

for f in "$FIXTURES_DIR" "$HARNESS_DOCKERFILE" "$HARNESS_MAIN"; do
    if ! [ -e "$f" ]; then
        precondition \
            "required path missing: ${f}" \
            "ls -la ${f}" \
            "verify scripts/smoke-enrichment/ is intact in this branch (afq4.11 added it)"
    fi
done

# Validate every fixture file is present. The Go harness reports its own
# WHAT/CHECK/FIX line for a missing fixture, but failing here gives a
# faster signal that doesn't require building the image first.
for f in email-body.html attachment-report.pdf attachment-screenshot.png attachment-blank.png; do
    if ! [ -f "${FIXTURES_DIR}/${f}" ]; then
        precondition \
            "fixture missing: ${FIXTURES_DIR}/${f}" \
            "ls -la ${FIXTURES_DIR}" \
            "restore the fixture from connector/enrich/{html,pdf,ocr}/testdata/; see scripts/smoke-enrichment/testdata/README"
    fi
done

# ---- build or pull the image -----------------------------------------

case "$MODE" in
    local)
        IMAGE="glovebox-smoke-enrichment:test"
        echo "${SCOPE}: building ${IMAGE} from ${HARNESS_DOCKERFILE}..."
        if ! docker build -f "$HARNESS_DOCKERFILE" -t "$IMAGE" "$REPO_ROOT"; then
            fail \
                "docker build of smoke image failed" \
                "docker build -f ${HARNESS_DOCKERFILE} -t ${IMAGE} ${REPO_ROOT}" \
                "inspect the build output above; common causes are an unreachable ghcr.io/leftathome/glovebox-enricher-runtime:latest base or a Go module download failure"
        fi
        ;;
    registry)
        IMAGE="ghcr.io/leftathome/glovebox-smoke-enrichment:${REGISTRY_TAG}"
        echo "${SCOPE}: pulling ${IMAGE}..."
        if ! docker pull "$IMAGE"; then
            fail \
                "docker pull ${IMAGE} failed" \
                "docker pull ${IMAGE}" \
                "verify the tag exists in the registry; for local dev pass --use-local-images instead"
        fi
        ;;
esac

# ---- run the harness against fixtures --------------------------------

SCRATCH_DIR="$(mktemp -d -t smoke-enrichment-XXXXXXXX)"
trap 'cleanup_on_exit' EXIT

cleanup_on_exit() {
    local rc=$?
    if [ "$rc" -eq 0 ]; then
        rm -rf "$SCRATCH_DIR"
    else
        echo "${SCOPE}: scratch dir preserved for forensics: ${SCRATCH_DIR}" >&2
    fi
}

mkdir -p "$SCRATCH_DIR/staging"
# Container runs as nonroot uid 65532; the bind-mounted staging dir
# must be writable by that uid. The simplest portable answer is 0777;
# this is a dev/CI test fixture, not a production artefact.
chmod 0777 "$SCRATCH_DIR/staging"

# Resolve a host-readable absolute path to the fixtures for the mount.
# We mount read-only since the container has no business mutating
# fixtures.
FIXTURES_ABS="$(cd "$FIXTURES_DIR" && pwd)"

# --network=none isolates the smoke from the host network. The Go
# harness does not need network; it reads fixtures and runs binary
# enrichers locally.
#
# The container's default ENTRYPOINT is the smoke binary itself, so we
# pass only the -fixtures-dir / -staging-dir flags.
echo "${SCOPE}: running smoke harness..."
HARNESS_LOG="$SCRATCH_DIR/harness.log"
HARNESS_RC=0
docker run --rm \
    --network=none \
    -v "${FIXTURES_ABS}:/fixtures:ro" \
    -v "${SCRATCH_DIR}/staging:/staging" \
    "$IMAGE" \
    -fixtures-dir=/fixtures \
    -staging-dir=/staging \
    2>&1 | tee "$HARNESS_LOG" || HARNESS_RC=$?

# pipefail is set at the top of this script (set -euo pipefail), so the
# `|| HARNESS_RC=$?` above already captures the rightmost non-zero status
# from the pipe — no PIPESTATUS recovery needed.

# ---- assertions on the harness output --------------------------------

if [ "$HARNESS_RC" -ne 0 ]; then
    fail \
        "smoke harness exited with code ${HARNESS_RC}" \
        "cat ${HARNESS_LOG}" \
        "review the harness log; each per-sidecar FAIL line carries its own WHAT/CHECK/FIX"
fi

# Independent host-side verification that staging dir has the expected
# items. This is belt-and-suspenders: the harness reports its own
# pass/fail counts, but a host-side check catches the case where the
# harness exited 0 but the bind mount silently mapped to a different
# path (e.g. SELinux relabeling failure).
STAGED_COUNT="$(find "$SCRATCH_DIR/staging" -mindepth 1 -maxdepth 1 -type d | wc -l)"
EXPECTED_COUNT=4
if [ "$STAGED_COUNT" -ne "$EXPECTED_COUNT" ]; then
    fail \
        "expected ${EXPECTED_COUNT} staged items in ${SCRATCH_DIR}/staging, found ${STAGED_COUNT}" \
        "ls -la ${SCRATCH_DIR}/staging" \
        "inspect the harness log for per-scenario errors; verify the bind mount is read-write"
fi

# Each item dir must contain a content.extracted.md.
MISSING_SIDECARS=0
while IFS= read -r d; do
    if ! [ -f "$d/content.extracted.md" ]; then
        echo "${SCOPE}: missing content.extracted.md in $(basename "$d")" >&2
        MISSING_SIDECARS=$((MISSING_SIDECARS + 1))
    fi
done < <(find "$SCRATCH_DIR/staging" -mindepth 1 -maxdepth 1 -type d)
if [ "$MISSING_SIDECARS" -ne 0 ]; then
    fail \
        "${MISSING_SIDECARS} staged item(s) missing content.extracted.md" \
        "find ${SCRATCH_DIR}/staging -mindepth 1 -maxdepth 1 -type d -exec ls -la {} \\;" \
        "inspect the harness log for per-enricher errors; verify pdftotext / tesseract / pandoc are on PATH in the smoke image"
fi

# Spec-level success criterion: harness reports PASS=N FAIL=0 line.
if ! grep -qE '^smoke-enrichment: PASS=[0-9]+ FAIL=0' "$HARNESS_LOG"; then
    fail \
        "harness output does not contain 'PASS=N FAIL=0' summary line" \
        "tail -5 ${HARNESS_LOG}" \
        "inspect the per-scenario FAIL lines in the log; resolve the underlying enricher failure"
fi

echo ""
echo "${SCOPE}: PASS — all 4 scenarios produced their expected sidecars"
