FROM docker.io/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/arxiv/

# Rebased onto glovebox-enricher-runtime per docs/specs/14-content-enrichment-design.md sec 6.2.
# arxiv items are predominantly PDFs; the base image carries pdftotext (poppler-utils)
# so the pdf enricher's init() check finds its binary and activates at startup.
# TODO: pin to an immutable tag (semver or sha) once a stable enricher-runtime release exists.
FROM ghcr.io/leftathome/glovebox-enricher-runtime:latest
COPY --from=build /connector /connector
COPY connectors/arxiv/config.json /etc/connector/config.json
USER nonroot:nonroot
ENTRYPOINT ["/connector"]
