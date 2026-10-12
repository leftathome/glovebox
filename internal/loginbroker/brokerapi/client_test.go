package brokerapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const callerToken = "projected-token-for-the-broker-audience"

func tokenSource() (string, error) { return callerToken, nil }

func TestClientSendsOneAuthenticatedRequestWithAFingerprintPrefix(t *testing.T) {
	var got struct {
		method, path, auth, contentType string
		body                            Request
	}
	until := time.Date(2026, 10, 13, 4, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		got.auth, got.contentType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_ = json.NewEncoder(w).Encode(Response{Site: "powerschool", Outcome: OutcomeDeferred, Code: CodeBlackout, RetryAfter: &until})
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, tokenSource, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	full := strings.Repeat("ab12", 16) // a 64-digit fingerprint
	resp, err := c.Refresh(context.Background(), ReasonSessionDead, full)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got.method != "POST" || got.path != RefreshPath || got.auth != "Bearer "+callerToken || got.contentType != "application/json" {
		t.Errorf("request = %+v", got)
	}
	if got.body.Reason != ReasonSessionDead || got.body.Fingerprint != full[:16] || !ValidFingerprint(got.body.Fingerprint) {
		t.Errorf("body = %+v: only a prefix of the fingerprint may travel", got.body)
	}
	if resp.Outcome != OutcomeDeferred || resp.Code != CodeBlackout || resp.RetryAfter == nil || !resp.RetryAfter.Equal(until) || resp.HasSession() {
		t.Errorf("answer = %+v", resp)
	}

	// No session: "none".
	if _, err := c.Refresh(context.Background(), ReasonNoSession, ""); err != nil || got.body.Fingerprint != NoFingerprint {
		t.Errorf("no session: fingerprint %q, %v", got.body.Fingerprint, err)
	}
}

// The token goes to the broker's address and nowhere else: not to another
// host, not after a redirect, and no error quotes it.
func TestClientIsPinnedToTheBroker(t *testing.T) {
	elsewhere := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere++ }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+RefreshPath, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, tokenSource, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Refresh(context.Background(), ReasonSessionDead, "")
	var se *StatusError
	if !errors.As(err, &se) || se.Status != http.StatusTemporaryRedirect {
		t.Fatalf("a redirect: %v", err)
	}
	if elsewhere != 0 {
		t.Fatal("the redirect was followed, token and all")
	}
	// The transport itself refuses another host.
	req, _ := http.NewRequest(http.MethodPost, other.URL+RefreshPath, nil)
	if resp, err := c.http.Do(req); err == nil || !errors.Is(err, ErrHostBlocked) {
		if resp != nil {
			resp.Body.Close()
		}
		t.Errorf("a request to another host: %v", err)
	}

	srv.Close()
	_, err = c.Refresh(context.Background(), ReasonSessionDead, "")
	if err == nil || strings.Contains(err.Error(), callerToken) || strings.Contains(err.Error(), RefreshPath) {
		t.Errorf("broker gone: %v", err)
	}
}

func TestClientReportsRefusalsAndGarbageWithoutQuotingThem(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   string
		check  func(resp *Response, err error) bool
	}{
		"forbidden": {403, `{"error":"forbidden"}`, func(_ *Response, err error) bool {
			var se *StatusError
			return errors.As(err, &se) && se.Status == 403 && se.Code == "forbidden"
		}},
		"an error page": {502, `<html>Bad Gateway SECRET</html>`, func(_ *Response, err error) bool {
			var se *StatusError
			return errors.As(err, &se) && se.Code == "" && !strings.Contains(err.Error(), "SECRET")
		}},
		"a hostile code": {401, `{"error":"Bearer ` + callerToken + ` <script>"}`, func(_ *Response, err error) bool {
			return err != nil && !strings.Contains(err.Error(), "<") && !strings.Contains(err.Error(), " "+callerToken)
		}},
		"not JSON":   {200, `SECRET not json`, func(_ *Response, err error) bool { return errors.Is(err, ErrUnexpectedAnswer) }},
		"no outcome": {200, `{"site":"powerschool"}`, func(_ *Response, err error) bool { return errors.Is(err, ErrUnexpectedAnswer) }},
		"codes are reduced to tokens": {200, `{"outcome":"failed","code":"Login Failed!","detail":"<b>x</b>"}`, func(r *Response, err error) bool {
			return err == nil && r.Code == "_ogin__ailed_" && r.Detail == "_b_x__b_"
		}},
		"a session": {200, `{"outcome":"refreshed","session":{"session":"{}"},"fingerprint":"0123456789abcdef"}`, func(r *Response, err error) bool {
			return err == nil && r.HasSession() && r.Session["session"] == "{}"
		}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = io.WriteString(w, c.body)
		}))
		client, err := NewClient(srv.URL, tokenSource, time.Minute, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Refresh(context.Background(), ReasonSessionDead, "")
		if !c.check(resp, err) {
			t.Errorf("%s: %+v, %v", name, resp, err)
		}
		srv.Close()
	}
}

func TestNewClientAndTokenFile(t *testing.T) {
	for _, bad := range []string{"", "broker:8080", "ftp://broker", "http://", "http://u:p@broker", "http://broker/v1", "http://broker?x=1"} {
		if _, err := NewClient(bad, tokenSource, 0, nil); err == nil {
			t.Errorf("NewClient(%q) succeeded", bad)
		}
	}
	if _, err := NewClient("http://broker:8080", nil, 0, nil); err == nil {
		t.Error("a client without a token source was built")
	}
	if c, err := NewClient("http://broker:8080/", tokenSource, 0, nil); err != nil || c.endpoint != "http://broker:8080"+RefreshPath || c.http.Timeout != DefaultTimeout {
		t.Errorf("client = %+v, %v", c, err)
	}

	path := filepath.Join(t.TempDir(), "token")
	src := TokenFile(path)
	if _, err := src(); err == nil || strings.Contains(err.Error(), path) {
		t.Errorf("a missing token file: %v", err)
	}
	_ = os.WriteFile(path, []byte("  \n"), 0o600)
	if _, err := src(); err == nil {
		t.Error("an empty token file was accepted")
	}
	// Re-read on every call: the kubelet rotates it in place.
	_ = os.WriteFile(path, []byte("first\n"), 0o600)
	a, _ := src()
	_ = os.WriteFile(path, []byte("second\n"), 0o600)
	b, _ := src()
	if a != "first" || b != "second" {
		t.Errorf("tokens = %q, %q", a, b)
	}
}

func TestFingerprintsAndReasons(t *testing.T) {
	if ShortFingerprint("") != NoFingerprint || ShortFingerprint("none") != NoFingerprint || ShortFingerprint("abc") != "abc" {
		t.Error("ShortFingerprint")
	}
	for fp, ok := range map[string]bool{"none": true, "0123456789abcdef": true, "0123456789ABCDEF": false, "0123": false, "": false, "0123456789abcdeg": false} {
		if ValidFingerprint(fp) != ok {
			t.Errorf("ValidFingerprint(%q) = %v", fp, !ok)
		}
	}
	for _, r := range []string{ReasonSessionDead, ReasonNoSession, ReasonSessionExpired, ReasonAuthFailure} {
		if !ValidReason(r) {
			t.Errorf("%q is not valid", r)
		}
	}
	if ValidReason("") || ValidReason("because") {
		t.Error("free text is a valid reason")
	}
}
