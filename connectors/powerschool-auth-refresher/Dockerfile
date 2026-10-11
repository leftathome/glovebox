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

FROM docker.io/debian:bookworm-slim
# UID 65532 is the "nonroot" UID of the distroless images and what the chart
# runs this job as. It needs a real account with a home directory: Chromium
# writes its profile and crash state under $HOME and /tmp.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        chromium \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 65532 -m -d /home/refresher -s /usr/sbin/nologin refresher
# chromedp looks for a browser on PATH by name ("chromium" among them), so
# /usr/bin/chromium needs no further configuration.
#
# --no-sandbox: Chromium's own sandbox needs either unprivileged user
# namespaces or its setuid helper. The pod runs with every capability
# dropped, no privilege escalation and the runtime's default seccomp profile,
# under which neither is available and Chromium exits at startup. The library
# adds the flag by itself only when it runs as root; this job does not, so
# the binary is told to ask for it. The container is the sandbox here: one
# process, one site (the portal's sign-in page), no persistent storage.
ENV HOME=/home/refresher \
    POWERSCHOOL_CHROME_NO_SANDBOX=true
COPY --from=build /refresher /refresher
USER 65532
ENTRYPOINT ["/refresher"]
