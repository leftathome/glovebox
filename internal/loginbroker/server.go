package loginbroker

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"

	"golang.org/x/time/rate"

	"github.com/leftathome/glovebox/internal/loginbroker/brokerapi"
)

// maxRequestBytes bounds a request body. A valid one is under 100 bytes.
const maxRequestBytes = 1 << 10

// Server is the broker's HTTP surface:
//
//	POST /v1/session/refresh   authenticated; the one thing a caller can ask
//	GET  /healthz              liveness; says nothing about the site
//
// and, on a listener of its own so that a NetworkPolicy can admit a
// scraper to it without admitting it to the endpoint above:
//
//	GET  /metrics
//	GET  /healthz
type Server struct {
	broker  *Broker
	auth    Authenticator
	log     *slog.Logger
	metrics *Metrics
	// limiter bounds how often the API server is asked to review a token,
	// whoever is sending them.
	limiter *rate.Limiter
}

// NewServer wires the HTTP surface to a broker.
func NewServer(broker *Broker, auth Authenticator, logger *slog.Logger, metrics *Metrics) (*Server, error) {
	if broker == nil || auth == nil {
		return nil, errors.New("loginbroker: a server needs a broker and an authenticator")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Server{
		broker:  broker,
		auth:    auth,
		log:     logger.With("site", broker.Site()),
		metrics: metrics,
		limiter: rate.NewLimiter(rate.Limit(1), 10),
	}, nil
}

// Handler serves the API listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(brokerapi.RefreshPath, s.handleRefresh)
	mux.HandleFunc("/healthz", handleHealthz)
	return mux
}

// MetricsHandler serves the metrics listener.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", s.metrics.Handler())
	mux.HandleFunc("/healthz", handleHealthz)
	return mux
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	// An answer may carry a session.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, brokerapi.Error{Error: code})
}

// refuse answers a request that never reached the policy.
func (s *Server) refuse(w http.ResponseWriter, status int, code string, attrs ...any) {
	s.metrics.refused(code)
	s.log.Warn("request refused", append([]any{"refused", code, "status", status}, attrs...)...)
	writeError(w, status, code)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		s.refuse(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		s.refuse(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if !s.limiter.Allow() {
		w.Header().Set("Retry-After", "5")
		s.refuse(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	caller, err := s.auth.Authenticate(r.Context(), token)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", "Bearer")
		s.refuse(w, http.StatusUnauthorized, "unauthenticated")
		return
	case errors.Is(err, ErrForbidden):
		// The identity is the API server's word and is safe to log; it is
		// what an operator needs to see when the allow-list is wrong.
		s.refuse(w, http.StatusForbidden, "forbidden",
			"caller_namespace", safeName(caller.Namespace), "caller_serviceaccount", safeName(caller.Name))
		return
	case err != nil:
		s.refuse(w, http.StatusServiceUnavailable, "auth_unavailable")
		return
	}

	// Only now is anything the caller sent beyond its token looked at.
	if mt, _, perr := mime.ParseMediaType(r.Header.Get("Content-Type")); perr != nil || mt != "application/json" {
		s.refuse(w, http.StatusBadRequest, "bad_request")
		return
	}
	var req brokerapi.Request
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		s.refuse(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !brokerapi.ValidReason(req.Reason) || !brokerapi.ValidFingerprint(req.Fingerprint) {
		s.refuse(w, http.StatusBadRequest, "bad_request")
		return
	}

	s.log.Info("refresh requested",
		"caller_namespace", safeName(caller.Namespace), "caller_serviceaccount", safeName(caller.Name), "reason", req.Reason)
	resp, err := s.broker.Refresh(r.Context(), req)
	if err != nil {
		// The caller went away. The refresh goes on and its session, if
		// any, is stored for the next request.
		s.log.Warn("the caller stopped waiting before the refresh finished")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// bearerToken extracts the token of an "Authorization: Bearer" header.
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxTokenBytes || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

// safeName bounds a Kubernetes name for a log line. Names from the API
// server are already DNS labels; this only makes that independent of it.
func safeName(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
		if sb.Len() >= 80 {
			break
		}
	}
	return sb.String()
}
