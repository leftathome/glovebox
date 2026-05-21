package schoology

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// TriggerHandler implements connector.Listener for the POST /v1/poll
// endpoint. Bearer-token auth, 60-second debounce by default. On accepted
// requests, signals the connector to poll via the supplied channel.
//
// TODO: candidate for extraction to connector primitive base type
// (any read-mostly connector might want a trigger endpoint).
type TriggerHandler struct {
	BearerToken      string
	DebounceDuration time.Duration
	PollSignal       chan<- struct{}

	mu          sync.Mutex
	lastTrigger time.Time
}

// NewTriggerHandler constructs a handler.
func NewTriggerHandler(token string, debounce time.Duration, pollSignal chan<- struct{}) *TriggerHandler {
	return &TriggerHandler{
		BearerToken:      token,
		DebounceDuration: debounce,
		PollSignal:       pollSignal,
	}
}

// Handler returns the http.Handler implementing the Listener interface.
func (h *TriggerHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/poll", h.handlePoll)
	return mux
}

func (h *TriggerHandler) handlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	auth := r.Header.Get("Authorization")
	expected := "Bearer " + h.BearerToken
	if auth != expected {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	h.mu.Lock()
	since := time.Since(h.lastTrigger)
	if since < h.DebounceDuration {
		remaining := h.DebounceDuration - since
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(remaining.Seconds())+1))
		h.mu.Unlock()
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	h.lastTrigger = time.Now()
	h.mu.Unlock()

	// Non-blocking send; drop if the channel buffer is full (the consumer
	// is already going to poll).
	select {
	case h.PollSignal <- struct{}{}:
	default:
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"poll_queued_at": time.Now().UTC().Format(time.RFC3339),
	})
}
