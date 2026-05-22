// Package ingest -- HTTP server wire-up for the scanner ingest
// endpoint (POST /v1/ingest). The spec 13 archive-delivery endpoint
// (/v1/archives*) is wired up by the sibling package's
// archives.StartArchiveListener, which avoids importing
// internal/ingest (otherwise we'd cycle: archives already imports
// internal/ingest for the WithDeliveredBy / BuildIdentity helpers in
// audit_provenance.go).
//
// The auth + archive wire-up (Wave C Task C3) lives in
// internal/ingest/archives/listener.go; callers invoke that during
// boot AFTER StartServer has set up the shared mux.
package ingest

import (
	"fmt"
	"net/http"
	"time"
)

// StartServer creates and returns an http.Server for the ingest endpoint.
// The caller is responsible for calling ListenAndServe on the returned server.
//
// StartServer wires the legacy /v1/ingest route only. The
// archive-delivery routes (/v1/archives*) are added by
// archives.StartArchiveListener with the supplied mux + handler;
// production callers invoke both functions during boot.
func StartServer(handler *Handler, port int, timeout time.Duration) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/v1/ingest", handler)

	return &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
	}
}

// StartServerWithMux is the variant that accepts an externally-built
// mux, so the caller can pre-mount the archive routes via
// archives.StartArchiveListener before wrapping the mux in the
// http.Server. The legacy /v1/ingest route is still attached here.
func StartServerWithMux(mux *http.ServeMux, handler *Handler, port int, timeout time.Duration) *http.Server {
	mux.Handle("/v1/ingest", handler)
	return &http.Server{
		Addr:         fmt.Sprintf(":%d", port),
		Handler:      mux,
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
	}
}
