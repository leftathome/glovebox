# ENRICHER_BASE must be declared in the global (pre-FROM) scope so it is in
# scope for the `FROM ${ENRICHER_BASE}` stage below. A stage-scoped ARG (after
# the first FROM) resolves blank there, which newer BuildKit rejects with
# UndefinedArgInFrom (enricher Dockerfile ARG-scope fix).
ARG ENRICHER_BASE=ghcr.io/leftathome/glovebox-enricher-runtime:sha-c4ee4a9
FROM docker.io/golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/semantic-scholar/

# Rebased onto glovebox-enricher-runtime per docs/specs/14-content-enrichment-design.md sec 6.2.
# semantic-scholar items are predominantly PDFs; the base image carries pdftotext
# (poppler-utils) so the pdf enricher's init() check finds its binary and activates.
# Pinned to the immutable enricher-runtime tag sha-c4ee4a9 (multi-arch,
# pandoc 3.10). Bump across all rebased connectors + the gitlab CI template
# together when the base changes -- manual, since sha-<short> tags are not
# renovate-version-sortable. The gitlab pipeline overrides ENRICHER_BASE via ARG.
FROM ${ENRICHER_BASE}
COPY --from=build /connector /connector
COPY connectors/semantic-scholar/config.json /etc/connector/config.json
USER nonroot:nonroot
ENTRYPOINT ["/connector"]
