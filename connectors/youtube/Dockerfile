FROM docker.io/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /connector ./connectors/youtube/

# Not distroless: the yt-dlp transcript provider (glovebox-9zmo) needs Python.
# yt-dlp fetches caption tracks only (--skip-download); no ffmpeg, no JS
# runtime, no PO-token provider -- subs-only extraction did not need them when
# tested on 2026-11-08. Add them here only if that stops being true.
FROM docker.io/library/python:3.13-slim-bookworm@sha256:f3dac913ab53b78c64f26d5518fcd7043d5a1a46522d6a493b598c3ed12f62a6
COPY connectors/youtube/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir --root-user-action=ignore --require-hashes --no-deps -r /tmp/requirements.txt \
    && rm /tmp/requirements.txt \
    && useradd --uid 65532 --no-create-home --home-dir /tmp --shell /usr/sbin/nologin nonroot
COPY --from=build /connector /connector
COPY connectors/youtube/config.json /etc/connector/config.json
ENV HOME=/tmp XDG_CACHE_HOME=/tmp/.cache
USER 65532:65532
ENTRYPOINT ["/connector"]
