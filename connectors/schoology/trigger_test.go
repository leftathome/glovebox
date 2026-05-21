package schoology

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTriggerHandler_Accepts(t *testing.T) {
	signal := make(chan struct{}, 1)
	h := NewTriggerHandler("secret", time.Minute, signal)
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
	h := NewTriggerHandler("secret", time.Minute, make(chan struct{}, 1))
	req := httptest.NewRequest(http.MethodPost, "/v1/poll", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d", rec.Code)
	}
}

func TestTriggerHandler_RejectsGET(t *testing.T) {
	h := NewTriggerHandler("secret", time.Minute, make(chan struct{}, 1))
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
	h := NewTriggerHandler("secret", time.Minute, signal)

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
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("Retry-After missing")
	}
}
