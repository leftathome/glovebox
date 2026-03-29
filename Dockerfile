FROM docker.io/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /glovebox .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /glovebox /glovebox
COPY configs/default-config.json /etc/glovebox/config.json
COPY configs/default-rules.json /etc/glovebox/rules.json
USER nonroot:nonroot
ENTRYPOINT ["/glovebox"]
CMD ["--config", "/etc/glovebox/config.json"]
