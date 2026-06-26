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
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/outlook/

# Rebased onto glovebox-enricher-runtime (debian:bookworm-slim + poppler-utils,
# tesseract-ocr+eng, pandoc, ca-certificates) so the outlook connector can run
# the pdf / ocr / office enrichers on attachment content. Spec
# docs/specs/14-content-enrichment-design.md §6.2 (outlook row).
#
# Tag rationale: :latest matches the moving tag the CI workflow publishes on
# main (see .github/workflows/ci.yml build-enricher-runtime job) and follows
# the prevailing convention in this repo (golang:1.26, debian:bookworm-slim,
# distroless/static-debian12:nonroot are all moving tags). Pin to an
# immutable sha-<short> or semver tag once the enricher-runtime base image
# stabilizes. The published base image already runs as nonroot uid/gid 65532
# so the USER directive below is a drop-in continuation of the prior
# distroless:nonroot identity.
FROM ${ENRICHER_BASE}
COPY --from=build /connector /connector
COPY connectors/outlook/config.json /etc/connector/config.json
USER nonroot:nonroot
ENTRYPOINT ["/connector"]
