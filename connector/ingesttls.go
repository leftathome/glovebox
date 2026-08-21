package connector

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// Environment variables that switch a connector onto the mutual-TLS ingest
// path. All three must be set; a partial configuration is an error rather
// than a silent fall back to plaintext, because falling back would quietly
// undo the control the operator was trying to turn on.
const (
	EnvIngestCA         = "GLOVEBOX_INGEST_CA"
	EnvIngestClientCert = "GLOVEBOX_INGEST_CLIENT_CERT"
	EnvIngestClientKey  = "GLOVEBOX_INGEST_CLIENT_KEY"
)

// IngestTLSConfigured reports whether any of the mTLS ingest variables are
// set, so the caller can distinguish "not configured" from "misconfigured".
func IngestTLSConfigured() bool {
	return os.Getenv(EnvIngestCA) != "" ||
		os.Getenv(EnvIngestClientCert) != "" ||
		os.Getenv(EnvIngestClientKey) != ""
}

// NewIngestHTTPClient returns the client connectors use to POST to
// /v1/ingest.
//
// When the mTLS variables are set it presents a client certificate and
// verifies the server against the supplied CA; otherwise it returns a
// plain client, preserving the pre-existing behaviour for deployments that
// have not migrated. Every connector picks this up from the framework, so
// no per-connector code changes.
//
// The client keypair is re-read when it changes on disk, matching the
// server side: cert-manager rotates a 24h certificate several times a day
// and a connector that cached the keypair at boot would start failing
// handshakes as soon as its copy expired.
func NewIngestHTTPClient(timeout time.Duration) (*http.Client, error) {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if !IngestTLSConfigured() {
		return &http.Client{Timeout: timeout}, nil
	}

	caFile := os.Getenv(EnvIngestCA)
	certFile := os.Getenv(EnvIngestClientCert)
	keyFile := os.Getenv(EnvIngestClientKey)
	if caFile == "" || certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("mTLS ingest requires all of %s, %s and %s; a partial configuration would silently fall back to plaintext",
			EnvIngestCA, EnvIngestClientCert, EnvIngestClientKey)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read ingest CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("ingest CA %s contained no usable certificates", caFile)
	}

	kp := &clientKeypair{certFile: certFile, keyFile: keyFile}
	if _, err := kp.load(); err != nil {
		return nil, err
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:           tls.VersionTLS13,
				RootCAs:              pool,
				GetClientCertificate: kp.GetClientCertificate,
			},
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     90 * time.Second,
		},
	}, nil
}

type clientKeypair struct {
	certFile string
	keyFile  string

	mu      sync.RWMutex
	cert    *tls.Certificate
	certMod time.Time
	keyMod  time.Time
}

func (k *clientKeypair) load() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(k.certFile, k.keyFile)
	if err != nil {
		return nil, fmt.Errorf("load ingest client keypair: %w", err)
	}
	certInfo, err := os.Stat(k.certFile)
	if err != nil {
		return nil, err
	}
	keyInfo, err := os.Stat(k.keyFile)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cert = &cert
	k.certMod = certInfo.ModTime()
	k.keyMod = keyInfo.ModTime()
	return &cert, nil
}

func (k *clientKeypair) changed() bool {
	certInfo, err := os.Stat(k.certFile)
	if err != nil {
		return false
	}
	keyInfo, err := os.Stat(k.keyFile)
	if err != nil {
		return false
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return !certInfo.ModTime().Equal(k.certMod) || !keyInfo.ModTime().Equal(k.keyMod)
}

// GetClientCertificate satisfies tls.Config.GetClientCertificate. A reload
// failure keeps the last good keypair rather than dropping every request
// mid-rotation.
func (k *clientKeypair) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	if k.changed() {
		if _, err := k.load(); err != nil {
			k.mu.RLock()
			defer k.mu.RUnlock()
			if k.cert != nil {
				return k.cert, nil
			}
			return nil, err
		}
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.cert, nil
}
