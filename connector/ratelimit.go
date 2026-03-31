package connector

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const maxRateLimitWait = 5 * time.Minute

// RateLimiter tracks API rate limit state from HTTP response headers
// and gates outgoing requests to stay within limits.
type RateLimiter struct {
	remaining int
	resetAt   time.Time
	retryAt   time.Time
	mu        sync.Mutex
	hasInfo   bool
}

// NewRateLimiter creates a RateLimiter with no rate limit state.
// Until Update is called with a response containing rate limit headers,
// Wait returns immediately.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{}
}

// Update reads rate limit headers from an HTTP response and updates
// internal state. Call after every API request.
//
// Supported headers:
//   - X-RateLimit-Remaining / X-RateLimit-Reset (unix timestamp) -- GitHub, GitLab, X
//   - RateLimit-Remaining / RateLimit-Reset (seconds until reset) -- IETF draft, LinkedIn
//   - Retry-After (seconds, on 429 responses)
//
// X-RateLimit-* takes precedence over RateLimit-* when both are present.
// Computed wait times are capped at maxRateLimitWait.
func (rl *RateLimiter) Update(resp *http.Response) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Handle Retry-After on 429 responses.
	if resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
				wait := time.Duration(secs) * time.Second
				if wait > maxRateLimitWait {
					log.Printf("ratelimit: Retry-After %ds exceeds cap, truncating to %v", secs, maxRateLimitWait)
					wait = maxRateLimitWait
				}
				rl.retryAt = time.Now().Add(wait)
				rl.hasInfo = true
			}
		}
	}

	// X-RateLimit-* headers (unix timestamp format) take precedence.
	if xRemaining := resp.Header.Get("X-Ratelimit-Remaining"); xRemaining != "" {
		if rem, err := strconv.Atoi(xRemaining); err == nil {
			rl.remaining = rem
			rl.hasInfo = true
		}
		if xReset := resp.Header.Get("X-Ratelimit-Reset"); xReset != "" {
			if ts, err := strconv.ParseInt(xReset, 10, 64); err == nil {
				resetTime := time.Unix(ts, 0)
				wait := time.Until(resetTime)
				if wait > maxRateLimitWait {
					log.Printf("ratelimit: X-RateLimit-Reset wait %v exceeds cap, truncating to %v", wait, maxRateLimitWait)
					resetTime = time.Now().Add(maxRateLimitWait)
				}
				rl.resetAt = resetTime
			}
		}
		return
	}

	// IETF draft RateLimit-* headers (seconds-until-reset format).
	if ietfRemaining := resp.Header.Get("Ratelimit-Remaining"); ietfRemaining != "" {
		if rem, err := strconv.Atoi(ietfRemaining); err == nil {
			rl.remaining = rem
			rl.hasInfo = true
		}
		if ietfReset := resp.Header.Get("Ratelimit-Reset"); ietfReset != "" {
			if secs, err := strconv.Atoi(ietfReset); err == nil {
				wait := time.Duration(secs) * time.Second
				if wait > maxRateLimitWait {
					log.Printf("ratelimit: RateLimit-Reset wait %v exceeds cap, truncating to %v", wait, maxRateLimitWait)
					wait = maxRateLimitWait
				}
				rl.resetAt = time.Now().Add(wait)
			}
		}
	}
}

// Wait blocks until the rate limit allows another request. Returns
// immediately if not rate-limited. Respects context cancellation.
//
// Behavior:
//   - If remaining > 0 or no rate limit info: return immediately
//   - If a Retry-After was received: sleep until retryAt (capped)
//   - If remaining == 0: sleep until resetAt (capped)
//   - Pre-emptive: remaining < 10 adds 100ms delay
//   - Context cancellation during sleep returns ctx.Err()
func (rl *RateLimiter) Wait(ctx context.Context) error {
	rl.mu.Lock()
	hasInfo := rl.hasInfo
	remaining := rl.remaining
	resetAt := rl.resetAt
	retryAt := rl.retryAt
	rl.mu.Unlock()

	if !hasInfo {
		return nil
	}

	now := time.Now()

	// Retry-After takes highest priority.
	if retryAt.After(now) {
		return sleepUntil(ctx, retryAt)
	}

	// If remaining is zero, wait until reset.
	if remaining == 0 && resetAt.After(now) {
		return sleepUntil(ctx, resetAt)
	}

	// Pre-emptive slowdown when getting close to the limit.
	if remaining > 0 && remaining < 10 {
		return sleepFor(ctx, 100*time.Millisecond)
	}

	return nil
}

// sleepUntil blocks until the target time or context cancellation.
func sleepUntil(ctx context.Context, target time.Time) error {
	d := time.Until(target)
	if d <= 0 {
		return nil
	}
	return sleepFor(ctx, d)
}

// sleepFor blocks for the given duration or until context cancellation.
func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
