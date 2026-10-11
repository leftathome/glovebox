package powerschool

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file is the connector's entire Kubernetes client: four calls on the
// batch/v1 API, made with net/http. It exists so the connector can ASK for
// a login -- by creating a Job from the login CronJob's template -- when it
// finds its session dead. The connector still never holds the account's
// username or password and never logs in: the Job it creates is the login
// workload (connectors/powerschool-auth-refresher), which runs in its own
// pod with its own ServiceAccount, its own Vault role and its own rules
// (blackout, latch, sign-in check). All the connector contributes is "now
// would be a good time".
//
// Two HTTP clients exist in this process and they do not overlap:
//
//   - the portal client (transport.go): pinned to the portal host, GET and
//     HEAD plus two allowlisted POSTs, carrying the session cookies;
//   - this one: pinned to the API server host, carrying the pod's
//     ServiceAccount token.
//
// Neither will send a request to the other's host (see KubeTransport and
// Transport), so the session cookie cannot reach the API server and the
// ServiceAccount token cannot reach the portal.

// ErrKubeHostBlocked is returned (wrapped) by KubeTransport for any request
// that is not an https request to exactly the API server.
var ErrKubeHostBlocked = errors.New("powerschool: request to a host other than the Kubernetes API server blocked")

// KubeTransport forwards only https requests whose URL host is exactly
// Host, and follows no redirect elsewhere (the client that owns it follows
// none at all).
type KubeTransport struct {
	// Host is the API server's host:port, exactly as it appears in request
	// URLs.
	Host string
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *KubeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL
	if u == nil || u.Scheme != "https" || u.Host != t.Host || u.User != nil || u.Opaque != "" ||
		(req.Host != "" && req.Host != t.Host) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		host := ""
		if u != nil {
			host = u.Host
		}
		return nil, fmt.Errorf("%w: %s", ErrKubeHostBlocked, host)
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// KubeError is a non-2xx answer from the API server. It carries the status
// and the API's machine-readable reason, never the response body.
type KubeError struct {
	Op     string
	Status int
	Reason string
}

func (e *KubeError) Error() string {
	return fmt.Sprintf("kubernetes %s: HTTP %d %s", e.Op, e.Status, e.Reason)
}

// kubeStatusOf returns the HTTP status of a KubeError, or 0.
func kubeStatusOf(err error) int {
	var ke *KubeError
	if errors.As(err, &ke) {
		return ke.Status
	}
	return 0
}

// KubeClient talks to the batch/v1 API of one namespace.
type KubeClient struct {
	base      string // https://host:port
	namespace string
	http      *http.Client
	token     func() (string, error)
}

// ServiceAccountDir is where a pod's ServiceAccount token, CA and namespace
// are mounted.
const ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// NewInClusterKubeClient builds the client from the pod's environment
// (KUBERNETES_SERVICE_HOST / _PORT) and the ServiceAccount files in saDir.
// namespace overrides the ServiceAccount's own when not empty.
func NewInClusterKubeClient(getenv func(string) string, saDir, namespace string) (*KubeClient, error) {
	host, port := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT are not set: not running in a pod?")
	}
	caPEM, err := os.ReadFile(filepath.Join(saDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read the cluster CA (is the ServiceAccount token mounted?): %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the cluster CA file holds no certificate")
	}
	if namespace == "" {
		raw, err := os.ReadFile(filepath.Join(saDir, "namespace"))
		if err != nil {
			return nil, fmt.Errorf("read the pod's namespace: %w", err)
		}
		namespace = strings.TrimSpace(string(raw))
	}
	tokenPath := filepath.Join(saDir, "token")
	if _, err := os.Stat(tokenPath); err != nil {
		return nil, fmt.Errorf("the ServiceAccount token is not mounted: %w", err)
	}
	transport := &http.Transport{
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
		// Explicitly no proxy: the token goes to the API server only.
		Proxy: nil,
	}
	// The projected token is rotated by the kubelet; read it per request.
	token := func() (string, error) {
		raw, err := os.ReadFile(tokenPath)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	return NewKubeClient("https://"+net.JoinHostPort(host, port), namespace, transport, token)
}

// NewKubeClient builds a client for baseURL ("https://host:port") on the
// given inner transport. Tests use it with an httptest server.
func NewKubeClient(baseURL, namespace string, inner http.RoundTripper, token func() (string, error)) (*KubeClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the Kubernetes API address must be an https URL")
	}
	if namespace == "" {
		return nil, errors.New("no namespace")
	}
	return &KubeClient{
		base:      "https://" + u.Host,
		namespace: namespace,
		token:     token,
		http: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &KubeTransport{Host: u.Host, Base: inner},
			// The API server has no business redirecting these calls.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Namespace returns the namespace the client works in.
func (k *KubeClient) Namespace() string { return k.namespace }

// HTTPClient returns the underlying client. For tests that prove what it
// refuses.
func (k *KubeClient) HTTPClient() *http.Client { return k.http }

// kubeCronJob is the part of a batch/v1 CronJob the connector reads.
type kubeCronJob struct {
	Metadata struct {
		Name string `json:"name"`
		UID  string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		JobTemplate struct {
			Metadata struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			// Spec is passed through untouched into the Job.
			Spec json.RawMessage `json:"spec"`
		} `json:"jobTemplate"`
	} `json:"spec"`
}

// KubeJob is the part of a batch/v1 Job the connector reads.
type KubeJob struct {
	Name      string
	Active    int
	Succeeded int
	Failed    int
	// Complete / FailedCondition mirror the Job's terminal conditions;
	// Reason is the Failed condition's reason (BackoffLimitExceeded,
	// DeadlineExceeded, ...).
	Complete        bool
	FailedCondition bool
	Reason          string
}

// Finished reports whether the Job has reached a terminal state.
func (j KubeJob) Finished() bool { return j.Complete || j.FailedCondition }

// State is a one-word description for a log line.
func (j KubeJob) State() string {
	switch {
	case j.Complete:
		return "succeeded"
	case j.FailedCondition:
		if j.Reason != "" {
			return "failed:" + j.Reason
		}
		return "failed"
	case j.Active > 0:
		return "running"
	}
	return "pending"
}

type kubeJobJSON struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Active     int `json:"active"`
		Succeeded  int `json:"succeeded"`
		Failed     int `json:"failed"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"conditions"`
	} `json:"status"`
}

func (j kubeJobJSON) toJob() KubeJob {
	out := KubeJob{Name: j.Metadata.Name, Active: j.Status.Active, Succeeded: j.Status.Succeeded, Failed: j.Status.Failed}
	for _, c := range j.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			out.Complete = true
		case "Failed":
			out.FailedCondition = true
			out.Reason = safeToken(c.Reason)
		}
	}
	return out
}

func (k *KubeClient) do(ctx context.Context, op, method, path string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, reader)
	if err != nil {
		return err
	}
	token, err := k.token()
	if err != nil || token == "" {
		return fmt.Errorf("kubernetes %s: could not read the ServiceAccount token", op)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.http.Do(req)
	if err != nil {
		// http.Client's error text carries the URL, which is harmless here,
		// but never the Authorization header.
		return fmt.Errorf("kubernetes %s: %w", op, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var status struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &status)
		return &KubeError{Op: op, Status: resp.StatusCode, Reason: safeToken(status.Reason)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("kubernetes %s: undecodable answer", op)
	}
	return nil
}

func (k *KubeClient) getCronJob(ctx context.Context, name string) (*kubeCronJob, error) {
	var cj kubeCronJob
	path := "/apis/batch/v1/namespaces/" + url.PathEscape(k.namespace) + "/cronjobs/" + url.PathEscape(name)
	if err := k.do(ctx, "get cronjob", http.MethodGet, path, nil, &cj); err != nil {
		return nil, err
	}
	return &cj, nil
}

// ListJobs returns the Jobs in the namespace matching labelSelector.
func (k *KubeClient) ListJobs(ctx context.Context, labelSelector string) ([]KubeJob, error) {
	var list struct {
		Items []kubeJobJSON `json:"items"`
	}
	path := "/apis/batch/v1/namespaces/" + url.PathEscape(k.namespace) + "/jobs?labelSelector=" + url.QueryEscape(labelSelector)
	if err := k.do(ctx, "list jobs", http.MethodGet, path, nil, &list); err != nil {
		return nil, err
	}
	jobs := make([]KubeJob, 0, len(list.Items))
	for _, item := range list.Items {
		jobs = append(jobs, item.toJob())
	}
	return jobs, nil
}

// GetJob returns one Job.
func (k *KubeClient) GetJob(ctx context.Context, name string) (KubeJob, error) {
	var j kubeJobJSON
	path := "/apis/batch/v1/namespaces/" + url.PathEscape(k.namespace) + "/jobs/" + url.PathEscape(name)
	if err := k.do(ctx, "get job", http.MethodGet, path, nil, &j); err != nil {
		return KubeJob{}, err
	}
	return j.toJob(), nil
}

// Labels and annotations of the login Jobs.
const (
	// LoginJobLabel marks every login Job, scheduled or on demand: the
	// chart puts it on the CronJobs' jobTemplate and CreateJobFromCronJob
	// adds it regardless. It is the selector for "is a login running?".
	LoginJobLabel = "glovebox.io/powerschool-login"
	// LoginTriggerLabel says who started the Job.
	LoginTriggerLabel = "glovebox.io/powerschool-login-trigger"
	// LoginTriggerOnDemand is LoginTriggerLabel's value for a Job this
	// connector created.
	LoginTriggerOnDemand = "on-demand"
	// LoginJobSelector selects every login Job.
	LoginJobSelector = LoginJobLabel + "=true"
)

// maxJobNameLen is the longest Job name whose job-name label is valid.
const maxJobNameLen = 63

// onDemandJobName builds "<cronjob>-ondemand-<unix>", shortening the
// CronJob part when the whole would not fit.
func onDemandJobName(cronJob string, now time.Time) string {
	suffix := fmt.Sprintf("-ondemand-%d", now.Unix())
	if room := maxJobNameLen - len(suffix); len(cronJob) > room {
		cronJob = strings.TrimRight(cronJob[:room], "-.")
	}
	return cronJob + suffix
}

// CreateJobFromCronJob creates a Job from the named CronJob's jobTemplate,
// the way `kubectl create job --from=cronjob/<name>` does: the template's
// spec unchanged (its ttlSecondsAfterFinished, backoffLimit and deadline
// included), its labels and annotations, and a controller ownerReference
// to the CronJob so that the CronJob's history limits prune it and its
// concurrencyPolicy counts it. It returns the Job's name.
//
// The connector adds nothing to the pod template: no environment, no
// arguments, no credentials. What the Job does is decided by the CronJob.
func (k *KubeClient) CreateJobFromCronJob(ctx context.Context, cronJobName string, now time.Time) (string, error) {
	cj, err := k.getCronJob(ctx, cronJobName)
	if err != nil {
		return "", err
	}
	if cj.Metadata.UID == "" || len(cj.Spec.JobTemplate.Spec) == 0 {
		return "", fmt.Errorf("kubernetes get cronjob: the answer has no uid or no jobTemplate")
	}
	labels := map[string]string{}
	for key, v := range cj.Spec.JobTemplate.Metadata.Labels {
		labels[key] = v
	}
	labels[LoginJobLabel] = "true"
	labels[LoginTriggerLabel] = LoginTriggerOnDemand
	annotations := map[string]string{}
	for key, v := range cj.Spec.JobTemplate.Metadata.Annotations {
		annotations[key] = v
	}
	annotations["cronjob.kubernetes.io/instantiate"] = "manual"

	name := onDemandJobName(cronJobName, now)
	controller := true
	body, err := json.Marshal(map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   k.namespace,
			"labels":      labels,
			"annotations": annotations,
			"ownerReferences": []map[string]any{{
				"apiVersion": "batch/v1",
				"kind":       "CronJob",
				"name":       cj.Metadata.Name,
				"uid":        cj.Metadata.UID,
				"controller": &controller,
			}},
		},
		"spec": cj.Spec.JobTemplate.Spec,
	})
	if err != nil {
		return "", err
	}
	path := "/apis/batch/v1/namespaces/" + url.PathEscape(k.namespace) + "/jobs"
	if err := k.do(ctx, "create job", http.MethodPost, path, body, nil); err != nil {
		return "", err
	}
	return name, nil
}
