package httpapi

import (
	"net/http"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
)

// handleNotes is POST /v1/notes (Task 26 replaces this placeholder).
func (s *Server) handleNotes(w http.ResponseWriter, r *http.Request, c call) {
	refuse(w, http.StatusNotImplemented, journalapi.ReasonNotImplemented, "")
}
