package ingest

import (
	"fmt"
	"net/http"
	"time"
)

// StartServer creates and returns an http.Server for the ingest endpoint.
// The caller is responsible for calling ListenAndServe on the returned server.
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
