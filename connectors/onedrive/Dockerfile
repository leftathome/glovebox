FROM docker.io/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/onedrive/
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /connector /connector
COPY connectors/onedrive/config.json /etc/connector/config.json
USER nonroot:nonroot
ENTRYPOINT ["/connector"]
