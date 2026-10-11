package powerschool

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
	"github.com/leftathome/glovebox/internal/psportal"
	pslib "github.com/leftathome/powerschool-go"
)

// AssignmentLookupPath is one of the two PowerSchool endpoints the connector may
// POST to. It is the query the portal's own scores page issues to list a
// section's assignments and scores: the JSON body is a filter (section ids,
// student ids, a date range) and the call changes nothing upstream. There
// is no GET form of it.
//
// It is a literal here, not the library's constant, so a library upgrade
// cannot widen what this transport lets through; a test pins the two
// together.
const AssignmentLookupPath = "/ws/xte/assignment/lookup"

// SwitchStudentPath and SwitchStudentField describe the other approved
// POST: the portal's own student-switcher form, which posts the one field
// selected_student_id to the home page. It changes which student the
// session displays; it creates, edits and submits nothing.
const (
	SwitchStudentPath  = "/guardian/home.html"
	SwitchStudentField = "selected_student_id"
)

// ErrHostBlocked is returned (wrapped) by Transport for any request that is
// not a plain https request to exactly the pinned PowerSchool host.
var ErrHostBlocked = fmt.Errorf("powerschool: request to unexpected host blocked")

// methodOverrideHeaders are request headers some servers and proxies honour
// as "treat this request as another method". A GET carrying one could be
// executed upstream as a write, so no request may carry any of them.
var methodOverrideHeaders = []string{
	"X-HTTP-Method-Override",
	"X-HTTP-Method",
	"X-Method-Override",
	"X-Original-Method",
	"X-Original-URL",
	"X-Rewrite-URL",
}

// Transport is the http.RoundTripper every post-login request goes through.
//
// The session belongs to a real parent account. These rules keep the
// connector from ever acting as that parent:
//
//  1. Host pin. Only https requests whose URL host is exactly Host are
//     forwarded -- no suffix or case-insensitive matching, no userinfo, no
//     opaque URL, no Host-header override -- so the session cookie cannot
//     be carried to another origin by a redirect or a library code path
//     (the library attaches the cookie itself).
//
//  2. No method override. A request carrying an X-HTTP-Method-Override
//     style header is refused whatever its method.
//
//  3. Read-only, deny by default. Everything is delegated to
//     connector.ReadOnlyTransport, which refuses any method but GET/HEAD,
//     except the requests on an explicit allowlist of two. Both are POSTs
//     that query or navigate and change no record upstream:
//
//     a. The assignment lookup (isAssignmentLookup): POST to exactly
//     AssignmentLookupPath, only when AllowAssignmentLookup is set, which
//     it is only while the assignments surface is enabled.
//
//     b. The student switch (isStudentSwitch): POST to exactly
//     SwitchStudentPath whose body is exactly the one form field
//     selected_student_id=<id>, where <id> is in SwitchStudentIDs -- the
//     student ids configured for the connector's kids. It changes which
//     student the session displays and nothing else.
//
// Login needs a browser and a credentialed POST and therefore cannot happen
// through this transport; it is a separate workload (spec 16 section 4).
type Transport struct {
	// Host is the pinned host[:port], lower-case, exactly as it will
	// appear in request URLs.
	Host string
	// AllowAssignmentLookup enables allowlist entry (a).
	AllowAssignmentLookup bool
	// SwitchStudentIDs is the set of student ids entry (b) may select.
	// Empty disables it.
	SwitchStudentIDs []string
	// Base is the wrapped transport. nil means http.DefaultTransport.
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if reason := t.originProblem(req); reason != "" {
		// Path only: a query string may carry tokens.
		slog.Error("powerschool: blocked request to unexpected host",
			"reason", reason, "scheme", req.URL.Scheme, "host", req.URL.Host, "path", req.URL.Path)
		closeBody(req)
		return nil, fmt.Errorf("%w (%s): %s://%s%s", ErrHostBlocked, reason, req.URL.Scheme, req.URL.Host, req.URL.Path)
	}

	for _, h := range methodOverrideHeaders {
		if _, present := req.Header[http.CanonicalHeaderKey(h)]; present {
			slog.Error("powerschool: blocked request carrying a method-override header",
				"method", req.Method, "header", h, "path", req.URL.Path)
			closeBody(req)
			return nil, fmt.Errorf("%w: %s %s%s carries %s",
				connector.ErrWriteBlocked, req.Method, req.URL.Host, req.URL.Path, h)
		}
	}

	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}

	if t.AllowAssignmentLookup && isAssignmentLookup(req) {
		return base.RoundTrip(req)
	}
	if switchReq, ok := t.studentSwitch(req); ok {
		return base.RoundTrip(switchReq)
	}

	ro := &connector.ReadOnlyTransport{Base: t.Base, Name: "powerschool"}
	return ro.RoundTrip(req)
}

// maxSwitchBody bounds the student-switch form body:
// "selected_student_id=" plus an id of at most 20 digits.
const maxSwitchBody = len(SwitchStudentField) + 1 + 20

// studentSwitch reports whether req is allowlist entry (b) and, if so,
// returns the request to forward.
//
// The body has to be read to be checked, so the forwarded request is a
// clone carrying the checked bytes: what was inspected is what is sent.
// The check is on the raw bytes, not on a parsed form -- the body must be
// byte-for-byte "selected_student_id=" followed by one of the configured
// ids -- so an extra field, a repeated field, an encoded separator or a
// multipart body cannot ride along. When it returns false the original
// body has been consumed or left alone, and the caller refuses the request
// either way.
func (t *Transport) studentSwitch(req *http.Request) (*http.Request, bool) {
	u := req.URL
	if len(t.SwitchStudentIDs) == 0 || req.Method != http.MethodPost {
		return nil, false
	}
	if u.RawPath != "" || u.Path != SwitchStudentPath || u.EscapedPath() != SwitchStudentPath {
		return nil, false
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return nil, false
	}
	if req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
		len(req.Header.Values("Content-Type")) != 1 {
		return nil, false
	}
	if req.Body == nil || req.ContentLength <= 0 || req.ContentLength > int64(maxSwitchBody) {
		// Unknown length (-1, chunked) is refused: the body must be small
		// and fully known before anything is sent.
		return nil, false
	}
	if len(req.TransferEncoding) != 0 || len(req.Trailer) != 0 {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, int64(maxSwitchBody)+1))
	_ = req.Body.Close()
	if err != nil || int64(len(body)) != req.ContentLength {
		return nil, false
	}
	id, ok := strings.CutPrefix(string(body), SwitchStudentField+"=")
	if !ok || !isDigits(id) || !t.mayselect(id) {
		return nil, false
	}

	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(body))
	out.ContentLength = int64(len(body))
	out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return out, true
}

// mayselect reports whether id is one of the configured student ids.
func (t *Transport) mayselect(id string) bool {
	for _, allowed := range t.SwitchStudentIDs {
		if allowed != "" && allowed == id {
			return true
		}
	}
	return false
}

// originProblem returns why req is not a plain https request to the pinned
// host, or "" when it is.
func (t *Transport) originProblem(req *http.Request) string {
	u := req.URL
	switch {
	case u == nil:
		return "no url"
	case u.Scheme != "https":
		return "scheme is not https"
	case t.Host == "" || u.Host != t.Host:
		// Exact, case-sensitive comparison with the lower-cased pin. A
		// suffix match would accept evil-ps.example.org for
		// ps.example.org.
		return "host is not the pinned host"
	case u.User != nil:
		return "url carries userinfo"
	case u.Opaque != "":
		// An opaque URL puts something other than Path on the wire.
		return "opaque url"
	case req.Host != "" && req.Host != t.Host:
		// req.Host overrides the Host header (and the TLS server name is
		// still u.Host): the request would be routed by another name.
		return "host header override"
	}
	return ""
}

// isAssignmentLookup reports whether req is allowlist entry (a), the lookup
// request: POST to exactly AssignmentLookupPath.
//
// The comparison is on what goes on the wire, not on the decoded path.
// URL.Path is decoded, so "/ws/xte/assignment%2Flookup" has the right Path
// and the wrong request line; EscapedPath() is the request line, and a
// non-empty RawPath means the URL was written with an encoding that differs
// from the default one. Requiring the escaped path to equal the literal and
// RawPath to be empty therefore rejects percent-encoding, a trailing slash,
// doubled slashes, dot segments and any change of case in one comparison.
// The query may carry only the library's cache-busting "_" parameter.
func isAssignmentLookup(req *http.Request) bool {
	u := req.URL
	if req.Method != http.MethodPost {
		return false
	}
	if u.RawPath != "" || u.Path != AssignmentLookupPath || u.EscapedPath() != AssignmentLookupPath {
		return false
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return false
	}
	for key, values := range u.Query() {
		if key != "_" || len(values) != 1 || !isDigits(values[0]) {
			return false
		}
	}
	// Query() drops what it cannot parse; a query that does not round-trip
	// is not the library's.
	if u.RawQuery != "" && u.Query().Encode() != u.RawQuery {
		return false
	}
	return true
}

func isDigits(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// reviewedLibraryVersion is the one powerschool-go version this connector
// may run on: the version whose source was read for requests, POST paths
// and form fields before it was allowed near a real parent account.
//
// Raising the go.mod requirement is not enough to change the library: the
// binary refuses to start (CheckLibrary) until this constant says the same
// version, and TestReviewedLibraryVersion fails CI. Change it only after
// reading the library diff between the two versions; say what was checked
// in the commit that changes it.
const reviewedLibraryVersion = "v0.4.0"

// ErrLibraryDrift is returned by CheckLibrary when the linked library's
// POST endpoints are not the approved ones.
var ErrLibraryDrift = errors.New("powerschool: linked powerschool-go posts to a path or form field " +
	"other than the two this connector's transport was approved for")

// ErrLibraryVersion is returned by CheckLibrary when the linked library is
// not, or cannot be shown to be, reviewedLibraryVersion.
var ErrLibraryVersion = errors.New("powerschool: linked powerschool-go is not the reviewed version " + reviewedLibraryVersion)

// readBuildInfo is debug.ReadBuildInfo. It is a variable only so that
// tests can present other build information to checkLibraryVersion; no
// production code assigns it, and there is no environment variable or
// flag that relaxes the check.
var readBuildInfo = debug.ReadBuildInfo

// CheckLibrary verifies, at startup, the assumptions this package makes
// about the linked powerschool-go that the compiler cannot:
//
//   - it is exactly reviewedLibraryVersion (checkLibraryVersion);
//   - it POSTs where, and what, the allowlist expects. If it does not, the
//     library is asking for an exception nobody approved.
func CheckLibrary() error {
	if err := checkLibraryVersion(readBuildInfo()); err != nil {
		return err
	}
	if pslib.AssignmentLookupPath != AssignmentLookupPath ||
		pslib.SwitchStudentPath != SwitchStudentPath ||
		pslib.SwitchStudentField != SwitchStudentField {
		return ErrLibraryDrift
	}
	return nil
}

// checkLibraryVersion compares the powerschool-go recorded in the binary's
// build information with reviewedLibraryVersion. It fails closed: anything
// it cannot positively identify as the reviewed release is refused.
//
//   - No build information (ok is false: a binary built without module
//     support), or build information that does not list the library:
//     refused. `go build`, `go run` and `go test` in this module all
//     record it, so a normal build is never in this case.
//   - The module is replaced (a `replace` directive, to a directory or to
//     another module or version): refused, whatever version the
//     replacement claims. A replacement is by definition not the reviewed
//     tag.
//   - Any other version, pseudo-versions and "(devel)" included: refused.
func checkLibraryVersion(bi *debug.BuildInfo, ok bool) error {
	if !ok || bi == nil {
		return fmt.Errorf("%w: the binary carries no build information, so the linked version cannot be verified", ErrLibraryVersion)
	}
	for _, dep := range bi.Deps {
		if dep == nil || dep.Path != libraryModule {
			continue
		}
		if dep.Replace != nil {
			return fmt.Errorf("%w: go.mod replaces the module (with %q %q)", ErrLibraryVersion, dep.Replace.Path, dep.Replace.Version)
		}
		if dep.Version != reviewedLibraryVersion {
			return fmt.Errorf("%w: linked version is %q", ErrLibraryVersion, dep.Version)
		}
		return nil
	}
	return fmt.Errorf("%w: the build information does not list %s", ErrLibraryVersion, libraryModule)
}

// refuseWriteRedirects is the client's CheckRedirect. A redirect that the
// client would follow with a GET or HEAD is followed (each hop still passes
// through Transport, so it stays on the pinned host); that includes the
// 302/303 a form post is normally answered with. A redirect that would
// replay a POST and its body at a location the server chose (307/308) is
// not followed.
func refuseWriteRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("powerschool: stopped after 5 redirects")
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return fmt.Errorf("%w: redirect that replays a %s request not followed", connector.ErrWriteBlocked, req.Method)
	}
	return nil
}

// NewHTTPClient returns the *http.Client handed to the library in place of
// its default. host is the pinned host[:port] from NormalizeBaseURL;
// switchStudentIDs are the configured kids' student ids, the only values
// the student-switch exception may post.
//
// The client has no cookie jar: the library attaches the session cookies to
// each request itself. What a response sets is not stored either, with one
// narrow exception that lives inside the pinned transport and dies with
// this client: the gateway-cookie overlay below.
func NewHTTPClient(host string, allowAssignmentLookup bool, switchStudentIDs []string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: refuseWriteRedirects,
		Transport: &Transport{
			Host:                  strings.ToLower(host),
			AllowAssignmentLookup: allowAssignmentLookup,
			SwitchStudentIDs:      append([]string(nil), switchStudentIDs...),
			// Underneath the pin and the read-only rule: carry a cookie the
			// access gateway renews in mid-request (see
			// psportal.GatewayCookies). One per client, so one per stored
			// session; nothing it collects is written anywhere.
			Base: psportal.NewGatewayCookies(nil),
		},
	}
}
