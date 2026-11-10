FROM docker.io/golang:1.27.3-bookworm@sha256:d9b6a1beb9474adc2a1ce842573639b44b2a0dd49b6669f51d3821baf9ec37c9 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/glovebox .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/rss-connector ./connectors/rss/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/imap-connector ./connectors/imap/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/github-connector ./connectors/github/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/gitlab-connector ./connectors/gitlab/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/jira-connector ./connectors/jira/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/trello-connector ./connectors/trello/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/linkedin-connector ./connectors/linkedin/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/meta-connector ./connectors/meta/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/bluesky-connector ./connectors/bluesky/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/x-connector ./connectors/x/
