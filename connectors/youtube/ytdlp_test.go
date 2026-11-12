package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func fakeYtdlp(t *testing.T, mode string) (ytdlpProvider, string) {
	t.Helper()
	bin, err := filepath.Abs("testdata/fake-yt-dlp.sh")
	if err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_YTDLP_MODE", mode)
	t.Setenv("FAKE_YTDLP_ARGS", argsFile)
	return ytdlpProvider{bin: bin, timeout: 10 * time.Second}, argsFile
}

func TestYtdlpPrefersCreatorCaptions(t *testing.T) {
	p, argsFile := fakeYtdlp(t, "both")
	r, err := p.Transcript(context.Background(), "vid1", []string{"en"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != transcriptAvailable || r.Language != "en-US, creator captions" {
		t.Fatalf("result = %+v, want available en-US creator captions", r)
	}
	if !strings.Contains(r.Text, "[0:00] Creator line one.") || !strings.Contains(r.Text, "[1:05] Creator line two & more.") {
		t.Fatalf("text = %q", r.Text)
	}

	raw, _ := os.ReadFile(argsFile)
	args := string(raw)
	if n := strings.Count(args, "---\n"); n != 2 {
		t.Errorf("yt-dlp ran %d times, want 2 (list, then one track)", n)
	}
	for _, want := range []string{"--dump-single-json\n", "--load-info-json\n", "--write-subs\n", "--sub-langs\n^en-US$\n", "--sub-format\nvtt\n", "--\nhttps://www.youtube.com/watch?v=vid1\n"} {
		if !strings.Contains(args, want) {
			t.Errorf("yt-dlp args missing %q:\n%s", want, args)
		}
	}
	// One exact track, never a pattern that also matches translations.
	if strings.Contains(args, ".*") || strings.Contains(args, "--write-auto-subs") {
		t.Errorf("yt-dlp asked for more than the one creator track:\n%s", args)
	}
	for _, bad := range []string{"--format", "-f\n", "--extract-audio", "-x\n"} {
		if strings.Contains(args, bad) {
			t.Errorf("yt-dlp args request media (%q):\n%s", bad, args)
		}
	}
}

func TestYtdlpFallsBackToAutoCaptionsAndDedupesRollingLines(t *testing.T) {
	p, _ := fakeYtdlp(t, "auto")
	r, err := p.Transcript(context.Background(), "vid1", []string{"en"})
	if err != nil {
		t.Fatal(err)
	}
	// en-orig (original-language ASR), never the machine-translated en-de.
	if r.Language != "en-orig, automatic captions" {
		t.Fatalf("language = %q, want en-orig automatic", r.Language)
	}
	if r.Text != "[0:00] hello world this is auto" {
		t.Fatalf("text = %q", r.Text)
	}
}

func TestYtdlpNoCaptionsIsUnavailableNotError(t *testing.T) {
	p, _ := fakeYtdlp(t, "none")
	r, err := p.Transcript(context.Background(), "vid1", []string{"en"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != transcriptUnavailable || r.Reason == "" {
		t.Fatalf("result = %+v", r)
	}
}

func TestYtdlpFailureIsAnError(t *testing.T) {
	p, _ := fakeYtdlp(t, "fail")
	_, err := p.Transcript(context.Background(), "vid1", []string{"en"})
	if err == nil || !strings.Contains(err.Error(), "not a bot") {
		t.Fatalf("err = %v, want the yt-dlp error line", err)
	}
}

func TestYtdlpTimeout(t *testing.T) {
	p, _ := fakeYtdlp(t, "sleep")
	p.timeout = 300 * time.Millisecond
	_, err := p.Transcript(context.Background(), "vid1", []string{"en"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", err)
	}
}

func TestNewTranscriptProviderYtdlp(t *testing.T) {
	p, err := newTranscriptProvider(TranscriptConfig{Provider: "yt-dlp", Timeout: "2m"})
	if err != nil {
		t.Fatal(err)
	}
	y, ok := p.(ytdlpProvider)
	if !ok || y.bin != "yt-dlp" || y.timeout != 2*time.Minute {
		t.Fatalf("provider = %#v", p)
	}
	if _, err := newTranscriptProvider(TranscriptConfig{Provider: "yt-dlp", Timeout: "soon"}); err == nil {
		t.Error("bad timeout accepted")
	}
}

func TestVTTToTextMarkers(t *testing.T) {
	vtt := "WEBVTT\n\n1\n00:00:01.000 --> 00:00:02.000\nfirst\n\n2\n00:00:30.000 --> 00:00:31.000\nsecond\n\n3\n01:02:03.000 --> 01:02:04.000\nthird\n"
	if got := vttToText(vtt); got != "[0:01] first second\n\n[1:02:03] third" {
		t.Fatalf("vttToText = %q", got)
	}
}

func TestChooseTrack(t *testing.T) {
	m := func(ks ...string) map[string]bool {
		out := map[string]bool{}
		for _, k := range ks {
			out[k] = true
		}
		return out
	}
	for _, tc := range []struct {
		manual, auto map[string]bool
		code         string
		isManual     bool
	}{
		{m("en"), m("en-orig"), "en", true},
		{m("en-GB", "fr"), m("en-orig"), "en-GB", true},
		{m("fr"), m("en", "en-orig", "en-de"), "en-orig", false},
		{m(), m("en", "en-de"), "en", false},
		{m(), m("en-de", "fr"), "", false}, // translation only: no
		{m("en-en-US"), m(), "", false},    // not a regional code
	} {
		code, isManual := chooseTrack([]string{"en"}, tc.manual, tc.auto)
		if code != tc.code || isManual != tc.isManual {
			t.Errorf("chooseTrack(%v, %v) = %q, %v; want %q, %v", tc.manual, tc.auto, code, isManual, tc.code, tc.isManual)
		}
	}
}

func TestYtdlpOversizeInfoJSONIsAnError(t *testing.T) {
	p, _ := fakeYtdlp(t, "bigjson")
	_, err := p.Transcript(context.Background(), "vid1", []string{"en"})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("err = %v, want output-exceeded error", err)
	}
}

func TestYtdlpOversizeCaptionFileIsAnError(t *testing.T) {
	p, _ := fakeYtdlp(t, "bigvtt")
	_, err := p.Transcript(context.Background(), "vid1", []string{"en"})
	if err == nil || !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("err = %v, want caption-file limit error", err)
	}
}

func TestYtdlpOversizeTranscriptIsTruncated(t *testing.T) {
	p, _ := fakeYtdlp(t, "bigtext")
	r, err := p.Transcript(context.Background(), "vid1", []string{"en"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != transcriptAvailable || len(r.Text) > maxTranscriptBytes || !strings.HasSuffix(r.Text, truncatedMarker) {
		t.Fatalf("status %v, text %d bytes", r.Status, len(r.Text))
	}
}

func TestTruncateTextRuneBoundary(t *testing.T) {
	in := strings.Repeat("\u00e9", 100)
	out := truncateText(in, 60)
	if len(out) > 60 || !utf8.ValidString(out) || !strings.HasSuffix(out, truncatedMarker) {
		t.Fatalf("out = %q (%d bytes)", out, len(out))
	}
	if truncateText("short", 60) != "short" {
		t.Fatal("short text changed")
	}
}

func TestTailWriterKeepsLastBytes(t *testing.T) {
	w := &tailWriter{max: 10}
	for i := 0; i < 100; i++ {
		w.Write([]byte("0123456789"))
	}
	w.Write([]byte("END"))
	if got := string(w.Bytes()); got != "3456789END" {
		t.Fatalf("tail = %q", got)
	}
}
