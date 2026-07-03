package sanitizeapi

import (
	"encoding/json"
	"net/http"

	"github.com/leftathome/glovebox/internal/engine"
)

// scanner is the subset of *scan.Scanner the handler needs; an interface so the
// fail-closed error path can be tested with a fake. *scan.Scanner satisfies it.
type scanner interface {
	Scan(content []byte, contentType string) (engine.ScanResult, error)
}

// SanitizeHandler implements the generated ServerInterface using the shared
// scanner. Classify-not-rewrite: it returns a verdict, never a cleaned body.
type SanitizeHandler struct{ scanner scanner }

// NewSanitizeHandler builds the handler over the shared scanner.
func NewSanitizeHandler(s scanner) *SanitizeHandler { return &SanitizeHandler{scanner: s} }

// Sanitize handles POST /v1/sanitize (the generated ServerInterface method).
func (h *SanitizeHandler) Sanitize(w http.ResponseWriter, r *http.Request) {
	var req SanitizeRequest
	// Lenient input for a gate: do NOT DisallowUnknownFields (the OpenAPI
	// schema does not set additionalProperties:false), so extra fields are
	// tolerated.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content == "" {
		http.Error(w, "malformed request: content required", http.StatusBadRequest)
		return
	}
	ct := "text/plain"
	if req.ContentType != nil && *req.ContentType != "" {
		ct = *req.ContentType
	}
	res, err := h.scanner.Scan([]byte(req.Content), ct)
	if err != nil {
		// Fail closed: an engine error is never a pass.
		http.Error(w, "scan error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, toResponse(res))
}

func toResponse(res engine.ScanResult) SanitizeResponse {
	sigs := make([]Signal, 0, len(res.Signals))
	for _, s := range res.Signals {
		sigs = append(sigs, Signal{Name: s.Name, Weight: s.Weight, Matched: s.Matched})
	}
	return SanitizeResponse{Verdict: SanitizeResponseVerdict(res.Verdict), TotalScore: res.TotalScore, Signals: sigs}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
