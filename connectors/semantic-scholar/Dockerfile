# ENRICHER_BASE must be declared in the global (pre-FROM) scope so it is in
# scope for the `FROM ${ENRICHER_BASE}` stage below. A stage-scoped ARG (after
# the first FROM) resolves blank there, which newer BuildKit rejects with
# UndefinedArgInFrom (enricher Dockerfile ARG-scope fix).
ARG ENRICHER_BASE=ghcr.io/leftathome/glovebox-enricher-runtime:latest
FROM docker.io/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/semantic-scholar/

# Rebased onto glovebox-enricher-runtime per docs/specs/14-content-enrichment-design.md sec 6.2.
# semantic-scholar items are predominantly PDFs; the base image carries pdftotext
# (poppler-utils) so the pdf enricher's init() check finds its binary and activates.
# TODO: pin to an immutable tag (semver or sha) once a stable enricher-runtime release exists.
FROM ${ENRICHER_BASE}
COPY --from=build /connector /connector
COPY connectors/semantic-scholar/config.json /etc/connector/config.json
USER nonroot:nonroot
ENTRYPOINT ["/connector"]
