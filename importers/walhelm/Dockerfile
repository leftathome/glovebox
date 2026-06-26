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
ARG LDFLAGS="-s -w"
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="${LDFLAGS}" -o /importer ./importers/walhelm/

# Rebased onto glovebox-enricher-runtime (debian:bookworm-slim + poppler-utils,
# tesseract-ocr+eng, pandoc, ca-certificates) so the walhelm importer can run
# the pdf / ocr / office enrichers on attachment content during 20-year
# backfill (the driver use case for this rebase). Spec
# docs/specs/14-content-enrichment-design.md §6.2 (mbox-importer row).
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
COPY --from=build /importer /importer
COPY importers/walhelm/config.json /etc/importer/config.json
USER nonroot:nonroot
ENTRYPOINT ["/importer"]
