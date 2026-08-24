# syntax=docker/dockerfile:1
#
# Schoology browser-session refresher.
#
# Stage 1 mirrors the project's connector build pattern (see
# connectors/imap/Dockerfile). Stage 2 is distroless: the refresher no
# longer bundles Chromium. It drives a remote, cluster-local Browserless
# browser via auth.WithBrowserURL (BROWSERLESS_URL/BROWSERLESS_TOKEN), so
# the runtime image only needs the pure-Go (CGO_ENABLED=0) binary.
FROM docker.io/golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /refresher ./connectors/schoology-auth-refresher/

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /refresher /refresher
USER nonroot:nonroot
ENTRYPOINT ["/refresher"]
