package auth

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitConfig parameters; see spec 10 §5.3 + §9.
type RateLimitConfig struct {
	// Window is the sliding-window length over which rejections are
	// counted (default 60s).
	Window time.Duration
	// PerIPMaxRejected is the cap on rejections per source IP per window.
	PerIPMaxRejected int
	// LRUCapacity bounds the number of distinct IPs tracked at once
	// (default 1000 per spec 10 §5.3 — small enough to shrink the
	// eviction-bypass surface).
	LRUCapacity int
	// GlobalMaxRejected is the process-wide 401 backstop per window:
	// trips even when an attacker rotates synthetic IPs to flush the LRU.
	GlobalMaxRejected int
}

// RateLimiter tracks 401 attempts per spec 10 §5.3. Successful auths do NOT
// consume budget; only rejections call AllowReject. Per-IP buckets + a single
// global bucket are checked together; either tripping returns false from
// AllowReject.
//
// State is in-memory only; lost on restart. Acceptable per the spec: a process
// restart resets a brute-forcer's progress to zero, which is no worse than the
// pre-rate-limit state.
type RateLimiter struct {
	cfg RateLimitConfig

	mu     sync.Mutex
	perIP  map[string]*list.Element
	lru    *list.List
	global *rate.Limiter
}

type bucketEntry struct {
	ip      string
	limiter *rate.Limiter
}

// NewRateLimiter constructs a RateLimiter. cfg is validated lightly — a zero
// LRUCapacity is replaced by the spec default (1000). Window and the two
// MaxRejected values are NOT defaulted; callers are expected to set them
// from the Helm values block (spec 10 §9).
// NewRateLimiter panics on a zero or negative value for Window /
// PerIPMaxRejected / GlobalMaxRejected: dividing by zero in
// rate.Every would either panic deep in golang.org/x/time/rate or
// produce a useless infinite-rate limiter. Surfacing the misconfig at
// startup is friendlier than the first 401 minutes later silently
// flapping. Helm defaults populate all three (spec 10 §9), so this
// only fires for direct-Go callers that forgot to set them.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	if cfg.Window <= 0 {
		panic("auth.NewRateLimiter: Window must be > 0")
	}
	if cfg.PerIPMaxRejected <= 0 {
		panic("auth.NewRateLimiter: PerIPMaxRejected must be > 0")
	}
	if cfg.GlobalMaxRejected <= 0 {
		panic("auth.NewRateLimiter: GlobalMaxRejected must be > 0")
	}
	if cfg.LRUCapacity <= 0 {
		cfg.LRUCapacity = 1000
	}
	// golang.org/x/time/rate is a token bucket: rate.Every gives one
	// token every (Window / Max) duration; the bucket capacity is Max.
	// This approximates a fixed-window count of Max rejections per Window:
	// the bucket starts full, drains at the rate the window would refill it,
	// and once empty stays empty until the next token tick.
	return &RateLimiter{
		cfg:   cfg,
		perIP: make(map[string]*list.Element, cfg.LRUCapacity),
		lru:   list.New(),
		global: rate.NewLimiter(
			rate.Every(cfg.Window/time.Duration(cfg.GlobalMaxRejected)),
			cfg.GlobalMaxRejected),
	}
}

// AllowReject returns true if the rejection is permitted under both the
// per-IP cap and the global backstop. Returns false when either has tripped;
// the caller responds 429 with Retry-After.
//
// The global counter is consumed FIRST. If the global counter has tripped,
// the per-IP bucket is NOT updated — that's deliberate: when the process-wide
// backstop is mid-trip the per-IP bucket should NOT bleed budget that would
// then quietly reset by the time the global window slides forward.
//
// Note: golang.org/x/time/rate uses token-bucket semantics, which differs
// subtly from a strict sliding-window counter. The bucket starts full and
// the first N calls within an empty bucket all return true; the (N+1)th
// returns false until enough wall time has elapsed for a token to refill.
// This matches the spec's intent ("10 rejected attempts per 60-second
// sliding window") closely enough for v1.
func (r *RateLimiter) AllowReject(ip string) bool {
	// Global check first; do not touch per-IP state if global tripped.
	if !r.global.Allow() {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.perIP[ip]
	if !ok {
		// LRU eviction happens BEFORE we add the new entry so the
		// capacity bound is never exceeded.
		if r.lru.Len() >= r.cfg.LRUCapacity {
			old := r.lru.Back()
			if old != nil {
				delete(r.perIP, old.Value.(*bucketEntry).ip)
				r.lru.Remove(old)
			}
		}
		lim := rate.NewLimiter(
			rate.Every(r.cfg.Window/time.Duration(r.cfg.PerIPMaxRejected)),
			r.cfg.PerIPMaxRejected)
		el = r.lru.PushFront(&bucketEntry{ip: ip, limiter: lim})
		r.perIP[ip] = el
	} else {
		r.lru.MoveToFront(el)
	}
	return el.Value.(*bucketEntry).limiter.Allow()
}
