# syntax=docker/dockerfile:1
#
# Login broker: the always-on service that logs in to a site when a
# connector asks (internal/loginbroker/DESIGN.md). One image for every
# site; one Deployment per site.
#
# This is the PowerSchool login job's image with another binary in it, and
# for the same reason: the PowerSchool driver logs in with powerschool-go,
# which can only start a LOCAL Chromium through chromedp's exec allocator.
# So: debian:bookworm-slim with the apt Chromium, run as a real non-root
# user, exactly as connectors/powerschool-auth-refresher/Dockerfile -- read
# that file for why each choice below is what it is (the digest pin, the
# unpinned Chromium, apt-get upgrade, --no-sandbox). Keep the two in step:
# a change to one base or package list is a change to both.
#
# A site whose driver uses a remote browser, or none, does not need the
# Chromium in here; it is carried anyway so that there is one image to
# scan, sign and pin.
FROM docker.io/golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /login-broker ./connectors/login-broker/

# Pinned by digest: this image types a parent account's password into a
# browser, so what it is built on changes only by a reviewed commit. The
# same digest as the PowerSchool login job's image (the multi-arch index of
# debian:bookworm-slim as of 2026-10-11); Renovate raises a PR for both when
# the tag moves.
FROM docker.io/debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
# UID 65532 is the "nonroot" UID of the distroless images and what the chart
# runs this Deployment as. It needs a real account with a home directory:
# Chromium writes its config and crash state under $HOME. (In the pod,
# $HOME, /tmp and /dev/shm are memory-backed volumes and the root
# filesystem is read-only.)
#
# apt-get upgrade: the base is pinned by digest, so it does not pick up
# Debian security updates published after that digest was built. The CI job
# `login-broker-image` (GitHub) scans the built image with Trivy and starts the
# browser in it before any image is published.
RUN apt-get update \
    && apt-get upgrade -y --no-install-recommends \
    && apt-get install -y --no-install-recommends \
        chromium \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 65532 -m -d /home/broker -s /usr/sbin/nologin broker \
    && dpkg-query -W -f='chromium ${Version}\n' chromium | tee /etc/chromium-version
# --no-sandbox: see the PowerSchool login job's Dockerfile. The container is
# the sandbox: one process tree, one site, a read-only root, no persistent
# storage, egress limited by NetworkPolicy.
#
# LOGIN_BROKER_OWNS_TMPDIR: /tmp in this image belongs to this process alone
# (the chart backs it with memory), so the binary removes browser profiles
# and browser scratch directories from it at startup and after every login
# attempt. A profile holds session cookies.
ENV HOME=/home/broker \
    TMPDIR=/tmp \
    POWERSCHOOL_CHROME_NO_SANDBOX=true \
    LOGIN_BROKER_OWNS_TMPDIR=true
COPY --from=build /login-broker /login-broker
USER 65532
EXPOSE 8080 9090
ENTRYPOINT ["/login-broker"]
