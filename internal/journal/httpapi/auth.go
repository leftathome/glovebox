package httpapi

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/leftathome/glovebox/internal/journal"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/policy"
	"github.com/leftathome/glovebox/internal/satoken"
)

// call is an authenticated request.
type call struct {
	caller string
	svc    policy.Service
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.status, w.wrote = code, true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// maxBearerBytes bounds a bearer token (satoken's bound). A projected
// ServiceAccount token is about a kilobyte.
const maxBearerBytes = 8 << 10

// token68 is RFC 6750's b64token: what a ServiceAccount token (a JWT) is.
var token68 = regexp.MustCompile(`^[A-Za-z0-9._~+/-]+=*$`)

// bearerToken accepts exactly one Authorization header of the exact form
// "Bearer <token>": the scheme in any case, one space, a token68 token. It
// is stricter than satoken.BearerToken (the broker's parser, which trims
// and tolerates extra spaces), and both must agree.
func bearerToken(r *http.Request) (string, bool) {
	vals := r.Header.Values("Authorization")
	if len(vals) != 1 {
		return "", false
	}
	h := vals[0]
	const scheme = "Bearer "
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return "", false
	}
	tok := h[len(scheme):]
	if len(tok) > maxBearerBytes || !token68.MatchString(tok) {
		return "", false
	}
	if t, ok := satoken.BearerToken(h); !ok || t != tok {
		return "", false
	}
	return tok, true
}

// route bounds, authenticates, counts and dispatches. Order (4.9): ready
// (503 unavailable), bearer token (401), an in-flight slot (503
// unavailable with Retry-After), TokenReview with the journal's audience
// and the policy's callers as the allow-list (401 / 403 forbidden / 503
// auth_unavailable, failing closed), then the endpoint's own checks. The
// slot is held for the TokenReview and the handler, so at most
// MaxInFlight requests do work at once whatever goes wrong inside one.
// No token is logged or echoed.
func (s *Server) route(endpoint string, h func(http.ResponseWriter, *http.Request, call)) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w := &statusWriter{ResponseWriter: rw, status: http.StatusOK}
		defer func() { s.d.Metrics.Request(endpoint, w.status) }()
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			s.d.Log.Error("journal handler panicked", "endpoint", endpoint, "panic", fmt.Sprint(v))
			if w.wrote {
				// The answer has started; all that is left is to cut it off.
				panic(http.ErrAbortHandler)
			}
			refuse(w, http.StatusInternalServerError, journalapi.ReasonInternal, "")
		}()
		if s.d.RequestTimeout > 0 {
			ctx, cancel := context.WithTimeout(r.Context(), s.d.RequestTimeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
		if !s.d.Ready() {
			refuse(w, http.StatusServiceUnavailable, journalapi.ReasonUnavailable, "the journal is starting or cannot reach its database or roster")
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			refuse(w, http.StatusUnauthorized, journalapi.ReasonUnauthenticated, "")
			return
		}
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		default:
			w.Header().Set("Retry-After", "1")
			refuse(w, http.StatusServiceUnavailable, journalapi.ReasonUnavailable, "the journal is at its limit of concurrent requests")
			return
		}
		caller, err := s.d.Auth.Authenticate(r.Context(), token)
		switch {
		case err == nil:
		case errors.Is(err, satoken.ErrUnauthenticated):
			w.Header().Set("WWW-Authenticate", "Bearer")
			refuse(w, http.StatusUnauthorized, journalapi.ReasonUnauthenticated, "")
			return
		case errors.Is(err, satoken.ErrForbidden):
			// The identity is the API server's word and safe to log.
			s.d.Log.Warn("journal caller not allow-listed", "endpoint", endpoint,
				"caller_namespace", satoken.SafeName(caller.Namespace), "caller_serviceaccount", satoken.SafeName(caller.Name))
			refuse(w, http.StatusForbidden, journalapi.ReasonForbidden, "")
			return
		default:
			// Fail closed: nobody is authenticated while TokenReview cannot
			// be asked. satoken's errors never carry the token.
			s.d.Log.Warn("journal authentication unavailable", "endpoint", endpoint, "err", err)
			refuse(w, http.StatusServiceUnavailable, journalapi.ReasonAuthUnavailable, "")
			return
		}
		svc, ok := s.d.Policy.Service(caller.String())
		if !ok {
			// Only reachable when the authenticator's allow-list and the
			// policy disagree; refuse rather than guess.
			refuse(w, http.StatusForbidden, journalapi.ReasonForbidden, "")
			return
		}
		h(w, r, call{caller: caller.String(), svc: svc})
	})
}

// readerOK checks the one access field a request may carry. allowed is the
// service's policy for this reader. It writes the refusal itself. NOTE (Task
// 19 review): the journal computes grants and confirms roster membership on
// every read, so this no longer resolves grants; it returns the reader id.
// Roster errors from the journal are mapped by readErr below. Used by the
// read handlers; a note's on_behalf_of is checked by MayWriteNote, whose
// refusal carries its own detail.
func (s *Server) readerOK(w http.ResponseWriter, reader string, allowed bool) (string, bool) {
	if reader == "" {
		refuse(w, http.StatusBadRequest, journalapi.ReasonBadRequest, "reader is required")
		return "", false
	}
	if !allowed {
		refuse(w, http.StatusForbidden, journalapi.ReasonPolicy, "this service may not act for that reader")
		return "", false
	}
	return reader, true
}

// readErr maps a journal read error (NOTE, Task 19 review): ErrUnknownReader
// is 403 policy, ErrNoRoster is 503, ErrBadQuery is 400, anything else 500.
func (s *Server) readErr(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, journal.ErrUnknownReader):
		refuse(w, http.StatusForbidden, journalapi.ReasonPolicy, "the reader is not in the roster")
	case errors.Is(err, journal.ErrNoRoster):
		refuse(w, http.StatusServiceUnavailable, journalapi.ReasonUnavailable, "no roster is loaded")
	case errors.Is(err, journal.ErrBadQuery):
		refuse(w, http.StatusBadRequest, journalapi.ReasonBadRequest, "bad query")
	default:
		s.internal(w, what, err)
	}
}

// queryParams admits only the named parameters, each at most once. A
// request naming anything else -- grants, audiences, tokens -- is refused:
// it names its reader and nothing more about access (4.9).
func queryParams(r *http.Request, allowed ...string) (url.Values, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errors.New("the query string does not parse")
	}
	for k, v := range q {
		// The errors name no parameter: a name is the caller's text, and a
		// refusal echoes nothing it was sent.
		if !slices.Contains(allowed, k) {
			return nil, errors.New("an unknown parameter: a request names its reader and its filters, nothing else")
		}
		if len(v) != 1 {
			return nil, errors.New("a parameter is given more than once")
		}
	}
	return q, nil
}

func isJSON(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}
