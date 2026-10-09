package powerschool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/connector"
	pslib "github.com/leftathome/powerschool-go"
)

// pinnedHost is the synthetic portal host the unit tests pin to.
const pinnedHost = "ps.example.org"

// countingBase is the wrapped transport in the unit tests. It never opens a
// socket: it records what Transport forwarded and answers 200.
type countingBase struct {
	mu     sync.Mutex
	seen   []string
	bodies []string
}

func (b *countingBase) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""
	if req.Body != nil {
		raw, _ := io.ReadAll(req.Body)
		_ = req.Body.Close()
		body = string(raw)
	}
	b.mu.Lock()
	b.seen = append(b.seen, req.Method+" "+req.URL.String())
	b.bodies = append(b.bodies, body)
	b.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("[]")),
		Request:    req,
	}, nil
}

func (b *countingBase) forwarded() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.seen...)
}

// roundTrip sends one request straight through a Transport pinned to
// pinnedHost and reports whether it was forwarded.
func roundTrip(t *testing.T, allowLookup bool, method, rawURL string, mutate func(*http.Request)) (forwarded bool, err error) {
	t.Helper()
	req, reqErr := http.NewRequest(method, rawURL, strings.NewReader("{}"))
	if reqErr != nil {
		t.Fatalf("NewRequest(%s %s): %v", method, rawURL, reqErr)
	}
	if mutate != nil {
		mutate(req)
	}
	base := &countingBase{}
	tr := &Transport{Host: pinnedHost, AllowAssignmentLookup: allowLookup, SwitchStudentIDs: configuredIDs, Base: base}
	resp, err := tr.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	n := len(base.forwarded())
	if (err == nil) != (n == 1) {
		t.Fatalf("%s %s: err=%v but %d request(s) forwarded", method, rawURL, err, n)
	}
	return n == 1, err
}

const lookupURL = "https://" + pinnedHost + AssignmentLookupPath

// configuredIDs are the student ids of the fixture config's two kids: the
// only ids the student-switch exception may post.
var configuredIDs = []string{"111111", "111112"}

// TestTransport_ReadsAreForwarded: the requests the connector exists to
// make go through, with the lookup exception on or off.
func TestTransport_ReadsAreForwarded(t *testing.T) {
	for _, allow := range []bool{false, true} {
		for _, tc := range []struct{ method, url string }{
			{http.MethodGet, "https://" + pinnedHost + "/guardian/home.html"},
			{http.MethodHead, "https://" + pinnedHost + "/guardian/home.html"},
			{http.MethodGet, "https://" + pinnedHost + "/guardian/scores.html?frn=00100&fg=Q1&schoolid=1"},
			// A GET of the lookup path is just a read.
			{http.MethodGet, lookupURL},
		} {
			if ok, err := roundTrip(t, allow, tc.method, tc.url, nil); !ok {
				t.Errorf("allowLookup=%v: %s %s refused: %v", allow, tc.method, tc.url, err)
			}
		}
	}
}

// TestTransport_TheOneApprovedPOST: exactly one write-shaped request is
// approved -- POST to the literal lookup path on the pinned host over https,
// and only while the assignments surface is enabled.
func TestTransport_TheOneApprovedPOST(t *testing.T) {
	t.Run("forwarded while the assignments surface is enabled", func(t *testing.T) {
		for _, u := range []string{lookupURL, lookupURL + "?_=1760000000000"} {
			if ok, err := roundTrip(t, true, http.MethodPost, u, nil); !ok {
				t.Errorf("POST %s refused: %v", u, err)
			}
		}
	})

	t.Run("refused while the assignments surface is disabled", func(t *testing.T) {
		for _, u := range []string{lookupURL, lookupURL + "?_=1760000000000"} {
			ok, err := roundTrip(t, false, http.MethodPost, u, nil)
			if ok || !errors.Is(err, connector.ErrWriteBlocked) {
				t.Errorf("POST %s: forwarded=%v err=%v, want ErrWriteBlocked", u, ok, err)
			}
		}
	})
}

// TestTransport_LookupPathVariantsAreRefused: every spelling of the lookup
// path other than the literal one is refused, with the exception enabled.
// Several of these decode to the approved URL.Path, which is why the check
// is on the escaped path.
func TestTransport_LookupPathVariantsAreRefused(t *testing.T) {
	base := "https://" + pinnedHost
	cases := []struct {
		name string
		url  string
	}{
		{"trailing slash", base + "/ws/xte/assignment/lookup/"},
		{"percent-encoded slash", base + "/ws/xte/assignment%2Flookup"},
		{"percent-encoded slash, lower-case hex", base + "/ws/xte/assignment%2flookup"},
		{"percent-encoded letter", base + "/ws/xte/assignment/%6Cookup"},
		{"percent-encoded dot segment", base + "/ws/xte/assignment/%2e%2e/assignment/lookup"},
		{"double slash at the start", base + "//ws/xte/assignment/lookup"},
		{"double slash inside", base + "/ws/xte//assignment/lookup"},
		{"dot segment", base + "/ws/xte/assignment/./lookup"},
		{"dot-dot segment", base + "/ws/xte/assignment/x/../lookup"},
		{"dot-dot escaping to another endpoint", base + "/ws/xte/assignment/lookup/../submit"},
		{"upper-case path", base + "/WS/XTE/ASSIGNMENT/LOOKUP"},
		{"mixed-case path", base + "/ws/xte/assignment/Lookup"},
		{"suffix", base + "/ws/xte/assignment/lookup/extra"},
		{"suffix without a slash", base + "/ws/xte/assignment/lookups"},
		{"prefix", base + "/x/ws/xte/assignment/lookup"},
		{"matrix parameter", base + "/ws/xte/assignment/lookup;jsessionid=x"},
		{"another ws endpoint", base + "/ws/xte/assignment/submit"},
		{"the home page (student switch / login form)", base + "/guardian/home.html"},
		{"method smuggled in the query", lookupURL + "?_method=DELETE"},
		{"extra query parameter", lookupURL + "?_=1&action=delete"},
		{"non-numeric cache buster", lookupURL + "?_=abc"},
		{"repeated cache buster", lookupURL + "?_=1&_=2"},
		{"fragment", lookupURL + "#x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := roundTrip(t, true, http.MethodPost, tc.url, nil)
			if ok || !errors.Is(err, connector.ErrWriteBlocked) {
				t.Errorf("POST %s: forwarded=%v err=%v, want ErrWriteBlocked", tc.url, ok, err)
			}
		})
	}

	// A URL built in code rather than parsed can carry the approved Path
	// with a RawPath or Opaque that puts something else on the wire.
	t.Run("RawPath set by hand", func(t *testing.T) {
		ok, err := roundTrip(t, true, http.MethodPost, lookupURL, func(r *http.Request) {
			r.URL.RawPath = "/ws/xte/assignment%2Flookup"
		})
		if ok || !errors.Is(err, connector.ErrWriteBlocked) {
			t.Errorf("forwarded=%v err=%v, want ErrWriteBlocked", ok, err)
		}
	})
	t.Run("opaque URL", func(t *testing.T) {
		ok, err := roundTrip(t, true, http.MethodPost, lookupURL, func(r *http.Request) {
			r.URL.Opaque = "//" + pinnedHost + "/ws/xte/assignment/submit"
		})
		if ok || !errors.Is(err, ErrHostBlocked) {
			t.Errorf("forwarded=%v err=%v, want ErrHostBlocked", ok, err)
		}
	})
}

// TestTransport_OtherMethodsAreRefused: no method but POST gets the
// exception, on the lookup path or anywhere else.
func TestTransport_OtherMethodsAreRefused(t *testing.T) {
	methods := []string{
		http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
		http.MethodTrace, http.MethodConnect,
		"PROPFIND", "MKCOL", "COPY", "MOVE", "LOCK", "REPORT", "PURGE",
		"post", "Post", "POSTX", "XPOST",
	}
	for _, m := range methods {
		for _, u := range []string{lookupURL, "https://" + pinnedHost + "/guardian/home.html"} {
			ok, err := roundTrip(t, true, m, u, nil)
			if ok || !errors.Is(err, connector.ErrWriteBlocked) {
				t.Errorf("%s %s: forwarded=%v err=%v, want ErrWriteBlocked", m, u, ok, err)
			}
		}
	}
	// And a POST anywhere but the lookup path, with the exception off too.
	for _, allow := range []bool{false, true} {
		ok, err := roundTrip(t, allow, http.MethodPost, "https://"+pinnedHost+"/guardian/home.html", nil)
		if ok || !errors.Is(err, connector.ErrWriteBlocked) {
			t.Errorf("allowLookup=%v: POST home.html forwarded=%v err=%v", allow, ok, err)
		}
	}
}

// TestTransport_HostAndSchemeArePinnedExactly: nothing leaves for any
// origin but https://<pinned host>, whatever the method or path.
func TestTransport_HostAndSchemeArePinnedExactly(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"different host", "https://other.example.org" + AssignmentLookupPath},
		{"host that ends with the pinned host", "https://evil-" + pinnedHost + AssignmentLookupPath},
		{"host suffix without a separator", "https://evilps.example.org" + AssignmentLookupPath},
		{"subdomain of the pinned host", "https://www." + pinnedHost + AssignmentLookupPath},
		{"pinned host as a subdomain of another", "https://" + pinnedHost + ".evil.example" + AssignmentLookupPath},
		{"parent domain", "https://example.org" + AssignmentLookupPath},
		{"explicit default port", "https://" + pinnedHost + ":443" + AssignmentLookupPath},
		{"another port", "https://" + pinnedHost + ":8443" + AssignmentLookupPath},
		{"upper-case host", "https://PS.EXAMPLE.ORG" + AssignmentLookupPath},
		{"trailing dot", "https://" + pinnedHost + "." + AssignmentLookupPath},
		{"userinfo", "https://user:pw@" + pinnedHost + AssignmentLookupPath},
		{"pinned host as userinfo", "https://" + pinnedHost + "@evil.example" + AssignmentLookupPath},
		{"http scheme", "http://" + pinnedHost + AssignmentLookupPath},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			t.Run(tc.name+"/"+method, func(t *testing.T) {
				ok, err := roundTrip(t, true, method, tc.url, nil)
				if ok || !errors.Is(err, ErrHostBlocked) {
					t.Errorf("%s %s: forwarded=%v err=%v, want ErrHostBlocked", method, tc.url, ok, err)
				}
				if err != nil && strings.Contains(err.Error(), "?") {
					t.Errorf("error carries a query string: %v", err)
				}
			})
		}
	}

	t.Run("Host header override", func(t *testing.T) {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			ok, err := roundTrip(t, true, method, lookupURL, func(r *http.Request) { r.Host = "other.example.org" })
			if ok || !errors.Is(err, ErrHostBlocked) {
				t.Errorf("%s with req.Host set: forwarded=%v err=%v, want ErrHostBlocked", method, ok, err)
			}
		}
	})

	t.Run("an unpinned transport forwards nothing", func(t *testing.T) {
		base := &countingBase{}
		req, _ := http.NewRequest(http.MethodGet, "https://"+pinnedHost+"/guardian/home.html", nil)
		resp, err := (&Transport{Base: base}).RoundTrip(req)
		if resp != nil {
			resp.Body.Close()
		}
		if !errors.Is(err, ErrHostBlocked) || len(base.forwarded()) != 0 {
			t.Errorf("err=%v forwarded=%v, want ErrHostBlocked and nothing forwarded", err, base.forwarded())
		}
	})
}

// TestTransport_MethodOverrideHeadersAreRefused: a header that asks the
// server to treat the request as another method is refused on every
// request, reads included.
func TestTransport_MethodOverrideHeadersAreRefused(t *testing.T) {
	headers := append([]string{"x-http-method-override", "X-Http-Method-Override"}, methodOverrideHeaders...)
	for _, h := range headers {
		for _, tc := range []struct{ method, url string }{
			{http.MethodGet, "https://" + pinnedHost + "/guardian/home.html"},
			{http.MethodHead, "https://" + pinnedHost + "/guardian/home.html"},
			{http.MethodPost, lookupURL},
		} {
			ok, err := roundTrip(t, true, tc.method, tc.url, func(r *http.Request) { r.Header.Set(h, "DELETE") })
			if ok || !errors.Is(err, connector.ErrWriteBlocked) {
				t.Errorf("%s with %s: forwarded=%v err=%v, want ErrWriteBlocked", tc.method, h, ok, err)
			}
		}
	}
	// Present but empty still counts.
	ok, err := roundTrip(t, true, http.MethodGet, "https://"+pinnedHost+"/guardian/home.html",
		func(r *http.Request) { r.Header["X-Http-Method-Override"] = []string{""} })
	if ok || !errors.Is(err, connector.ErrWriteBlocked) {
		t.Errorf("empty override header: forwarded=%v err=%v", ok, err)
	}
}

// --- over a real (loopback) TLS connection ------------------------------

// recordingServer is a TLS test server on loopback that records
// "<METHOD> <path>" for every request that reaches it. A request the
// transport blocks never shows up here, which is the assertion that matters.
type recordingServer struct {
	*httptest.Server
	mu      sync.Mutex
	seen    []string
	handler http.HandlerFunc // optional; default writes body
}

func newRecordingServer(t *testing.T, body string) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.seen = append(rs.seen, r.Method+" "+r.URL.Path)
		h := rs.handler
		rs.mu.Unlock()
		// Drain the body (so the connection is reusable) but leave a copy
		// for the handler.
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(rs.Close)
	return rs
}

func (rs *recordingServer) requests() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.seen...)
}

func (rs *recordingServer) host(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(rs.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return u.Host
}

// client returns the http.Client main.go builds (NewHTTPClient), pinned to
// this server and trusting its test certificate.
func (rs *recordingServer) client(t *testing.T, allowLookup bool) *http.Client {
	t.Helper()
	c := NewHTTPClient(rs.host(t), allowLookup, configuredIDs, 5*time.Second)
	c.Transport.(*Transport).Base = rs.Server.Client().Transport
	return c
}

// TestTransport_RedirectOfTheLookupIsNotFollowed: the server answers the
// approved POST with a redirect to another path on the SAME host. A 307 or
// 308 would replay the POST and its body there. Nothing but the original
// lookup may reach the server -- with the production client, and with the
// bare Transport under a default client as well.
func TestTransport_RedirectOfTheLookupIsNotFollowed(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect, http.StatusFound, http.StatusSeeOther} {
		for _, bare := range []bool{false, true} {
			name := http.StatusText(status) + "/production client"
			if bare {
				name = http.StatusText(status) + "/transport only"
			}
			t.Run(name, func(t *testing.T) {
				rs := newRecordingServer(t, "[]")
				rs.handler = func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == AssignmentLookupPath {
						http.Redirect(w, r, "/ws/xte/assignment/submit", status)
						return
					}
					_, _ = w.Write([]byte("[]"))
				}
				client := rs.client(t, true)
				if bare {
					client = &http.Client{Transport: client.Transport}
				}

				resp, err := client.Post(rs.URL+AssignmentLookupPath, "application/json", strings.NewReader("{}"))
				if resp != nil {
					resp.Body.Close()
				}
				for _, r := range rs.requests() {
					if strings.HasPrefix(r, "POST ") && r != "POST "+AssignmentLookupPath {
						t.Errorf("a POST reached another path: %s", r)
					}
				}
				replays := status == http.StatusTemporaryRedirect || status == http.StatusPermanentRedirect
				if replays {
					// 307/308 replay the POST and its body. The production
					// client refuses to follow; under a default client the
					// transport stops the replay at the new path.
					if !errors.Is(err, connector.ErrWriteBlocked) {
						t.Errorf("err = %v, want ErrWriteBlocked", err)
					}
					if got := rs.requests(); len(got) != 1 {
						t.Errorf("server saw %v, want only the original lookup", got)
					}
				} else if err != nil {
					// 302/303 are followed with a GET, which is a read.
					t.Errorf("redirect followed with a GET was refused: %v", err)
				}
			})
		}
	}
}

// TestTransport_RedirectOffHostBlocked: a redirect cannot carry the
// request (and the cookie header the library set) to another origin.
func TestTransport_RedirectOffHostBlocked(t *testing.T) {
	other := newRecordingServer(t, "[]")
	rs := newRecordingServer(t, "[]")
	rs.handler = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/landing", http.StatusFound)
	}

	resp, err := rs.client(t, true).Get(rs.URL + "/guardian/home.html")
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrHostBlocked) {
		t.Fatalf("err = %v, want ErrHostBlocked", err)
	}
	if got := other.requests(); len(got) != 0 {
		t.Errorf("redirect target was contacted: %v", got)
	}
}

// TestTransport_PlainHTTPToPinnedHostBlocked: the host pin includes the
// scheme, so the session cookie is never sent in the clear.
func TestTransport_PlainHTTPToPinnedHostBlocked(t *testing.T) {
	rs := newRecordingServer(t, "[]")
	req, _ := http.NewRequest(http.MethodGet, "http://"+rs.host(t)+"/guardian/home.html", nil)
	resp, err := rs.client(t, true).Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrHostBlocked) {
		t.Fatalf("err = %v, want ErrHostBlocked", err)
	}
	if got := rs.requests(); len(got) != 0 {
		t.Errorf("blocked request reached the server: %v", got)
	}
}

func TestNewHTTPClient(t *testing.T) {
	c := NewHTTPClient("PS.Example.ORG", true, []string{"111111"}, 7*time.Second)
	tr, ok := c.Transport.(*Transport)
	if !ok {
		t.Fatalf("transport is %T, want *Transport", c.Transport)
	}
	if tr.Host != "ps.example.org" || !tr.AllowAssignmentLookup || len(tr.SwitchStudentIDs) != 1 || tr.SwitchStudentIDs[0] != "111111" {
		t.Errorf("transport = %+v", tr)
	}
	if c.Jar != nil {
		t.Error("client has a cookie jar; the library owns the cookies")
	}
	if c.CheckRedirect == nil {
		t.Error("client follows redirects of non-read requests")
	}
	if c.Timeout != 7*time.Second {
		t.Errorf("timeout = %v", c.Timeout)
	}
}

// --- the real library on the real transport -----------------------------

// syntheticLookupResponse is a minimal /ws/xte/assignment/lookup body in
// the shape the library decodes. Hand-written; no real record.
const syntheticLookupResponse = `[{"assignmentid":5001,"_assignmentsections":[{
  "name":"Lab report","duedate":"2026-10-08","scoreentrypoints":10,
  "_assignmentcategoryassociations":[{"isprimary":true,"_teachercategory":{"name":"Classwork"}}],
  "_assignmentscores":[{"ismissing":true}]}]}]`

// libraryOn builds a real powerschool-go client on the connector's
// transport, pointed at rs. This is the wiring main.go does.
func libraryOn(t *testing.T, rs *recordingServer, allowLookup bool) *pslib.Client {
	t.Helper()
	lib, err := pslib.NewClient(rs.URL,
		pslib.WithHTTPClient(rs.client(t, allowLookup)),
		pslib.WithHTTPPageFetch(),
		pslib.WithSession([]*http.Cookie{{Name: "JSESSIONID", Value: "synthetic"}}, time.Now().Add(time.Hour)),
		pslib.WithLogLevel(pslib.LogLevelNone),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return lib
}

// lookupGrade carries the ids the library would otherwise scrape from the
// scores page, so GetAssignments goes straight to the lookup request.
func lookupGrade() *pslib.Grade {
	return &pslib.Grade{
		CourseID:     "00100",
		ScoresURL:    "/guardian/scores.html?frn=00100&fg=Q1",
		SectionID:    "654321",
		StudentAPIID: "999999",
	}
}

// TestReadOnlyWiring_LibraryAssignmentLookup drives the real library
// through the transport: with the assignments surface on, its lookup POST
// is the one write-shaped request that gets out; with it off, nothing
// does.
func TestReadOnlyWiring_LibraryAssignmentLookup(t *testing.T) {
	t.Run("surface enabled: the lookup is forwarded", func(t *testing.T) {
		rs := newRecordingServer(t, syntheticLookupResponse)
		got, err := libraryOn(t, rs, true).GetAssignments(context.Background(), "111111", lookupGrade())
		if err != nil {
			t.Fatalf("GetAssignments: %v", err)
		}
		if len(got) != 1 || got[0].Title != "Lab report" || got[0].Status != pslib.StatusMissing {
			t.Errorf("assignments = %+v", got)
		}
		if seen := rs.requests(); len(seen) != 1 || seen[0] != "POST "+AssignmentLookupPath {
			t.Errorf("server saw %v, want one lookup POST", seen)
		}
	})

	t.Run("surface disabled: the lookup is refused before it leaves", func(t *testing.T) {
		rs := newRecordingServer(t, syntheticLookupResponse)
		_, err := libraryOn(t, rs, false).GetAssignments(context.Background(), "111111", lookupGrade())
		if !errors.Is(err, connector.ErrWriteBlocked) {
			t.Fatalf("err = %v, want ErrWriteBlocked", err)
		}
		if classifyErr(err) != "write_blocked" {
			t.Errorf("classifyErr = %q, want write_blocked", classifyErr(err))
		}
		if seen := rs.requests(); len(seen) != 0 {
			t.Errorf("blocked lookup reached the server: %v", seen)
		}
	})
}

// TestReadOnlyWiring_SessionOnlyClientCannotLogIn: the connector hands the
// library a session and no credentials, so an expired session is an error
// rather than a silent browser login.
func TestReadOnlyWiring_SessionOnlyClientCannotLogIn(t *testing.T) {
	rs := newRecordingServer(t, "[]")
	lib, err := pslib.NewClient(rs.URL,
		pslib.WithHTTPClient(rs.client(t, true)),
		pslib.WithHTTPPageFetch(),
		pslib.WithSession([]*http.Cookie{{Name: "JSESSIONID", Value: "synthetic"}}, time.Now().Add(-time.Minute)),
		pslib.WithLogLevel(pslib.LogLevelNone),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if lib.HasCredentials() {
		t.Fatal("library client has credentials")
	}

	_, err = lib.GetAssignments(context.Background(), "111111", lookupGrade())
	if !pslib.IsSessionExpired(err) {
		t.Fatalf("err = %v, want session expired", err)
	}
	if err := lib.Authenticate(context.Background()); !errors.Is(err, pslib.ErrNoCredentials) {
		t.Errorf("Authenticate = %v, want ErrNoCredentials", err)
	}
	if seen := rs.requests(); len(seen) != 0 {
		t.Errorf("expired session still produced requests: %v", seen)
	}
}

// syntheticHome is a hand-written stand-in for /guardian/home.html in the
// shape the live page has: the student switcher with its explicit marker
// (on `selected`, or on nobody when ""), the switcher form and script, the
// student-demo table and one grades-table row. The number and the grade
// are the selected student's. No value comes from a real portal.
func syntheticHome(selected string) string {
	li := func(id, name string) string {
		attr := ""
		if id == selected {
			attr = `class="selected"`
		}
		return `<li ` + attr + `><a href="javascript:switchStudent(` + id + `);">` + name + `</a></li>`
	}
	letter, percent := "C", "70"
	if g := studentGrades[selected]; g != "" {
		letter, percent = splitGrade(g)
	}
	return `<!doctype html><html><body>
<ul id="students-list">` + li("111111", "Student One") + li("111112", "Student Two") + `</ul>
<form id="switch_student_form" action="home.html" method="post"><input type="hidden" name="selected_student_id"/></form>
<script>function switchStudent(studentId){ var f = document.getElementById('switch_student_form'); f.selected_student_id.value=studentId; f.submit(); }</script>
<table class="student-demo"><tr><td class="lbl">Student ID #:</td><td>` + studentNumbers[selected] + `</td></tr></table>
<table id="tblgrades">
<tr class="center">
  <td>1(A)</td>
  <td></td><td></td><td></td><td></td><td></td><td></td><td></td><td></td><td></td><td></td>
  <td>Example Science 7&nbsp;<br/><a href="mailto:teacher@example.org">Email Teacher, Example</a></td>
  <td><a href="scores.html?frn=00100&begdate=09/03/2025&enddate=11/05/2025&fg=Q1&schoolid=1">` + letter + `<br/>` + percent + `</a></td>
  <td></td><td></td><td></td><td></td><td></td>
  <td>2</td>
  <td>1</td>
</tr>
</table>
</body></html>`
}

// syntheticScores is a stand-in scores.html carrying the two ids the
// library scrapes; studentFRN is "001" + the student id it reports.
func syntheticScores(apiID string) string {
	return `<html><body><div class="xteContentWrapper" data-ng-init="studentFRN = '001` + apiID + `';">
<div data-pss-student-assignment-scores="" data-sectionid="654321"></div></div></body></html>`
}

// fakePortal is a stateful loopback stand-in for the portal. Like the real
// one it keeps the session's selected student on the server side, changes
// it on a post of the switcher form, and serves every page for whoever is
// selected at that moment.
type fakePortal struct {
	*recordingServer
	mu sync.Mutex

	selected     string // student id; "" with noMarker renders no marker
	noMarker     bool
	ignoreSwitch bool   // accept the form post, change nothing
	flipAfter    string // after the first scores page is served, select this id
	loginPage    bool   // answer the home page with the sign-in form (200)

	posts []string // switcher form bodies, in order
}

func newFakePortal(t *testing.T, selected string) *fakePortal {
	t.Helper()
	p := &fakePortal{selected: selected}
	p.recordingServer = newRecordingServer(t, "")
	p.recordingServer.handler = p.serve
	return p
}

const syntheticLogin = `<html><body><form id="LoginForm" action="/guardian/home.html" method="post">
<input type="text" id="fieldAccount" name="account"><input type="password" id="fieldPassword" name="pw"></form></body></html>`

func (p *fakePortal) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/guardian/home.html":
		switch {
		case p.loginPage:
			_, _ = w.Write([]byte(syntheticLogin))
		case p.noMarker:
			_, _ = w.Write([]byte(syntheticHome("")))
		default:
			_, _ = w.Write([]byte(syntheticHome(p.selected)))
		}
	case r.Method == http.MethodPost && r.URL.Path == SwitchStudentPath:
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		p.posts = append(p.posts, body)
		if !p.ignoreSwitch {
			p.selected = strings.TrimPrefix(body, SwitchStudentField+"=")
		}
		http.Redirect(w, r, "/guardian/home.html", http.StatusFound)
	case r.Method == http.MethodGet && r.URL.Path == "/guardian/scores.html":
		_, _ = w.Write([]byte(syntheticScores(studentAPIIDs[p.selected])))
		if p.flipAfter != "" {
			p.selected, p.flipAfter = p.flipAfter, ""
		}
	case r.Method == http.MethodPost && r.URL.Path == AssignmentLookupPath:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(syntheticLookupResponse))
	default:
		http.NotFound(w, r)
	}
}

func (p *fakePortal) formPosts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.posts...)
}

// TestReadOnlyWiring_LibraryPageReads drives the linked library's page
// reads through the transport. It runs against the powerschool-go version
// go.mod resolves (it does not skip): page reads are plain GETs, the home
// page is fetched once, and the selection comes from the explicit marker.
func TestReadOnlyWiring_LibraryPageReads(t *testing.T) {
	portal := newFakePortal(t, "111112")
	client, err := NewProductionClient(libraryOn(t, portal.recordingServer, true))
	if err != nil {
		t.Fatalf("NewProductionClient: %v", err)
	}

	page, err := client.GetHomePage(context.Background())
	if err != nil {
		t.Fatalf("GetHomePage: %v", err)
	}
	selected, err := SelectedStudent(page)
	if err != nil {
		t.Fatalf("SelectedStudent: %v", err)
	}
	// The marker is on the SECOND student; the page's details are theirs.
	if selected.ID != "111112" || selected.StudentNumber != "9000002" {
		t.Errorf("selected = %+v, want 111112 / 9000002", selected)
	}
	if first := page.Students[0]; first.Selected || first.StudentNumber != "" {
		t.Errorf("first student carries the selected student's details: %+v", first)
	}
	if len(page.Grades) != 1 || page.Grades[0].CourseName != "Example Science 7" ||
		page.Grades[0].Absences != 2 || page.Grades[0].StudentID != "111112" {
		t.Fatalf("grades = %+v", page.Grades)
	}

	if seen := portal.requests(); len(seen) != 1 || seen[0] != "GET /guardian/home.html" {
		t.Fatalf("server saw %v, want exactly one GET of the home page", seen)
	}
}

// TestEndToEnd_RealLibrary runs whole polls with the real library on the
// real transport against the loopback portal: the requests are built and
// the bytes parsed by powerschool-go, the transport is the production one,
// and nothing is handed over by a fake client.
func TestEndToEnd_RealLibrary(t *testing.T) {
	poll := func(t *testing.T, portal *fakePortal) (items []stagedItem, cp connector.Checkpoint, err error) {
		t.Helper()
		captureLogs(t)
		client, cerr := NewProductionClient(libraryOn(t, portal.recordingServer, true))
		if cerr != nil {
			t.Fatalf("NewProductionClient: %v", cerr)
		}
		c, cp, dir := newWiredConnector(t, client, testConfig())
		err = c.Poll(context.Background(), cp)
		return readItems(t, dir), cp, err
	}
	kidTurn := []string{"GET /guardian/scores.html", "POST " + AssignmentLookupPath, "GET /guardian/home.html"}
	home, post := "GET /guardian/home.html", "POST "+SwitchStudentPath

	// (a) The portal switches correctly.
	t.Run("switch works: every kid staged under its own label", func(t *testing.T) {
		portal := newFakePortal(t, "111111")
		items, _, err := poll(t, portal)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		assertKidItems(t, items, "k1", "k2")
		if got := portal.formPosts(); len(got) != 1 || got[0] != "selected_student_id=111112" {
			t.Errorf("switcher form posts = %q, want exactly [selected_student_id=111112]", got)
		}
		// k1: read, pages, confirm. k2: read, switch (+ the redirect the
		// client follows), read to confirm the switch, pages, confirm.
		want := append([]string{home}, kidTurn...)
		want = append(want, home, post, home, home)
		want = append(want, kidTurn...)
		if got := portal.requests(); strings.Join(got, ", ") != strings.Join(want, ", ") {
			t.Errorf("server saw\n  %v\nwant\n  %v", got, want)
		}
	})

	t.Run("switch works from the other child", func(t *testing.T) {
		portal := newFakePortal(t, "111112")
		items, _, err := poll(t, portal)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		assertKidItems(t, items, "k1", "k2")
		if got := strings.Join(portal.formPosts(), ","); got != "selected_student_id=111111,selected_student_id=111112" {
			t.Errorf("switcher form posts = %q", got)
		}
	})

	// (b) The portal ignores the switch.
	t.Run("switch ignored: the unselectable kid gets nothing", func(t *testing.T) {
		portal := newFakePortal(t, "111111")
		portal.ignoreSwitch = true
		items, cp, err := poll(t, portal)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		assertKidItems(t, items, "k1")
		assertNoFingerprints(t, cp, "k2")
		scores := 0
		for _, r := range portal.requests() {
			if r == "GET /guardian/scores.html" {
				scores++
			}
		}
		if scores != 1 {
			t.Errorf("%d scores pages read, want 1 (none for the kid that could not be selected)", scores)
		}
	})

	// (c) The selection flips while a kid's pages are being read.
	t.Run("selection flips mid-read: that kid's poll is discarded", func(t *testing.T) {
		portal := newFakePortal(t, "111111")
		portal.flipAfter = "111112"
		items, cp, err := poll(t, portal)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		// k1's turn read a scores page as k1 and then found the session on
		// the other child: nothing of k1's is staged. k2's turn is clean.
		assertKidItems(t, items, "k2")
		assertNoFingerprints(t, cp, "k1")
		if got := portal.formPosts(); len(got) != 0 {
			t.Errorf("switcher form posted %q; the connector must not move the selection back and carry on", got)
		}
	})

	// The markup the cross-child defect needed: no marker at all. The old
	// library enriched the first student and k1 was staged with the second
	// child's records.
	t.Run("no marker: nothing staged, nothing posted, one read", func(t *testing.T) {
		portal := newFakePortal(t, "111112")
		portal.noMarker = true
		items, cp, err := poll(t, portal)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		if len(items) != 0 {
			t.Fatalf("staged %d items without a selection marker", len(items))
		}
		assertNoFingerprints(t, cp, "k1", "k2")
		if got := portal.requests(); len(got) != 1 || got[0] != home {
			t.Errorf("server saw %v, want only one read of the home page", got)
		}
	})

	t.Run("dead session answered with the login form and a 200", func(t *testing.T) {
		portal := newFakePortal(t, "111111")
		portal.loginPage = true
		items, _, err := poll(t, portal)
		if err == nil || !connector.IsPermanent(err) {
			t.Fatalf("Poll = %v, want a PermanentError (session rejected)", err)
		}
		if len(items) != 0 {
			t.Errorf("staged %d items from a login page", len(items))
		}
		if got := portal.requests(); len(got) != 1 {
			t.Errorf("server saw %v, want one request", got)
		}
	})

	// The transport is what makes the switch possible, and only for the
	// configured ids: a library asked to select a student the connector
	// was not configured for gets no further than the transport.
	t.Run("a switch to an unconfigured student never leaves", func(t *testing.T) {
		portal := newFakePortal(t, "111111")
		client := portal.client(t, true)
		client.Transport.(*Transport).SwitchStudentIDs = []string{"111111"} // 111112 not configured
		lib, err := pslib.NewClient(portal.URL,
			pslib.WithHTTPClient(client),
			pslib.WithHTTPPageFetch(),
			pslib.WithSession([]*http.Cookie{{Name: "JSESSIONID", Value: "synthetic"}}, time.Now().Add(time.Hour)),
			pslib.WithLogLevel(pslib.LogLevelNone),
		)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		_, err = lib.SelectStudent(context.Background(), "111112")
		if !errors.Is(err, connector.ErrWriteBlocked) {
			t.Fatalf("SelectStudent = %v, want ErrWriteBlocked", err)
		}
		if got := portal.formPosts(); len(got) != 0 {
			t.Errorf("switcher form reached the portal: %q", got)
		}
	})
}
