# Dockerfile for the spec-14 §7.3 end-to-end smoke harness.
#
# Build context is the glovebox repo root (the build needs go.mod /
# go.sum and the connector/enrich/* packages). Tagged by
# scripts/smoke-enrichment.sh as glovebox-smoke-enrichment:test.
#
# Tag rationale: the FROM base uses the same :latest enricher-runtime
# moving tag as the connector Dockerfiles (gmail, imap, outlook,
# mbox-importer, arxiv, semantic-scholar) so the smoke runs in an
# environment byte-identical to those connectors' runtime base.

FROM docker.io/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG LDFLAGS="-s -w"
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="${LDFLAGS}" -o /smoke-enrichment ./scripts/smoke-enrichment/

# The smoke harness must run on the same base image as the gmail / imap /
# outlook / mbox-importer / arxiv / semantic-scholar connectors so that
# absence of pdftotext / tesseract / pandoc surfaces here exactly as it
# would in production. Spec docs/specs/14-content-enrichment-design.md §6.2.
FROM ghcr.io/leftathome/glovebox-enricher-runtime:latest
COPY --from=build /smoke-enrichment /smoke-enrichment
USER nonroot:nonroot
ENTRYPOINT ["/smoke-enrichment"]
