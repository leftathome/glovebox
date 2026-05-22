package auth

import (
	"testing"
	"time"
)

func TestRateLimiter_AllowsBelowThreshold(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		Window:            time.Minute,
		PerIPMaxRejected:  10,
		LRUCapacity:       100,
		GlobalMaxRejected: 1000,
	})
	for i := 0; i < 10; i++ {
		if !rl.AllowReject("198.51.100.5") {
			t.Fatalf("attempt %d unexpectedly rate-limited", i+1)
		}
	}
}

func TestRateLimiter_TripsAtThreshold(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		Window:            time.Minute,
		PerIPMaxRejected:  3,
		LRUCapacity:       100,
		GlobalMaxRejected: 1000,
	})
	for i := 0; i < 3; i++ {
		if !rl.AllowReject("198.51.100.5") {
			t.Fatalf("attempt %d unexpectedly rate-limited", i+1)
		}
	}
	if rl.AllowReject("198.51.100.5") {
		t.Error("4th attempt should have been rate-limited")
	}
}

func TestRateLimiter_GlobalBackstop(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{
		Window:            time.Minute,
		PerIPMaxRejected:  1000, // effectively disabled per-IP
		LRUCapacity:       10000,
		GlobalMaxRejected: 3,
	})
	if !rl.AllowReject("198.51.100.1") {
		t.Fatal("1")
	}
	if !rl.AllowReject("198.51.100.2") {
		t.Fatal("2")
	}
	if !rl.AllowReject("198.51.100.3") {
		t.Fatal("3")
	}
	if rl.AllowReject("198.51.100.4") {
		t.Error("global backstop should have tripped at attempt 4")
	}
}

func TestNewRateLimiter_PanicsOnZeroConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  RateLimitConfig
	}{
		{"zero Window", RateLimitConfig{Window: 0, PerIPMaxRejected: 10, GlobalMaxRejected: 100}},
		{"zero PerIPMaxRejected", RateLimitConfig{Window: time.Minute, PerIPMaxRejected: 0, GlobalMaxRejected: 100}},
		{"zero GlobalMaxRejected", RateLimitConfig{Window: time.Minute, PerIPMaxRejected: 10, GlobalMaxRejected: 0}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewRateLimiter(%+v) did not panic", tc.cfg)
				}
			}()
			NewRateLimiter(tc.cfg)
		})
	}
}
