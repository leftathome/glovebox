// Package auth -- HTTP middleware (spec 10 §5).
//
// Middleware ties together the three Wave A primitives (TokenStore,
// RateLimiter, ProxyResolver) and a B4 Telemetry hook into a single
// http.Handler wrapper that the C3 server wire-up applies to the
// /v1/archives route surface. The combined behavior:
//
//   - Bad / missing / unknown Authorization header  -> 401 (or 429 if
//     rate-limited).
//   - Per-IP or global rate-limit trip on rejected attempt -> 429 +
//     Retry-After per spec §5.3.
//   - Accepted token -> ctx.delivered_by set, request forwarded to the
//     inner handler, audit-friendly log emitted per spec §6.1.
//
// The middleware is intentionally NOT exported as a method on TokenStore
// or RateLimiter because tying the three primitives + telemetry into
// one wrapper is most naturally written at wire-up time. See the C3
// plan §Task C3 "Note on middleware".
package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/leftathome/glovebox/internal/ingest"
)

// TelemetryHook is the minimal interface Middleware needs from the
// archive-delivery Telemetry. Declared here (rather than depending on
// internal/ingest/archives directly) to keep the auth package free of
// the archives import. *archives.Telemetry satisfies this interface
// automatically because its existing method set matches.
type TelemetryHook interface {
	RecordAuth(ctx context.Context, endpoint, status string)
	RecordAuthRejectedByIP(ctx context.Context, ipBucket string)
}

// authBearerPrefix is the canonical "Bearer " prefix on the
// Authorization header. The space is significant.
const authBearerPrefix = "Bearer "

// authTokenHexLen is the exact length of a valid token hex string
// (32 bytes encoded as 64 lowercase hex chars).
const authTokenHexLen = 64

// Middleware returns an http.Handler wrapper that performs spec 10 §5
// bearer-token authentication. The wrapper:
//
//  1. Resolves the client IP via the ProxyResolver (X-Forwarded-For
//     honored only from trusted CIDRs).
//  2. Parses the Authorization header. On any format failure, runs the
//     onReject path (rate-limit check + 401 or 429).
//  3. Looks the token up in the TokenStore via constant-time compare.
//     On miss, runs the onReject path.
//  4. On accept: records the auth-accepted metric, logs the §6.1 event,
//     attaches delivered_by to the request context via
//     ingest.WithDeliveredBy, and forwards to the wrapped handler.
//
// rl, pr, and tel may NOT be nil; the caller MUST construct all three
// (the production wire-up does, and tests pass minimal fakes).
//
// store may be empty (zero tokens loaded) -- every request then falls
// through to onReject which 401s; this is the spec-mandated behavior
// when the Vault load returns no entries.
func Middleware(store *TokenStore, rl *RateLimiter, pr *ProxyResolver, tel TelemetryHook) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			clientIP := pr.ResolveClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"))
			ipBucket := BucketIP(clientIP)

			// Validate Authorization header shape.
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, authBearerPrefix) {
				onReject(w, r, rl, tel, clientIP, ipBucket)
				return
			}
			hex := auth[len(authBearerPrefix):]
			if len(hex) != authTokenHexLen {
				onReject(w, r, rl, tel, clientIP, ipBucket)
				return
			}

			// Decode + lookup. Both failures map to the same opaque 401
			// per spec §5.1 / §5.2 (no distinguishing format-bad vs
			// unknown-token via response shape).
			tokBytes, ok := decodeToken(hex)
			if !ok {
				onReject(w, r, rl, tel, clientIP, ipBucket)
				return
			}
			sourceID, ok := store.Lookup(tokBytes)
			if !ok {
				onReject(w, r, rl, tel, clientIP, ipBucket)
				return
			}

			// Accept: record audit-log + telemetry, attach delivered_by
			// to the context, forward to the inner handler. The log
			// shape matches spec 10 §6.1 (EventIngestAuthOK).
			tel.RecordAuth(r.Context(), endpointLabel(r.URL.Path), "accepted")
			slog.Info("glovebox ingest authenticated",
				"source_id", sourceID,
				"remote_addr", clientIP,
				"endpoint", r.Method+" "+r.URL.Path)
			ctx := ingest.WithDeliveredBy(r.Context(), sourceID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// endpointLabel buckets a request path into one of the low-cardinality
// labels expected by glovebox_ingest_auth_total{endpoint=...}. Paths
// under /v1/archives* collapse to "/v1/archives"; /v1/ingest stays
// itself; anything else lands in "other" to bound the cardinality.
func endpointLabel(p string) string {
	switch {
	case strings.HasPrefix(p, "/v1/archives"):
		return "/v1/archives"
	case p == "/v1/ingest":
		return "/v1/ingest"
	default:
		return "other"
	}
}

// onReject is the shared 401/429 emitter. The rate-limit check fires
// FIRST: a tripped limiter means we don't even spend the budget on a
// 401 body, because the client gets 429 instead. Per spec §5.3 the
// 429 response includes Retry-After: 60 (a coarse advisory; the real
// recovery time depends on the sliding window's drain rate).
//
// On 401, the body is the opaque "Bearer" challenge per RFC 6750 §3:
// the WWW-Authenticate header carries `Bearer error="invalid_token"`,
// and the body is a one-line text string. Spec §5.1 does not require
// JSON; plain text is sufficient and easier to parse for curl-based
// debugging.
func onReject(w http.ResponseWriter, r *http.Request, rl *RateLimiter, tel TelemetryHook, clientIP, ipBucket string) {
	ep := endpointLabel(r.URL.Path)
	if !rl.AllowReject(clientIP) {
		// Rate-limited. Spec §5.3: 429 + Retry-After.
		w.Header().Set("Retry-After", "60")
		tel.RecordAuth(r.Context(), ep, "rate_limited")
		slog.Warn("glovebox ingest auth rate limited",
			"remote_addr", clientIP,
			"endpoint", r.Method+" "+r.URL.Path)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	// Normal reject.
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	tel.RecordAuth(r.Context(), ep, "rejected")
	tel.RecordAuthRejectedByIP(r.Context(), ipBucket)
	slog.Warn("glovebox ingest auth rejected",
		"remote_addr", clientIP,
		"remote_ip_bucket", ipBucket,
		"endpoint", r.Method+" "+r.URL.Path)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
