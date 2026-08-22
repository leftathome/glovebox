package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/leftathome/glovebox/internal/config"
)

// The plaintext HTTP surface carries three route families with three
// different auth models:
//
//	/v1/ingest     connector intake; authenticated by mTLS peer identity
//	               when ingest.tls.mode is permissive/required, and by
//	               nothing but a NetworkPolicy podSelector otherwise
//	/v1/archives*  tus.io archive upload; bearer token (spec 10)
//	/v1/sanitize   synchronous scan gate; bearer token (spec 10)
//
// They all used to share one mux on one listener whose lifecycle was tied
// to ingest.tls.mode, which had two consequences worth naming:
//
//  1. ingest.tls.mode=required opened only the mTLS listener, and that
//     listener mounts /v1/ingest alone -- so switching to required took
//     the two bearer-authenticated endpoints offline. They authenticate
//     themselves; nothing about moving the connector transport to mTLS
//     should have silenced them.
//  2. The recognizer namespace needs /v1/archives, so the chart grants it
//     ingress on the ingest port -- and that port also served
//     unauthenticated /v1/ingest (security review P0-7).
//
// planPlaintextListeners separates the two lifecycles. The bearer surface
// is opened whenever anything is mounted on it, in every tls mode; whether
// it gets a port of its own is ingest.bearer_port's job.

// plaintextListener is one plaintext HTTP listener and the route families
// it carries.
type plaintextListener struct {
	Port int
	// Ingest carries POST /v1/ingest.
	Ingest bool
	// Bearer carries /v1/archives* and /v1/sanitize.
	Bearer bool
}

// planPlaintextListeners decides which plaintext listeners to open. It is
// pure so the mode x split matrix is testable without binding ports; the
// returned slice is ordered ingest-first for stable logging.
func planPlaintextListeners(cfg config.IngestConfig) []plaintextListener {
	if !cfg.Enabled {
		return nil
	}
	// /v1/ingest is plaintext-served in every mode except required, where
	// the only path to it is the mTLS listener.
	ingestActive := cfg.TLS.PlaintextActive()
	bearerActive := cfg.BearerRoutesEnabled()

	if !cfg.BearerSplit() {
		if !ingestActive && !bearerActive {
			return nil
		}
		return []plaintextListener{{
			Port:   cfg.Port,
			Ingest: ingestActive,
			Bearer: bearerActive,
		}}
	}

	var out []plaintextListener
	if ingestActive {
		out = append(out, plaintextListener{Port: cfg.Port, Ingest: true})
	}
	if bearerActive {
		out = append(out, plaintextListener{Port: cfg.EffectiveBearerPort(), Bearer: true})
	}
	return out
}

// buildPlaintextMux assembles the mux for one planned listener.
// mountIngest attaches the connector intake handler; mountBearer attaches
// /v1/archives* and /v1/sanitize. Each is called at most once, only for
// the families this listener carries -- so a route family that is not on
// this listener is not merely blocked, it is not registered, and the
// listener answers 404 for it.
func buildPlaintextMux(l plaintextListener, mountIngest func(*http.ServeMux), mountBearer func(*http.ServeMux) error) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	if l.Ingest {
		mountIngest(mux)
	}
	if l.Bearer {
		if err := mountBearer(mux); err != nil {
			return nil, err
		}
	}
	return mux, nil
}

// newPlaintextServer wraps a mux in the http.Server the ingest plane uses.
//
// ReadHeaderTimeout bounds ONLY the header-read phase (slowloris
// protection). ReadTimeout/WriteTimeout are left 0 (unbounded) so a
// multi-GB archive PATCH -- the listener advertises Tus-Max-Size 30 GiB --
// is not killed by a whole-request deadline (glovebox-dddn: a 60s
// ReadTimeout force-closed any upload >60s). Per-route body bounds remain
// in place: /v1/ingest via http.MaxBytesReader (size), /v1/archives PATCH
// via the handler's patchBodyReader idle timeout.
func newPlaintextServer(port int, mux *http.ServeMux, headerTimeout time.Duration) *http.Server {
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: headerTimeout,
	}
}

// describe renders a listener for the boot log, so the pod log says which
// port is answering which endpoints without the operator having to infer
// it from the tls mode.
func (l plaintextListener) describe() string {
	switch {
	case l.Ingest && l.Bearer:
		return "/v1/ingest, /v1/archives*, /v1/sanitize"
	case l.Ingest:
		return "/v1/ingest"
	default:
		return "/v1/archives*, /v1/sanitize"
	}
}
