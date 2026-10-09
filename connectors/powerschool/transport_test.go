package powerschool

import (
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

// recordingServer is a TLS test server that records "<METHOD> <path>" for
// every request that reaches it. A request the transport blocks never
// shows up here, which is the assertion that matters.
type recordingServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newRecordingServer(t *testing.T, body string) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		rs.seen = append(rs.seen, r.Method+" "+r.URL.Path)
		rs.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
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

// client returns an http.Client on a Transport pinned to this server.
func (rs *recordingServer) client(t *testing.T, allowLookup bool) *http.Client {
	t.Helper()
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &Transport{
			Host:                  rs.host(t),
			AllowAssignmentLookup: allowLookup,
			Base:                  rs.Server.Client().Transport,
		},
	}
}

func TestTransport(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		path        string // relative to the pinned server unless absURL is set
		absURL      string
		allowLookup bool
		wantErr     error // nil = forwarded
	}{
		{name: "GET page", method: http.MethodGet, path: "/guardian/home.html"},
		{name: "HEAD page", method: http.MethodHead, path: "/guardian/home.html"},
		{name: "GET with lookup allowed", method: http.MethodGet, path: "/guardian/scores.html", allowLookup: true},
		{name: "POST lookup when allowed", method: http.MethodPost, path: AssignmentLookupPath, allowLookup: true},
		{name: "POST lookup when not allowed", method: http.MethodPost, path: AssignmentLookupPath, wantErr: connector.ErrWriteBlocked},
		{name: "POST another ws endpoint", method: http.MethodPost, path: "/ws/xte/assignment/submit", allowLookup: true, wantErr: connector.ErrWriteBlocked},
		{name: "POST lookup with a suffix", method: http.MethodPost, path: AssignmentLookupPath + "/extra", allowLookup: true, wantErr: connector.ErrWriteBlocked},
		{name: "POST lookup with a prefix", method: http.MethodPost, path: "/x" + AssignmentLookupPath, allowLookup: true, wantErr: connector.ErrWriteBlocked},
		{name: "POST student switch form", method: http.MethodPost, path: "/guardian/home.html", allowLookup: true, wantErr: connector.ErrWriteBlocked},
		{name: "POST login form", method: http.MethodPost, path: "/guardian/home.html", wantErr: connector.ErrWriteBlocked},
		{name: "PUT lookup path", method: http.MethodPut, path: AssignmentLookupPath, allowLookup: true, wantErr: connector.ErrWriteBlocked},
		{name: "PATCH lookup path", method: http.MethodPatch, path: AssignmentLookupPath, allowLookup: true, wantErr: connector.ErrWriteBlocked},
		{name: "DELETE lookup path", method: http.MethodDelete, path: AssignmentLookupPath, allowLookup: true, wantErr: connector.ErrWriteBlocked},
		{name: "GET another host", method: http.MethodGet, absURL: "https://other.example.org/guardian/home.html", wantErr: ErrHostBlocked},
		{name: "POST lookup on another host", method: http.MethodPost, absURL: "https://other.example.org" + AssignmentLookupPath, allowLookup: true, wantErr: ErrHostBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRecordingServer(t, "[]")
			target := rs.URL + tc.path
			if tc.absURL != "" {
				target = tc.absURL
			}
			req, err := http.NewRequest(tc.method, target, strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}

			resp, err := rs.client(t, tc.allowLookup).Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("request refused: %v", err)
				}
				if got := rs.requests(); len(got) != 1 || got[0] != tc.method+" "+tc.path {
					t.Errorf("server saw %v, want one %s %s", got, tc.method, tc.path)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got := rs.requests(); len(got) != 0 {
				t.Errorf("blocked request reached the server: %v", got)
			}
		})
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

// TestTransport_RedirectOffHostBlocked: a redirect cannot carry the
// request (and the cookie header the library set) to another origin.
func TestTransport_RedirectOffHostBlocked(t *testing.T) {
	other := newRecordingServer(t, "[]")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/landing", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	client := &http.Client{Transport: &Transport{Host: u.Host, Base: srv.Client().Transport}}

	resp, err := client.Get(srv.URL + "/guardian/home.html")
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

func TestNewHTTPClient(t *testing.T) {
	c := NewHTTPClient("PS.Example.ORG", true, 7*time.Second)
	tr, ok := c.Transport.(*Transport)
	if !ok {
		t.Fatalf("transport is %T, want *Transport", c.Transport)
	}
	if tr.Host != "ps.example.org" || !tr.AllowAssignmentLookup {
		t.Errorf("transport = %+v", tr)
	}
	if c.Jar != nil {
		t.Error("client has a cookie jar; the library owns the cookies")
	}
	if c.Timeout != 7*time.Second {
		t.Errorf("timeout = %v", c.Timeout)
	}
}

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

// syntheticHomePage is a hand-written stand-in for /guardian/home.html with
// the two structures the library parses: the student switcher and one
// grades-table row. No part of it comes from a real portal.
const syntheticHomePage = `<!doctype html><html><body>
<ul id="students-list">
  <li class="selected"><a href="javascript:switchStudent(111111);">Student One</a></li>
  <li><a href="javascript:switchStudent(111112);">Student Two</a></li>
</ul>
<table class="student-demo"><tr><td class="lbl">Student ID #:</td><td>9000001</td></tr></table>
<table id="tblgrades">
<tr class="center">
  <td>1(A)</td>
  <td></td><td></td><td></td><td></td><td></td><td></td><td></td><td></td><td></td><td></td>
  <td>Example Science 7&nbsp;<br/><a href="mailto:teacher@example.org">Email Teacher, Example</a></td>
  <td><a href="scores.html?frn=00100&begdate=09/03/2025&enddate=11/05/2025&fg=Q1&schoolid=1">B+<br/>88</a></td>
  <td></td><td></td><td></td><td></td><td></td>
  <td>2</td>
  <td>1</td>
</tr>
</table>
</body></html>`

// TestReadOnlyWiring_LibraryPageReads drives the real library's page reads
// through the transport and asserts that only GETs reach the server.
//
// It SKIPS against a powerschool-go without HTTP page fetch (v0.1.0): there
// GetStudents would start a headless Chrome, which is exactly what the
// connector refuses to do. It starts running, unchanged, once go.mod pins a
// release that has the capability.
func TestReadOnlyWiring_LibraryPageReads(t *testing.T) {
	rs := newRecordingServer(t, syntheticHomePage)
	lib := libraryOn(t, rs, true)
	client, err := NewProductionClient(lib)
	if errors.Is(err, ErrBrowserReads) {
		t.Skip("linked powerschool-go has no HTTP page fetch; page reads cannot be exercised without a browser")
	}
	if err != nil {
		t.Fatalf("NewProductionClient: %v", err)
	}

	ctx := context.Background()
	students, err := client.GetStudents(ctx)
	if err != nil {
		t.Fatalf("GetStudents: %v", err)
	}
	selected, err := SelectedStudentID(students)
	if err != nil || selected != "111111" {
		t.Fatalf("SelectedStudentID = (%q, %v), want 111111", selected, err)
	}
	grades, err := client.GetGrades(ctx, selected)
	if err != nil {
		t.Fatalf("GetGrades: %v", err)
	}
	if len(grades) != 1 || grades[0].CourseName != "Example Science 7" || grades[0].Absences != 2 {
		t.Fatalf("grades = %+v", grades)
	}

	seen := rs.requests()
	if len(seen) != 2 {
		t.Fatalf("server saw %v, want two page reads", seen)
	}
	for _, r := range seen {
		if r != "GET /guardian/home.html" {
			t.Errorf("unexpected request %q", r)
		}
	}
}
