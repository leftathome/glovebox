package httpapi

import (
	"net/http"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// Placeholders: Task 27 replaces these bodies.

func (s *Server) handleHeld(w http.ResponseWriter, r *http.Request, c call) {
	refuse(w, http.StatusNotImplemented, journalapi.ReasonNotImplemented, "")
}

func (s *Server) handleErase(w http.ResponseWriter, r *http.Request, c call) {
	refuse(w, http.StatusNotImplemented, journalapi.ReasonNotImplemented, "")
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request, c call) {
	refuse(w, http.StatusNotImplemented, journalapi.ReasonNotImplemented, "")
}
