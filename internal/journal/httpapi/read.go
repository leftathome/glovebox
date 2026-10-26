package httpapi

import (
	"net/http"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// Placeholders: Task 26 replaces these bodies.

func (s *Server) handleState(w http.ResponseWriter, r *http.Request, c call) {
	refuse(w, http.StatusNotImplemented, journalapi.ReasonNotImplemented, "")
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, c call) {
	refuse(w, http.StatusNotImplemented, journalapi.ReasonNotImplemented, "")
}

func (s *Server) handleCounts(w http.ResponseWriter, r *http.Request, c call) {
	refuse(w, http.StatusNotImplemented, journalapi.ReasonNotImplemented, "")
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request, c call) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
