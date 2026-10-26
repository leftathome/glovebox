package httpapi

import "net/http"

// Placeholders: Task 27 replaces these bodies.

func (s *Server) handleHeld(w http.ResponseWriter, r *http.Request, c call) {
	refuse(w, http.StatusNotImplemented, "not_implemented", "")
}

func (s *Server) handleErase(w http.ResponseWriter, r *http.Request, c call) {
	refuse(w, http.StatusNotImplemented, "not_implemented", "")
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request, c call) {
	refuse(w, http.StatusNotImplemented, "not_implemented", "")
}
