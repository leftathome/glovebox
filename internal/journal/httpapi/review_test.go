package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/satoken"
)

// The caps follow the widest record type version.
func TestSnapshotKeyLimitsFollowTheRegistry(t *testing.T) {
	fields := map[string]record.FieldDef{}
	for i := 0; i < 30; i++ {
		fields[fmt.Sprintf("f%d", i)] = record.FieldDef{Type: record.String}
	}
	wide := record.Type{Name: "wide_type", SchemaVersion: 1, Owner: "powerschool", DefaultAudience: []string{"guardians"}, Fields: fields}
	got := snapshotKeyLimits(journaltest.Registry(t, wide))
	if got.MaxObjectKeys != 30+16 || got.MaxTotalKeys != 12+record.MaxRecords*(4+30)+1024 {
		t.Errorf("wide registry: %+v", got)
	}
	if got := snapshotKeyLimits(journaltest.Registry(t)); got.MaxObjectKeys != 9+16 || got.MaxTotalKeys != 12+record.MaxRecords*(4+9)+1024 {
		t.Errorf("fixtures: %+v", got)
	}
}

// exchange sends req and returns the response with its body read.
func exchange(t *testing.T, req *http.Request) (*http.Response, journalapi.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body journalapi.Response
	_ = json.Unmarshal(raw, &body)
	return resp, body, raw
}

func request(t *testing.T, r *rig, method, path string, body io.Reader, headers ...string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	return req
}

// A token TokenReview rejects is a 401 with a Bearer challenge.
func TestRejectedTokenHasChallenge(t *testing.T) {
	r := newAuthRig(t)
	for _, tok := range []string{"never-issued", "denied", "wrong-audience"} {
		resp, body, _ := exchange(t, request(t, r, "GET", journalapi.PathHealthz, nil, "Authorization", "Bearer "+tok))
		if resp.StatusCode != 401 || body.Reason != journalapi.ReasonUnauthenticated || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: %d %+v %q", tok, resp.StatusCode, body, resp.Header.Get("WWW-Authenticate"))
		}
	}
	resp, _, _ := exchange(t, request(t, r, "GET", journalapi.PathHealthz, nil, "Authorization", "Bearer rss"))
	if resp.StatusCode != 403 || resp.Header.Get("WWW-Authenticate") != "" {
		t.Errorf("forbidden: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

// Exactly one Authorization header of the form "Bearer <token>".
func TestBearerFormIsExact(t *testing.T) {
	r := newAuthRig(t)
	for name, c := range map[string]struct {
		headers []string
		status  int
	}{
		"plain":                {[]string{"Authorization", "Bearer gw"}, 200},
		"lower-case scheme":    {[]string{"Authorization", "bearer gw"}, 200},
		"upper-case scheme":    {[]string{"Authorization", "BEARER gw"}, 200},
		"two headers":          {[]string{"Authorization", "Bearer gw", "Authorization", "Bearer gw"}, 401},
		"good then bad":        {[]string{"Authorization", "Bearer gw", "Authorization", "Bearer x"}, 401},
		"bad then good":        {[]string{"Authorization", "Bearer x", "Authorization", "Bearer gw"}, 401},
		"two spaces":           {[]string{"Authorization", "Bearer  gw"}, 401},
		"tab":                  {[]string{"Authorization", "Bearer\tgw"}, 401},
		"no space":             {[]string{"Authorization", "Bearergw"}, 401},
		"comma":                {[]string{"Authorization", "Bearer gw,ps"}, 401},
		"other scheme":         {[]string{"Authorization", "Token gw"}, 401},
		"token68 padding only": {[]string{"Authorization", "Bearer ="}, 401},
	} {
		var n int
		before, _, _ := r.api.Requests()
		resp, body, _ := exchange(t, request(t, r, "GET", journalapi.PathHealthz, nil, c.headers...))
		after, _, _ := r.api.Requests()
		n = len(after) - len(before)
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d %+v", name, resp.StatusCode, body)
		}
		if c.status == 401 && (n != 0 || body.Reason != journalapi.ReasonUnauthenticated || resp.Header.Get("WWW-Authenticate") != "Bearer") {
			t.Errorf("%s: %d reviews, %+v", name, n, body)
		}
	}
	for _, tok := range []string{"a.b-c_d~e+f/g==", strings.Repeat("t", maxBearerBytes)} {
		if got, ok := bearerToken(request(t, r, "GET", "/", nil, "Authorization", "Bearer "+tok)); !ok || got != tok {
			t.Errorf("token68 %.20q refused", tok)
		}
	}
	if _, ok := bearerToken(request(t, r, "GET", "/", nil, "Authorization", "Bearer "+strings.Repeat("t", maxBearerBytes+1))); ok {
		t.Error("an oversized token accepted")
	}
}

// Every answer is JSON that is neither cached nor sniffed.
func TestResponseHeaders(t *testing.T) {
	r := newAuthRig(t)
	for name, req := range map[string]*http.Request{
		"ok":        request(t, r, "GET", journalapi.PathHealthz, nil, "Authorization", "Bearer gw"),
		"refused":   request(t, r, "GET", journalapi.PathHealthz, nil),
		"unrouted":  request(t, r, "GET", "/nowhere", nil),
		"forbidden": request(t, r, "POST", journalapi.PathRecords, strings.NewReader("{}"), "Authorization", "Bearer dash", "Content-Type", "application/json"),
	} {
		resp, _, _ := exchange(t, req)
		if resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("Cache-Control") != "no-store" ||
			resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %v", name, resp.Header)
		}
	}
}

// A chunked body (no Content-Length) over 8 MiB is 413, and the connection
// is closed rather than drained.
func TestChunkedBodyOverLimit(t *testing.T) {
	r := newAuthRig(t)
	body := io.MultiReader(strings.NewReader(`{"snapshot_id": "`), bytes.NewReader(bytes.Repeat([]byte("x"), 9<<20)), strings.NewReader(`"}`))
	req := request(t, r, "POST", journalapi.PathRecords, body, "Authorization", "Bearer ps", "Content-Type", "application/json")
	if req.ContentLength != 0 {
		t.Fatalf("the request has a length: %d", req.ContentLength)
	}
	resp, out, _ := exchange(t, req)
	if resp.StatusCode != 413 || out.Reason != journalapi.ReasonTooLarge || !resp.Close {
		t.Errorf("chunked 9 MiB: %d %+v close=%v", resp.StatusCode, out, resp.Close)
	}
}

// One byte over the limit with a Content-Length: net/http would drain so
// small a rest and keep the connection; the journal closes it instead.
func TestOverLimitClosesTheConnection(t *testing.T) {
	r := newAuthRig(t)
	body := bytes.Repeat([]byte(" "), maxSnapshotBytes+1)
	resp, out, _ := exchange(t, request(t, r, "POST", journalapi.PathRecords, bytes.NewReader(body), "Authorization", "Bearer ps", "Content-Type", "application/json"))
	if resp.StatusCode != 413 || out.Reason != journalapi.ReasonTooLarge || !resp.Close {
		t.Errorf("8 MiB + 1: %d %+v close=%v", resp.StatusCode, out, resp.Close)
	}
}

// Content-Type is checked before the body is read: a large body of the
// wrong type is 400, not 413.
func TestContentTypeBeforeBody(t *testing.T) {
	r := newAuthRig(t)
	big := bytes.Repeat([]byte("x"), 9<<20)
	resp, out, _ := exchange(t, request(t, r, "POST", journalapi.PathRecords, bytes.NewReader(big), "Authorization", "Bearer ps", "Content-Type", "text/plain"))
	if resp.StatusCode != 400 || out.Reason != journalapi.ReasonBadRequest {
		t.Errorf("%d %+v", resp.StatusCode, out)
	}
}

type failingBody struct{ prefix io.Reader }

func (f *failingBody) Read(p []byte) (int, error) {
	if n, err := f.prefix.Read(p); n > 0 || err != io.EOF {
		return n, err
	}
	return 0, errors.New("i/o timeout")
}

// A body that cannot be read (a timeout, a broken connection) is not the
// snapshot's fault: 500, never a 4xx the spool would treat as final.
func TestBodyReadErrorIs500(t *testing.T) {
	r := newAuthRig(t)
	req := httptest.NewRequest("POST", journalapi.PathRecords, &failingBody{prefix: strings.NewReader(`{"snapshot_id":`)})
	req.Header.Set("Authorization", "Bearer ps")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.s.Handler().ServeHTTP(rec, req)
	resp := decodeRefusal(t, rec)
	if rec.Code != 500 || resp.Reason != journalapi.ReasonInternal || resp.Detail != "" || rec.Header().Get("Connection") != "close" {
		t.Errorf("%d %+v %v", rec.Code, resp, rec.Header())
	}
}

// keyBody is a POST /v1/records body with exactly total keys, none in an
// object wider than perObject, and an unparseable id so the request never
// needs the database.
func keyBody(total, perObject int) []byte {
	var sb strings.Builder
	top := []string{`"snapshot_id":"x"`, `"record_type":"assignment"`, `"schema_version":1`, `"scope":[]`,
		`"complete":true`, `"complete_range":null`, `"observed_at":"2026-10-16T12:00:00Z"`, `"approximate":false`}
	sb.WriteString("{" + strings.Join(top, ",") + `,"records":[`)
	left := total - 9
	for i := 0; left > 0; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		n := min(perObject, left-3)
		if left <= 3 {
			// Fewer keys than a record has: an object of that many.
			sb.WriteByte('{')
			for j := 0; j < left; j++ {
				if j > 0 {
					sb.WriteByte(',')
				}
				fmt.Fprintf(&sb, `"k%d":1`, j)
			}
			sb.WriteByte('}')
			break
		}
		fmt.Fprintf(&sb, `{"key":"k%d","data_subject":"e_child1","fields":{`, i)
		for j := 0; j < n; j++ {
			if j > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `"f%d":1`, j)
		}
		sb.WriteString("}}")
		left -= 3 + n
	}
	sb.WriteString("]}")
	return []byte(sb.String())
}

// The key caps of a snapshot body, at and over each boundary.
func TestSnapshotKeyCaps(t *testing.T) {
	r := newAuthRig(t)
	lim := r.s.snapshotKeys
	if lim.MaxObjectKeys != 25 || lim.MaxTotalKeys != 66036 {
		t.Fatalf("limits %+v", lim)
	}
	post := func(b []byte) (int, journalapi.Response) {
		resp, out, _ := exchange(t, request(t, r, "POST", journalapi.PathRecords, bytes.NewReader(b), "Authorization", "Bearer ps", "Content-Type", "application/json"))
		return resp.StatusCode, out
	}
	const tooMany = "the body has more keys than any snapshot can"
	for name, c := range map[string]struct {
		body   []byte
		capped bool
	}{
		"object at the cap":   {keyBody(9+3+lim.MaxObjectKeys, lim.MaxObjectKeys), false},
		"object over the cap": {keyBody(9+3+lim.MaxObjectKeys+1, lim.MaxObjectKeys+1), true},
		"total at the cap":    {keyBody(lim.MaxTotalKeys, 20), false},
		"total over the cap":  {keyBody(lim.MaxTotalKeys+1, 20), true},
	} {
		code, out := post(c.body)
		if c.capped && (code != 413 || out.Reason != journalapi.ReasonTooLarge || out.Detail != tooMany) {
			t.Errorf("%s: %d %+v", name, code, out)
		}
		if !c.capped && (code == 413 && out.Detail == tooMany || code/100 != 4) {
			t.Errorf("%s: %d %+v", name, code, out)
		}
	}
	// 100k keys in one object are refused in well under a second.
	var sb strings.Builder
	sb.WriteString(`{"records":[{"fields":{`)
	for i := 0; i < 100_000; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"k%d":1`, i)
	}
	sb.WriteString("}}]}")
	start := time.Now()
	code, out := post([]byte(sb.String()))
	if code != 413 || out.Detail != tooMany {
		t.Errorf("100k keys: %d %+v", code, out)
	}
	if el := time.Since(start); el > time.Second {
		t.Errorf("100k keys took %s", el)
	}
}

// gateAuth holds every review until released.
type gateAuth struct {
	entered chan struct{}
	release chan struct{}
}

func (g gateAuth) Authenticate(ctx context.Context, _ string) (satoken.Caller, error) {
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
	}
	return satoken.Caller{Namespace: "openclaw", Name: "openclaw-gateway"}, nil
}

// At most 16 requests do work at once; the next is 503 with Retry-After
// and never reaches TokenReview.
func TestInFlightLimit(t *testing.T) {
	g := gateAuth{entered: make(chan struct{}, 64), release: make(chan struct{})}
	r := newAuthRig(t, func(d *Deps) { d.Auth = g })
	if DefaultMaxInFlight != 16 {
		t.Fatalf("DefaultMaxInFlight = %d", DefaultMaxInFlight)
	}
	var wg sync.WaitGroup
	codes := make(chan int, DefaultMaxInFlight)
	for i := 0; i < DefaultMaxInFlight; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.DefaultClient.Do(request(t, r, "GET", journalapi.PathHealthz, nil, "Authorization", "Bearer gw"))
			if err != nil {
				codes <- 0
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	for i := 0; i < DefaultMaxInFlight; i++ {
		select {
		case <-g.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d requests reached the authenticator", i)
		}
	}
	resp, out, _ := exchange(t, request(t, r, "GET", journalapi.PathHealthz, nil, "Authorization", "Bearer gw"))
	if resp.StatusCode != 503 || out.Reason != journalapi.ReasonUnavailable || resp.Header.Get("Retry-After") != "1" {
		t.Errorf("request 17: %d %+v %v", resp.StatusCode, out, resp.Header)
	}
	if len(g.entered) != 0 {
		t.Errorf("request 17 reached the authenticator")
	}
	close(g.release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Errorf("a held request answered %d", c)
		}
	}
	// The slots are free again, also after a panic.
	r2 := newAuthRig(t, func(d *Deps) { d.MaxInFlight = 1 })
	h := r2.s.route("records", func(http.ResponseWriter, *http.Request, call) { panic("x") })
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("Authorization", "Bearer gw")
		h.ServeHTTP(rec, req)
		if rec.Code != 500 {
			t.Errorf("attempt %d after a panic: %d", i, rec.Code)
		}
	}
}

// http.ErrAbortHandler is passed on: the connection is cut, no answer is
// made up.
func TestAbortHandlerIsPassedOn(t *testing.T) {
	r := newAuthRig(t)
	srv := httptest.NewServer(r.s.route("records", func(http.ResponseWriter, *http.Request, call) { panic(http.ErrAbortHandler) }))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Authorization", "Bearer gw")
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Errorf("an aborted handler answered %d", resp.StatusCode)
	}
	if strings.Contains(r.logs.String(), "panicked") {
		t.Errorf("an abort was logged as a panic")
	}
}
