// Package vaulttest is a fake Vault for tests: an httptest TLS server
// speaking just enough of the HTTP API for internal/loginvault --
// Kubernetes auth login, KV v2 read and write (with check-and-set) on any
// path under secret/, sys/capabilities-self and token revoke-self.
//
// It is test support. Nothing outside _test files imports it.
package vaulttest

import (
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/loginvault"
)

// Token values the fake hands out and expects. Tests scan logs for them.
const (
	SAToken    = "sa-jwt-for-vault-tests"
	VaultToken = "vault-token-for-tests"
)

// Vault is the fake.
type Vault struct {
	Server *httptest.Server

	mu       sync.Mutex
	requests []string // "METHOD path"
	secrets  map[string]map[string]any
	versions map[string]int
	created  map[string]string
	roles    []string
	logins   int
	revokes  int
	// Status, when set for "METHOD path", is returned instead of handling.
	Status map[string]int
}

// New starts a fake Vault that is closed with the test.
func New(t *testing.T) *Vault {
	t.Helper()
	v := &Vault{
		secrets:  map[string]map[string]any{},
		versions: map[string]int{},
		created:  map[string]string{},
		Status:   map[string]int{},
	}
	v.Server = httptest.NewTLSServer(http.HandlerFunc(v.handle))
	t.Cleanup(v.Server.Close)
	return v
}

func dataPath(kvPath string) string { return "/v1/secret/data/" + strings.Trim(kvPath, "/") }

func (v *Vault) handle(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	v.requests = append(v.requests, key)
	body, _ := io.ReadAll(r.Body)

	writeJSON := func(code int, payload any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(payload)
	}
	if code, ok := v.Status[key]; ok {
		writeJSON(code, map[string]any{"errors": []string{"permission denied"}})
		return
	}
	if r.URL.Path == "/v1/auth/kubernetes/login" {
		var in struct {
			Role string `json:"role"`
			JWT  string `json:"jwt"`
		}
		_ = json.Unmarshal(body, &in)
		if in.JWT != SAToken {
			writeJSON(403, map[string]any{"errors": []string{"permission denied"}})
			return
		}
		v.logins++
		v.roles = append(v.roles, in.Role)
		writeJSON(200, map[string]any{"auth": map[string]any{
			"client_token": VaultToken, "lease_duration": 600, "renewable": false,
		}})
		return
	}
	if r.Header.Get("X-Vault-Token") != VaultToken {
		writeJSON(403, map[string]any{"errors": []string{"permission denied"}})
		return
	}
	switch {
	case r.URL.Path == "/v1/auth/token/revoke-self":
		v.revokes++
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/v1/sys/capabilities-self":
		caps := []string{"create", "read", "update"}
		var in struct {
			Paths []string `json:"paths"`
		}
		_ = json.Unmarshal(body, &in)
		out := map[string]any{"capabilities": caps}
		for _, p := range in.Paths {
			out[p] = caps
		}
		writeJSON(200, map[string]any{"data": out})
	case strings.HasPrefix(r.URL.Path, "/v1/secret/data/") && r.Method == http.MethodGet:
		fields, ok := v.secrets[r.URL.Path]
		if !ok {
			writeJSON(404, map[string]any{"errors": []string{}})
			return
		}
		writeJSON(200, map[string]any{"data": map[string]any{
			"data": fields,
			"metadata": map[string]any{
				"version": v.versions[r.URL.Path], "created_time": v.created[r.URL.Path],
				"deletion_time": "", "destroyed": false,
			},
		}})
	case strings.HasPrefix(r.URL.Path, "/v1/secret/data/") && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		var in struct {
			Data    map[string]any `json:"data"`
			Options map[string]any `json:"options"`
		}
		_ = json.Unmarshal(body, &in)
		if raw, ok := in.Options["cas"]; ok {
			if n, ok := raw.(float64); ok && int(n) != v.versions[r.URL.Path] {
				writeJSON(400, map[string]any{"errors": []string{"check-and-set parameter did not match the current version"}})
				return
			}
		}
		v.put(r.URL.Path, in.Data)
		writeJSON(200, map[string]any{"data": map[string]any{
			"version": v.versions[r.URL.Path], "created_time": v.created[r.URL.Path],
			"deletion_time": "", "destroyed": false,
		}})
	default:
		writeJSON(404, map[string]any{"errors": []string{"unsupported path"}})
	}
}

func (v *Vault) put(path string, fields map[string]any) {
	v.secrets[path] = fields
	v.versions[path]++
	v.created[path] = time.Now().UTC().Format(time.RFC3339Nano)
}

// Config returns a loginvault.Config pointed at the fake, trusting its
// certificate through a CA bundle file -- the mechanism the chart uses --
// with a ServiceAccount token file holding SAToken.
func (v *Vault) Config(t *testing.T, role, sessionPath, statePath string) loginvault.Config {
	t.Helper()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "vault-ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: v.Server.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(SAToken), 0o600); err != nil {
		t.Fatal(err)
	}
	return loginvault.Config{
		Addr: v.Server.URL, CACert: caPath, K8sRole: role, KVMount: "secret",
		SessionPath: sessionPath, StatePath: statePath, K8sTokenPath: tokenPath,
	}
}

// CleanEnv keeps the developer's own Vault settings out of the client.
func CleanEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"VAULT_ADDR", "VAULT_TOKEN", "VAULT_CACERT", "VAULT_CAPATH", "VAULT_SKIP_VERIFY",
		"VAULT_NAMESPACE", "VAULT_CLIENT_CERT", "VAULT_CLIENT_KEY", "VAULT_TLS_SERVER_NAME", "VAULT_AGENT_ADDR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// Secret returns a copy of the fields at a KV path (nil when absent) and
// its version.
func (v *Vault) Secret(kvPath string) (map[string]any, int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	src, ok := v.secrets[dataPath(kvPath)]
	if !ok {
		return nil, 0
	}
	out := make(map[string]any, len(src))
	for k, val := range src {
		out[k] = val
	}
	return out, v.versions[dataPath(kvPath)]
}

// Put writes a secret as another writer would, bumping its version.
func (v *Vault) Put(kvPath string, fields map[string]any) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.put(dataPath(kvPath), fields)
}

// Delete removes a secret, as an operator clearing a latch does.
func (v *Vault) Delete(kvPath string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.secrets, dataPath(kvPath))
	delete(v.versions, dataPath(kvPath))
}

// Requests returns every request so far, "METHOD path".
func (v *Vault) Requests() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.requests...)
}

// Counts returns how many Kubernetes logins and token revocations were
// made, and the roles logged in with.
func (v *Vault) Counts() (logins, revokes int, roles []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.logins, v.revokes, append([]string(nil), v.roles...)
}
