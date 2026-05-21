# syntax=docker/dockerfile:1
#
# Schoology browser-session refresher.
#
# Stage 1 mirrors the project's connector build pattern (see
# connectors/imap/Dockerfile). Stage 2 cannot be distroless because the
# refresher needs Chromium for the go-rod driven login flow, so we use
# debian:bookworm-slim with the apt-installed Chromium. The
# ROD_BROWSER_PATH env var pins the binary so go-rod does not try to
# download one at runtime in-cluster.
FROM docker.io/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /refresher ./connectors/schoology-auth-refresher/

FROM docker.io/debian:bookworm-slim
# UID 65532 (the "nonroot" UID used by distroless images) is intentional
# even though the rest of the chart runs as 65534 ("nobody"). 65532 is
# the convention for "non-root in a debian-derived image with a real
# user account", which we need for the home dir useradd creates above.
# Using 65534 here would require a different useradd invocation and add
# confusion without changing the security posture.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        chromium \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 65532 -m -s /usr/sbin/nologin refresher
ENV ROD_BROWSER_PATH=/usr/bin/chromium
COPY --from=build /refresher /refresher
USER 65532
ENTRYPOINT ["/refresher"]
