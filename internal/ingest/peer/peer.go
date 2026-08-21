// Package peer -- verified client identity from mutual TLS.
//
// This is a leaf package on purpose: internal/ingest/auth already imports
// internal/ingest, so the identity types cannot live in auth without
// creating a cycle when the handler consumes them.
//
// Bearer tokens (spec 10) answer "is this caller allowed in". Peer identity
// answers "which caller is this", which is the question spec 08 section
// 3.10 left open: the handler trusted metadata.source, identity.provider
// and destination_agent as supplied, so any client that reached the port
// could stamp another connector's provenance onto an item. Encrypting the
// hop alone would not have fixed that; binding the claim to a verified
// certificate does.
package peer

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Peer is the verified identity of an mTLS client.
type Peer struct {
	// Kind is the SPIFFE path's first segment: "connector", "producer",
	// "importer".
	Kind string
	// Name is the second segment: the connector or producer name, e.g.
	// "rss" for spiffe://glovebox/connector/rss.
	Name string
	// URI is the full SPIFFE ID, recorded in audit provenance.
	URI string
}

// String renders the peer for logs and audit entries.
func (p Peer) String() string { return p.URI }

type peerContextKey struct{}

// NewContext attaches a verified peer to the request context.
func NewContext(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerContextKey{}, p)
}

// FromContext returns the verified peer, if the request arrived over mTLS.
// Handlers must treat "no peer" as "unauthenticated transport", never as
// "trusted".
func FromContext(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerContextKey{}).(Peer)
	return p, ok
}

// ErrNoSPIFFEID is returned when a client certificate carries no usable
// SPIFFE URI SAN.
var ErrNoSPIFFEID = errors.New("client certificate has no spiffe:// URI SAN")

// FromCertificate extracts the SPIFFE identity from a verified client
// certificate.
//
// Identity is read from a URI SAN rather than the Common Name: CN is free
// text with no structure and no uniqueness guarantee, while a URI SAN is
// what SPIFFE-aware issuers populate and what a policy can match on
// exactly.
func FromCertificate(cert *x509.Certificate, trustDomain string) (Peer, error) {
	if cert == nil {
		return Peer{}, ErrNoSPIFFEID
	}
	for _, u := range cert.URIs {
		if u == nil || u.Scheme != "spiffe" {
			continue
		}
		if !strings.EqualFold(u.Host, trustDomain) {
			// A certificate from another trust domain is not ours to
			// honour, even if the CA happens to chain.
			continue
		}
		kind, name, err := splitSPIFFEPath(u)
		if err != nil {
			return Peer{}, err
		}
		return Peer{Kind: kind, Name: name, URI: u.String()}, nil
	}
	return Peer{}, ErrNoSPIFFEID
}

func splitSPIFFEPath(u *url.URL) (kind, name string, err error) {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("spiffe id %q must have the form spiffe://<trust-domain>/<kind>/<name>", u.String())
	}
	return parts[0], parts[1], nil
}

// Middleware requires a verified client certificate and attaches the
// resulting identity to the request context.
//
// It is mounted only on the mTLS listener. The plaintext listener (while
// one is still open during migration) never reaches this, and so never
// produces a peer -- which is exactly why handlers must fail closed on a
// missing peer rather than assuming the transport was checked.
func Middleware(trustDomain string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				writeAuthError(w, http.StatusUnauthorized, "client_certificate_required")
				return
			}
			id, err := FromCertificate(r.TLS.PeerCertificates[0], trustDomain)
			if err != nil {
				writeAuthError(w, http.StatusForbidden, "unrecognized_client_identity")
				return
			}
			next.ServeHTTP(w, r.WithContext(NewContext(r.Context(), id)))
		})
	}
}

// writeAuthError emits the same opaque JSON shape the bearer path uses, so
// a caller cannot distinguish "no certificate" from "certificate we do not
// recognise" by response body.
func writeAuthError(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, "{\"error\":%q}\n", reason)
}
