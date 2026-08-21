package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A certificate that is loaded once at boot stops being valid partway
// through the pod's life, which is what makes short-lived certificates
// impractical. The reloader must pick up a rotation from disk.
func TestCertReloader_PicksUpRotation(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	first := mustSelfSigned(t, "first")
	writeFile(t, certPath, first.certPEM)
	writeFile(t, keyPath, first.keyPEM)

	r, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}
	before, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}

	// Rotate. The mtime must actually move for the change to be visible.
	second := mustSelfSigned(t, "second")
	time.Sleep(10 * time.Millisecond)
	writeFile(t, certPath, second.certPEM)
	writeFile(t, keyPath, second.keyPEM)
	future := time.Now().Add(time.Second)
	os.Chtimes(certPath, future, future)
	os.Chtimes(keyPath, future, future)

	after, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate after rotation: %v", err)
	}
	if string(before.Certificate[0]) == string(after.Certificate[0]) {
		t.Error("reloader kept serving the old certificate after rotation")
	}
}

// Half a rotation on disk must not take the listener down: the last good
// keypair keeps serving.
func TestCertReloader_KeepsLastGoodOnBadReload(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	good := mustSelfSigned(t, "good")
	writeFile(t, certPath, good.certPEM)
	writeFile(t, keyPath, good.keyPEM)

	r, err := NewCertReloader(certPath, keyPath)
	if err != nil {
		t.Fatalf("NewCertReloader: %v", err)
	}

	writeFile(t, certPath, []byte("-----BEGIN CERTIFICATE-----\ngarbage\n-----END CERTIFICATE-----\n"))
	future := time.Now().Add(time.Second)
	os.Chtimes(certPath, future, future)

	cert, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate returned an error instead of the last good cert: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("no certificate served after a failed reload")
	}
}

func TestNewCertReloader_FailsFastOnMissingFiles(t *testing.T) {
	if _, err := NewCertReloader("/nonexistent/tls.crt", "/nonexistent/tls.key"); err == nil {
		t.Error("expected an error for missing keypair; a misconfiguration must fail at boot, not at first handshake")
	}
}

func TestBuildMTLSConfig_RejectsEmptyCABundle(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")
	caPath := filepath.Join(dir, "ca.crt")

	kp := mustSelfSigned(t, "server")
	writeFile(t, certPath, kp.certPEM)
	writeFile(t, keyPath, kp.keyPEM)
	writeFile(t, caPath, []byte("not a certificate\n"))

	if _, err := BuildMTLSConfig(certPath, keyPath, caPath); err == nil {
		t.Error("expected an error for a CA bundle with no usable certificates")
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
