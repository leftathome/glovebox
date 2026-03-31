package connector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// helper: build a robots.txt body from lines.
func robotsTxt(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestRobotsAllowedPath(t *testing.T) {
	body := robotsTxt(
		"User-agent: GloveboxBot",
		"Disallow: /secret/",
		"Allow: /public/",
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rc := NewRobotsChecker(srv.Client())
	ctx := context.Background()

	if !rc.Allowed(ctx, srv.URL+"/public/page.html") {
		t.Error("expected /public/page.html to be allowed")
	}
}

func TestRobotsDisallowedPath(t *testing.T) {
	body := robotsTxt(
		"User-agent: GloveboxBot",
		"Disallow: /secret/",
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rc := NewRobotsChecker(srv.Client())
	ctx := context.Background()

	if rc.Allowed(ctx, srv.URL+"/secret/stuff") {
		t.Error("expected /secret/stuff to be disallowed")
	}
}

func TestRobotsMissing404Allows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rc := NewRobotsChecker(srv.Client())
	ctx := context.Background()

	if !rc.Allowed(ctx, srv.URL+"/anything") {
		t.Error("expected allow when robots.txt returns 404")
	}
}

func TestRobotsServerError500Denies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	rc := NewRobotsChecker(srv.Client())
	ctx := context.Background()

	if rc.Allowed(ctx, srv.URL+"/anything") {
		t.Error("expected deny when robots.txt returns 500")
	}
}

func TestRobotsCacheHit(t *testing.T) {
	fetchCount := 0
	body := robotsTxt(
		"User-agent: GloveboxBot",
		"Allow: /",
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fetchCount++
			fmt.Fprint(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rc := NewRobotsChecker(srv.Client())
	ctx := context.Background()

	rc.Allowed(ctx, srv.URL+"/a")
	rc.Allowed(ctx, srv.URL+"/b")

	if fetchCount != 1 {
		t.Errorf("expected 1 fetch of robots.txt, got %d", fetchCount)
	}
}

func TestRobotsResetClearsCache(t *testing.T) {
	fetchCount := 0
	body := robotsTxt(
		"User-agent: GloveboxBot",
		"Allow: /",
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fetchCount++
			fmt.Fprint(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rc := NewRobotsChecker(srv.Client())
	ctx := context.Background()

	rc.Allowed(ctx, srv.URL+"/a")
	rc.Reset()
	rc.Allowed(ctx, srv.URL+"/b")

	if fetchCount != 2 {
		t.Errorf("expected 2 fetches after Reset, got %d", fetchCount)
	}
}

func TestRobotsCrawlDelay(t *testing.T) {
	body := robotsTxt(
		"User-agent: GloveboxBot",
		"Crawl-delay: 5",
		"Allow: /",
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rc := NewRobotsChecker(srv.Client())
	ctx := context.Background()

	rc.Allowed(ctx, srv.URL+"/page")

	rc.mu.Lock()
	origin := originFromURL(srv.URL + "/page")
	rules, ok := rc.cache[origin]
	rc.mu.Unlock()

	if !ok {
		t.Fatal("expected cache entry for origin")
	}
	if rules.crawlDelay != 5 {
		t.Errorf("expected crawl-delay 5, got %d", rules.crawlDelay)
	}
}

func TestRobotsWildcardUserAgent(t *testing.T) {
	body := robotsTxt(
		"User-agent: *",
		"Disallow: /admin/",
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rc := NewRobotsChecker(srv.Client())
	ctx := context.Background()

	if rc.Allowed(ctx, srv.URL+"/admin/settings") {
		t.Error("expected /admin/settings disallowed by wildcard rule")
	}
	if !rc.Allowed(ctx, srv.URL+"/public/page") {
		t.Error("expected /public/page allowed (not matched by wildcard disallow)")
	}
}

func TestRobotsGloveboxBotOverridesWildcard(t *testing.T) {
	// GloveboxBot-specific rules should take precedence over wildcard.
	body := robotsTxt(
		"User-agent: *",
		"Disallow: /",
		"",
		"User-agent: GloveboxBot",
		"Allow: /",
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			fmt.Fprint(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	rc := NewRobotsChecker(srv.Client())
	ctx := context.Background()

	if !rc.Allowed(ctx, srv.URL+"/anything") {
		t.Error("expected GloveboxBot-specific Allow to override wildcard Disallow")
	}
}

func TestRobotsLRUEviction(t *testing.T) {
	// Create a checker with maxCache=3 to test eviction.
	rc := &RobotsChecker{
		client:    http.DefaultClient,
		userAgent: "GloveboxBot",
		cache:     make(map[string]*robotsRules),
		maxCache:  3,
	}

	// Manually populate cache with 3 entries and access order.
	rc.cache["http://a.example.com"] = &robotsRules{accessOrder: 1}
	rc.cache["http://b.example.com"] = &robotsRules{accessOrder: 2}
	rc.cache["http://c.example.com"] = &robotsRules{accessOrder: 3}
	rc.nextOrder = 4

	// Insert a 4th entry -- should evict the oldest (a.example.com).
	rc.evictIfNeeded()
	rc.cache["http://d.example.com"] = &robotsRules{accessOrder: rc.nextOrder}
	rc.nextOrder++

	if _, ok := rc.cache["http://a.example.com"]; ok {
		t.Error("expected http://a.example.com to be evicted")
	}
	if len(rc.cache) != 3 {
		t.Errorf("expected cache size 3, got %d", len(rc.cache))
	}
}

func TestRobotsRedirectSameOriginFollowed(t *testing.T) {
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	redirectCount := 0
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		redirectCount++
		http.Redirect(w, r, srv.URL+"/actual-robots.txt", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/actual-robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, robotsTxt(
			"User-agent: GloveboxBot",
			"Disallow: /nope/",
		))
	})

	// Use a client that does NOT auto-follow redirects so we can test
	// that the checker's own redirect logic works.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// But the test server's client has the right transport, so just use it
	// with the default redirect policy.
	_ = client
	rc := NewRobotsChecker(srv.Client())
	ctx := context.Background()

	if rc.Allowed(ctx, srv.URL+"/nope/secret") {
		t.Error("expected /nope/secret to be disallowed after redirect")
	}
	if !rc.Allowed(ctx, srv.URL+"/ok/page") {
		t.Error("expected /ok/page to be allowed after redirect")
	}
}
