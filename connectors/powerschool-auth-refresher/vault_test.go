package main

import (
	"context"
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

	"github.com/leftathome/glovebox/connectors/powerschool"
)

const (
	testSAToken    = "sa-jwt-for-tests"
	testVaultToken = "vault-token-for-tests"
	testDataPath   = "/v1/secret/data/glovebox/powerschool/default/session"
)

// fakeVault is an httptest Vault speaking just enough of the HTTP API:
// Kubernetes auth login, KV v2 read/write on any path, and
// sys/capabilities-self. It serves TLS with httptest's own certificate.
type fakeVault struct {
	srv *httptest.Server

	mu           sync.Mutex
	requests     []string // "METHOD path"
	loginBodies  []map[string]any
	secrets      map[string]map[string]any // data path -> fields
	versions     map[string]int
	createdAt    map[string]string
	capabilities []string
	// status, when set for "METHOD path", is returned instead of handling.
	status map[string]int
	// tokens seen on authenticated requests.
	tokens []string
}

func newFakeVault(t *testing.T) *fakeVault {
	t.Helper()
	v := &fakeVault{
		secrets:      map[string]map[string]any{},
		versions:     map[string]int{},
		createdAt:    map[string]string{},
		capabilities: []string{"create", "read", "update"},
		status:       map[string]int{},
	}
	v.srv = httptest.NewTLSServer(http.HandlerFunc(v.handle))
	t.Cleanup(v.srv.Close)
	return v
}

func (v *fakeVault) handle(w http.ResponseWriter, r *http.Request) {
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
	if code, ok := v.status[key]; ok {
		writeJSON(code, map[string]any{"errors": []string{"permission denied"}})
		return
	}

	if r.URL.Path == "/v1/auth/kubernetes/login" {
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		v.loginBodies = append(v.loginBodies, in)
		writeJSON(200, map[string]any{"auth": map[string]any{
			"client_token": testVaultToken, "lease_duration": 600, "renewable": false,
			"policies": []string{"glovebox-powerschool-refresher"},
		}})
		return
	}

	v.tokens = append(v.tokens, r.Header.Get("X-Vault-Token"))
	if r.Header.Get("X-Vault-Token") != testVaultToken {
		writeJSON(403, map[string]any{"errors": []string{"permission denied"}})
		return
	}

	switch {
	case r.URL.Path == "/v1/sys/capabilities-self":
		var in struct {
			Paths []string `json:"paths"`
		}
		_ = json.Unmarshal(body, &in)
		out := map[string]any{}
		for _, p := range in.Paths {
			out[p] = v.capabilities
		}
		out["capabilities"] = v.capabilities
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
				"version": v.versions[r.URL.Path], "created_time": v.createdAt[r.URL.Path],
				"deletion_time": "", "destroyed": false,
			},
		}})
	case strings.HasPrefix(r.URL.Path, "/v1/secret/data/") && (r.Method == http.MethodPut || r.Method == http.MethodPost):
		var in struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(body, &in)
		v.secrets[r.URL.Path] = in.Data
		v.versions[r.URL.Path]++
		v.createdAt[r.URL.Path] = time.Now().UTC().Format(time.RFC3339Nano)
		writeJSON(200, map[string]any{"data": map[string]any{
			"version": v.versions[r.URL.Path], "created_time": v.createdAt[r.URL.Path],
			"deletion_time": "", "destroyed": false,
		}})
	default:
		writeJSON(404, map[string]any{"errors": []string{"unsupported path"}})
	}
}

// config returns a refresher config pointed at the fake, trusting its
// certificate through a CA bundle file -- the mechanism the chart uses.
func (v *fakeVault) config(t *testing.T) config {
	t.Helper()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "tls-ca-bundle.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: v.srv.Certificate().Raw})
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte(testSAToken), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.VaultAddr = v.srv.URL
	cfg.VaultCACert = caPath
	cfg.VaultK8sTokenPath = tokenPath
	return cfg
}

// cleanVaultEnv keeps the developer's own Vault settings out of the client.
func cleanVaultEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"VAULT_ADDR", "VAULT_TOKEN", "VAULT_CACERT", "VAULT_CAPATH", "VAULT_SKIP_VERIFY",
		"VAULT_NAMESPACE", "VAULT_CLIENT_CERT", "VAULT_CLIENT_KEY", "VAULT_TLS_SERVER_NAME", "VAULT_AGENT_ADDR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestVaultStoreLogsInWithTheKubernetesRoleAndWritesTheSessionPath(t *testing.T) {
	cleanVaultEnv(t)
	v := newFakeVault(t)
	cfg := v.config(t)
	ctx := context.Background()

	store, err := newVaultStore(ctx, cfg)
	if err != nil {
		t.Fatalf("newVaultStore: %v", err)
	}
	if len(v.loginBodies) != 1 {
		t.Fatalf("kubernetes logins = %d, want 1", len(v.loginBodies))
	}
	if got := v.loginBodies[0]["role"]; got != "glovebox-powerschool-refresher" {
		t.Errorf("login role = %v", got)
	}
	if got := v.loginBodies[0]["jwt"]; got != testSAToken {
		t.Errorf("login jwt = %v, want the ServiceAccount token from the file", got)
	}

	// Nothing stored yet.
	if stored, err := store.Read(ctx); err != nil || stored != nil {
		t.Fatalf("Read on an empty path = (%v, %v), want (nil, nil)", stored, err)
	}
	if ok, err := store.CanWrite(ctx); err != nil || !ok {
		t.Fatalf("CanWrite = (%v, %v)", ok, err)
	}

	loggedInAt := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	session := normalizeSession(librarySession(loggedInAt), loggedInAt.Add(defaultSessionLifetime))
	data, err := sessionData(session, loggedInAt, testStudentID)
	if err != nil {
		t.Fatal(err)
	}
	version, err := store.Write(ctx, data)
	if err != nil || version != 1 {
		t.Fatalf("Write = (%d, %v), want version 1", version, err)
	}
	if _, ok := v.secrets[testDataPath]; !ok {
		t.Fatalf("nothing at %s; requests: %v", testDataPath, v.requests)
	}

	// What ESO will copy into the connector's session.json is the "session"
	// property, and the connector's parser accepts it.
	property, _ := v.secrets[testDataPath]["session"].(string)
	parsed, err := powerschool.ParseSession([]byte(property), loggedInAt)
	if err != nil {
		t.Fatalf("connector ParseSession on the stored property: %v", err)
	}
	if powerschool.SessionFingerprint(parsed) != powerschool.SessionFingerprint(session) {
		t.Error("the stored session is not the one written")
	}

	// And the next run reads it back with its login time.
	stored, err := store.Read(ctx)
	if err != nil || stored == nil {
		t.Fatalf("Read = (%v, %v)", stored, err)
	}
	if stored.Document != property || !stored.LoggedInAt.Equal(loggedInAt) {
		t.Errorf("Read returned login time %s (want %s)", stored.LoggedInAt, loggedInAt)
	}

	for _, tok := range v.tokens {
		if tok != testVaultToken {
			t.Errorf("a request carried token %q, want the one from the Kubernetes login", tok)
		}
	}
	for _, req := range v.requests {
		if strings.Contains(req, "/v1/secret/") && !strings.HasSuffix(req, " "+testDataPath) {
			t.Errorf("a request touched another secret path: %s", req)
		}
	}
}

func TestVaultStoreMountAndPathAreConfigurable(t *testing.T) {
	cleanVaultEnv(t)
	v := newFakeVault(t)
	cfg := v.config(t)
	cfg.VaultSessionPath = "glovebox/powerschool/other-household/session"
	store, err := newVaultStore(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Write(context.Background(), map[string]any{"session": "{}"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := v.secrets["/v1/secret/data/glovebox/powerschool/other-household/session"]; !ok {
		t.Errorf("requests: %v", v.requests)
	}
}

// A secret written by hand, without logged_in_at, takes its age from the KV
// version's creation time.
func TestVaultStoreFallsBackToTheVersionCreationTime(t *testing.T) {
	cleanVaultEnv(t)
	v := newFakeVault(t)
	created := time.Date(2026, 10, 12, 4, 0, 0, 0, time.UTC)
	v.secrets[testDataPath] = map[string]any{"session": `{"cookies":[]}`}
	v.versions[testDataPath] = 3
	v.createdAt[testDataPath] = created.Format(time.RFC3339Nano)

	store, err := newVaultStore(context.Background(), v.config(t))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Read(context.Background())
	if err != nil || stored == nil {
		t.Fatalf("Read = (%v, %v)", stored, err)
	}
	if !stored.LoggedInAt.Equal(created) {
		t.Errorf("LoggedInAt = %s, want the version's created_time %s", stored.LoggedInAt, created)
	}

	// A secret with no session field is "nothing stored".
	v.secrets[testDataPath] = map[string]any{"note": "placeholder"}
	if stored, err := store.Read(context.Background()); err != nil || stored != nil {
		t.Errorf("Read of a secret without a session = (%v, %v), want (nil, nil)", stored, err)
	}
}

func TestVaultStoreReportsMissingWriteCapability(t *testing.T) {
	cleanVaultEnv(t)
	for name, c := range map[string]struct {
		caps []string
		want bool
	}{
		"create, read and update": {[]string{"create", "read", "update"}, true},
		"root":                    {[]string{"root"}, true},
		"read only":               {[]string{"read"}, false},
		"update without create":   {[]string{"read", "update"}, false},
		"deny":                    {[]string{"deny"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			v := newFakeVault(t)
			v.capabilities = c.caps
			store, err := newVaultStore(context.Background(), v.config(t))
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.CanWrite(context.Background())
			if err != nil || got != c.want {
				t.Errorf("CanWrite = (%v, %v), want %v", got, err, c.want)
			}
		})
	}
}

func TestVaultStoreErrorsAreClassified(t *testing.T) {
	cleanVaultEnv(t)
	v := newFakeVault(t)
	store, err := newVaultStore(context.Background(), v.config(t))
	if err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	v.status["GET "+testDataPath] = 403
	v.status["PUT "+testDataPath] = 403
	v.mu.Unlock()

	if _, err := store.Read(context.Background()); err == nil || !isClientError(err) {
		t.Errorf("a 403 read: err = %v, isClientError = %v", err, isClientError(err))
	}
	if _, err := store.Write(context.Background(), map[string]any{"session": "{}"}); err == nil || !isClientError(err) {
		t.Errorf("a 403 write: err = %v, isClientError = %v", err, isClientError(err))
	}
}

// TLS verification is on. Without the CA bundle the fake's certificate is
// unknown and the login must fail -- also when VAULT_SKIP_VERIFY is set in
// the environment, which the Vault client would otherwise honour.
func TestVaultStoreVerifiesTheServerCertificate(t *testing.T) {
	cleanVaultEnv(t)
	v := newFakeVault(t)
	cfg := v.config(t)
	cfg.VaultCACert = ""

	if _, err := newVaultStore(context.Background(), cfg); err == nil {
		t.Fatal("connected to a Vault whose certificate is not trusted")
	} else if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("err = %v, want a certificate verification failure", err)
	}

	t.Setenv("VAULT_SKIP_VERIFY", "true")
	if _, err := newVaultStore(context.Background(), cfg); err == nil {
		t.Fatal("VAULT_SKIP_VERIFY switched verification off")
	}
	if len(v.loginBodies) != 0 {
		t.Errorf("the ServiceAccount token was sent to an unverified server %d time(s)", len(v.loginBodies))
	}
}

// A token in the environment must not stand in for the role.
func TestVaultStoreIgnoresAnAmbientToken(t *testing.T) {
	cleanVaultEnv(t)
	t.Setenv("VAULT_TOKEN", "ambient-root-token")
	v := newFakeVault(t)
	store, err := newVaultStore(context.Background(), v.config(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(v.loginBodies) != 1 {
		t.Errorf("kubernetes logins = %d, want 1", len(v.loginBodies))
	}
	for _, tok := range v.tokens {
		if tok != testVaultToken {
			t.Errorf("request carried token %q", tok)
		}
	}
}

// run() end to end against the httptest Vault: first run logs in and
// writes; a second run a few hours later finds the session young and good
// and writes nothing; a run the next morning replaces it.
func TestRunAgainstVaultAcrossThreeRuns(t *testing.T) {
	cleanVaultEnv(t)
	v := newFakeVault(t)
	cfg := v.config(t)

	h := newHarness(t, morning(t))
	h.cfg = cfg
	d := h.deps()
	d.connect = func(ctx context.Context) (sessionStore, error) { return newVaultStore(ctx, cfg) }

	if code := run(context.Background(), cfg, d); code != exitSuccess {
		t.Fatalf("first run: exit %d\n%s", code, h.logs.String())
	}
	if h.logins != 1 || v.versions[testDataPath] != 1 {
		t.Fatalf("first run: logins = %d, version = %d", h.logins, v.versions[testDataPath])
	}

	h.clock = h.clock.Add(4 * time.Hour)
	if code := run(context.Background(), cfg, d); code != exitSuccess {
		t.Fatalf("second run: exit %d\n%s", code, h.logs.String())
	}
	if h.logins != 1 || v.versions[testDataPath] != 1 {
		t.Errorf("second run logged in or wrote: logins = %d, version = %d", h.logins, v.versions[testDataPath])
	}
	if !h.logged("session still good") {
		t.Errorf("no 'session still good' line:\n%s", h.logs.String())
	}

	h.clock = h.clock.Add(20 * time.Hour)
	if code := run(context.Background(), cfg, d); code != exitSuccess {
		t.Fatalf("third run: exit %d\n%s", code, h.logs.String())
	}
	if h.logins != 2 || v.versions[testDataPath] != 2 {
		t.Errorf("third run: logins = %d, version = %d, want 2 and 2", h.logins, v.versions[testDataPath])
	}
	h.assertNoSecretsLogged()
	if strings.Contains(h.logs.String(), testVaultToken) || strings.Contains(h.logs.String(), testSAToken) {
		t.Errorf("a token appears in the log:\n%s", h.logs.String())
	}
}
