FROM docker.io/golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG LDFLAGS="-s -w"
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="${LDFLAGS}" -o /glovebox .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /glovebox /glovebox
COPY configs/default-config.json /etc/glovebox/config.json
COPY configs/default-rules.json /etc/glovebox/rules.json
USER nonroot:nonroot
ENTRYPOINT ["/glovebox"]
CMD ["--config", "/etc/glovebox/config.json"]
