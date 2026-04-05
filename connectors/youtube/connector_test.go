package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// newTestConnector creates a YouTubeConnector wired to temp directories and a
// test HTTP server base URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, channelIDs []string, apiBase string, rules []connector.Rule) (*YouTubeConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "youtube")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &YouTubeConnector{
		config: Config{
			ChannelIDs: channelIDs,
		},
		writer:       writer,
		matcher:      matcher,
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		tokenSource:  connector.NewStaticTokenSource("test-api-key"),
		apiBase:      apiBase,
	}

	return c, stagingDir, stateDir
}

func newCheckpoint(t *testing.T, stateDir string) connector.Checkpoint {
	t.Helper()
	cp, err := connector.NewCheckpoint(stateDir)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	return cp
}

func countStagedItems(t *testing.T, stagingDir string) int {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			count++
		}
	}
	return count
}

// makeSearchResponse builds a YouTube search API JSON response.
func makeSearchResponse(videoIDs ...string) []byte {
	type id struct {
		VideoID string `json:"videoId"`
	}
	type item struct {
		ID id `json:"id"`
	}
	type resp struct {
		Items []item `json:"items"`
	}
	r := resp{}
	for _, vid := range videoIDs {
		r.Items = append(r.Items, item{ID: id{VideoID: vid}})
	}
	data, _ := json.Marshal(r)
	return data
}

// makeVideoResponse builds a YouTube videos API JSON response.
func makeVideoResponse(videoID, title, description, publishedAt, channelTitle string) []byte {
	type snippet struct {
		Title        string `json:"title"`
		Description  string `json:"description"`
		PublishedAt  string `json:"publishedAt"`
		ChannelTitle string `json:"channelTitle"`
	}
	type item struct {
		ID      string  `json:"id"`
		Snippet snippet `json:"snippet"`
	}
	type resp struct {
		Items []item `json:"items"`
	}
	r := resp{
		Items: []item{
			{
				ID: videoID,
				Snippet: snippet{
					Title:        title,
					Description:  description,
					PublishedAt:  publishedAt,
					ChannelTitle: channelTitle,
				},
			},
		},
	}
	data, _ := json.Marshal(r)
	return data
}

func TestPollFetchesVideosAndStages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		path := r.URL.Path
		if strings.HasSuffix(path, "/search") {
			w.Write(makeSearchResponse("vid1", "vid2"))
			return
		}
		if strings.HasSuffix(path, "/videos") {
			videoID := r.URL.Query().Get("id")
			w.Write(makeVideoResponse(
				videoID,
				"Test Video "+videoID,
				"Description for "+videoID,
				"2026-03-28T12:00:00Z",
				"Test Channel",
			))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	channelIDs := []string{"UC_test_channel"}
	rules := []connector.Rule{
		{Match: "channel:UC_test_channel", Destination: "media-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channelIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 2 {
		t.Errorf("expected 2 staged items, got %d", count)
	}
}

func TestCheckpointPreventsDuplicates(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		path := r.URL.Path
		if strings.HasSuffix(path, "/search") {
			if callCount == 0 {
				w.Write(makeSearchResponse("vid1", "vid2"))
			} else {
				// Second poll: same videos plus one new one.
				w.Write(makeSearchResponse("vid3", "vid1", "vid2"))
			}
			callCount++
			return
		}
		if strings.HasSuffix(path, "/videos") {
			videoID := r.URL.Query().Get("id")
			// vid1 published earlier, vid2 later, vid3 latest.
			pubTime := "2026-03-28T10:00:00Z"
			switch videoID {
			case "vid2":
				pubTime = "2026-03-28T11:00:00Z"
			case "vid3":
				pubTime = "2026-03-28T12:00:00Z"
			}
			w.Write(makeVideoResponse(
				videoID,
				"Video "+videoID,
				"Desc "+videoID,
				pubTime,
				"Test Channel",
			))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	channelIDs := []string{"UC_test_channel"}
	rules := []connector.Rule{
		{Match: "channel:UC_test_channel", Destination: "media-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channelIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	// First poll: should stage 2 videos.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", got)
	}

	// Second poll: should stage only 1 new video (vid3).
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 3 {
		t.Errorf("expected 3 items total after second poll, got %d", got)
	}
}

func TestAPIKeyInQueryParam(t *testing.T) {
	var capturedKeys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedKeys = append(capturedKeys, r.URL.Query().Get("key"))

		// Verify no Authorization header is set.
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected no Authorization header, got %q", auth)
		}

		w.Header().Set("Content-Type", "application/json")

		path := r.URL.Path
		if strings.HasSuffix(path, "/search") {
			w.Write(makeSearchResponse("vid1"))
			return
		}
		if strings.HasSuffix(path, "/videos") {
			videoID := r.URL.Query().Get("id")
			w.Write(makeVideoResponse(videoID, "Title", "Desc", "2026-03-28T12:00:00Z", "Chan"))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	channelIDs := []string{"UC_test_channel"}
	rules := []connector.Rule{
		{Match: "channel:UC_test_channel", Destination: "media-agent"},
	}
	c, _, stateDir := newTestConnector(t, channelIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Should have made at least 2 requests (1 search + 1 video detail).
	if len(capturedKeys) < 2 {
		t.Fatalf("expected at least 2 API calls, got %d", len(capturedKeys))
	}

	for i, key := range capturedKeys {
		if key != "test-api-key" {
			t.Errorf("request %d: expected key=test-api-key in query, got %q", i, key)
		}
	}
}

func TestIdentityFieldsInMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if strings.HasSuffix(path, "/search") {
			w.Write(makeSearchResponse("vid1"))
			return
		}
		if strings.HasSuffix(path, "/videos") {
			videoID := r.URL.Query().Get("id")
			w.Write(makeVideoResponse(videoID, "Title", "Desc", "2026-03-28T12:00:00Z", "Chan"))
			return
		}
	}))
	defer srv.Close()

	channelIDs := []string{"UC_test_channel"}
	rules := []connector.Rule{
		{Match: "channel:UC_test_channel", Destination: "media-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channelIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		found = true
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}

		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}

		identity, ok := meta["identity"].(map[string]interface{})
		if !ok {
			t.Fatal("expected identity object in metadata")
		}
		if identity["provider"] != "youtube" {
			t.Errorf("expected identity provider 'youtube', got %v", identity["provider"])
		}
		if identity["auth_method"] != "api_key" {
			t.Errorf("expected identity auth_method 'api_key', got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

// readFirstStagedContent reads the content.raw file from the first staged item
// and unmarshals it into a videoContent struct.
func readFirstStagedContent(t *testing.T, stagingDir string) videoContent {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			t.Fatalf("read content.raw: %v", err)
		}
		var vc videoContent
		if err := json.Unmarshal(data, &vc); err != nil {
			t.Fatalf("unmarshal videoContent: %v", err)
		}
		return vc
	}
	t.Fatal("no staged items found")
	return videoContent{}
}

// makeCommentThreadsResponse builds a YouTube commentThreads API JSON response.
func makeCommentThreadsResponse(comments ...string) []byte {
	type commentSnippet struct {
		TextDisplay string `json:"textDisplay"`
	}
	type topLevelComment struct {
		Snippet commentSnippet `json:"snippet"`
	}
	type threadSnippet struct {
		TopLevelComment topLevelComment `json:"topLevelComment"`
	}
	type threadItem struct {
		Snippet threadSnippet `json:"snippet"`
	}
	type resp struct {
		Items []threadItem `json:"items"`
	}
	r := resp{}
	for _, c := range comments {
		r.Items = append(r.Items, threadItem{
			Snippet: threadSnippet{
				TopLevelComment: topLevelComment{
					Snippet: commentSnippet{TextDisplay: c},
				},
			},
		})
	}
	data, _ := json.Marshal(r)
	return data
}

// makeCaptionsResponse builds a YouTube captions API JSON response.
func makeCaptionsResponse(languages ...string) []byte {
	type captionSnippet struct {
		Language string `json:"language"`
		Name     string `json:"name"`
	}
	type captionItem struct {
		Snippet captionSnippet `json:"snippet"`
	}
	type resp struct {
		Items []captionItem `json:"items"`
	}
	r := resp{}
	for _, lang := range languages {
		r.Items = append(r.Items, captionItem{
			Snippet: captionSnippet{Language: lang, Name: lang + " auto"},
		})
	}
	data, _ := json.Marshal(r)
	return data
}

func TestCommentsFetchedAndIncluded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		if strings.HasSuffix(path, "/search") {
			w.Write(makeSearchResponse("vid1"))
			return
		}
		if strings.HasSuffix(path, "/videos") {
			videoID := r.URL.Query().Get("id")
			w.Write(makeVideoResponse(videoID, "Title", "Desc", "2026-03-28T12:00:00Z", "Chan"))
			return
		}
		if strings.HasSuffix(path, "/commentThreads") {
			w.Write(makeCommentThreadsResponse("Great video!", "Very helpful", "Thanks for sharing"))
			return
		}
		if strings.HasSuffix(path, "/captions") {
			w.Write(makeCaptionsResponse())
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	channelIDs := []string{"UC_test_channel"}
	rules := []connector.Rule{
		{Match: "channel:UC_test_channel", Destination: "media-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channelIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	vc := readFirstStagedContent(t, stagingDir)
	if len(vc.Comments) != 3 {
		t.Fatalf("expected 3 comments, got %d", len(vc.Comments))
	}
	if vc.Comments[0] != "Great video!" {
		t.Errorf("expected first comment 'Great video!', got %q", vc.Comments[0])
	}
	if vc.Comments[1] != "Very helpful" {
		t.Errorf("expected second comment 'Very helpful', got %q", vc.Comments[1])
	}
}

func TestCaptionsMetadataIncluded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		if strings.HasSuffix(path, "/search") {
			w.Write(makeSearchResponse("vid1"))
			return
		}
		if strings.HasSuffix(path, "/videos") {
			videoID := r.URL.Query().Get("id")
			w.Write(makeVideoResponse(videoID, "Title", "Desc", "2026-03-28T12:00:00Z", "Chan"))
			return
		}
		if strings.HasSuffix(path, "/commentThreads") {
			w.Write(makeCommentThreadsResponse())
			return
		}
		if strings.HasSuffix(path, "/captions") {
			w.Write(makeCaptionsResponse("en", "es", "fr"))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	channelIDs := []string{"UC_test_channel"}
	rules := []connector.Rule{
		{Match: "channel:UC_test_channel", Destination: "media-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channelIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	vc := readFirstStagedContent(t, stagingDir)
	if len(vc.CaptionLanguages) != 3 {
		t.Fatalf("expected 3 caption languages, got %d", len(vc.CaptionLanguages))
	}
	if vc.CaptionLanguages[0] != "en" {
		t.Errorf("expected first caption language 'en', got %q", vc.CaptionLanguages[0])
	}
	if vc.CaptionLanguages[1] != "es" {
		t.Errorf("expected second caption language 'es', got %q", vc.CaptionLanguages[1])
	}
}

func TestVideoWithoutCommentsHandledGracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		if strings.HasSuffix(path, "/search") {
			w.Write(makeSearchResponse("vid1"))
			return
		}
		if strings.HasSuffix(path, "/videos") {
			videoID := r.URL.Query().Get("id")
			w.Write(makeVideoResponse(videoID, "Title", "Desc", "2026-03-28T12:00:00Z", "Chan"))
			return
		}
		if strings.HasSuffix(path, "/commentThreads") {
			// Empty items -- no comments on this video.
			w.Write([]byte(`{"items":[]}`))
			return
		}
		if strings.HasSuffix(path, "/captions") {
			w.Write(makeCaptionsResponse("en"))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	channelIDs := []string{"UC_test_channel"}
	rules := []connector.Rule{
		{Match: "channel:UC_test_channel", Destination: "media-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channelIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 1 {
		t.Fatalf("expected 1 staged item, got %d", count)
	}

	vc := readFirstStagedContent(t, stagingDir)
	if vc.Comments != nil && len(vc.Comments) != 0 {
		t.Errorf("expected no comments, got %d", len(vc.Comments))
	}
}

func TestVideoWithoutCaptionsHandledGracefully(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		if strings.HasSuffix(path, "/search") {
			w.Write(makeSearchResponse("vid1"))
			return
		}
		if strings.HasSuffix(path, "/videos") {
			videoID := r.URL.Query().Get("id")
			w.Write(makeVideoResponse(videoID, "Title", "Desc", "2026-03-28T12:00:00Z", "Chan"))
			return
		}
		if strings.HasSuffix(path, "/commentThreads") {
			w.Write(makeCommentThreadsResponse("A comment"))
			return
		}
		if strings.HasSuffix(path, "/captions") {
			// Empty items -- no captions on this video.
			w.Write([]byte(`{"items":[]}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	channelIDs := []string{"UC_test_channel"}
	rules := []connector.Rule{
		{Match: "channel:UC_test_channel", Destination: "media-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channelIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 1 {
		t.Fatalf("expected 1 staged item, got %d", count)
	}

	vc := readFirstStagedContent(t, stagingDir)
	if vc.CaptionLanguages != nil && len(vc.CaptionLanguages) != 0 {
		t.Errorf("expected no captions, got %d", len(vc.CaptionLanguages))
	}
	// Comments should still be present.
	if len(vc.Comments) != 1 {
		t.Errorf("expected 1 comment, got %d", len(vc.Comments))
	}
}

func TestFetchCommentsFalseSkipsComments(t *testing.T) {
	commentEndpointCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path

		if strings.HasSuffix(path, "/search") {
			w.Write(makeSearchResponse("vid1"))
			return
		}
		if strings.HasSuffix(path, "/videos") {
			videoID := r.URL.Query().Get("id")
			w.Write(makeVideoResponse(videoID, "Title", "Desc", "2026-03-28T12:00:00Z", "Chan"))
			return
		}
		if strings.HasSuffix(path, "/commentThreads") {
			commentEndpointCalled = true
			w.Write(makeCommentThreadsResponse("should not appear"))
			return
		}
		if strings.HasSuffix(path, "/captions") {
			w.Write(makeCaptionsResponse())
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	channelIDs := []string{"UC_test_channel"}
	rules := []connector.Rule{
		{Match: "channel:UC_test_channel", Destination: "media-agent"},
	}
	c, stagingDir, stateDir := newTestConnector(t, channelIDs, srv.URL, rules)
	c.config.FetchComments = boolPtr(false)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if commentEndpointCalled {
		t.Error("commentThreads endpoint should not have been called when FetchComments is false")
	}

	vc := readFirstStagedContent(t, stagingDir)
	if vc.Comments != nil {
		t.Errorf("expected nil comments when fetch disabled, got %v", vc.Comments)
	}
}

func boolPtr(b bool) *bool { return &b }

func TestRuleTagsInMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := r.URL.Path
		if strings.HasSuffix(path, "/search") {
			w.Write(makeSearchResponse("vid1"))
			return
		}
		if strings.HasSuffix(path, "/videos") {
			videoID := r.URL.Query().Get("id")
			w.Write(makeVideoResponse(videoID, "Title", "Desc", "2026-03-28T12:00:00Z", "Chan"))
			return
		}
	}))
	defer srv.Close()

	channelIDs := []string{"UC_test_channel"}
	rules := []connector.Rule{
		{
			Match:       "channel:UC_test_channel",
			Destination: "media-agent",
			Tags:        map[string]string{"source_type": "video", "priority": "low"},
		},
	}
	c, stagingDir, stateDir := newTestConnector(t, channelIDs, srv.URL, rules)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		found = true
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}

		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}

		tags, ok := meta["tags"].(map[string]interface{})
		if !ok {
			t.Fatal("expected tags object in metadata")
		}
		if tags["source_type"] != "video" {
			t.Errorf("expected tag source_type 'video', got %v", tags["source_type"])
		}
		if tags["priority"] != "low" {
			t.Errorf("expected tag priority 'low', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}
