package kubecm

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testToken     = "sa-token-for-tests"
	testConfigMap = "glovebox-glovebox-powerschool-login-request"
	cmPath        = "/api/v1/namespaces/openclaw/configmaps/" + testConfigMap
)

// apiServer is an httptest Kubernetes API that knows one ConfigMap.
type apiServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD path"
	auth     []string
	types    []string // Content-Type of each request
	bodies   []string
	data     map[string]string
	// status overrides the answer for "METHOD path".
	status map[string]int
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	a := &apiServer{data: map[string]string{}, status: map[string]int{}}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		body, _ := io.ReadAll(r.Body)
		// The path as sent, still escaped: what the API server routes on.
		a.requests = append(a.requests, r.Method+" "+r.URL.EscapedPath())
		a.auth = append(a.auth, r.Header.Get("Authorization"))
		a.types = append(a.types, r.Header.Get("Content-Type"))
		a.bodies = append(a.bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		if code, ok := a.status[key]; ok {
			w.WriteHeader(code)
			reason := map[int]string{403: "Forbidden", 404: "NotFound", 409: "Conflict", 500: "InternalError"}[code]
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"` + reason + `","message":"secret detail that must not be logged"}`))
			return
		}
		switch key {
		case "GET " + cmPath:
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "ConfigMap", "metadata": map[string]any{"name": testConfigMap}, "data": a.data})
		case "PATCH " + cmPath:
			var patch struct {
				Data map[string]string `json:"data"`
			}
			if err := json.Unmarshal(body, &patch); err != nil {
				w.WriteHeader(400)
				return
			}
			for k, v := range patch.Data {
				a.data[k] = v
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"kind": "ConfigMap", "data": a.data})
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"NotFound"}`))
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *apiServer) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(a.srv.URL, "openclaw", a.srv.Client().Transport, func() (string, error) { return testToken, nil })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSetFieldAndGetField(t *testing.T) {
	api := newAPIServer(t)
	api.data["other"] = "untouched"
	c := api.client(t)
	ctx := context.Background()

	if v, found, err := c.GetField(ctx, testConfigMap, "request"); err != nil || found || v != "" {
		t.Fatalf("GetField on an unset key = (%q, %v, %v)", v, found, err)
	}
	if err := c.SetField(ctx, testConfigMap, "request", "2026-10-12T13:05:09Z 5f2a9c0e11d24b7a"); err != nil {
		t.Fatalf("SetField: %v", err)
	}
	v, found, err := c.GetField(ctx, testConfigMap, "request")
	if err != nil || !found || v != "2026-10-12T13:05:09Z 5f2a9c0e11d24b7a" {
		t.Fatalf("GetField = (%q, %v, %v)", v, found, err)
	}
	if api.data["other"] != "untouched" {
		t.Error("SetField touched another key")
	}

	want := []string{"GET " + cmPath, "PATCH " + cmPath, "GET " + cmPath}
	if strings.Join(api.requests, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", api.requests, want)
	}
	// A merge patch of exactly one key: it cannot replace the object, and
	// it carries nothing but that key.
	if api.types[1] != "application/merge-patch+json" {
		t.Errorf("patch Content-Type = %q", api.types[1])
	}
	if api.bodies[1] != `{"data":{"request":"2026-10-12T13:05:09Z 5f2a9c0e11d24b7a"}}` {
		t.Errorf("patch body = %s", api.bodies[1])
	}
	for _, a := range api.auth {
		if a != "Bearer "+testToken {
			t.Errorf("Authorization = %q", a)
		}
	}
}

func TestErrorsCarryTheStatusAndNotTheBody(t *testing.T) {
	for _, status := range []int{403, 404, 409, 500} {
		api := newAPIServer(t)
		api.status["GET "+cmPath] = status
		api.status["PATCH "+cmPath] = status
		c := api.client(t)

		_, _, getErr := c.GetField(context.Background(), testConfigMap, "request")
		setErr := c.SetField(context.Background(), testConfigMap, "request", "x")
		for _, err := range []error{getErr, setErr} {
			if StatusOf(err) != status {
				t.Errorf("status %d: err = %v", status, err)
				continue
			}
			if strings.Contains(err.Error(), "secret detail") || strings.Contains(err.Error(), testToken) {
				t.Errorf("the error quotes the response or the token: %v", err)
			}
		}
	}
	if StatusOf(errors.New("x")) != 0 || StatusOf(nil) != 0 {
		t.Error("StatusOf invents a status")
	}
}

// The client is two calls on one resource. It has no way to name another
// kind of object: every request path is a ConfigMap in its namespace.
func TestEveryRequestIsAConfigMapInTheNamespace(t *testing.T) {
	api := newAPIServer(t)
	c := api.client(t)
	for _, name := range []string{testConfigMap, "../../../apis/batch/v1/namespaces/openclaw/jobs", "x/y", "a?b=c", "..%2f..%2fsecrets%2fcreds"} {
		_, _, _ = c.GetField(context.Background(), name, "request")
		_ = c.SetField(context.Background(), name, "request", "v")
	}
	for _, req := range api.requests {
		method, path, _ := strings.Cut(req, " ")
		if method != "GET" && method != "PATCH" {
			t.Errorf("unexpected method in %q", req)
		}
		if !strings.HasPrefix(path, "/api/v1/namespaces/openclaw/configmaps/") || strings.Count(path, "/") != 6 {
			t.Errorf("request outside the namespace's ConfigMaps: %q", req)
		}
	}
}

func TestClientIsPinnedToTheAPIServer(t *testing.T) {
	api := newAPIServer(t)
	elsewhere := 0
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere++ }))
	defer other.Close()
	c := api.client(t)
	apiHost := strings.TrimPrefix(api.srv.URL, "https://")

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPatch} {
		req, _ := http.NewRequest(method, other.URL+"/guardian/home.html", strings.NewReader("x"))
		resp, err := c.HTTPClient().Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("a %s to another host went through", method)
		}
		if !errors.Is(err, ErrHostBlocked) {
			t.Errorf("%s to another host: err = %v, want ErrHostBlocked", method, err)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+apiHost+"/api", nil)
	if resp, err := c.HTTPClient().Do(req); err == nil {
		resp.Body.Close()
		t.Error("plain http went through")
	}
	req, _ = http.NewRequest(http.MethodGet, api.srv.URL+"/api", nil)
	req.Host = "elsewhere.example.org"
	if resp, err := c.HTTPClient().Do(req); err == nil {
		resp.Body.Close()
		t.Error("a Host header override went through")
	}

	// A redirect from the API server is an answer, not something to follow.
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer redirecting.Close()
	rc, _ := New(redirecting.URL, "openclaw", redirecting.Client().Transport, func() (string, error) { return testToken, nil })
	if _, _, err := rc.GetField(context.Background(), testConfigMap, "request"); err == nil {
		t.Error("a redirect was treated as a ConfigMap")
	}
	if elsewhere != 0 {
		t.Fatalf("%d request(s) reached another host", elsewhere)
	}

	if _, err := New("http://"+apiHost, "openclaw", nil, nil); err == nil {
		t.Error("New accepted an http address")
	}
	for _, ns := range []string{"", "a/b", "a b"} {
		if _, err := New(api.srv.URL, ns, nil, nil); err == nil {
			t.Errorf("New accepted namespace %q", ns)
		}
	}
}

func TestNoRequestWithoutAToken(t *testing.T) {
	api := newAPIServer(t)
	c, _ := New(api.srv.URL, "openclaw", api.srv.Client().Transport, func() (string, error) { return "", errors.New("gone") })
	if _, _, err := c.GetField(context.Background(), testConfigMap, "request"); err == nil {
		t.Error("a request was made without a token")
	}
	if len(api.requests) != 0 {
		t.Error("an unauthenticated request reached the API server")
	}
}

func pemCert(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// unrelatedCA returns a fresh self-signed CA certificate that signed
// nothing. (httptest servers all share one certificate.)
func unrelatedCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "some other cluster"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestNewInCluster(t *testing.T) {
	api := newAPIServer(t)
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host, port, _ := strings.Cut(strings.TrimPrefix(api.srv.URL, "https://"), ":")
	env := map[string]string{"KUBERNETES_SERVICE_HOST": host, "KUBERNETES_SERVICE_PORT": port}
	getenv := func(k string) string { return env[k] }

	if _, err := NewInCluster(func(string) string { return "" }, dir, ""); err == nil {
		t.Error("built a client outside a pod")
	}
	if _, err := NewInCluster(getenv, dir, ""); err == nil {
		t.Error("built a client without a CA")
	}
	write("ca.crt", "not a certificate")
	if _, err := NewInCluster(getenv, dir, ""); err == nil {
		t.Error("built a client with an empty CA")
	}
	write("ca.crt", pemCert(api.srv.Certificate().Raw))
	if _, err := NewInCluster(getenv, dir, ""); err == nil {
		t.Error("built a client without a namespace file")
	}
	write("namespace", "openclaw\n")
	if _, err := NewInCluster(getenv, dir, ""); err == nil {
		t.Error("built a client without a token")
	}
	write("token", testToken+"\n")

	c, err := NewInCluster(getenv, dir, "")
	if err != nil {
		t.Fatalf("NewInCluster: %v", err)
	}
	if c.Namespace() != "openclaw" {
		t.Errorf("namespace = %q", c.Namespace())
	}
	if _, _, err := c.GetField(context.Background(), testConfigMap, "request"); err != nil {
		t.Fatalf("GetField over verified TLS: %v", err)
	}
	// The token is read per request: a rotated token is picked up.
	write("token", "rotated-token")
	_, _, _ = c.GetField(context.Background(), testConfigMap, "request")
	if api.auth[0] != "Bearer "+testToken || api.auth[1] != "Bearer rotated-token" {
		t.Errorf("Authorization headers = %v", api.auth)
	}

	if c2, err := NewInCluster(getenv, dir, "elsewhere"); err != nil || c2.Namespace() != "elsewhere" {
		t.Errorf("namespace override = %v, %v", c2, err)
	}

	// Another CA: the API server is not trusted and nothing is sent.
	write("ca.crt", pemCert(unrelatedCA(t)))
	before := len(api.requests)
	c3, err := NewInCluster(getenv, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c3.GetField(context.Background(), testConfigMap, "request"); err == nil {
		t.Error("talked to an API server whose certificate the cluster CA did not sign")
	}
	if len(api.requests) != before {
		t.Error("a request reached the unverified server")
	}
}
