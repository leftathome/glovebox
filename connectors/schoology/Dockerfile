FROM docker.io/golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/schoology/cmd/schoology

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /connector /connector
COPY connectors/schoology/config.json /etc/connector/config.json
USER nonroot:nonroot
ENTRYPOINT ["/connector"]
