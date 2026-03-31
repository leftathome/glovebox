package connector

import (
	"sync"
	"testing"
)

func TestFetchAllowed_WithinLimits(t *testing.T) {
	fc := NewFetchCounter(FetchLimits{PerSource: 10, PerPoll: 100})

	status := fc.TryFetch("source-a")
	if status != FetchAllowed {
		t.Fatalf("expected FetchAllowed, got %d", status)
	}
	if !status.Allowed() {
		t.Fatal("expected Allowed() to return true")
	}
	if fc.Count() != 1 {
		t.Fatalf("expected count 1, got %d", fc.Count())
	}
}

func TestFetchSourceLimit(t *testing.T) {
	fc := NewFetchCounter(FetchLimits{PerSource: 3, PerPoll: 100})

	for i := 0; i < 3; i++ {
		status := fc.TryFetch("source-a")
		if status != FetchAllowed {
			t.Fatalf("fetch %d: expected FetchAllowed, got %d", i+1, status)
		}
	}

	status := fc.TryFetch("source-a")
	if status != FetchSourceLimit {
		t.Fatalf("expected FetchSourceLimit, got %d", status)
	}
	if status.Allowed() {
		t.Fatal("expected Allowed() to return false for FetchSourceLimit")
	}

	// A different source should still be allowed.
	status = fc.TryFetch("source-b")
	if status != FetchAllowed {
		t.Fatalf("expected FetchAllowed for different source, got %d", status)
	}
}

func TestFetchPollLimit(t *testing.T) {
	fc := NewFetchCounter(FetchLimits{PerSource: 100, PerPoll: 5})

	for i := 0; i < 5; i++ {
		status := fc.TryFetch("source-a")
		if status != FetchAllowed {
			t.Fatalf("fetch %d: expected FetchAllowed, got %d", i+1, status)
		}
	}

	status := fc.TryFetch("source-a")
	if status != FetchPollLimit {
		t.Fatalf("expected FetchPollLimit, got %d", status)
	}
	if status.Allowed() {
		t.Fatal("expected Allowed() to return false for FetchPollLimit")
	}

	// Even a different source should be blocked by poll limit.
	status = fc.TryFetch("source-b")
	if status != FetchPollLimit {
		t.Fatalf("expected FetchPollLimit for different source, got %d", status)
	}
}

func TestFetchReset(t *testing.T) {
	fc := NewFetchCounter(FetchLimits{PerSource: 2, PerPoll: 5})

	fc.TryFetch("source-a")
	fc.TryFetch("source-a")
	fc.TryFetch("source-b")

	if fc.Count() != 3 {
		t.Fatalf("expected count 3 before reset, got %d", fc.Count())
	}

	fc.Reset()

	if fc.Count() != 0 {
		t.Fatalf("expected count 0 after reset, got %d", fc.Count())
	}

	// Should be able to fetch again after reset.
	status := fc.TryFetch("source-a")
	if status != FetchAllowed {
		t.Fatalf("expected FetchAllowed after reset, got %d", status)
	}
}

func TestFetchZeroLimitsUnlimited(t *testing.T) {
	fc := NewFetchCounter(FetchLimits{PerSource: 0, PerPoll: 0})

	// Should allow a large number of fetches without hitting any limit.
	for i := 0; i < 1000; i++ {
		status := fc.TryFetch("source-a")
		if status != FetchAllowed {
			t.Fatalf("fetch %d: expected FetchAllowed with zero limits, got %d", i+1, status)
		}
	}

	if fc.Count() != 1000 {
		t.Fatalf("expected count 1000, got %d", fc.Count())
	}
}

func TestFetchConcurrentSafety(t *testing.T) {
	fc := NewFetchCounter(FetchLimits{PerSource: 0, PerPoll: 0})

	const goroutines = 50
	const fetchesPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			source := "source"
			for i := 0; i < fetchesPerGoroutine; i++ {
				status := fc.TryFetch(source)
				if !status.Allowed() {
					t.Errorf("goroutine %d fetch %d: unexpected denial", id, i)
					return
				}
			}
		}(g)
	}

	wg.Wait()

	expected := goroutines * fetchesPerGoroutine
	if fc.Count() != expected {
		t.Fatalf("expected count %d after concurrent fetches, got %d", expected, fc.Count())
	}
}
