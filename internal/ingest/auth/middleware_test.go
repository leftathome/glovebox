// Tests for the auth.Middleware HTTP wrapper (spec 10 §5).
//
// Coverage:
//
//   TestMiddleware_NoAuthHeader_401
//   TestMiddleware_NonBearer_401
//   TestMiddleware_BearerWrongLength_401
//   TestMiddleware_BearerNonHex_401
//   TestMiddleware_UnknownToken_401
//   TestMiddleware_HappyPath_200_AndDeliveredBy
//   TestMiddleware_RateLimitTrips_429
//   TestMiddleware_XForwardedFor_HonoredFromTrustedProxy
//   TestMiddleware_XForwardedFor_IgnoredFromUntrusted
//   TestMiddleware_TelemetryCounters

package auth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/ingest"
)

// fakeTel is a minimal TelemetryHook implementation that counts calls.
type fakeTel struct {
	mu                sync.Mutex
	authAccepted      int
	authRejected      int
	authRateLimited   int
	rejectedByIP      int
	lastEndpoint      string
	lastIPBucket      string
	lastRejectStatus  string
}

func (f *fakeTel) RecordAuth(_ context.Context, endpoint, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastEndpoint = endpoint
	switch status {
	case "accepted":
		f.authAccepted++
	case "rejected":
		f.authRejected++
		f.lastRejectStatus = status
	case "rate_limited":
		f.authRateLimited++
		f.lastRejectStatus = status
	}
}

func (f *fakeTel) RecordAuthRejectedByIP(_ context.Context, ipBucket string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejectedByIP++
	f.lastIPBucket = ipBucket
}

func newMiddlewareTestRig(t *testing.T) (*TokenStore, *RateLimiter, *ProxyResolver, *fakeTel) {
	t.Helper()
	// One canonical token.
	canonicalHex := "11" + strings.Repeat("00", 31)
	store := mustNewTokenStore(t, map[string]string{
		"alpha": canonicalHex,
	})
	rl := NewRateLimiter(RateLimitConfig{
		Window:            time.Minute,
		PerIPMaxRejected:  3,
		LRUCapacity:       100,
		GlobalMaxRejected: 1000,
	})
	pr := &ProxyResolver{} // no trusted CIDRs by default
	tel := &fakeTel{}
	return store, rl, pr, tel
}

// makeNext returns an http.Handler that records the captured delivered_by
// for happy-path assertions, plus a status counter.
func makeNext() (http.Handler, *string, *int) {
	var capturedSourceID string
	var calls int
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		sid, _ := ingest.DeliveredBy(r.Context())
		capturedSourceID = sid
		w.WriteHeader(http.StatusOK)
	})
	return h, &capturedSourceID, &calls
}

func TestMiddleware_NoAuthHeader_401(t *testing.T) {
	store, rl, pr, tel := newMiddlewareTestRig(t)
	next, _, calls := makeNext()
	mw := Middleware(store, rl, pr, tel)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
	req.RemoteAddr = "203.0.113.1:443"
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("missing WWW-Authenticate header")
	}
	if *calls != 0 {
		t.Errorf("inner handler called %d times, want 0", *calls)
	}
	if tel.authRejected != 1 || tel.rejectedByIP != 1 {
		t.Errorf("telemetry counters = (%d, %d), want (1, 1)", tel.authRejected, tel.rejectedByIP)
	}
}

func TestMiddleware_NonBearer_401(t *testing.T) {
	store, rl, pr, tel := newMiddlewareTestRig(t)
	next, _, _ := makeNext()
	mw := Middleware(store, rl, pr, tel)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	req.RemoteAddr = "203.0.113.1:443"
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_BearerWrongLength_401(t *testing.T) {
	store, rl, pr, tel := newMiddlewareTestRig(t)
	next, _, _ := makeNext()
	mw := Middleware(store, rl, pr, tel)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
	req.Header.Set("Authorization", "Bearer abc123")
	req.RemoteAddr = "203.0.113.1:443"
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if tel.authRejected != 1 {
		t.Errorf("authRejected = %d, want 1", tel.authRejected)
	}
}

func TestMiddleware_BearerNonHex_401(t *testing.T) {
	store, rl, pr, tel := newMiddlewareTestRig(t)
	next, _, _ := makeNext()
	mw := Middleware(store, rl, pr, tel)(next)

	// 64-char string but contains uppercase / non-hex.
	req := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("Z", 64))
	req.RemoteAddr = "203.0.113.1:443"
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestMiddleware_UnknownToken_401(t *testing.T) {
	store, rl, pr, tel := newMiddlewareTestRig(t)
	next, _, _ := makeNext()
	mw := Middleware(store, rl, pr, tel)(next)

	// 64 hex chars, but not in the store.
	req := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("ab", 32))
	req.RemoteAddr = "203.0.113.1:443"
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if tel.authRejected != 1 {
		t.Errorf("authRejected = %d, want 1", tel.authRejected)
	}
}

func TestMiddleware_HappyPath_200_AndDeliveredBy(t *testing.T) {
	store, rl, pr, tel := newMiddlewareTestRig(t)
	next, captured, calls := makeNext()
	mw := Middleware(store, rl, pr, tel)(next)

	canonicalHex := "11" + strings.Repeat("00", 31)
	req := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
	req.Header.Set("Authorization", "Bearer "+canonicalHex)
	req.RemoteAddr = "203.0.113.1:443"
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if *calls != 1 {
		t.Errorf("inner calls = %d, want 1", *calls)
	}
	if *captured != "alpha" {
		t.Errorf("delivered_by = %q, want \"alpha\"", *captured)
	}
	if tel.authAccepted != 1 {
		t.Errorf("authAccepted = %d, want 1", tel.authAccepted)
	}
	if tel.lastEndpoint != "/v1/archives" {
		t.Errorf("endpoint label = %q, want /v1/archives", tel.lastEndpoint)
	}
}

func TestMiddleware_RateLimitTrips_429(t *testing.T) {
	canonicalHex := "11" + strings.Repeat("00", 31)
	store := mustNewTokenStore(t, map[string]string{"alpha": canonicalHex})
	rl := NewRateLimiter(RateLimitConfig{
		Window:            time.Minute,
		PerIPMaxRejected:  2,
		LRUCapacity:       100,
		GlobalMaxRejected: 1000,
	})
	pr := &ProxyResolver{}
	tel := &fakeTel{}

	next, _, _ := makeNext()
	mw := Middleware(store, rl, pr, tel)(next)

	// Fire 2 bad-auth rejections from the same IP, both should 401.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
		req.RemoteAddr = "198.51.100.5:443"
		req.Header.Set("Authorization", "Bearer "+strings.Repeat("ab", 32))
		rec := httptest.NewRecorder()
		mw.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("attempt %d: status = %d, want 401", i+1, rec.Code)
		}
	}
	// Third should 429.
	req := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
	req.RemoteAddr = "198.51.100.5:443"
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("ab", 32))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("3rd attempt status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "60" {
		t.Errorf("Retry-After = %q, want 60", rec.Header().Get("Retry-After"))
	}
	if tel.authRateLimited != 1 {
		t.Errorf("authRateLimited = %d, want 1", tel.authRateLimited)
	}
}

func TestMiddleware_XForwardedFor_HonoredFromTrustedProxy(t *testing.T) {
	store, rl, _, tel := newMiddlewareTestRig(t)
	// Trust the immediate peer's network.
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	pr := &ProxyResolver{TrustedCIDRs: []*net.IPNet{cidr}}

	next, _, _ := makeNext()
	mw := Middleware(store, rl, pr, tel)(next)

	// Bad token but XFF says the actual client is 198.51.100.99. Since
	// the peer is in the trusted CIDR, the rate-limit key should be the
	// XFF value, NOT the peer.
	req := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
	req.RemoteAddr = "10.0.0.5:33333"
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("ab", 32))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// The recorded ipBucket should reflect the XFF address: 198.51.100.0/24.
	if !strings.HasPrefix(tel.lastIPBucket, "198.51.100.") {
		t.Errorf("ipBucket = %q, want 198.51.100.0/24 prefix", tel.lastIPBucket)
	}
}

func TestMiddleware_XForwardedFor_IgnoredFromUntrusted(t *testing.T) {
	store, rl, _, tel := newMiddlewareTestRig(t)
	pr := &ProxyResolver{} // no trusted CIDRs

	next, _, _ := makeNext()
	mw := Middleware(store, rl, pr, tel)(next)

	req := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
	req.RemoteAddr = "203.0.113.10:33333"
	req.Header.Set("X-Forwarded-For", "198.51.100.99")
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("ab", 32))
	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	// The recorded ipBucket should reflect the PEER address, not XFF.
	if !strings.HasPrefix(tel.lastIPBucket, "203.0.113.") {
		t.Errorf("ipBucket = %q, want 203.0.113.0/24 prefix (XFF should be ignored)", tel.lastIPBucket)
	}
}

func TestMiddleware_TelemetryCounters(t *testing.T) {
	canonicalHex := "11" + strings.Repeat("00", 31)
	store := mustNewTokenStore(t, map[string]string{"alpha": canonicalHex})
	rl := NewRateLimiter(RateLimitConfig{
		Window:            time.Minute,
		PerIPMaxRejected:  100,
		LRUCapacity:       100,
		GlobalMaxRejected: 1000,
	})
	pr := &ProxyResolver{}
	tel := &fakeTel{}
	next, _, _ := makeNext()
	mw := Middleware(store, rl, pr, tel)(next)

	// 1 accept.
	r1 := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
	r1.Header.Set("Authorization", "Bearer "+canonicalHex)
	r1.RemoteAddr = "203.0.113.1:443"
	mw.ServeHTTP(httptest.NewRecorder(), r1)

	// 2 rejects.
	for i := 0; i < 2; i++ {
		r2 := httptest.NewRequest(http.MethodGet, "/v1/archives", nil)
		r2.Header.Set("Authorization", "Bearer "+strings.Repeat("ab", 32))
		r2.RemoteAddr = "203.0.113.1:443"
		mw.ServeHTTP(httptest.NewRecorder(), r2)
	}

	if tel.authAccepted != 1 {
		t.Errorf("authAccepted = %d, want 1", tel.authAccepted)
	}
	if tel.authRejected != 2 {
		t.Errorf("authRejected = %d, want 2", tel.authRejected)
	}
	if tel.rejectedByIP != 2 {
		t.Errorf("rejectedByIP = %d, want 2", tel.rejectedByIP)
	}
}
