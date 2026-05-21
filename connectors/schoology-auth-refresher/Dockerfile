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
