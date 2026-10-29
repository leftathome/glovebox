package records

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

func newTokenFile(t *testing.T, content string) string {
	t.Helper()
	tok := filepath.Join(t.TempDir(), "token")
	mustWrite(t, tok, content)
	return tok
}

func TestSendPresentsTheProjectedToken(t *testing.T) {
	var gotAuth, gotType, gotPath, gotQuery, gotMethod string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType, gotPath, gotQuery, gotMethod = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path, r.URL.RawQuery, r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"applied","stats":{"created":1}}`)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL+"/", TokenFile(newTokenFile(t, "tok-1\n")), time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"snapshot_id":"x"}`)
	a, err := c.Send(context.Background(), body)
	if err != nil || a.Code != 200 || a.Status != "applied" || a.Reason != "" || a.RetryAfter != 0 {
		t.Fatalf("answer %+v err %v", a, err)
	}
	if gotMethod != http.MethodPost || gotAuth != "Bearer tok-1" || gotType != "application/json" || gotPath != journalapi.PathRecords || gotQuery != "" || !bytes.Equal(gotBody, body) {
		t.Fatalf("request: %q %q %q %q %q %q", gotMethod, gotAuth, gotType, gotPath, gotQuery, gotBody)
	}
}

func TestSendRereadsTheTokenFile(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"status":"applied"}`)
	}))
	defer srv.Close()
	tok := newTokenFile(t, "tok-1")
	c, _ := NewClient(srv.URL, TokenFile(tok), time.Second, nil)
	_, _ = c.Send(context.Background(), []byte(`{}`))
	mustWrite(t, tok, " tok-2 \n")
	_, _ = c.Send(context.Background(), []byte(`{}`))
	if strings.Join(seen, ",") != "Bearer tok-1,Bearer tok-2" {
		t.Fatalf("tokens seen %v", seen)
	}
}

func TestSendWithoutATokenSendsNothing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	empty := newTokenFile(t, " \n")
	for _, src := range []func() (string, error){TokenFile(empty), TokenFile(filepath.Join(t.TempDir(), "missing"))} {
		c, _ := NewClient(srv.URL, src, time.Second, nil)
		if _, err := c.Send(context.Background(), []byte(`{}`)); err == nil {
			t.Fatal("no error without a token")
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests without a token", hits.Load())
	}
}

func TestSendErrorsNeverCarryTheTokenOrTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()
	tok := newTokenFile(t, "secret-token-value")
	c, _ := NewClient(srv.URL, TokenFile(tok), time.Second, nil)
	_, err := c.Send(context.Background(), []byte(`{"value":"secret-body-value"}`))
	if err == nil {
		t.Fatal("a dropped connection is not an error")
	}
	if strings.Contains(err.Error(), "secret-token-value") || strings.Contains(err.Error(), "secret-body-value") {
		t.Fatalf("error leaks: %v", err)
	}
	failing := func() (string, error) { return "", errors.New("could not read secret-token-value") }
	c2, _ := NewClient(srv.URL, failing, time.Second, nil)
	if _, err := c2.Send(context.Background(), nil); err == nil || strings.Contains(err.Error(), "secret-token-value") {
		t.Fatalf("token source error leaks: %v", err)
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	var otherHits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherHits.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+journalapi.PathRecords, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, TokenFile(newTokenFile(t, "t")), time.Second, nil)
	a, err := c.Send(context.Background(), []byte(`{}`))
	if err != nil || a.Code != http.StatusTemporaryRedirect || otherHits.Load() != 0 {
		t.Fatalf("answer %+v err %v other hits %d", a, err, otherHits.Load())
	}
	if Classify(a.Code, a.Status, a.Reason) != ActRetryAlert {
		t.Fatal("a redirect is not alerted on")
	}
}

func TestClientRefusesOtherHosts(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	c, _ := NewClient("http://journal.invalid:9095", TokenFile(newTokenFile(t, "t")), time.Second, nil)
	resp, err := c.http.Get(other.URL)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrHostBlocked) || hits.Load() != 0 {
		t.Fatalf("err %v hits %d", err, hits.Load())
	}
}

// recordingRT counts the requests that got past the pin.
type recordingRT struct{ hits atomic.Int32 }

func (r *recordingRT) RoundTrip(*http.Request) (*http.Response, error) {
	r.hits.Add(1)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

// The pin checks the Host header and refuses userinfo, not only the URL's
// scheme and host.
func TestPinnedRefusesAnotherHostHeaderOrUserinfo(t *testing.T) {
	base := &recordingRT{}
	p := &pinned{scheme: "http", host: "journal.invalid:9095", base: base}
	mk := func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, "http://journal.invalid:9095"+journalapi.PathRecords, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	if resp, err := p.RoundTrip(mk()); err != nil || resp.StatusCode != 200 || base.hits.Load() != 1 {
		t.Fatalf("the journal's own request: %v (hits %d)", err, base.hits.Load())
	}
	hostHeader := mk()
	hostHeader.Host = "other.invalid"
	withUser := mk()
	withUser.URL.User = url.UserPassword("u", "p")
	for name, req := range map[string]*http.Request{"host header": hostHeader, "userinfo": withUser} {
		if _, err := p.RoundTrip(req); !errors.Is(err, ErrHostBlocked) {
			t.Errorf("%s: err %v", name, err)
		}
	}
	if base.hits.Load() != 1 {
		t.Fatalf("%d requests forwarded, want 1", base.hits.Load())
	}
}

func TestNewClientRefusesBadAddresses(t *testing.T) {
	tok := TokenFile("/t")
	for _, u := range []string{"", "ftp://j", "http://j/v1", "http://u@j", "http://j?a=1", "http://j#f", "http://"} {
		if _, err := NewClient(u, tok, time.Second, nil); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if _, err := NewClient("http://j", nil, time.Second, nil); err == nil {
		t.Error("nil token source accepted")
	}
}

func TestRetryAfter(t *testing.T) {
	for header, want := range map[string]time.Duration{"1": time.Second, "120": 2 * time.Minute, "": 0, "x": 0, "-5": 0,
		"Wed, 21 Oct 2026 07:28:00 GMT": 0, "999999999": 24 * time.Hour} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if header != "" {
				w.Header().Set("Retry-After", header)
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"status":"rejected","reason":"unavailable"}`)
		}))
		c, _ := NewClient(srv.URL, TokenFile(newTokenFile(t, "t")), time.Second, nil)
		a, err := c.Send(context.Background(), []byte(`{}`))
		srv.Close()
		if err != nil || a.Code != 503 || a.Reason != journalapi.ReasonUnavailable || a.RetryAfter != want {
			t.Errorf("Retry-After %q: %+v err %v, want %v", header, a, err, want)
		}
	}
}

// An answer that is not the journal's JSON keeps its HTTP code and has no
// status or reason: a 5xx is retried, anything else is kept and alerted on.
func TestNonJSONAnswer(t *testing.T) {
	for code, want := range map[int]Action{200: ActRetryAlert, 404: ActRetryAlert, 502: ActRetry} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, "<html>gateway</html>")
		}))
		c, _ := NewClient(srv.URL, TokenFile(newTokenFile(t, "t")), time.Second, nil)
		a, err := c.Send(context.Background(), []byte(`{}`))
		srv.Close()
		if err != nil || a.Code != code || a.Status != "" || a.Reason != "" || Classify(a.Code, a.Status, a.Reason) != want {
			t.Errorf("%d: %+v err %v", code, a, err)
		}
	}
}

func TestOversizedAnswerIsNotTrusted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"applied","detail":"`+strings.Repeat("a", 70<<10)+`"}`)
	}))
	defer srv.Close()
	c, _ := NewClient(srv.URL, TokenFile(newTokenFile(t, "t")), time.Second, nil)
	a, err := c.Send(context.Background(), []byte(`{}`))
	if err != nil || a.Status != "" {
		t.Fatalf("%+v %v", a, err)
	}
}

func TestSendIsBoundedByTheTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	c, _ := NewClient(srv.URL, TokenFile(newTokenFile(t, "t")), 50*time.Millisecond, nil)
	start := time.Now()
	_, err := c.Send(context.Background(), []byte(`{}`))
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestClientTimeoutDefault(t *testing.T) {
	c, _ := NewClient("http://j:9095", TokenFile("/t"), 0, nil)
	if c.http.Timeout != DefaultTimeoutSeconds*time.Second {
		t.Fatalf("timeout %v", c.http.Timeout)
	}
}
