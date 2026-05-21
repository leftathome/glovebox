package schoology

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// TriggerHandler implements connector.Listener for the POST /v1/poll
// endpoint. Bearer-token auth with constant-time comparison, configurable
// debounce. On accepted requests, signals the connector to poll via the
// supplied channel.
//
// Fields are set once before serving and not mutated thereafter; the
// handler itself uses a mutex to guard lastTrigger across concurrent
// requests.
//
// Tel is optional (nil-safe) so older call sites that don't yet pass
// a Telemetry value continue to work.
//
// TODO: candidate for extraction to connector primitive base type
// (any read-mostly connector might want a trigger endpoint).
type TriggerHandler struct {
	BearerToken      string
	DebounceDuration time.Duration
	PollSignal       chan<- struct{}
	Tel              *Telemetry

	mu          sync.Mutex
	lastTrigger time.Time
}

// NewTriggerHandler constructs a handler. tel is optional (nil-safe).
func NewTriggerHandler(token string, debounce time.Duration, pollSignal chan<- struct{}, tel *Telemetry) *TriggerHandler {
	return &TriggerHandler{
		BearerToken:      token,
		DebounceDuration: debounce,
		PollSignal:       pollSignal,
		Tel:              tel,
	}
}

// Handler returns the http.Handler implementing the Listener interface.
func (h *TriggerHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/poll", h.handlePoll)
	return mux
}

func (h *TriggerHandler) handlePoll(w http.ResponseWriter, r *http.Request) {
	ctx, span := h.Tel.StartSpan(r.Context(), "schoology.trigger.handle",
		attribute.String("client_addr", r.RemoteAddr),
	)
	defer span.End()
	_ = ctx // future: pass into downstream poll signaling

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		span.SetAttributes(attribute.String("outcome", "method_not_allowed"))
		return
	}
	auth := []byte(r.Header.Get("Authorization"))
	expected := []byte("Bearer " + h.BearerToken)
	if subtle.ConstantTimeCompare(auth, expected) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		h.Tel.RecordTriggerRequest("unauthorized")
		span.SetAttributes(attribute.String("outcome", "unauthorized"))
		return
	}

	h.mu.Lock()
	since := time.Since(h.lastTrigger)
	if since < h.DebounceDuration {
		remaining := h.DebounceDuration - since
		h.mu.Unlock()
		// Ceil to the next whole second so Retry-After never undershoots.
		secs := int((remaining + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		h.Tel.RecordTriggerRequest("debounced")
		span.SetAttributes(attribute.String("outcome", "debounced"))
		return
	}

	now := time.Now()
	// Non-blocking send. Only advance lastTrigger if the signal actually
	// queues; if the consumer's buffer is full, the trigger coalesces
	// with the pending one rather than poisoning the debounce window.
	select {
	case h.PollSignal <- struct{}{}:
		h.lastTrigger = now
	default:
	}
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"poll_queued_at": now.UTC().Format(time.RFC3339),
	})
	h.Tel.RecordTriggerRequest("accepted")
	span.SetAttributes(attribute.String("outcome", "accepted"))
}
