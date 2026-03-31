package connector

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// robotsDirective is a single Allow or Disallow rule from robots.txt.
type robotsDirective struct {
	allow bool   // true = Allow, false = Disallow
	path  string // path prefix
}

// robotsRules holds the parsed robots.txt data for a single origin.
type robotsRules struct {
	directives  []robotsDirective // from the matched user-agent section
	crawlDelay  int               // seconds, 0 = not specified
	denyAll     bool              // true when robots.txt could not be fetched (5xx, timeout)
	accessOrder int64             // for LRU eviction
}

// RobotsChecker checks robots.txt rules for the GloveboxBot user-agent.
// It caches parsed rules per origin and supports LRU eviction.
type RobotsChecker struct {
	client    *http.Client
	userAgent string
	cache     map[string]*robotsRules // origin -> rules
	mu        sync.Mutex
	maxCache  int
	nextOrder int64
}

// NewRobotsChecker returns a RobotsChecker that uses the given HTTP client
// to fetch robots.txt files. The client should already have appropriate
// timeouts configured.
func NewRobotsChecker(client *http.Client) *RobotsChecker {
	return &RobotsChecker{
		client:    client,
		userAgent: "GloveboxBot",
		cache:     make(map[string]*robotsRules),
		maxCache:  100,
	}
}

// Allowed checks if GloveboxBot may fetch the given URL per robots.txt.
// It fetches and caches robots.txt per origin. On cache miss, it fetches
// the robots.txt from the target origin.
//
// Failure policy:
//   - 4xx response: allow (standard convention)
//   - 5xx response or fetch error: deny (conservative)
func (rc *RobotsChecker) Allowed(ctx context.Context, targetURL string) bool {
	origin := originFromURL(targetURL)
	if origin == "" {
		return false
	}

	rc.mu.Lock()
	rules, ok := rc.cache[origin]
	if ok {
		rules.accessOrder = rc.nextOrder
		rc.nextOrder++
		rc.mu.Unlock()
		return rc.checkRules(rules, targetURL)
	}
	rc.mu.Unlock()

	// Cache miss -- fetch robots.txt.
	rules = rc.fetchRobots(ctx, origin)

	rc.mu.Lock()
	rc.evictIfNeeded()
	rules.accessOrder = rc.nextOrder
	rc.nextOrder++
	rc.cache[origin] = rules
	rc.mu.Unlock()

	return rc.checkRules(rules, targetURL)
}

// Reset clears the cache. Call between poll cycles.
func (rc *RobotsChecker) Reset() {
	rc.mu.Lock()
	rc.cache = make(map[string]*robotsRules)
	rc.nextOrder = 0
	rc.mu.Unlock()
}

// CrawlDelay returns the crawl-delay for the given origin, or 0 if unknown.
func (rc *RobotsChecker) CrawlDelay(origin string) int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rules, ok := rc.cache[origin]; ok {
		return rules.crawlDelay
	}
	return 0
}

// originFromURL extracts the scheme + host (origin) from a URL string.
// Only http and https schemes are allowed to prevent SSRF.
func originFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	if u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// fetchRobots fetches and parses robots.txt for the given origin.
// Follows up to 3 redirects, same origin only.
func (rc *RobotsChecker) fetchRobots(ctx context.Context, origin string) *robotsRules {
	robotsURL := origin + "/robots.txt"

	// Use a client that does not auto-follow redirects so we can enforce
	// same-origin and hop limits ourselves.
	noRedirectClient := *rc.client
	noRedirectClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	const maxRedirects = 3
	for i := 0; i <= maxRedirects; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL, nil)
		if err != nil {
			return &robotsRules{denyAll: true}
		}
		req.Header.Set("User-Agent", DefaultUserAgent)

		resp, err := noRedirectClient.Do(req)
		if err != nil {
			return &robotsRules{denyAll: true}
		}

		// Handle redirects.
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			resp.Body.Close()
			loc := resp.Header.Get("Location")
			if loc == "" {
				return &robotsRules{denyAll: true}
			}
			redirectURL, err := url.Parse(loc)
			if err != nil {
				return &robotsRules{denyAll: true}
			}
			// Resolve relative redirects.
			base, _ := url.Parse(robotsURL)
			resolved := base.ResolveReference(redirectURL)
			// Same-origin check.
			if resolved.Scheme+"://"+resolved.Host != origin {
				return &robotsRules{denyAll: true}
			}
			robotsURL = resolved.String()
			continue
		}

		// 4xx = allow (no robots.txt or forbidden).
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			resp.Body.Close()
			return &robotsRules{}
		}

		// 5xx = deny.
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			return &robotsRules{denyAll: true}
		}

		// 2xx = parse the body (capped at 512KB to prevent OOM).
		const maxRobotsSize = 512 << 10
		limited := io.LimitReader(resp.Body, maxRobotsSize)
		rules := parseRobotsTxt(limited, rc.userAgent)
		resp.Body.Close()
		return rules
	}

	// Exhausted redirect hops.
	return &robotsRules{denyAll: true}
}

// parseRobotsTxt reads a robots.txt body and extracts rules for the given
// bot user-agent. It looks for a section matching the bot name exactly,
// falling back to the wildcard (*) section.
func parseRobotsTxt(r io.Reader, botName string) *robotsRules {
	scanner := bufio.NewScanner(r)

	type section struct {
		directives []robotsDirective
		crawlDelay int
	}

	var (
		botSection      *section
		wildcardSection *section
		current         *section
		inBotSection    bool
		inWildcard      bool
	)

	lowerBot := strings.ToLower(botName)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Strip comments.
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}

		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		keyLower := strings.ToLower(key)

		if keyLower == "user-agent" {
			valueLower := strings.ToLower(value)

			if valueLower == lowerBot {
				if botSection == nil {
					botSection = &section{}
				}
				current = botSection
				inBotSection = true
				inWildcard = false
			} else if valueLower == "*" {
				if wildcardSection == nil {
					wildcardSection = &section{}
				}
				current = wildcardSection
				inBotSection = false
				inWildcard = true
			} else {
				current = nil
				inBotSection = false
				inWildcard = false
			}
			continue
		}

		// Only process directives if we are in a relevant section.
		if current == nil {
			continue
		}

		// Once we hit a non-user-agent directive, we are in the body of
		// the current section. If a new User-agent line appeared above
		// that did not match, current would already be nil.

		switch keyLower {
		case "disallow":
			current.directives = append(current.directives, robotsDirective{
				allow: false,
				path:  value,
			})
		case "allow":
			current.directives = append(current.directives, robotsDirective{
				allow: true,
				path:  value,
			})
		case "crawl-delay":
			if delay, err := strconv.Atoi(value); err == nil && delay > 0 {
				current.crawlDelay = delay
			}
		}

		// If a user-agent line for a different agent appears after
		// directives, we should stop adding to this section. But per the
		// standard, a new user-agent line starts a new group, which is
		// handled above by resetting current.
		_ = inBotSection
		_ = inWildcard
	}

	// Prefer bot-specific section over wildcard.
	chosen := botSection
	if chosen == nil {
		chosen = wildcardSection
	}
	if chosen == nil {
		return &robotsRules{}
	}

	return &robotsRules{
		directives: chosen.directives,
		crawlDelay: chosen.crawlDelay,
	}
}

// checkRules evaluates the parsed rules against the target URL path.
func (rc *RobotsChecker) checkRules(rules *robotsRules, targetURL string) bool {
	if rules.denyAll {
		return false
	}

	u, err := url.Parse(targetURL)
	if err != nil {
		return false
	}
	path := u.Path
	if path == "" {
		path = "/"
	}

	// Find the most specific matching directive (longest path prefix).
	bestLen := -1
	allowed := true

	for _, d := range rules.directives {
		if d.path == "" {
			// Empty Disallow means allow all.
			if !d.allow {
				continue
			}
		}
		if strings.HasPrefix(path, d.path) {
			if len(d.path) > bestLen {
				bestLen = len(d.path)
				allowed = d.allow
			}
		}
	}

	// No matching directive means allowed.
	if bestLen < 0 {
		return true
	}

	return allowed
}

// evictIfNeeded removes the least-recently-used cache entry if the cache
// is at capacity. Must be called with rc.mu held.
func (rc *RobotsChecker) evictIfNeeded() {
	if len(rc.cache) < rc.maxCache {
		return
	}

	var oldestKey string
	var oldestOrder int64 = -1

	for k, v := range rc.cache {
		if oldestOrder < 0 || v.accessOrder < oldestOrder {
			oldestOrder = v.accessOrder
			oldestKey = k
		}
	}

	if oldestKey != "" {
		delete(rc.cache, oldestKey)
	}
}

// String returns a human-readable summary, useful for debugging.
func (rc *RobotsChecker) String() string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return fmt.Sprintf("RobotsChecker{cached=%d, maxCache=%d}", len(rc.cache), rc.maxCache)
}
