package powerschool

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
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
	testKubeToken   = "sa-token-for-tests"
	testCronJobName = "glovebox-glovebox-powerschool-login-morning"
	testCronJobUID  = "0b9d7c1e-1111-4222-8333-444455556666"
)

// cronJobJSON is a CronJob as the API server returns it, shaped like the
// one the chart renders: suspended, with the login label on its template.
const cronJobJSON = `{
  "apiVersion": "batch/v1", "kind": "CronJob",
  "metadata": {"name": "` + testCronJobName + `", "namespace": "openclaw", "uid": "` + testCronJobUID + `",
               "labels": {"app.kubernetes.io/component": "powerschool-auth-refresher"}},
  "spec": {
    "schedule": "0 5 * * 1-5", "suspend": true, "concurrencyPolicy": "Forbid",
    "jobTemplate": {
      "metadata": {"labels": {"glovebox.io/powerschool-login": "true", "app.kubernetes.io/component": "powerschool-auth-refresher"},
                   "annotations": {"example.org/note": "kept"}},
      "spec": {
        "backoffLimit": 0, "activeDeadlineSeconds": 600, "ttlSecondsAfterFinished": 3600,
        "template": {"spec": {"restartPolicy": "Never", "serviceAccountName": "glovebox-glovebox-powerschool-auth-refresher",
          "containers": [{"name": "refresher", "image": "example/refresher:1",
            "env": [{"name": "POWERSCHOOL_MIN_SESSION_AGE", "value": "10h"},
                    {"name": "POWERSCHOOL_PASSWORD", "valueFrom": {"secretKeyRef": {"name": "creds", "key": "POWERSCHOOL_PASSWORD"}}}]}]}}
      }
    }
  },
  "status": {"lastScheduleTime": "2026-10-09T12:00:00Z"}
}`

// fakeAPIServer is an httptest Kubernetes API speaking the four batch/v1
// calls the connector makes.
type fakeAPIServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD path?query"
	auth     []string
	created  []map[string]any
	// status overrides the answer for "METHOD path".
	status map[string]int
	jobs   string // JSON list body for GET .../jobs
	job    string // JSON body for GET .../jobs/<name>
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{status: map[string]int{}, jobs: `{"items":[]}`}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if code, ok := f.status[key]; ok {
			w.WriteHeader(code)
			reason := map[int]string{403: "Forbidden", 404: "NotFound", 409: "AlreadyExists", 500: "InternalError", 503: "ServiceUnavailable"}[code]
			_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"` + reason + `","message":"secret detail that must not be logged"}`))
			return
		}
		const base = "/apis/batch/v1/namespaces/openclaw"
		switch {
		case key == "GET "+base+"/cronjobs/"+testCronJobName:
			_, _ = w.Write([]byte(cronJobJSON))
		case key == "GET "+base+"/jobs":
			_, _ = w.Write([]byte(f.jobs))
		case key == "POST "+base+"/jobs":
			var obj map[string]any
			if err := json.Unmarshal(body, &obj); err != nil {
				w.WriteHeader(400)
				return
			}
			f.created = append(f.created, obj)
			w.WriteHeader(201)
			_, _ = w.Write(body)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, base+"/jobs/"):
			_, _ = w.Write([]byte(f.job))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"kind":"Status","reason":"NotFound"}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPIServer) client(t *testing.T) *KubeClient {
	t.Helper()
	k, err := NewKubeClient(f.srv.URL, "openclaw", f.srv.Client().Transport, func() (string, error) { return testKubeToken, nil })
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// The Job is the CronJob's template and nothing else: the same spec, the
// template's labels and annotations, the login labels, and a controller
// ownerReference to the CronJob.
func TestKubeCreateJobFromCronJob(t *testing.T) {
	api := newFakeAPIServer(t)
	k := api.client(t)
	now := time.Unix(1791000000, 0)

	name, err := k.CreateJobFromCronJob(context.Background(), testCronJobName, now)
	if err != nil {
		t.Fatalf("CreateJobFromCronJob: %v", err)
	}
	if name != testCronJobName+"-ondemand-1791000000" || len(name) > 63 {
		t.Errorf("job name = %q (%d chars)", name, len(name))
	}
	want := []string{
		"GET /apis/batch/v1/namespaces/openclaw/cronjobs/" + testCronJobName,
		"POST /apis/batch/v1/namespaces/openclaw/jobs",
	}
	if strings.Join(api.requests, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", api.requests, want)
	}
	for _, a := range api.auth {
		if a != "Bearer "+testKubeToken {
			t.Errorf("Authorization = %q", a)
		}
	}

	job := api.created[0]
	if job["apiVersion"] != "batch/v1" || job["kind"] != "Job" {
		t.Errorf("apiVersion/kind = %v/%v", job["apiVersion"], job["kind"])
	}
	meta := job["metadata"].(map[string]any)
	if meta["name"] != name || meta["namespace"] != "openclaw" {
		t.Errorf("metadata name/namespace = %v/%v", meta["name"], meta["namespace"])
	}
	labels := meta["labels"].(map[string]any)
	for key, v := range map[string]string{
		"glovebox.io/powerschool-login":         "true",
		"glovebox.io/powerschool-login-trigger": "on-demand",
		"app.kubernetes.io/component":           "powerschool-auth-refresher",
	} {
		if labels[key] != v {
			t.Errorf("label %s = %v, want %s", key, labels[key], v)
		}
	}
	annotations := meta["annotations"].(map[string]any)
	if annotations["cronjob.kubernetes.io/instantiate"] != "manual" || annotations["example.org/note"] != "kept" {
		t.Errorf("annotations = %v", annotations)
	}
	owners := meta["ownerReferences"].([]any)
	if len(owners) != 1 {
		t.Fatalf("ownerReferences = %v", owners)
	}
	owner := owners[0].(map[string]any)
	if owner["apiVersion"] != "batch/v1" || owner["kind"] != "CronJob" || owner["name"] != testCronJobName ||
		owner["uid"] != testCronJobUID || owner["controller"] != true {
		t.Errorf("ownerReference = %v", owner)
	}
	if _, set := owner["blockOwnerDeletion"]; set {
		t.Error("blockOwnerDeletion is set; it needs a permission on the CronJob the Role does not grant")
	}

	// The spec is the template's, byte for byte in meaning.
	var cj struct {
		Spec struct {
			JobTemplate struct {
				Spec map[string]any `json:"spec"`
			} `json:"jobTemplate"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(cronJobJSON), &cj); err != nil {
		t.Fatal(err)
	}
	gotSpec, _ := json.Marshal(job["spec"])
	wantSpec, _ := json.Marshal(cj.Spec.JobTemplate.Spec)
	if string(gotSpec) != string(wantSpec) {
		t.Errorf("job spec differs from the CronJob's jobTemplate.spec:\n got %s\nwant %s", gotSpec, wantSpec)
	}
	spec := job["spec"].(map[string]any)
	if spec["ttlSecondsAfterFinished"] != float64(3600) || spec["backoffLimit"] != float64(0) {
		t.Errorf("ttl/backoffLimit = %v/%v", spec["ttlSecondsAfterFinished"], spec["backoffLimit"])
	}
	// The connector sends nothing of its own into the pod.
	raw, _ := json.Marshal(job)
	if strings.Contains(string(raw), testKubeToken) || strings.Contains(string(raw), "JSESSIONID") {
		t.Errorf("the Job carries something of the connector's: %s", raw)
	}
}

func TestOnDemandJobNameFitsTheLimit(t *testing.T) {
	now := time.Unix(1791000000, 0)
	for _, cron := range []string{"x", testCronJobName, strings.Repeat("a", 52), "abc-" + strings.Repeat("b", 39) + "-" + "cdef"} {
		name := onDemandJobName(cron, now)
		if len(name) > 63 || !strings.HasSuffix(name, "-ondemand-1791000000") || strings.Contains(name, "--") {
			t.Errorf("onDemandJobName(%q) = %q (%d chars)", cron, name, len(name))
		}
	}
}

func TestKubeErrorsCarryTheStatusAndNotTheBody(t *testing.T) {
	const base = "/apis/batch/v1/namespaces/openclaw"
	for name, c := range map[string]struct {
		key    string
		status int
		call   func(k *KubeClient) error
	}{
		"cronjob get forbidden": {"GET " + base + "/cronjobs/" + testCronJobName, 403, func(k *KubeClient) error {
			_, err := k.CreateJobFromCronJob(context.Background(), testCronJobName, time.Now())
			return err
		}},
		"cronjob missing": {"GET " + base + "/cronjobs/" + testCronJobName, 404, func(k *KubeClient) error {
			_, err := k.CreateJobFromCronJob(context.Background(), testCronJobName, time.Now())
			return err
		}},
		"create forbidden": {"POST " + base + "/jobs", 403, func(k *KubeClient) error {
			_, err := k.CreateJobFromCronJob(context.Background(), testCronJobName, time.Now())
			return err
		}},
		"create conflict": {"POST " + base + "/jobs", 409, func(k *KubeClient) error {
			_, err := k.CreateJobFromCronJob(context.Background(), testCronJobName, time.Now())
			return err
		}},
		"create server error": {"POST " + base + "/jobs", 500, func(k *KubeClient) error {
			_, err := k.CreateJobFromCronJob(context.Background(), testCronJobName, time.Now())
			return err
		}},
		"list forbidden": {"GET " + base + "/jobs", 403, func(k *KubeClient) error {
			_, err := k.ListJobs(context.Background(), LoginJobSelector)
			return err
		}},
		"list unavailable": {"GET " + base + "/jobs", 503, func(k *KubeClient) error {
			_, err := k.ListJobs(context.Background(), LoginJobSelector)
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPIServer(t)
			api.status[c.key] = c.status
			err := c.call(api.client(t))
			if got := kubeStatusOf(err); got != c.status {
				t.Fatalf("err = %v (status %d), want status %d", err, got, c.status)
			}
			if strings.Contains(err.Error(), "secret detail") || strings.Contains(err.Error(), testKubeToken) {
				t.Errorf("the error quotes the response or the token: %v", err)
			}
			if c.status != 0 && strings.HasPrefix(c.key, "GET "+base+"/cronjobs") && len(api.created) != 0 {
				t.Error("a Job was created although the CronJob could not be read")
			}
		})
	}
}

func TestKubeListAndGetJobs(t *testing.T) {
	api := newFakeAPIServer(t)
	api.jobs = `{"items":[
	  {"metadata":{"name":"done"},"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}},
	  {"metadata":{"name":"running"},"status":{"active":1}},
	  {"metadata":{"name":"failed"},"status":{"failed":1,"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}},
	  {"metadata":{"name":"new"},"status":{}},
	  {"metadata":{"name":"not-yet"},"status":{"conditions":[{"type":"Failed","status":"False"}]}}
	]}`
	k := api.client(t)
	jobs, err := k.ListJobs(context.Background(), LoginJobSelector)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, j := range jobs {
		got = append(got, j.Name+"="+j.State())
	}
	want := "done=succeeded,running=running,failed=failed:BackoffLimitExceeded,new=pending,not-yet=pending"
	if strings.Join(got, ",") != want {
		t.Errorf("jobs = %v, want %s", got, want)
	}
	for i, finished := range []bool{true, false, true, false, false} {
		if jobs[i].Finished() != finished {
			t.Errorf("%s.Finished() = %v", jobs[i].Name, jobs[i].Finished())
		}
	}
	if req := api.requests[0]; req != "GET /apis/batch/v1/namespaces/openclaw/jobs?labelSelector=glovebox.io%2Fpowerschool-login%3Dtrue" {
		t.Errorf("list request = %q", req)
	}

	api.job = `{"metadata":{"name":"j1"},"status":{"failed":1,"conditions":[{"type":"Failed","status":"True","reason":"DeadlineExceeded"}]}}`
	j, err := k.GetJob(context.Background(), "j1")
	if err != nil || j.State() != "failed:DeadlineExceeded" {
		t.Errorf("GetJob = %+v, %v", j, err)
	}
}

func TestNewInClusterKubeClient(t *testing.T) {
	api := newFakeAPIServer(t)
	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hostport := strings.TrimPrefix(api.srv.URL, "https://")
	host, port, _ := strings.Cut(hostport, ":")
	env := map[string]string{"KUBERNETES_SERVICE_HOST": host, "KUBERNETES_SERVICE_PORT": port}
	getenv := func(k string) string { return env[k] }

	if _, err := NewInClusterKubeClient(func(string) string { return "" }, dir, ""); err == nil {
		t.Error("built a client outside a pod")
	}
	if _, err := NewInClusterKubeClient(getenv, dir, ""); err == nil {
		t.Error("built a client without a CA")
	}
	write("ca.crt", "not a certificate")
	if _, err := NewInClusterKubeClient(getenv, dir, ""); err == nil {
		t.Error("built a client with an empty CA")
	}
	// The test server's own certificate as the cluster CA.
	write("ca.crt", string(pemEncode(api.srv.Certificate().Raw)))
	if _, err := NewInClusterKubeClient(getenv, dir, ""); err == nil {
		t.Error("built a client without a namespace file")
	}
	write("namespace", "openclaw\n")
	if _, err := NewInClusterKubeClient(getenv, dir, ""); err == nil {
		t.Error("built a client without a token")
	}
	write("token", testKubeToken+"\n")

	k, err := NewInClusterKubeClient(getenv, dir, "")
	if err != nil {
		t.Fatalf("NewInClusterKubeClient: %v", err)
	}
	if k.Namespace() != "openclaw" {
		t.Errorf("namespace = %q", k.Namespace())
	}
	if _, err := k.ListJobs(context.Background(), LoginJobSelector); err != nil {
		t.Fatalf("ListJobs over verified TLS: %v", err)
	}
	if api.auth[0] != "Bearer "+testKubeToken {
		t.Errorf("Authorization = %q", api.auth[0])
	}
	// The token is read per request: a rotated token is picked up.
	write("token", "rotated-token")
	_, _ = k.ListJobs(context.Background(), LoginJobSelector)
	if api.auth[1] != "Bearer rotated-token" {
		t.Errorf("after rotation Authorization = %q", api.auth[1])
	}

	// A configured namespace wins over the ServiceAccount's.
	k2, err := NewInClusterKubeClient(getenv, dir, "elsewhere")
	if err != nil || k2.Namespace() != "elsewhere" {
		t.Errorf("namespace override = %v, %v", k2, err)
	}

	// Another CA: the API server is not trusted and nothing is sent.
	write("ca.crt", string(pemEncode(unrelatedCertificate(t))))
	before := len(api.requests)
	k3, err := NewInClusterKubeClient(getenv, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k3.ListJobs(context.Background(), LoginJobSelector); err == nil {
		t.Error("talked to an API server whose certificate the cluster CA did not sign")
	}
	if len(api.requests) != before {
		t.Error("a request reached the unverified server")
	}
}

// --- the two clients cannot be used for each other's host -------------------

// The Kubernetes client sends its ServiceAccount token to the API server
// and nowhere else; the portal client sends the session cookie to the
// portal and nowhere else. Neither host is reachable through the other's
// client, and the portal client still refuses every write it is not
// explicitly allowed.
func TestKubeAndPortalClientsAreSeparate(t *testing.T) {
	api := newFakeAPIServer(t)
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
	apiHost := strings.TrimPrefix(api.srv.URL, "https://")

	kube := api.client(t)
	portalClient := NewHTTPClient(portalHost, true, []string{"111111"}, 5*time.Second)
	portalClient.Transport.(*Transport).Base = portal.Client().Transport

	// 1. The Kubernetes client will not talk to the portal, for any method.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequest(method, portal.URL+"/guardian/home.html", strings.NewReader("x"))
		resp, err := kube.HTTPClient().Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("the Kubernetes client sent a %s to the portal", method)
		}
		if !errors.Is(err, ErrKubeHostBlocked) {
			t.Errorf("%s to the portal: err = %v, want ErrKubeHostBlocked", method, err)
		}
	}
	// ...nor to plain http on its own host, nor does it follow a redirect.
	req, _ := http.NewRequest(http.MethodGet, "http://"+apiHost+"/apis", nil)
	if resp, err := kube.HTTPClient().Do(req); err == nil {
		resp.Body.Close()
		t.Error("the Kubernetes client used plain http")
	}
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, portal.URL+"/guardian/home.html", http.StatusFound)
	}))
	defer redirecting.Close()
	rk, _ := NewKubeClient(redirecting.URL, "openclaw", redirecting.Client().Transport, func() (string, error) { return testKubeToken, nil })
	if _, err := rk.ListJobs(context.Background(), LoginJobSelector); err == nil {
		t.Error("a redirect from the API server was treated as an answer")
	}
	if portalRequests != 0 {
		t.Fatalf("%d request(s) reached the portal through the Kubernetes client", portalRequests)
	}

	// 2. The portal client will not talk to the API server, for any method.
	before := len(api.requests)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req, _ := http.NewRequest(method, api.srv.URL+"/apis/batch/v1/namespaces/openclaw/jobs", strings.NewReader("{}"))
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

	// 3. And on its own host the portal client is as read-only as before: a
	//    Job-shaped POST, or any POST outside its allowlist, is refused.
	for _, path := range []string{"/apis/batch/v1/namespaces/openclaw/jobs", "/guardian/home.html", "/public/"} {
		resp, err := portalClient.Post(portal.URL+path, "application/json", strings.NewReader(`{"kind":"Job"}`))
		if err == nil {
			resp.Body.Close()
			t.Errorf("the portal client POSTed to %s", path)
		}
	}
	if portalRequests != 0 {
		t.Errorf("%d write(s) reached the portal", portalRequests)
	}

	// 4. NewKubeClient refuses a plaintext API address outright.
	if _, err := NewKubeClient("http://"+apiHost, "openclaw", nil, func() (string, error) { return "t", nil }); err == nil {
		t.Error("NewKubeClient accepted an http address")
	}
	if _, err := NewKubeClient(api.srv.URL, "", nil, func() (string, error) { return "t", nil }); err == nil {
		t.Error("NewKubeClient accepted an empty namespace")
	}
}

func TestKubeClientDoesNotSendWithoutAToken(t *testing.T) {
	api := newFakeAPIServer(t)
	k, _ := NewKubeClient(api.srv.URL, "openclaw", api.srv.Client().Transport, func() (string, error) { return "", errors.New("gone") })
	if _, err := k.ListJobs(context.Background(), LoginJobSelector); err == nil {
		t.Error("a request was made without a token")
	}
	if len(api.requests) != 0 {
		t.Error("an unauthenticated request reached the API server")
	}
}

// unrelatedCertificate returns a fresh self-signed CA certificate (DER)
// that signed nothing. httptest servers all share one certificate, so
// "another server's" would not do.
func unrelatedCertificate(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "some other cluster"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
