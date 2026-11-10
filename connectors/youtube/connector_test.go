package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// fakeVideo is one upload on the fake API.
type fakeVideo struct {
	ID        string
	Published string
	Views     int
	Comments  int // -1 = comments disabled (commentCount absent, commentThreads 403)
	Category  string
	Desc      string
}

// fakeAPI is an httptest YouTube Data API v3 covering the four methods the
// connector uses. It records every request for quota and shape assertions.
type fakeAPI struct {
	t      *testing.T
	mu     sync.Mutex
	videos []fakeVideo // newest first, as the uploads playlist returns them
	calls  []*http.Request
	// handles maps a forHandle value (with or without "@") to a channel id.
	handles map[string]string
	// quotaExceeded makes every call return 403 quotaExceeded.
	quotaExceeded bool
}

const testChannelID = "UCtestchannel0000000000"

func newFakeAPI(t *testing.T, videos ...fakeVideo) (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{t: t, videos: videos, handles: map[string]string{"@TestHandle": testChannelID}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAPI) setVideos(v ...fakeVideo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.videos = v
}

func (f *fakeAPI) callsTo(method string) []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*http.Request
	for _, r := range f.calls {
		if strings.HasSuffix(r.URL.Path, "/"+method) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeAPI) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

func writeAPIError(w http.ResponseWriter, status int, reason string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"code":%d,"errors":[{"reason":%q}]}}`, status, reason)
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r)
	videos := append([]fakeVideo(nil), f.videos...)
	quota := f.quotaExceeded
	f.mu.Unlock()

	if r.Header.Get("Authorization") != "" {
		f.t.Errorf("unexpected Authorization header on %s", r.URL.Path)
	}
	if r.URL.Query().Get("key") != "test-api-key" {
		f.t.Errorf("%s: key query param = %q", r.URL.Path, r.URL.Query().Get("key"))
	}
	if quota {
		writeAPIError(w, http.StatusForbidden, "quotaExceeded")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	q := r.URL.Query()

	switch {
	case strings.HasSuffix(r.URL.Path, "/channels"):
		id := q.Get("id")
		if h := q.Get("forHandle"); h != "" {
			id = f.handles[h]
		}
		if id == "" {
			fmt.Fprint(w, `{"items":[]}`)
			return
		}
		fmt.Fprintf(w, `{"items":[{"id":%q,"snippet":{"title":"Test Channel","customUrl":"@testhandle"},
			"contentDetails":{"relatedPlaylists":{"uploads":%q}}}]}`, id, "UU"+strings.TrimPrefix(id, "UC"))

	case strings.HasSuffix(r.URL.Path, "/playlistItems"):
		type cd struct {
			VideoID          string `json:"videoId"`
			VideoPublishedAt string `json:"videoPublishedAt"`
		}
		var items []map[string]cd
		for _, v := range videos {
			items = append(items, map[string]cd{"contentDetails": {v.ID, v.Published}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})

	case strings.HasSuffix(r.URL.Path, "/videos"):
		want := map[string]bool{}
		for _, id := range strings.Split(q.Get("id"), ",") {
			want[id] = true
		}
		var items []map[string]any
		for _, v := range videos {
			if !want[v.ID] {
				continue
			}
			stats := map[string]any{"viewCount": fmt.Sprint(v.Views), "likeCount": "77777"}
			if v.Comments >= 0 {
				stats["commentCount"] = fmt.Sprint(v.Comments)
			}
			cat := v.Category
			if cat == "" {
				cat = "28"
			}
			items = append(items, map[string]any{
				"id": v.ID,
				"snippet": map[string]any{
					"title": "Video " + v.ID, "description": v.Desc, "publishedAt": v.Published,
					"channelId": testChannelID, "channelTitle": "Test Channel", "categoryId": cat,
					"tags":                 []string{"tag-a", "tag-b"},
					"liveBroadcastContent": "none", "defaultAudioLanguage": "en",
					"thumbnails": map[string]any{
						"high":   map[string]any{"url": "https://i.ytimg.com/vi/" + v.ID + "/hqdefault.jpg"},
						"maxres": map[string]any{"url": "https://i.ytimg.com/vi/" + v.ID + "/maxresdefault.jpg"},
					},
				},
				"contentDetails": map[string]any{"duration": "PT10M5S"},
				"statistics":     stats,
				"topicDetails":   map[string]any{"topicCategories": []string{"https://en.wikipedia.org/wiki/Food"}},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})

	case strings.HasSuffix(r.URL.Path, "/commentThreads"):
		for _, v := range videos {
			if v.ID == q.Get("videoId") && v.Comments < 0 {
				writeAPIError(w, http.StatusForbidden, "commentsDisabled")
				return
			}
		}
		fmt.Fprint(w, `{"items":[{"snippet":{"topLevelComment":{"snippet":{"authorDisplayName":"@fan","textDisplay":"Great one"}}}}]}`)

	default:
		f.t.Errorf("unexpected API call %s (search.list and captions.list must not be used)", r.URL.Path)
		http.Error(w, "not found", http.StatusNotFound)
	}
}

var testNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// vid builds a fake upload published daysAgo before testNow.
func vid(id string, daysAgo, views, comments int) fakeVideo {
	return fakeVideo{ID: id, Published: testNow.Add(-time.Duration(daysAgo) * 24 * time.Hour).Format(time.RFC3339), Views: views, Comments: comments}
}

func newTestConnector(t *testing.T, cfg Config, apiBase string, rules []connector.Rule) (*YouTubeConnector, string, connector.Checkpoint) {
	t.Helper()
	stagingDir := t.TempDir()
	writer, err := connector.NewStagingWriter(stagingDir, "youtube")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}
	cp, err := connector.NewCheckpoint(t.TempDir())
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	if rules == nil {
		rules = []connector.Rule{{Match: "*", Destination: "media"}}
	}
	c := &YouTubeConnector{
		config:       cfg,
		writer:       writer,
		matcher:      connector.NewRuleMatcher(rules),
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tokenSource:  connector.NewStaticTokenSource("test-api-key"),
		apiBase:      apiBase,
		now:          func() time.Time { return testNow },
	}
	return c, stagingDir, cp
}

type stagedItem struct {
	Meta    map[string]any
	Content string
}

func readStaged(t *testing.T, stagingDir string) []stagedItem {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	var out []stagedItem
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(stagingDir, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}
		var meta map[string]any
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}
		content, err := os.ReadFile(filepath.Join(dir, "content.raw"))
		if err != nil {
			t.Fatalf("read content: %v", err)
		}
		out = append(out, stagedItem{meta, string(content)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Content < out[j].Content })
	return out
}

func stagedVideoIDs(t *testing.T, stagingDir string) []string {
	var ids []string
	re := regexp.MustCompile(`(?m)^- video_id: (\S+)$`)
	for _, it := range readStaged(t, stagingDir) {
		if m := re.FindStringSubmatch(it.Content); m != nil {
			ids = append(ids, m[1])
		}
	}
	sort.Strings(ids)
	return ids
}

func poll(t *testing.T, c *YouTubeConnector, cp connector.Checkpoint) {
	t.Helper()
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}
}

func TestFirstPollStagesOnlyInitialBackfill(t *testing.T) {
	_, srv := newFakeAPI(t, vid("v5", 1, 10, 1), vid("v4", 2, 10, 1), vid("v3", 3, 10, 1), vid("v2", 4, 10, 1), vid("v1", 5, 10, 1))
	c, dir, cp := newTestConnector(t, Config{Channels: []string{testChannelID}}, srv.URL, nil)
	poll(t, c, cp)
	if got := strings.Join(stagedVideoIDs(t, dir), ","); got != "v3,v4,v5" {
		t.Fatalf("first poll staged %s, want the 3 newest (v3,v4,v5)", got)
	}
}

func TestCheckpointStagesOnlyNewUploads(t *testing.T) {
	f, srv := newFakeAPI(t, vid("v2", 2, 10, 1), vid("v1", 3, 10, 1))
	c, dir, cp := newTestConnector(t, Config{Channels: []string{testChannelID}}, srv.URL, nil)
	poll(t, c, cp)

	f.setVideos(vid("v3", 1, 10, 1), vid("v2", 2, 10, 1), vid("v1", 3, 10, 1))
	f.resetCalls()
	poll(t, c, cp)
	if got := strings.Join(stagedVideoIDs(t, dir), ","); got != "v1,v2,v3" {
		t.Fatalf("staged %s, want v1,v2,v3 with no duplicates", got)
	}
	if n := len(f.callsTo("channels")); n != 0 {
		t.Errorf("second poll called channels.list %d times; resolution must be cached", n)
	}

	// Nothing new: exactly one call (playlistItems), no videos.list.
	f.resetCalls()
	poll(t, c, cp)
	if n := len(f.calls); n != 1 || len(f.callsTo("playlistItems")) != 1 {
		t.Errorf("idle poll made %d calls; want exactly 1 playlistItems.list", n)
	}
}

func TestMaxNewPerPollKeepsNewest(t *testing.T) {
	f, srv := newFakeAPI(t, vid("v0", 10, 10, 1))
	c, dir, cp := newTestConnector(t, Config{Channels: []string{testChannelID}, MaxNewPerPoll: 2}, srv.URL, nil)
	poll(t, c, cp)
	f.setVideos(vid("v4", 1, 10, 1), vid("v3", 2, 10, 1), vid("v2", 3, 10, 1), vid("v0", 10, 10, 1))
	poll(t, c, cp)
	if got := strings.Join(stagedVideoIDs(t, dir), ","); got != "v0,v3,v4" {
		t.Fatalf("staged %s, want v0 then the 2 newest (v3,v4)", got)
	}
}

func TestVideosListIsOneBatchedCallWithAllParts(t *testing.T) {
	f, srv := newFakeAPI(t, vid("v3", 1, 10, 1), vid("v2", 2, 10, 1), vid("v1", 3, 10, 1))
	c, _, cp := newTestConnector(t, Config{Channels: []string{testChannelID}}, srv.URL, nil)
	poll(t, c, cp)

	calls := f.callsTo("videos")
	if len(calls) != 1 {
		t.Fatalf("videos.list called %d times, want 1 batched call", len(calls))
	}
	q := calls[0].URL.Query()
	if got := q.Get("id"); got != "v3,v2,v1" {
		t.Errorf("videos.list id = %q, want comma-batched v3,v2,v1", got)
	}
	for _, part := range []string{"snippet", "contentDetails", "topicDetails"} {
		if !strings.Contains(q.Get("part"), part) {
			t.Errorf("videos.list part %q missing %s", q.Get("part"), part)
		}
	}
	// Engagement is assessed 30 days after first sight, so first sight
	// does not even request statistics.
	if strings.Contains(q.Get("part"), "statistics") {
		t.Errorf("videos.list part %q requests statistics at first sight", q.Get("part"))
	}
}

func TestHandleResolvesViaForHandle(t *testing.T) {
	f, srv := newFakeAPI(t, vid("v1", 1, 10, 1))
	rules := []connector.Rule{{Match: "channel:@TestHandle", Destination: "main"}}
	c, dir, cp := newTestConnector(t, Config{Channels: []string{"@TestHandle"}}, srv.URL, rules)
	poll(t, c, cp)

	calls := f.callsTo("channels")
	if len(calls) != 1 || calls[0].URL.Query().Get("forHandle") != "@TestHandle" {
		t.Fatalf("expected one channels.list?forHandle=@TestHandle, got %d calls", len(calls))
	}
	if got := f.callsTo("playlistItems")[0].URL.Query().Get("playlistId"); got != "UUtestchannel0000000000" {
		t.Errorf("playlistId = %q, want the resolved uploads playlist", got)
	}
	items := readStaged(t, dir)
	if len(items) != 1 {
		t.Fatalf("staged %d items, want 1", len(items))
	}
	if items[0].Meta["destination_agent"] != "main" {
		t.Errorf("destination_agent = %v, want main (rule keyed on the handle)", items[0].Meta["destination_agent"])
	}
	if items[0].Meta["sender"] != testChannelID {
		t.Errorf("sender = %v, want the resolved channel id", items[0].Meta["sender"])
	}
}

func TestUnknownHandleSkipsChannelWithoutFailingPoll(t *testing.T) {
	_, srv := newFakeAPI(t, vid("v1", 1, 10, 1))
	c, dir, cp := newTestConnector(t, Config{Channels: []string{"@nobody", testChannelID}}, srv.URL, nil)
	poll(t, c, cp)
	if got := len(readStaged(t, dir)); got != 1 {
		t.Fatalf("staged %d, want 1 (bad handle skipped, good channel polled)", got)
	}
}

func TestStagedItemContentAndMetadata(t *testing.T) {
	v := vid("v1", 3, 5000, 40)
	v.Desc = "About the dish.\n0:00 Intro\n1:15 Prep\n6:30 Plating"
	_, srv := newFakeAPI(t, v)
	c, dir, cp := newTestConnector(t, Config{Channels: []string{testChannelID}}, srv.URL,
		[]connector.Rule{{Match: "*", Destination: "media", Tags: map[string]string{"source_type": "video"}}})
	poll(t, c, cp)

	items := readStaged(t, dir)
	if len(items) != 1 {
		t.Fatalf("staged %d items, want 1", len(items))
	}
	it := items[0]
	for k, want := range map[string]any{
		"source": "youtube", "content_type": "text/markdown", "destination_agent": "media", "subject": "Video v1",
	} {
		if it.Meta[k] != want {
			t.Errorf("metadata %s = %v, want %v", k, it.Meta[k], want)
		}
	}
	if it.Meta["tier"] != "feed" && it.Meta["tier"] != nil {
		t.Errorf("tier = %v", it.Meta["tier"])
	}
	id, _ := it.Meta["identity"].(map[string]any)
	if id["provider"] != "youtube" || id["auth_method"] != "api_key" {
		t.Errorf("identity = %v", id)
	}
	if tags, _ := it.Meta["tags"].(map[string]any); tags["source_type"] != "video" {
		t.Errorf("rule tags missing: %v", it.Meta["tags"])
	}

	for _, want := range []string{
		"- watch_url: https://www.youtube.com/watch?v=v1",
		"- thumbnail_url: https://i.ytimg.com/vi/v1/maxresdefault.jpg",
		"- duration: 10:05",
		"- tags: tag-a, tag-b",
		"- topics: Food",
		"- channel: Test Channel (@testhandle, " + testChannelID + ")",
		"- 1:15 Prep (offset_seconds: 75)",
		"- @fan: Great one",
		"- transcript: unavailable (no transcript provider configured)",
		"About the dish.",
	} {
		if !strings.Contains(it.Content, want) {
			t.Errorf("content missing %q\n---\n%s", want, it.Content)
		}
	}
}

// Raw statistics must never be staged (Developer Policy III.E.4,
// glovebox-8wu1). The fake API returns distinctive counts; none may appear.
func TestRawCountsAreNeverStaged(t *testing.T) {
	var vs []fakeVideo
	for i := 0; i < 8; i++ {
		vs = append(vs, vid(fmt.Sprintf("v%d", i), i+1, 912345+i, 4321+i))
	}
	_, srv := newFakeAPI(t, vs...)
	c, dir, cp := newTestConnector(t, Config{Channels: []string{testChannelID}}, srv.URL, nil)
	poll(t, c, cp)

	items := readStaged(t, dir)
	if len(items) == 0 {
		t.Fatal("nothing staged")
	}
	for _, it := range items {
		for _, raw := range []string{"91234", "432", "77777"} {
			if strings.Contains(it.Content, raw) {
				t.Errorf("raw statistic %q leaked into staged content:\n%s", raw, it.Content)
			}
		}
		for _, want := range []string{
			"- viewership: pending\n",
			"- comment_activity: pending\n",
			"- engagement_note: assessed 30 days after first seen, on or after 2026-12-07\n",
		} {
			if !strings.Contains(it.Content, want) {
				t.Errorf("content missing %q:\n%s", want, it.Content)
			}
		}
	}
	// The first-seen date the 30-day assessment is scheduled from.
	for _, id := range []string{"v0", "v1", "v2"} {
		if got, ok := cp.Load(firstSeenKey(id)); !ok || got != testNow.Format(time.RFC3339) {
			t.Errorf("first-seen for %s = %q, %v; want %s", id, got, ok, testNow.Format(time.RFC3339))
		}
	}
}

func TestCommentsDisabledIsNotAnError(t *testing.T) {
	_, srv := newFakeAPI(t, vid("v1", 1, 10, -1))
	c, dir, cp := newTestConnector(t, Config{Channels: []string{testChannelID}}, srv.URL, nil)
	poll(t, c, cp)
	items := readStaged(t, dir)
	if len(items) != 1 || strings.Contains(items[0].Content, "## Top comments") {
		t.Fatalf("want 1 item with no comments section, got %d", len(items))
	}
}

func TestFetchCommentsFalseSkipsCommentThreads(t *testing.T) {
	f, srv := newFakeAPI(t, vid("v1", 1, 10, 1))
	no := false
	c, _, cp := newTestConnector(t, Config{Channels: []string{testChannelID}, FetchComments: &no}, srv.URL, nil)
	poll(t, c, cp)
	if n := len(f.callsTo("commentThreads")); n != 0 {
		t.Fatalf("commentThreads called %d times with fetch_comments=false", n)
	}
}

type stubTranscripts struct{ calls int }

func (s *stubTranscripts) Transcript(_ context.Context, id string, langs []string) (transcriptResult, error) {
	s.calls++
	return transcriptResult{Status: transcriptAvailable, Language: langs[0], Text: "transcript of " + id}, nil
}

func TestTranscriptProviderUsedAndMusicSkipped(t *testing.T) {
	music := vid("m1", 2, 10, 1)
	music.Category = "10"
	_, srv := newFakeAPI(t, vid("v1", 1, 10, 1), music)
	c, dir, cp := newTestConnector(t, Config{Channels: []string{testChannelID}}, srv.URL, nil)
	stub := &stubTranscripts{}
	c.transcripts = stub
	poll(t, c, cp)

	if stub.calls != 1 {
		t.Errorf("provider called %d times, want 1 (music video skipped)", stub.calls)
	}
	var sawMusic, sawText bool
	for _, it := range readStaged(t, dir) {
		if strings.Contains(it.Content, "- transcript: skipped (category 10)") {
			sawMusic = true
		}
		if strings.Contains(it.Content, "## Transcript\n\ntranscript of v1") {
			sawText = true
		}
	}
	if !sawMusic || !sawText {
		t.Fatalf("music skipped marker=%v, transcript text=%v", sawMusic, sawText)
	}
}

func TestQuotaExceededStopsThePoll(t *testing.T) {
	f, srv := newFakeAPI(t, vid("v1", 1, 10, 1))
	f.quotaExceeded = true
	c, _, cp := newTestConnector(t, Config{Channels: []string{testChannelID, "UCsecond00000000000000"}}, srv.URL, nil)
	poll(t, c, cp)
	if n := len(f.calls); n != 1 {
		t.Fatalf("made %d calls after quotaExceeded, want 1 (stop for this poll)", n)
	}
}

func TestNoRuleSkipsChannelWithoutSpendingQuota(t *testing.T) {
	f, srv := newFakeAPI(t, vid("v1", 1, 10, 1))
	rules := []connector.Rule{{Match: "channel:@other", Destination: "media"}}
	c, dir, cp := newTestConnector(t, Config{Channels: []string{testChannelID}}, srv.URL, rules)
	poll(t, c, cp)
	if len(f.calls) != 0 || len(readStaged(t, dir)) != 0 {
		t.Fatalf("unrouted channel made %d calls", len(f.calls))
	}
}

func TestConfigDefaults(t *testing.T) {
	var cfg Config
	if d, err := cfg.EffectivePollInterval(); err != nil || d != time.Hour {
		t.Errorf("default poll interval = %v, %v; want 1h", d, err)
	}
	cfg.PollInterval = "30m"
	if d, _ := cfg.EffectivePollInterval(); d != 30*time.Minute {
		t.Errorf("poll interval 30m parsed as %v", d)
	}
	cfg.PollInterval = "1m"
	if _, err := cfg.EffectivePollInterval(); err == nil {
		t.Error("poll interval below 5m minimum accepted")
	}
	cfg.PollInterval = "soon"
	if _, err := cfg.EffectivePollInterval(); err == nil {
		t.Error("unparseable poll interval accepted")
	}
	cfg = Config{Channels: []string{"@a", "UCb"}, ChannelIDs: []string{"UCb", "UCc"}}
	if got := strings.Join(cfg.AllChannels(), ","); got != "@a,UCb,UCc" {
		t.Errorf("AllChannels = %s", got)
	}
	if _, err := newTranscriptProvider(TranscriptConfig{Provider: "whisper"}); err == nil {
		t.Error("unknown transcript provider accepted")
	}
	if got := (TranscriptConfig{}).EffectiveSkipCategories(); len(got) != 1 || got[0] != "10" {
		t.Errorf("default skip categories = %v, want [10]", got)
	}
}

// The shipped sample config must route to a destination in the chart's
// default agentAllowlist (glovebox-07il) and parse cleanly.
func TestSampleConfig(t *testing.T) {
	raw, err := os.ReadFile("config.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse config.json: %v", err)
	}
	if len(cfg.AllChannels()) == 0 {
		t.Error("sample config has no channels")
	}
	for _, r := range cfg.Rules {
		if r.Destination != "media" {
			t.Errorf("sample rule destination %q, want media (in the chart's default allowlist)", r.Destination)
		}
	}
	if _, err := cfg.EffectivePollInterval(); err != nil {
		t.Error(err)
	}
}
