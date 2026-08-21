package ingest

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// CertReloader serves the current server certificate, re-reading it from
// disk when the files change.
//
// cert-manager renews a Certificate's Secret well before expiry and the
// kubelet updates the mounted files in place. Loading the keypair once at
// boot would mean the pod kept serving the old certificate until it
// happened to restart -- and eventually served an expired one. Reading on
// change keeps a short (24h) certificate lifetime practical, which is what
// makes a stolen key a small problem rather than a large one.
type CertReloader struct {
	certFile string
	keyFile  string

	mu       sync.RWMutex
	cert     *tls.Certificate
	certMod  time.Time
	keyMod   time.Time
	lastLoad time.Time
}

// NewCertReloader loads the keypair once so a misconfiguration fails at
// boot rather than at the first handshake.
func NewCertReloader(certFile, keyFile string) (*CertReloader, error) {
	r := &CertReloader{certFile: certFile, keyFile: keyFile}
	if err := r.reload(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *CertReloader) reload() error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load keypair %s/%s: %w", r.certFile, r.keyFile, err)
	}
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return fmt.Errorf("stat cert: %w", err)
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return fmt.Errorf("stat key: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.cert = &cert
	r.certMod = certInfo.ModTime()
	r.keyMod = keyInfo.ModTime()
	r.lastLoad = time.Now()
	return nil
}

// changed reports whether either file's mtime moved since the last load.
func (r *CertReloader) changed() bool {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return false
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !certInfo.ModTime().Equal(r.certMod) || !keyInfo.ModTime().Equal(r.keyMod)
}

// GetCertificate satisfies tls.Config.GetCertificate. A reload failure is
// not fatal: the previously good certificate keeps being served, because
// refusing every handshake because half a rotation is visible on disk
// would be a self-inflicted outage.
func (r *CertReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	if r.changed() {
		if err := r.reload(); err != nil {
			r.mu.RLock()
			defer r.mu.RUnlock()
			if r.cert != nil {
				return r.cert, nil
			}
			return nil, err
		}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cert == nil {
		return nil, errors.New("no certificate loaded")
	}
	return r.cert, nil
}

// MTLSOptions configures StartServerMTLS.
type MTLSOptions struct {
	Port         int
	Timeout      time.Duration
	CertFile     string
	KeyFile      string
	ClientCAFile string
}

// StartServerMTLS builds the mutual-TLS ingest server on its own mux.
//
// The caller runs ListenAndServeTLS("", "") -- the certificate comes from
// the reloader, not from the arguments.
func StartServerMTLS(mux *http.ServeMux, opts MTLSOptions) (*http.Server, error) {
	tlsConfig, err := BuildMTLSConfig(opts.CertFile, opts.KeyFile, opts.ClientCAFile)
	if err != nil {
		return nil, err
	}
	return &http.Server{
		Addr:      fmt.Sprintf(":%d", opts.Port),
		Handler:   mux,
		TLSConfig: tlsConfig,
		// Mirrors the plaintext listener: only the header read is bounded
		// here, so a large but legitimate ingest body is not cut off.
		ReadHeaderTimeout: opts.Timeout,
	}, nil
}

// BuildMTLSConfig returns a tls.Config that requires and verifies a client
// certificate signed by clientCAFile.
//
// TLS 1.3 is the floor: everything that talks to this endpoint is a Go
// client we ship, so there is no legacy peer to accommodate, and pinning
// the floor high removes the downgrade surface entirely.
func BuildMTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("client CA %s contained no usable certificates", clientCAFile)
	}

	reloader, err := NewCertReloader(certFile, keyFile)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      pool,
		GetCertificate: reloader.GetCertificate,
	}, nil
}
