FROM docker.io/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG LDFLAGS="-s -w"
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="${LDFLAGS}" -o /importer ./importers/mbox/

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /importer /importer
COPY importers/mbox/config.json /etc/importer/config.json
USER nonroot:nonroot
ENTRYPOINT ["/importer"]
