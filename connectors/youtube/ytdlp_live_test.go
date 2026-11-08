package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestYtdlpLive runs the real yt-dlp against a real video. It reaches
// YouTube, so it runs only with YOUTUBE_YTDLP_LIVE=<video id>, e.g. inside
// the connector image:
//
//	go test -c -o yt.test ./connectors/youtube
//	podman run --rm --read-only --tmpfs /tmp -v ./yt.test:/yt.test:ro \
//	  -e YOUTUBE_YTDLP_LIVE=vFMU2m-TZ3s --entrypoint /yt.test <image> -test.run Live -test.v
func TestYtdlpLive(t *testing.T) {
	id := os.Getenv("YOUTUBE_YTDLP_LIVE")
	if id == "" {
		t.Skip("set YOUTUBE_YTDLP_LIVE=<video id> to run against YouTube")
	}
	p := ytdlpProvider{bin: "yt-dlp", timeout: 2 * time.Minute}
	r, err := p.Transcript(context.Background(), id, []string{"en"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != transcriptAvailable || len(r.Text) < 200 || !strings.HasPrefix(r.Text, "[0:") {
		t.Fatalf("result = %+v (text %d bytes)", r, len(r.Text))
	}
	t.Logf("%s: %d bytes; starts %q", r.Language, len(r.Text), r.Text[:120])
}
