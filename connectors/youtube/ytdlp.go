package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ytdlpProvider fetches English captions with yt-dlp, subtitles ONLY
// (glovebox-9zmo). --skip-download means no video or audio stream is ever
// requested or written; the operator green-lit yt-dlp on exactly that
// condition (2026-11-07).
//
// This is outside the official Data API (captions.download is owner-only).
// It is an operator decision, recorded in glovebox-iwqm / glovebox-9zmo.
//
// Two yt-dlp runs per video, both --skip-download: the first lists the
// caption tracks (info JSON), the second writes exactly the one chosen,
// reusing that info (--load-info-json) so the video is extracted once.
// Creator-uploaded tracks are preferred because they are cleaner. The chosen VTT is reduced to plain text with a [m:ss] marker
// roughly every minute so a reader can line it up with the chapters.
type ytdlpProvider struct {
	bin     string        // yt-dlp executable
	timeout time.Duration // per video
}

func (p ytdlpProvider) Transcript(ctx context.Context, videoID string, langs []string) (transcriptResult, error) {
	dir, err := os.MkdirTemp("", "yt-subs-")
	if err != nil {
		return transcriptResult{}, fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	// Step 1: metadata only. Lists which caption tracks exist.
	info, err := p.run(ctx, dir, "--skip-download", "--no-playlist", "--dump-single-json", "--", watchURL(videoID))
	if err != nil {
		return transcriptResult{}, err
	}
	infoPath := filepath.Join(dir, "info.json")
	if err := os.WriteFile(infoPath, info, 0o600); err != nil {
		return transcriptResult{}, fmt.Errorf("write info: %w", err)
	}
	manual, auto := trackKinds(info)
	code, isManual := chooseTrack(langs, manual, auto)
	if code == "" {
		return transcriptResult{Status: transcriptUnavailable, Reason: "no English captions on this video"}, nil
	}

	// Step 2: exactly one caption track, from the info already fetched (no
	// second extraction). A broad --sub-langs pattern would also pull every
	// machine-translated track ("en-de", "en-en-US", ...) and trip YouTube's
	// rate limit.
	which := "--write-auto-subs"
	kind := "automatic captions"
	if isManual {
		which, kind = "--write-subs", "creator captions"
	}
	if _, err := p.run(ctx, dir,
		"--load-info-json", infoPath,
		"--skip-download", which,
		"--sub-langs", "^"+regexp.QuoteMeta(code)+"$",
		"--sub-format", "vtt",
		"--paths", dir, "-o", "%(id)s.%(ext)s",
	); err != nil {
		return transcriptResult{}, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, videoID+"."+code+".vtt"))
	if err != nil {
		return transcriptResult{}, fmt.Errorf("read captions: %w", err)
	}
	text := vttToText(string(raw))
	if strings.TrimSpace(text) == "" {
		return transcriptResult{Status: transcriptUnavailable, Reason: "caption track was empty"}, nil
	}
	return transcriptResult{Status: transcriptAvailable, Language: code + ", " + kind, Text: text}, nil
}

// run executes yt-dlp in dir and returns its stdout.
func (p ytdlpProvider) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	base := []string{"--no-progress", "--quiet", "--no-warnings"}
	cmd := exec.CommandContext(ctx, p.bin, append(base, args...)...)
	cmd.Dir = dir
	// yt-dlp may leave children holding the output pipe; kill the whole
	// group on timeout and stop waiting soon after.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("yt-dlp timed out after %s", p.timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("yt-dlp: %w: %s", err, lastLine([]byte(stderr.String())))
	}
	return out, nil
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) == 0 {
		return ""
	}
	l := lines[len(lines)-1]
	if len(l) > 300 {
		l = l[:300]
	}
	return l
}

// trackKinds returns the caption language codes yt-dlp's info JSON lists as
// creator-uploaded ("subtitles") and automatic ("automatic_captions").
func trackKinds(info []byte) (manual, auto map[string]bool) {
	manual, auto = map[string]bool{}, map[string]bool{}
	var v struct {
		Subtitles         map[string]json.RawMessage `json:"subtitles"`
		AutomaticCaptions map[string]json.RawMessage `json:"automatic_captions"`
	}
	if json.Unmarshal(info, &v) != nil {
		return
	}
	for k := range v.Subtitles {
		if k != "live_chat" {
			manual[k] = true
		}
	}
	for k := range v.AutomaticCaptions {
		auto[k] = true
	}
	return
}

// chooseTrack picks ONE track: a creator track in a preferred language
// (exact, then regional such as en-US), else the automatic original-language
// track ("<lang>-orig"), else the automatic "<lang>" track. Automatic tracks
// are never matched by prefix: "en-xx" there is a machine translation.
func chooseTrack(langs []string, manual, auto map[string]bool) (code string, isManual bool) {
	sorted := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	for _, want := range langs {
		if manual[want] {
			return want, true
		}
		for _, c := range sorted(manual) {
			if strings.HasPrefix(c, want+"-") && len(c) <= len(want)+3 {
				return c, true
			}
		}
	}
	for _, want := range langs {
		if auto[want+"-orig"] {
			return want + "-orig", false
		}
		if auto[want] {
			return want, false
		}
	}
	return "", false
}

var (
	vttCue     = regexp.MustCompile(`^(\d{1,2}:)?(\d{2}):(\d{2})\.\d{3}\s+-->`)
	vttTag     = regexp.MustCompile(`<[^>]*>`)
	vttHeaders = []string{"WEBVTT", "Kind:", "Language:", "NOTE", "STYLE", "REGION"}
)

// markerGap is the spacing, in seconds, of the [m:ss] markers in the text.
const markerGap = 60

// vttToText reduces a WebVTT track to readable text. Automatic captions roll:
// each cue repeats the previous line before adding new words, so a line
// identical to the one just emitted is dropped.
func vttToText(vtt string) string {
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(vtt))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	lastMarker := -markerGap
	cueSec := 0
	prev := ""
	lineLen := 0
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" {
			continue
		}
		if m := vttCue.FindStringSubmatch(ln); m != nil {
			h := 0
			if m[1] != "" {
				fmt.Sscanf(strings.TrimSuffix(m[1], ":"), "%d", &h)
			}
			var mm, ss int
			fmt.Sscanf(m[2], "%d", &mm)
			fmt.Sscanf(m[3], "%d", &ss)
			cueSec = h*3600 + mm*60 + ss
			continue
		}
		skip := false
		for _, h := range vttHeaders {
			if strings.HasPrefix(ln, h) {
				skip = true
				break
			}
		}
		if skip || isCueNumber(ln) {
			continue
		}
		text := strings.TrimSpace(htmlUnescape(vttTag.ReplaceAllString(ln, "")))
		if text == "" || text == prev {
			continue
		}
		prev = text
		if cueSec-lastMarker >= markerGap {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			fmt.Fprintf(&b, "[%s] ", formatOffset(cueSec))
			lastMarker = cueSec
			lineLen = 0
		} else if lineLen > 0 {
			b.WriteString(" ")
		}
		b.WriteString(text)
		lineLen += len(text)
	}
	return b.String()
}

func isCueNumber(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func htmlUnescape(s string) string {
	r := strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&nbsp;", " ")
	return r.Replace(s)
}
