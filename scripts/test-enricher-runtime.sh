#!/usr/bin/env bash
#
# test-enricher-runtime.sh -- build + verify glovebox-enricher-runtime base image.
#
# Spec: docs/specs/14-content-enrichment-design.md section 6.1.
# Task: glovebox-afq4.5.
#
# Builds Dockerfile.enricher-runtime, then asserts:
#   1. Image size is between 100 MB and 400 MB (catches silent apt failures
#      and accidental dep bloat; see size-bound rationale below).
#   2. pdftotext, tesseract, pandoc are present and executable as the
#      shipped runtime user.
#   2b. pandoc actually READS xlsx (converts the committed sample.xlsx) and
#      pptx (md->pptx->md round-trip) -- not via --list-input-formats, which
#      is unreliable on CI (glovebox-afq4.13; needs pinned upstream pandoc
#      >=3.5, not Debian apt pandoc).
#   3. The runtime user is uid 65532 / gid 65532 (nonroot).
#   4. /bin/sh exists (required for the inline test commands).
#
# Failure messages follow the WHAT/CHECK/FIX shape so the operator can
# diagnose without re-reading the script.
#
# Usage:
#   bash scripts/test-enricher-runtime.sh
# Requires:
#   - docker (or podman aliased as docker) on PATH
#   - jq on PATH (for parsing `docker image inspect` output)
#
# CI: invoked by .github/workflows/ci.yml job `build-enricher-runtime`
# before the publish step.

set -euo pipefail

SCOPE="enricher-runtime"
IMAGE="${ENRICHER_RUNTIME_IMAGE:-glovebox-enricher-runtime:test}"
DOCKERFILE="${ENRICHER_RUNTIME_DOCKERFILE:-Dockerfile.enricher-runtime}"
CONTEXT_DIR="${ENRICHER_RUNTIME_CONTEXT:-.}"

PASS=0
FAIL=0

fail() {
    # fail "<what failed[: why]>" "<check cmd>" "<fix action>"
    local what="$1" check="$2" fix="$3"
    echo "${SCOPE}: ${what}; CHECK ${check}; FIX ${fix}" >&2
    FAIL=$((FAIL + 1))
}

pass() {
    echo "${SCOPE}: PASS $1"
    PASS=$((PASS + 1))
}

require() {
    local tool="$1"
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "${SCOPE}: required tool '${tool}' not on PATH; CHECK 'command -v ${tool}'; FIX install ${tool} or set PATH" >&2
        exit 2
    fi
}

require docker
require jq

echo "${SCOPE}: building ${IMAGE} from ${DOCKERFILE}..."
if ! docker build -f "${DOCKERFILE}" -t "${IMAGE}" "${CONTEXT_DIR}"; then
    fail "docker build failed" \
         "docker build -f ${DOCKERFILE} -t ${IMAGE} ${CONTEXT_DIR}" \
         "inspect the build log above; common causes are apt mirror outage or wrong base image tag"
    echo ""
    echo "${SCOPE}: aborting -- nothing to test without an image"
    exit 1
fi
pass "docker build succeeded"

# --- 1. Image size sanity bound (100 MB - 250 MB) ----------------------------
SIZE_BYTES=$(docker image inspect "${IMAGE}" | jq '.[0].Size')
if [ -z "${SIZE_BYTES}" ] || [ "${SIZE_BYTES}" = "null" ]; then
    fail "could not read image size from docker inspect" \
         "docker image inspect ${IMAGE} | jq '.[0].Size'" \
         "verify the image actually built; rerun docker build manually"
else
    SIZE_MB=$((SIZE_BYTES / 1024 / 1024))
    # Sanity bounds. The spec (sec 6.1) estimated 150-200 MB but did not
    # account for pandoc's Haskell runtime (~110 MB) on top of tesseract
    # (~55 MB) + poppler-utils (~25 MB) + the bookworm-slim base (~75 MB)
    # + ca-certificates + transitive deps. Empirically the image lands
    # near 340 MB on debian:bookworm-slim as of 2026-06. Floor catches
    # silent apt-install failures; ceiling catches accidental bloat
    # (e.g. someone adding tex-live or all tesseract language packs).
    MIN_MB=100
    MAX_MB=400
    if [ "${SIZE_MB}" -lt "${MIN_MB}" ]; then
        fail "image size ${SIZE_MB} MB is below floor ${MIN_MB} MB -- likely a silent apt install failure" \
             "docker run --rm ${IMAGE} sh -c 'dpkg -l | grep -E \"poppler|tesseract|pandoc\"'" \
             "check apt-get output during build for 'Unable to locate package' or repo errors"
    elif [ "${SIZE_MB}" -gt "${MAX_MB}" ]; then
        fail "image size ${SIZE_MB} MB exceeds ceiling ${MAX_MB} MB -- unintended dep crept in" \
             "docker run --rm ${IMAGE} sh -c 'dpkg -l | sort -k3 -rh | head'" \
             "review the Dockerfile apt-get install list; remove unneeded packages or split into a separate image"
    else
        pass "image size ${SIZE_MB} MB within ${MIN_MB}-${MAX_MB} MB"
    fi
fi

# All runtime checks use --network=none: the tests don't need a network,
# and this sidesteps host CNI/networking flakiness (e.g. podman on hosts
# with nftables vs iptables-legacy mismatch). docker and podman both
# support --network=none.
DOCKER_RUN="docker run --rm --network=none"

# --- 2. Required binaries present & executable as the shipped user -----------
check_binary() {
    local bin="$1" expect="$2" args="$3"
    # Run with the image's default USER (nonroot:nonroot); do NOT override.
    # We pipe the tool's output through head -1 *inside* the container so
    # we get a single canonical line; we capture only stdout and discard
    # container-runtime stderr noise.
    if ! output=$(${DOCKER_RUN} "${IMAGE}" sh -c "${bin} ${args} 2>&1 | head -1" 2>/dev/null); then
        fail "running '${bin} ${args}' as nonroot exited non-zero" \
             "${DOCKER_RUN} ${IMAGE} sh -c '${bin} ${args}'" \
             "verify ${bin} is in PATH for nonroot and has execute permission"
        return
    fi
    if echo "${output}" | grep -qi "${expect}"; then
        pass "${bin} present (output: ${output})"
    else
        fail "'${bin} ${args}' output did not contain '${expect}' (got: ${output})" \
             "${DOCKER_RUN} ${IMAGE} sh -c '${bin} ${args}'" \
             "verify the apt package providing ${bin} is installed and not a wrapper renaming the tool"
    fi
}

check_binary pdftotext pdftotext "-v"
check_binary tesseract tesseract "--version"
check_binary pandoc pandoc "--version"

# --- 2b. pandoc must READ xlsx + pptx (glovebox-afq4.13) ---------------------
# xlsx-input requires pandoc >=3.5, pptx-input >=3.0. Debian's apt pandoc is
# too old on every current stable (bookworm 2.17, trixie 3.1.11), so the
# Dockerfile installs a pinned upstream pandoc .deb.
#
# We verify the actual READ CAPABILITY (an actual pandoc conversion), never
# `--list-input-formats`. That list proved unreliable on GitHub's runners:
# one run omitted xlsx while listing pptx, a later run returned an EMPTY
# list (failing pptx) even though `pandoc -f xlsx`/`-f pptx` both convert
# fine. So a list check is both flaky and weaker than exercising the real
# conversion the office enricher performs in production.
#
# Diagnostics are dumped unconditionally so a future regression is easy to
# triage from the CI log alone.
echo "${SCOPE}: pandoc version + input formats (diagnostic):"
${DOCKER_RUN} "${IMAGE}" sh -c 'pandoc --version | head -1; echo "input-formats:"; pandoc --list-input-formats | tr "\n" " "; echo' 2>/dev/null || true

# Absolute path to the committed office fixtures, mounted read-only so the
# pandoc reader runs against the same xlsx the office enricher's tests use.
OFFICE_TD="$(cd "${CONTEXT_DIR}/connector/enrich/office/testdata" 2>/dev/null && pwd || true)"

check_pandoc_reads_xlsx() {
    if [ -z "${OFFICE_TD}" ] || [ ! -f "${OFFICE_TD}/sample.xlsx" ]; then
        fail "office xlsx fixture not found at connector/enrich/office/testdata/sample.xlsx" \
             "ls -la ${CONTEXT_DIR}/connector/enrich/office/testdata/" \
             "restore the committed sample.xlsx fixture (added in glovebox-afq4.13)"
        return
    fi
    # Convert the fixture and assert a known cell appears. ZEBRA is in row 2.
    local out
    out=$(${DOCKER_RUN} -v "${OFFICE_TD}:/td:ro" "${IMAGE}" \
        sh -c 'pandoc -f xlsx -t markdown /td/sample.xlsx 2>&1' 2>/dev/null || true)
    if echo "${out}" | grep -q "ZEBRA"; then
        pass "pandoc reads xlsx (sample.xlsx -> markdown, cell ZEBRA present)"
    else
        fail "pandoc could not read xlsx (sample.xlsx -> markdown missing cell ZEBRA); got: ${out}" \
             "${DOCKER_RUN} -v ${OFFICE_TD}:/td:ro ${IMAGE} sh -c 'pandoc -f xlsx -t markdown /td/sample.xlsx'" \
             "bump the pinned upstream pandoc (PANDOC_VERSION + PANDOC_DEB_SHA256) in Dockerfile.enricher-runtime to a build with a working xlsx reader (>=3.5)"
    fi
}
check_pandoc_reads_xlsx

# pptx read capability. Unlike xlsx, pandoc CAN write pptx, so we verify
# via a self-contained md -> pptx -> md round-trip (no committed binary
# fixture, no reliance on --list-input-formats): generate a pptx with a
# known slide title, read it back, and assert the title survives.
check_pandoc_reads_pptx() {
    local out
    out=$(${DOCKER_RUN} "${IMAGE}" sh -c \
        'printf "# PptxTitle\n\nbody\n" | pandoc -f markdown -t pptx -o /tmp/t.pptx && pandoc -f pptx -t markdown /tmp/t.pptx 2>&1' 2>/dev/null || true)
    if echo "${out}" | grep -q "PptxTitle"; then
        pass "pandoc reads pptx (md -> pptx -> md round-trip, title present)"
    else
        fail "pandoc could not round-trip pptx (md -> pptx -> md missing title); got: ${out}" \
             "${DOCKER_RUN} ${IMAGE} sh -c 'printf \"# PptxTitle\\n\\nbody\\n\" | pandoc -f markdown -t pptx -o /tmp/t.pptx && pandoc -f pptx -t markdown /tmp/t.pptx'" \
             "bump the pinned upstream pandoc (PANDOC_VERSION + per-arch sha256) in Dockerfile.enricher-runtime to a build with a working pptx reader (>=3.0)"
    fi
}
check_pandoc_reads_pptx

# --- 3. Runtime user is uid 65532 / gid 65532 (nonroot) ----------------------
ID_OUTPUT=$(${DOCKER_RUN} "${IMAGE}" id 2>/dev/null || true)
if echo "${ID_OUTPUT}" | grep -q "uid=65532" && echo "${ID_OUTPUT}" | grep -q "gid=65532"; then
    pass "runtime user is nonroot uid=65532 gid=65532 (output: ${ID_OUTPUT})"
else
    fail "runtime user is not uid=65532/gid=65532 (got: ${ID_OUTPUT})" \
         "${DOCKER_RUN} ${IMAGE} id" \
         "verify the USER directive in Dockerfile.enricher-runtime is 'nonroot:nonroot' and the user was created with -u 65532 -g 65532"
fi

# --- 4. /bin/sh exists -------------------------------------------------------
# debian:bookworm-slim ships /bin/sh by default; this catches a future
# regression where a minimization step removes it.
if SH_OUT=$(${DOCKER_RUN} "${IMAGE}" sh -c 'echo SHELL_OK' 2>/dev/null) && [ "${SH_OUT}" = "SHELL_OK" ]; then
    pass "/bin/sh is functional"
else
    fail "/bin/sh did not echo SHELL_OK (got: ${SH_OUT})" \
         "${DOCKER_RUN} ${IMAGE} sh -c 'echo SHELL_OK'" \
         "verify the base image still ships /bin/sh; debian:bookworm-slim should"
fi

# --- Summary -----------------------------------------------------------------
echo ""
echo "${SCOPE}: PASS=${PASS} FAIL=${FAIL}"
if [ "${FAIL}" -ne 0 ]; then
    exit 1
fi
