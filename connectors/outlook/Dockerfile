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
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/outlook/

# Rebased onto glovebox-enricher-runtime (debian:bookworm-slim + poppler-utils,
# tesseract-ocr+eng, pandoc, ca-certificates) so the outlook connector can run
# the pdf / ocr / office enrichers on attachment content. Spec
# docs/specs/14-content-enrichment-design.md §6.2 (outlook row).
#
# Tag rationale: ENRICHER_BASE is pinned to the immutable multi-arch tag
# sha-c4ee4a9 -- the enricher-runtime build for that commit (debian:bookworm-slim
# + poppler-utils + tesseract + pinned upstream pandoc 3.10). When the base
# image changes, bump this tag across all rebased connectors AND the gitlab CI
# template together; it is a manual bump because sha-<short> tags are not
# renovate-version-sortable. The gitlab pipeline overrides ENRICHER_BASE via
# ARG to its in-cluster registry tag, so this default governs github + local
# builds. The base runs as nonroot uid/gid 65532, so the USER directive below
# is a drop-in continuation of the prior distroless:nonroot identity.
FROM ${ENRICHER_BASE}
COPY --from=build /connector /connector
COPY connectors/outlook/config.json /etc/connector/config.json
USER nonroot:nonroot
ENTRYPOINT ["/connector"]
