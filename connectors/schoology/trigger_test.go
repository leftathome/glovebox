package schoology

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestTriggerHandler_Accepts(t *testing.T) {
	signal := make(chan struct{}, 1)
	h := NewTriggerHandler("secret", time.Minute, signal, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/poll", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("status: got %d, want 202", rec.Code)
	}
	select {
	case <-signal:
	case <-time.After(100 * time.Millisecond):
		t.Error("PollSignal not fired")
	}
}

func TestTriggerHandler_RejectsBadToken(t *testing.T) {
	h := NewTriggerHandler("secret", time.Minute, make(chan struct{}, 1), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/poll", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d", rec.Code)
	}
}

func TestTriggerHandler_RejectsGET(t *testing.T) {
	h := NewTriggerHandler("secret", time.Minute, make(chan struct{}, 1), nil)
	req := httptest.NewRequest(http.MethodGet, "/v1/poll", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status: got %d", rec.Code)
	}
}

func TestTriggerHandler_Debounces(t *testing.T) {
	signal := make(chan struct{}, 1)
	h := NewTriggerHandler("secret", time.Minute, signal, nil)

	// First trigger: accepted.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/poll", nil)
	req1.Header.Set("Authorization", "Bearer secret")
	rec1 := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusAccepted {
		t.Errorf("first trigger: got %d", rec1.Code)
	}

	// Second trigger immediately: debounced.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/poll", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	rec2 := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Errorf("second trigger: got %d", rec2.Code)
	}
	ra := rec2.Header().Get("Retry-After")
	if ra == "" {
		t.Error("Retry-After missing")
	}
	secs, err := strconv.Atoi(ra)
	if err != nil || secs <= 0 || secs > 60 {
		t.Errorf("Retry-After should be a small positive int <= 60s, got %q (err=%v)", ra, err)
	}
}

func TestTriggerHandler_MissingAuthRejected(t *testing.T) {
	h := NewTriggerHandler("secret", time.Minute, make(chan struct{}, 1), nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/poll", nil) // no Authorization header
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("missing-auth status: got %d, want 401", rec.Code)
	}
}

func TestTriggerHandler_CoalescesWhenSignalFull(t *testing.T) {
	// When the PollSignal channel is already full (a poll is queued and the
	// consumer hasn't drained it), a subsequent trigger must still return
	// 202 but must NOT advance lastTrigger. The debounce window for any
	// future trigger should be measured from the FIRST trigger's time
	// rather than this no-op one. A practical proof: the next trigger
	// arriving right after the coalesced one should be debounced (429)
	// because the first trigger was within the debounce window.
	signal := make(chan struct{}, 1)
	signal <- struct{}{} // pre-fill so the first send drops to default

	h := NewTriggerHandler("secret", time.Minute, signal, nil)
	// Manually set lastTrigger to "long ago" so the debounce window has elapsed.
	h.lastTrigger = time.Now().Add(-2 * time.Minute)

	req := httptest.NewRequest(http.MethodPost, "/v1/poll", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Errorf("coalesced trigger should still get 202, got %d", rec.Code)
	}
	// lastTrigger must NOT have advanced (still ~2 minutes ago, well past debounce).
	since := time.Since(h.lastTrigger)
	if since < time.Minute {
		t.Errorf("lastTrigger advanced despite coalesce; since=%v should still be > 1m", since)
	}
}
