package ingest_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/config"
	"github.com/leftathome/glovebox/internal/ingest"
	"github.com/leftathome/glovebox/internal/ingest/peer"
)

// testCA is a throwaway CA minted per test run. Real fixtures on disk would
// expire and would tempt someone to reuse them somewhere real.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	dir  string
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "glovebox-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return &testCA{cert: cert, key: key, dir: t.TempDir()}
}

// issue mints a leaf signed by the CA. spiffeURI may be empty (a cert with
// no SPIFFE SAN), and dnsName is set for server certs.
func (ca *testCA) issue(t *testing.T, name, spiffeURI, dnsName string, isServer bool) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	if isServer {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		tmpl.DNSNames = []string{dnsName}
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	} else {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	if spiffeURI != "" {
		u, err := url.Parse(spiffeURI)
		if err != nil {
			t.Fatalf("parse spiffe uri: %v", err)
		}
		tmpl.URIs = []*url.URL{u}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	certPath = filepath.Join(ca.dir, name+".crt")
	keyPath = filepath.Join(ca.dir, name+".key")
	writePEM(t, certPath, "CERTIFICATE", der)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
	return certPath, keyPath
}

func (ca *testCA) bundlePath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(ca.dir, "ca.crt")
	writePEM(t, p, "CERTIFICATE", ca.cert.Raw)
	return p
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatalf("encode pem: %v", err)
	}
}

// startMTLSIngest brings up the real mTLS server on a loopback port with a
// handler configured exactly as main.go configures it.
func startMTLSIngest(t *testing.T, ca *testCA, enforceSource bool) (addr string, stagingDir string) {
	t.Helper()
	stagingDir = t.TempDir()

	serverCert, serverKey := ca.issue(t, "server", "", "localhost", true)
	caBundle := ca.bundlePath(t)

	h := ingest.NewHandler(stagingDir, config.IngestConfig{
		Enabled:               true,
		MaxBodyBytes:          1 << 20,
		MaxMetadataBytes:      1 << 16,
		BackpressureThreshold: 1000,
		RequestTimeoutSeconds: 30,
	}, []string{"messaging"})
	h.SetReady()
	h.SetPeerEnforcement(enforceSource, "mtls")

	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", peer.Middleware("glovebox")(h))

	tlsCfg, err := ingest.BuildMTLSConfig(serverCert, serverKey, caBundle)
	if err != nil {
		t.Fatalf("BuildMTLSConfig: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, TLSConfig: tlsCfg}
	go srv.ServeTLS(ln, "", "")
	t.Cleanup(func() { srv.Close() })

	return ln.Addr().String(), stagingDir
}

func clientFor(t *testing.T, ca *testCA, certPath, keyPath string) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}
	if certPath != "" {
		kp, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			t.Fatalf("load client keypair: %v", err)
		}
		cfg.Certificates = []tls.Certificate{kp}
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}
}

func postItem(t *testing.T, client *http.Client, addr, source string) (int, string) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	meta := fmt.Sprintf(`{"source":%q,"sender":"a@example.com","timestamp":"2026-08-21T00:00:00Z","destination_agent":"messaging","content_type":"text/plain"}`, source)
	mh := make(map[string][]string)
	mh["Content-Disposition"] = []string{`form-data; name="metadata"`}
	mh["Content-Type"] = []string{"application/json"}
	part, err := mw.CreatePart(mh)
	if err != nil {
		t.Fatalf("create metadata part: %v", err)
	}
	part.Write([]byte(meta))

	cp, err := mw.CreateFormFile("content", "content.raw")
	if err != nil {
		t.Fatalf("create content part: %v", err)
	}
	cp.Write([]byte("Ordinary message body."))
	mw.Close()

	req, err := http.NewRequest(http.MethodPost, "https://"+addr+"/v1/ingest", &body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

// The handshake matrix: only a client holding a certificate from our CA
// gets to speak to the endpoint at all.
func TestMTLSIngest_HandshakeMatrix(t *testing.T) {
	ca := newTestCA(t)
	addr, _ := startMTLSIngest(t, ca, false)

	t.Run("no client certificate", func(t *testing.T) {
		client := clientFor(t, ca, "", "")
		status, body := postItem(t, client, addr, "rss")
		if status != 0 {
			t.Errorf("status = %d (%s), want a handshake failure", status, body)
		}
	})

	t.Run("certificate from another CA", func(t *testing.T) {
		other := newTestCA(t)
		certPath, keyPath := other.issue(t, "rogue", "spiffe://glovebox/connector/rss", "", false)
		client := clientFor(t, ca, certPath, keyPath)
		status, body := postItem(t, client, addr, "rss")
		if status != 0 {
			t.Errorf("status = %d (%s), want a handshake failure for an untrusted CA", status, body)
		}
	})

	t.Run("valid certificate", func(t *testing.T) {
		certPath, keyPath := ca.issue(t, "rss", "spiffe://glovebox/connector/rss", "", false)
		client := clientFor(t, ca, certPath, keyPath)
		status, body := postItem(t, client, addr, "rss")
		if status != http.StatusAccepted {
			t.Errorf("status = %d (%s), want 202", status, body)
		}
	})
}

// The point of the exercise: a connector cannot stamp another connector's
// source onto an item. Spec 08 section 3.10's "known limitation".
func TestMTLSIngest_SourceSpoofingRejected(t *testing.T) {
	ca := newTestCA(t)
	addr, stagingDir := startMTLSIngest(t, ca, true)

	certPath, keyPath := ca.issue(t, "rss", "spiffe://glovebox/connector/rss", "", false)
	client := clientFor(t, ca, certPath, keyPath)

	t.Run("matching source accepted", func(t *testing.T) {
		status, body := postItem(t, client, addr, "rss")
		if status != http.StatusAccepted {
			t.Fatalf("status = %d (%s), want 202", status, body)
		}
	})

	t.Run("spoofed source rejected", func(t *testing.T) {
		status, body := postItem(t, client, addr, "gmail")
		if status != http.StatusForbidden {
			t.Errorf("status = %d (%s), want 403: the rss connector must not be able to claim gmail's provenance", status, body)
		}
	})

	// Exactly one item should have been staged: the honest one.
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		t.Fatalf("read staging: %v", err)
	}
	staged := 0
	for _, e := range entries {
		if e.IsDir() && e.Name() != ".ingest-tmp" {
			staged++
		}
	}
	if staged != 1 {
		t.Errorf("staged %d items, want 1 (the spoofed item must not be written)", staged)
	}
}

// A certificate signed by our CA but carrying no SPIFFE identity gets
// through the handshake and must then be refused by the middleware.
func TestMTLSIngest_CertificateWithoutIdentityRejected(t *testing.T) {
	ca := newTestCA(t)
	addr, _ := startMTLSIngest(t, ca, true)

	certPath, keyPath := ca.issue(t, "anon", "", "", false)
	client := clientFor(t, ca, certPath, keyPath)

	status, body := postItem(t, client, addr, "rss")
	if status != http.StatusForbidden {
		t.Errorf("status = %d (%s), want 403 for a certificate with no SPIFFE identity", status, body)
	}
}

// With enforcement on, a request that somehow reaches the handler without a
// peer must fail closed rather than be treated as trusted.
func TestHandler_EnforcementFailsClosedWithoutPeer(t *testing.T) {
	stagingDir := t.TempDir()
	h := ingest.NewHandler(stagingDir, config.IngestConfig{
		Enabled:               true,
		MaxBodyBytes:          1 << 20,
		MaxMetadataBytes:      1 << 16,
		BackpressureThreshold: 1000,
	}, []string{"messaging"})
	h.SetReady()
	h.SetPeerEnforcement(true, "mtls")

	srv := &http.Server{Handler: h}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mh := map[string][]string{
		"Content-Disposition": {`form-data; name="metadata"`},
		"Content-Type":        {"application/json"},
	}
	part, _ := mw.CreatePart(mh)
	part.Write([]byte(`{"source":"rss","sender":"a@example.com","timestamp":"2026-08-21T00:00:00Z","destination_agent":"messaging","content_type":"text/plain"}`))
	cp, _ := mw.CreateFormFile("content", "content.raw")
	cp.Write([]byte("body"))
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v1/ingest", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 when enforcement is on but no peer is present", resp.StatusCode)
	}
}
