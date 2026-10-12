package powerschool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/kubecm"
	"github.com/leftathome/glovebox/internal/psportal"
)

const requestConfigMap = "glovebox-glovebox-powerschool-login-request"

// requestAPI is an httptest Kubernetes API holding the one request
// ConfigMap. It records every request it is sent.
type requestAPI struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD escaped-path"
	bodies   []string
	data     map[string]string
}

func newRequestAPI(t *testing.T) *requestAPI {
	t.Helper()
	a := &requestAPI{data: map[string]string{}}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		a.requests = append(a.requests, r.Method+" "+r.URL.EscapedPath())
		a.bodies = append(a.bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/namespaces/openclaw/configmaps/"+requestConfigMap {
			var patch struct {
				Data map[string]string `json:"data"`
			}
			_ = json.Unmarshal(body, &patch)
			for k, v := range patch.Data {
				a.data[k] = v
			}
			_, _ = w.Write([]byte(`{"kind":"ConfigMap"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","reason":"Forbidden"}`))
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *requestAPI) client(t *testing.T) *kubecm.Client {
	t.Helper()
	c, err := kubecm.New(a.srv.URL, "openclaw", a.srv.Client().Transport, func() (string, error) { return "sa-token", nil })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Everything the connector does at the Kubernetes API for a login: one
// PATCH of one key of one ConfigMap, carrying a time and a fingerprint
// prefix. No Job, no pod, no Secret, no read of anything.
func TestConfigMapRequesterWritesOneFieldAndNothingElse(t *testing.T) {
	api := newRequestAPI(t)
	requester := ConfigMapRequester{Client: api.client(t), ConfigMap: requestConfigMap}
	at := time.Date(2026, 10, 12, 13, 5, 9, 0, time.UTC)
	fp := SessionFingerprint(testSession("dead", at.Add(time.Hour)))

	if err := requester.RequestLogin(context.Background(), psportal.FormatLoginRequest(at, fp)); err != nil {
		t.Fatalf("RequestLogin: %v", err)
	}
	if len(api.requests) != 1 || api.requests[0] != "PATCH /api/v1/namespaces/openclaw/configmaps/"+requestConfigMap {
		t.Fatalf("requests = %v, want one PATCH of the request ConfigMap", api.requests)
	}
	want := `{"data":{"request":"2026-10-12T13:05:09Z ` + fp[:16] + `"}}`
	if api.bodies[0] != want {
		t.Errorf("body = %s, want %s", api.bodies[0], want)
	}
	for _, leak := range []string{"dead", "mrh-dead", "JSESSIONID", "sa-token"} {
		if strings.Contains(api.bodies[0], leak) {
			t.Errorf("the request body carries %q", leak)
		}
	}
	// What the login workload reads back parses as a request.
	if _, err := psportal.ParseLoginRequest(api.data[psportal.LoginRequestKey]); err != nil {
		t.Errorf("the stored field does not parse: %v", err)
	}
}

// The whole gate against the httptest API: a dead session leads to exactly
// one PATCH, and the API server is asked nothing else during the wait.
func TestGateAgainstTheAPIServerMakesExactlyOneRequest(t *testing.T) {
	api := newRequestAPI(t)
	r := newGateRig(t, true)
	r.gate.requester = ConfigMapRequester{Client: api.client(t), ConfigMap: requestConfigMap}
	r.file = r.session("old", SessionDead, ReasonSignInPage)
	r.arrives, r.arriveAfter = r.session("new", SessionLive, ReasonSignedIn), 4*time.Minute

	if res := r.ensure(); res.Outcome != GateLive {
		t.Fatalf("Ensure = %d %q", res.Outcome, res.Reason)
	}
	if len(api.requests) != 1 || !strings.HasPrefix(api.requests[0], "PATCH ") {
		t.Errorf("API requests = %v, want exactly one PATCH", api.requests)
	}

	// A refused write (the Role is missing) loses the poll and says so.
	api2 := newRequestAPI(t)
	r2 := newGateRig(t, true)
	r2.gate.requester = ConfigMapRequester{Client: api2.client(t), ConfigMap: "some-other-configmap"}
	r2.file = r2.session("old", SessionDead, ReasonSignInPage)
	res := r2.ensure()
	if res.Outcome != GateSkip || res.Reason != "login_request_failed" || !res.Failed {
		t.Errorf("a 403 on the write: %d %q failed=%v", res.Outcome, res.Reason, res.Failed)
	}
}

// --- the two clients cannot be used for each other's host -------------------

// The Kubernetes client sends its ServiceAccount token to the API server
// and nowhere else; the portal client sends the session cookie to the
// portal and nowhere else. Neither host is reachable through the other's
// client, and the portal client still refuses every write it is not
// explicitly allowed.
func TestKubernetesAndPortalClientsAreSeparate(t *testing.T) {
	api := newRequestAPI(t)
	portalRequests := 0
	portal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		portalRequests++
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the portal received an Authorization header")
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer portal.Close()
	portalHost := strings.TrimPrefix(portal.URL, "https://")

	kube := api.client(t)
	portalClient := NewHTTPClient(portalHost, true, []string{"111111"}, 5*time.Second)
	portalClient.Transport.(*Transport).Base = portal.Client().Transport

	// 1. The Kubernetes client will not talk to the portal, for any method.
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch} {
		req, _ := http.NewRequest(method, portal.URL+"/guardian/home.html", strings.NewReader("x"))
		resp, err := kube.HTTPClient().Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("the Kubernetes client sent a %s to the portal", method)
		}
		if !errors.Is(err, kubecm.ErrHostBlocked) {
			t.Errorf("%s to the portal: err = %v, want kubecm.ErrHostBlocked", method, err)
		}
	}
	if portalRequests != 0 {
		t.Fatalf("%d request(s) reached the portal through the Kubernetes client", portalRequests)
	}

	// 2. The portal client will not talk to the API server, for any method.
	before := len(api.requests)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch} {
		req, _ := http.NewRequest(method, api.srv.URL+"/api/v1/namespaces/openclaw/configmaps/"+requestConfigMap, strings.NewReader("{}"))
		req.AddCookie(&http.Cookie{Name: "JSESSIONID", Value: "session-cookie"})
		resp, err := portalClient.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("the portal client sent a %s to the API server", method)
		}
		if !errors.Is(err, ErrHostBlocked) {
			t.Errorf("%s to the API server: err = %v, want ErrHostBlocked", method, err)
		}
	}
	if len(api.requests) != before {
		t.Fatal("a request reached the API server through the portal client")
	}

	// 3. And on its own host the portal client is as read-only as before:
	//    a PATCH, or a POST outside its allowlist, is refused.
	for _, c := range []struct{ method, path string }{
		{http.MethodPatch, "/api/v1/namespaces/openclaw/configmaps/" + requestConfigMap},
		{http.MethodPost, "/apis/batch/v1/namespaces/openclaw/jobs"},
		{http.MethodPost, "/guardian/home.html"},
		{http.MethodPost, "/public/"},
		{http.MethodPut, "/guardian/home.html"},
		{http.MethodDelete, "/guardian/home.html"},
	} {
		req, _ := http.NewRequest(c.method, portal.URL+c.path, strings.NewReader(`{"kind":"Job"}`))
		resp, err := portalClient.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Errorf("the portal client sent %s %s", c.method, c.path)
		}
	}
	if portalRequests != 0 {
		t.Errorf("%d write(s) reached the portal", portalRequests)
	}
}

// The package has no way left to create a workload: the only Kubernetes
// call in it is the ConfigMap write above.
func TestConnectorHasNoWorkloadCreatingSurface(t *testing.T) {
	var requester LoginRequester = ConfigMapRequester{}
	if _, canCreate := requester.(interface {
		CreateJobFromCronJob(context.Context, string, time.Time) (string, error)
	}); canCreate {
		t.Error("the requester can create Jobs")
	}
}
