# syntax=docker/dockerfile:1
#
# PowerSchool session login job.
#
# Stage 1 is the project's connector build (see connectors/imap/Dockerfile).
#
# Stage 2 cannot be distroless the way schoology-auth-refresher now is. That
# binary drives a remote Browserless browser; powerschool-go (v0.4.0) can
# only start a LOCAL Chromium through chromedp's exec allocator and has no
# remote-browser option. So this image is what the Schoology refresher's was
# before it moved to Browserless: debian:bookworm-slim with the apt
# Chromium, run as a real non-root user.
FROM docker.io/golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /refresher ./connectors/powerschool-auth-refresher/

# Pinned by digest: this image types a parent account's password into a
# browser, so what it is built on changes only by a reviewed commit. The
# digest is the multi-arch index of debian:bookworm-slim as of 2026-10-11;
# Renovate (config:recommended) raises a PR when the tag moves.
FROM docker.io/debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
# UID 65532 is the "nonroot" UID of the distroless images and what the chart
# runs this job as. It needs a real account with a home directory: Chromium
# writes its config and crash state under $HOME. (In the pod, $HOME, /tmp and
# /dev/shm are memory-backed volumes and the root filesystem is read-only.)
#
# Chromium is NOT pinned to a package version, deliberately:
#
#   - bookworm-security carries exactly one chromium version at a time. When
#     a DSA replaces it the old version leaves the archive, so a pin of the
#     form chromium=<version> breaks the build at the next browser security
#     update -- typically within two weeks -- and the only fix is to bump
#     the pin by hand to whatever is current, which is what an unpinned
#     install does anyway.
#   - the alternative that does freeze the version, building against
#     snapshot.debian.org at a fixed date, freezes it with its known
#     vulnerabilities, in the one component that parses a remote site.
#
# So the version is whatever bookworm-security holds when the image is
# built. It is printed below (so it is in the build log), recorded in the
# image at /etc/chromium-version, and it changes only when the image is
# rebuilt: on a
# release tag, or when the base digest above is bumped. The CI job
# `powerschool-auth-refresher-image` (GitHub) scans the built image with
# Trivy and starts the browser in it before any image is published.
# apt-get upgrade: the base is pinned by digest, so it does not pick up
# Debian security updates published after that digest was built. Without
# the upgrade the image shipped fixed CRITICALs in perl-base (0.10.1's
# Trivy gate caught CVE-2026-13221, CVE-2026-42496 and CVE-2026-8376 and
# the image was never published). Same approach as
# Dockerfile.enricher-runtime.
RUN apt-get update \
    && apt-get upgrade -y --no-install-recommends \
    && apt-get install -y --no-install-recommends \
        chromium \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 65532 -m -d /home/refresher -s /usr/sbin/nologin refresher \
    && dpkg-query -W -f='chromium ${Version}\n' chromium | tee /etc/chromium-version
# chromedp looks for a browser on PATH by name ("chromium" among them), so
# /usr/bin/chromium needs no further configuration.
#
# --no-sandbox: Chromium's own sandbox needs either unprivileged user
# namespaces or its setuid helper. The pod runs with every capability
# dropped, no privilege escalation and the runtime's default seccomp profile,
# under which neither is available and Chromium exits at startup. The library
# adds the flag by itself only when it runs as root; this job does not, so
# the binary is told to ask for it. The container is the sandbox here: one
# process tree, one site (the portal's sign-in page), a read-only root, no
# persistent storage, egress limited by NetworkPolicy.
ENV HOME=/home/refresher \
    TMPDIR=/tmp \
    POWERSCHOOL_CHROME_NO_SANDBOX=true
COPY --from=build /refresher /refresher
USER 65532
ENTRYPOINT ["/refresher"]
