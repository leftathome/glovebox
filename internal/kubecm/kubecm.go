// Package kubecm is a Kubernetes client that can do two things to one kind
// of object: read a ConfigMap and set one key of a ConfigMap. Nothing else
// is implemented, so nothing else can be asked of it.
//
// It exists for the PowerSchool login request (internal/psportal): the
// connector writes the request, the login workload reads it. Neither needs
// client-go for that, and neither should hold a client that can create
// workloads.
//
// The client is pinned to the API server: its transport refuses any request
// to another host or over plain http, and it follows no redirects. The
// ServiceAccount token it carries therefore goes to the API server and
// nowhere else.
package kubecm

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

// ErrHostBlocked is returned (wrapped) by Transport for any request that is
// not an https request to exactly the API server.
var ErrHostBlocked = errors.New("kubecm: request to a host other than the Kubernetes API server blocked")

// Transport forwards only https requests whose URL host is exactly Host.
type Transport struct {
	// Host is the API server's host:port, exactly as it appears in request
	// URLs.
	Host string
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
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
		return nil, fmt.Errorf("%w: %s", ErrHostBlocked, host)
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// Error is a non-2xx answer from the API server. It carries the status and
// the API's machine-readable reason, never the response body.
type Error struct {
	Op     string
	Status int
	Reason string
}

func (e *Error) Error() string {
	return fmt.Sprintf("kubernetes %s: HTTP %d %s", e.Op, e.Status, e.Reason)
}

// StatusOf returns the HTTP status of an *Error in err's chain, or 0.
func StatusOf(err error) int {
	var ke *Error
	if errors.As(err, &ke) {
		return ke.Status
	}
	return 0
}

// Client reads and patches ConfigMaps of one namespace.
type Client struct {
	base      string // https://host:port
	namespace string
	http      *http.Client
	token     func() (string, error)
}

// ServiceAccountDir is where a pod's ServiceAccount token, CA and namespace
// are mounted.
const ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// Access is how a pod reaches the API server as its own ServiceAccount:
// the address, a transport that verifies the cluster CA and uses no proxy,
// and the token, re-read for every request because the kubelet rotates it.
// It grants nothing by itself; internal/loginbroker builds its TokenReview
// client from it.
type Access struct {
	// BaseURL is "https://host:port".
	BaseURL string
	// Transport verifies the API server against the cluster CA. Wrap it in
	// a Transport (this package) to pin requests to the API server.
	Transport http.RoundTripper
	// Token returns the ServiceAccount token.
	Token func() (string, error)
	// Namespace is the ServiceAccount's own.
	Namespace string
}

// InCluster reads the pod's environment (KUBERNETES_SERVICE_HOST / _PORT)
// and the ServiceAccount files in saDir.
func InCluster(getenv func(string) string, saDir string) (*Access, error) {
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
	raw, err := os.ReadFile(filepath.Join(saDir, "namespace"))
	if err != nil {
		return nil, fmt.Errorf("read the pod's namespace: %w", err)
	}
	tokenPath := filepath.Join(saDir, "token")
	if _, err := os.Stat(tokenPath); err != nil {
		return nil, fmt.Errorf("the ServiceAccount token is not mounted: %w", err)
	}
	return &Access{
		BaseURL: "https://" + net.JoinHostPort(host, port),
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 10 * time.Second,
			// Explicitly no proxy: the token goes to the API server only.
			Proxy: nil,
		},
		// The projected token is rotated by the kubelet; read it per request.
		Token: func() (string, error) {
			raw, err := os.ReadFile(tokenPath)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(raw)), nil
		},
		Namespace: strings.TrimSpace(string(raw)),
	}, nil
}

// NewInCluster builds the client from the pod's environment
// (KUBERNETES_SERVICE_HOST / _PORT) and the ServiceAccount files in saDir.
// namespace overrides the ServiceAccount's own when not empty.
func NewInCluster(getenv func(string) string, saDir, namespace string) (*Client, error) {
	access, err := InCluster(getenv, saDir)
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		namespace = access.Namespace
	}
	return New(access.BaseURL, namespace, access.Transport, access.Token)
}

// New builds a client for baseURL ("https://host:port") on the given inner
// transport. Tests use it with an httptest server.
func New(baseURL, namespace string, inner http.RoundTripper, token func() (string, error)) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the Kubernetes API address must be an https URL")
	}
	if namespace == "" || strings.ContainsAny(namespace, "/ \t\n") {
		return nil, errors.New("no usable namespace")
	}
	return &Client{
		base:      "https://" + u.Host,
		namespace: namespace,
		token:     token,
		http: &http.Client{
			Timeout:   20 * time.Second,
			Transport: &Transport{Host: u.Host, Base: inner},
			// The API server has no business redirecting these calls.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Namespace returns the namespace the client works in.
func (c *Client) Namespace() string { return c.namespace }

// HTTPClient returns the underlying client, for tests that prove what it
// refuses.
func (c *Client) HTTPClient() *http.Client { return c.http }

func (c *Client) path(configMap string) string {
	return "/api/v1/namespaces/" + url.PathEscape(c.namespace) + "/configmaps/" + url.PathEscape(configMap)
}

// maxAnswer bounds how much of an answer is read.
const maxAnswer = 1 << 20

func (c *Client) do(ctx context.Context, op, method, path, contentType string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	token, err := c.token()
	if err != nil || token == "" {
		return fmt.Errorf("kubernetes %s: could not read the ServiceAccount token", op)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kubernetes %s: %w", op, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var status struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &status)
		return &Error{Op: op, Status: resp.StatusCode, Reason: safeToken(status.Reason)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("kubernetes %s: undecodable answer", op)
	}
	return nil
}

// GetField returns the value of one data key of a ConfigMap. found is false
// when the ConfigMap exists but has no such key. A ConfigMap that does not
// exist is an error (status 404).
func (c *Client) GetField(ctx context.Context, configMap, key string) (value string, found bool, err error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := c.do(ctx, "get configmap", http.MethodGet, c.path(configMap), "", nil, &cm); err != nil {
		return "", false, err
	}
	value, found = cm.Data[key]
	return value, found, nil
}

// SetField sets one data key of an existing ConfigMap with a JSON merge
// patch. It touches no other key and cannot create the ConfigMap.
func (c *Client) SetField(ctx context.Context, configMap, key, value string) error {
	body, err := json.Marshal(map[string]any{"data": map[string]string{key: value}})
	if err != nil {
		return err
	}
	return c.do(ctx, "patch configmap", http.MethodPatch, c.path(configMap), "application/merge-patch+json", body, nil)
}

// safeToken keeps a short identifier-like prefix of s for an error message.
func safeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	return b.String()
}
