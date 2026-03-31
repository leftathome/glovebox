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
	"github.com/leftathome/glovebox/connector/content"
)

// rssTemplate returns a minimal RSS 2.0 feed with the given items XML fragment.
func rssTemplate(items string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0">
  <channel>
    <title>Test Feed</title>` + items + `
  </channel>
</rss>`
}

// atomTemplate returns a minimal Atom feed with the given entries XML fragment.
func atomTemplate(entries string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns="http://www.w3.org/2005/Atom">
  <title>Test Atom Feed</title>` + entries + `
</feed>`
}

// newTestConnector creates an RSSConnector wired to temp directories and a
// test HTTP server URL. Returns the connector, staging dir, and state dir.
func newTestConnector(t *testing.T, feeds []FeedConfig, fetchLinks bool, linkPolicyCfg content.LinkPolicyConfig) (*RSSConnector, string, string) {
	t.Helper()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "rss")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := make([]connector.Rule, 0, len(feeds))
	for _, f := range feeds {
		rules = append(rules, connector.Rule{
			Match:       "feed:" + f.Name,
			Destination: "test-agent",
		})
	}

	matcher := connector.NewRuleMatcher(rules)

	c := &RSSConnector{
		config: Config{
			Feeds:      feeds,
			FetchLinks: fetchLinks,
			LinkPolicy: linkPolicyCfg,
		},
		writer:       writer,
		matcher:      matcher,
		linkPolicy:   content.NewLinkPolicy(linkPolicyCfg),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
	}

	return c, stagingDir, stateDir
}

func countStagedItems(t *testing.T, stagingDir string) int {
	t.Helper()
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging dir: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			count++
		}
	}
	return count
}

func newCheckpoint(t *testing.T, stateDir string) connector.Checkpoint {
	t.Helper()
	cp, err := connector.NewCheckpoint(stateDir)
	if err != nil {
		t.Fatalf("NewCheckpoint: %v", err)
	}
	return cp
}

func TestPollRSS(t *testing.T) {
	feedXML := rssTemplate(`
    <item>
      <title>Post One</title>
      <link>https://example.com/1</link>
      <description>First post body</description>
      <guid>guid-1</guid>
      <pubDate>Mon, 01 Jan 2024 12:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Post Two</title>
      <link>https://example.com/2</link>
      <description>Second post body</description>
      <guid>guid-2</guid>
      <pubDate>Tue, 02 Jan 2024 12:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Post Three</title>
      <link>https://example.com/3</link>
      <description>Third post body</description>
      <guid>guid-3</guid>
      <pubDate>Wed, 03 Jan 2024 12:00:00 +0000</pubDate>
    </item>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(feedXML))
	}))
	defer srv.Close()

	feeds := []FeedConfig{{Name: "test", URL: srv.URL}}
	c, stagingDir, stateDir := newTestConnector(t, feeds, false, content.LinkPolicyConfig{})
	cp := newCheckpoint(t, stateDir)

	err := c.Poll(context.Background(), cp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	count := countStagedItems(t, stagingDir)
	if count != 3 {
		t.Errorf("expected 3 staged items, got %d", count)
	}

	// The XML has guid-1 first (oldest), guid-3 last (newest).
	// RSS parsing reverses to oldest-first, so processing order is:
	// guid-3, guid-2, guid-1. Checkpoint should be the last processed ID.
	lastID, ok := cp.Load("last:test")
	if !ok {
		t.Fatal("checkpoint not saved for feed 'test'")
	}
	if lastID != "guid-1" {
		t.Errorf("expected checkpoint 'guid-1', got %q", lastID)
	}
}

func TestPollAtom(t *testing.T) {
	feedXML := atomTemplate(`
    <entry>
      <title>Atom Entry 1</title>
      <id>atom-1</id>
      <link href="https://example.com/atom/1" rel="alternate"/>
      <summary>First atom entry</summary>
      <published>2024-01-01T12:00:00Z</published>
    </entry>
    <entry>
      <title>Atom Entry 2</title>
      <id>atom-2</id>
      <link href="https://example.com/atom/2" rel="alternate"/>
      <content type="html">Second atom entry content</content>
      <published>2024-01-02T12:00:00Z</published>
    </entry>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write([]byte(feedXML))
	}))
	defer srv.Close()

	feeds := []FeedConfig{{Name: "atomfeed", URL: srv.URL}}
	c, stagingDir, stateDir := newTestConnector(t, feeds, false, content.LinkPolicyConfig{})
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

func TestCheckpointSkipsDuplicates(t *testing.T) {
	feedXML := rssTemplate(`
    <item>
      <title>Post A</title>
      <link>https://example.com/a</link>
      <description>Body A</description>
      <guid>guid-a</guid>
      <pubDate>Mon, 01 Jan 2024 12:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Post B</title>
      <link>https://example.com/b</link>
      <description>Body B</description>
      <guid>guid-b</guid>
      <pubDate>Tue, 02 Jan 2024 12:00:00 +0000</pubDate>
    </item>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(feedXML))
	}))
	defer srv.Close()

	feeds := []FeedConfig{{Name: "dupetest", URL: srv.URL}}
	c, stagingDir, stateDir := newTestConnector(t, feeds, false, content.LinkPolicyConfig{})
	cp := newCheckpoint(t, stateDir)

	// First poll: process all items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	firstCount := countStagedItems(t, stagingDir)
	if firstCount != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", firstCount)
	}

	// Second poll with same feed: should produce 0 new items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	secondCount := countStagedItems(t, stagingDir)
	if secondCount != 2 {
		t.Errorf("expected still 2 items after second poll (no new), got %d", secondCount)
	}
}

func TestNewEntriesAfterCheckpoint(t *testing.T) {
	callCount := 0
	feedXMLFirst := rssTemplate(`
    <item>
      <title>Newer Item</title>
      <link>https://example.com/2</link>
      <description>Newer</description>
      <guid>guid-2</guid>
      <pubDate>Tue, 02 Jan 2024 12:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Older Item</title>
      <link>https://example.com/1</link>
      <description>Older</description>
      <guid>guid-1</guid>
      <pubDate>Mon, 01 Jan 2024 12:00:00 +0000</pubDate>
    </item>`)

	feedXMLSecond := rssTemplate(`
    <item>
      <title>Brand New</title>
      <link>https://example.com/3</link>
      <description>Brand new item</description>
      <guid>guid-3</guid>
      <pubDate>Wed, 03 Jan 2024 12:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Newer Item</title>
      <link>https://example.com/2</link>
      <description>Newer</description>
      <guid>guid-2</guid>
      <pubDate>Tue, 02 Jan 2024 12:00:00 +0000</pubDate>
    </item>
    <item>
      <title>Older Item</title>
      <link>https://example.com/1</link>
      <description>Older</description>
      <guid>guid-1</guid>
      <pubDate>Mon, 01 Jan 2024 12:00:00 +0000</pubDate>
    </item>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		if callCount == 0 {
			w.Write([]byte(feedXMLFirst))
		} else {
			w.Write([]byte(feedXMLSecond))
		}
		callCount++
	}))
	defer srv.Close()

	feeds := []FeedConfig{{Name: "incremental", URL: srv.URL}}
	c, stagingDir, stateDir := newTestConnector(t, feeds, false, content.LinkPolicyConfig{})
	cp := newCheckpoint(t, stateDir)

	// First poll: 2 items.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 2 {
		t.Fatalf("expected 2 items on first poll, got %d", got)
	}

	// Second poll: 1 new item.
	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if got := countStagedItems(t, stagingDir); got != 3 {
		t.Errorf("expected 3 items total after second poll, got %d", got)
	}
}

func TestLinkFetching(t *testing.T) {
	linkedPage := `<html><body><h1>Article Title</h1><p>Article body text here.</p></body></html>`

	mux := http.NewServeMux()
	mux.HandleFunc("/feed.xml", func(w http.ResponseWriter, r *http.Request) {
		// We need the server URL in the feed, so build it dynamically.
		// This handler will be set up after the server starts.
	})
	mux.HandleFunc("/article", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(linkedPage))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Now build the feed XML referencing the test server for linked pages.
	feedXML := rssTemplate(`
    <item>
      <title>Linked Post</title>
      <link>` + srv.URL + `/article</link>
      <description>Check the link</description>
      <guid>linked-1</guid>
      <pubDate>Mon, 01 Jan 2024 12:00:00 +0000</pubDate>
    </item>`)

	// Replace the feed handler to serve the actual feed.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/rss+xml")
			w.Write([]byte(feedXML))
		}
	})

	feeds := []FeedConfig{{Name: "linkfeed", URL: srv.URL + "/"}}
	linkCfg := content.LinkPolicyConfig{Default: "unrestricted"}
	c, stagingDir, stateDir := newTestConnector(t, feeds, true, linkCfg)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if got := countStagedItems(t, stagingDir); got != 1 {
		t.Fatalf("expected 1 staged item, got %d", got)
	}

	// Verify the staged item content includes text from the linked page.
	entries, _ := os.ReadDir(stagingDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			t.Fatalf("read content: %v", err)
		}
		text := string(data)
		if !strings.Contains(text, "Article body text here.") {
			t.Errorf("expected linked page text in content, got:\n%s", text)
		}
		if !strings.Contains(text, "Linked page") {
			t.Errorf("expected 'Linked page' separator in content, got:\n%s", text)
		}
	}
}

func TestLinkPolicyDeniesPrivateIP(t *testing.T) {
	// Feed with a link pointing to a private IP address.
	feedXML := rssTemplate(`
    <item>
      <title>Private Link</title>
      <link>http://192.168.1.1/secret</link>
      <description>Trying to reach private network</description>
      <guid>private-1</guid>
      <pubDate>Mon, 01 Jan 2024 12:00:00 +0000</pubDate>
    </item>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(feedXML))
	}))
	defer srv.Close()

	feeds := []FeedConfig{{Name: "privatefeed", URL: srv.URL}}
	// Default "safe" mode denies private IPs and non-HTTPS.
	linkCfg := content.LinkPolicyConfig{Default: "safe"}
	c, stagingDir, stateDir := newTestConnector(t, feeds, true, linkCfg)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// Item should still be staged (link fetch is best-effort).
	if got := countStagedItems(t, stagingDir); got != 1 {
		t.Fatalf("expected 1 staged item, got %d", got)
	}

	// Verify the content does NOT include linked page text (the fetch should
	// have been blocked by link policy).
	entries, _ := os.ReadDir(stagingDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			t.Fatalf("read content: %v", err)
		}
		text := string(data)
		if strings.Contains(text, "Linked page") {
			t.Error("content should NOT contain linked page text when policy denies the link")
		}
	}
}

func TestLinkPolicyAllowsConfiguredDomain(t *testing.T) {
	linkedPage := `<html><body><p>Allowed domain content.</p></body></html>`

	mux := http.NewServeMux()
	mux.HandleFunc("/article", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(linkedPage))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The test server is on 127.0.0.1 which is a private IP. In safe mode
	// it would be blocked. We add a domain rule to allow it. Since httptest
	// uses 127.0.0.1 directly, we use a network rule instead.
	feedXML := rssTemplate(`
    <item>
      <title>Allowed Link</title>
      <link>` + srv.URL + `/article</link>
      <description>This link should be fetched</description>
      <guid>allowed-1</guid>
      <pubDate>Mon, 01 Jan 2024 12:00:00 +0000</pubDate>
    </item>`)

	feedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(feedXML))
	}))
	defer feedSrv.Close()

	feeds := []FeedConfig{{Name: "allowedfeed", URL: feedSrv.URL}}
	// Allow the loopback network explicitly so the link fetch succeeds
	// even in safe mode.
	linkCfg := content.LinkPolicyConfig{
		Default: "safe",
		Rules: []content.LinkPolicyRule{
			{Match: "network:127.0.0.0/8", Allow: true},
			{Match: "scheme:http", Allow: true},
		},
	}
	c, stagingDir, stateDir := newTestConnector(t, feeds, true, linkCfg)
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if got := countStagedItems(t, stagingDir); got != 1 {
		t.Fatalf("expected 1 staged item, got %d", got)
	}

	entries, _ := os.ReadDir(stagingDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		contentPath := filepath.Join(stagingDir, e.Name(), "content.raw")
		data, err := os.ReadFile(contentPath)
		if err != nil {
			t.Fatalf("read content: %v", err)
		}
		text := string(data)
		if !strings.Contains(text, "Allowed domain content.") {
			t.Errorf("expected allowed domain content in output, got:\n%s", text)
		}
	}
}

func TestMetadataFields(t *testing.T) {
	feedXML := rssTemplate(`
    <item>
      <title>Metadata Check</title>
      <link>https://example.com/meta</link>
      <description>Testing metadata</description>
      <guid>meta-1</guid>
      <pubDate>Mon, 01 Jan 2024 12:00:00 +0000</pubDate>
    </item>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(feedXML))
	}))
	defer srv.Close()

	feeds := []FeedConfig{{Name: "metafeed", URL: srv.URL}}
	c, stagingDir, stateDir := newTestConnector(t, feeds, false, content.LinkPolicyConfig{})
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		metaPath := filepath.Join(stagingDir, e.Name(), "metadata.json")
		data, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatalf("read metadata: %v", err)
		}

		var meta map[string]interface{}
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatalf("parse metadata: %v", err)
		}

		if meta["source"] != "rss" {
			t.Errorf("expected source 'rss', got %v", meta["source"])
		}
		if meta["sender"] != "metafeed" {
			t.Errorf("expected sender 'metafeed', got %v", meta["sender"])
		}
		if meta["subject"] != "Metadata Check" {
			t.Errorf("expected subject 'Metadata Check', got %v", meta["subject"])
		}
		if meta["destination_agent"] != "test-agent" {
			t.Errorf("expected destination_agent 'test-agent', got %v", meta["destination_agent"])
		}
	}
}

func TestIdentityInMetadata(t *testing.T) {
	feedXML := rssTemplate(`
    <item>
      <title>Identity Test</title>
      <link>https://example.com/id</link>
      <description>Testing identity</description>
      <guid>id-1</guid>
      <pubDate>Mon, 01 Jan 2024 12:00:00 +0000</pubDate>
    </item>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(feedXML))
	}))
	defer srv.Close()

	feeds := []FeedConfig{{Name: "idfeed", URL: srv.URL}}
	c, stagingDir, stateDir := newTestConnector(t, feeds, false, content.LinkPolicyConfig{})
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() {
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
		if identity["provider"] != "rss" {
			t.Errorf("expected identity provider 'rss', got %v", identity["provider"])
		}
		if identity["auth_method"] != "none" {
			t.Errorf("expected identity auth_method 'none', got %v", identity["auth_method"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

func TestRuleTagsInMetadata(t *testing.T) {
	feedXML := rssTemplate(`
    <item>
      <title>Tags Test</title>
      <link>https://example.com/tags</link>
      <description>Testing rule tags</description>
      <guid>tags-1</guid>
      <pubDate>Mon, 01 Jan 2024 12:00:00 +0000</pubDate>
    </item>`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(feedXML))
	}))
	defer srv.Close()

	stagingDir := t.TempDir()
	stateDir := t.TempDir()

	writer, err := connector.NewStagingWriter(stagingDir, "rss")
	if err != nil {
		t.Fatalf("NewStagingWriter: %v", err)
	}

	rules := []connector.Rule{
		{
			Match:       "feed:tagfeed",
			Destination: "test-agent",
			Tags:        map[string]string{"category": "news", "priority": "high"},
		},
	}
	matcher := connector.NewRuleMatcher(rules)

	feeds := []FeedConfig{{Name: "tagfeed", URL: srv.URL}}
	c := &RSSConnector{
		config: Config{
			Feeds: feeds,
		},
		writer:       writer,
		matcher:      matcher,
		linkPolicy:   content.NewLinkPolicy(content.LinkPolicyConfig{}),
		httpClient:   &http.Client{Timeout: 10 * time.Second},
		fetchCounter: connector.NewFetchCounter(connector.FetchLimits{}),
	}
	cp := newCheckpoint(t, stateDir)

	if err := c.Poll(context.Background(), cp); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	entries, _ := os.ReadDir(stagingDir)
	found := false
	for _, e := range entries {
		if !e.IsDir() {
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
		if tags["category"] != "news" {
			t.Errorf("expected tag category 'news', got %v", tags["category"])
		}
		if tags["priority"] != "high" {
			t.Errorf("expected tag priority 'high', got %v", tags["priority"])
		}
	}
	if !found {
		t.Fatal("no staged items found")
	}
}

