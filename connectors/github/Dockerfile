FROM docker.io/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/github/
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /connector /connector
COPY connectors/github/config.json /etc/connector/config.json
USER nonroot:nonroot
ENTRYPOINT ["/connector"]
