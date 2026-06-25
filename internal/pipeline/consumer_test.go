package pipeline

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TestConsumeResults_RoutesUnorderedAndExitsOnClose: unordered items are
// delivered as they arrive, and the consumer returns when output closes.
func TestConsumeResults_RoutesUnorderedAndExitsOnClose(t *testing.T) {
	var mu sync.Mutex
	var delivered []string
	router := NewRouter(func(resp ScanResponse) error {
		mu.Lock()
		delivered = append(delivered, resp.Item.DirPath)
		mu.Unlock()
		return nil
	})

	out := make(chan ScanResponse, 2)
	out <- unorderedResponse("a")
	out <- unorderedResponse("b")
	close(out)

	done := make(chan struct{})
	go func() {
		ConsumeResults(out, router, 0, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ConsumeResults did not return after output closed")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 {
		t.Fatalf("delivered %d, want 2", len(delivered))
	}
}

// TestConsumeResults_PeriodicFlushDeliversOrdered is the glovebox-v815 guard:
// ordered items must be delivered during runtime via the periodic flush, NOT
// held in memory until shutdown.
func TestConsumeResults_PeriodicFlushDeliversOrdered(t *testing.T) {
	var mu sync.Mutex
	delivered := make(map[string]bool)
	router := NewRouter(func(resp ScanResponse) error {
		mu.Lock()
		delivered[resp.Item.DirPath] = true
		mu.Unlock()
		return nil
	})

	out := make(chan ScanResponse) // unbuffered, never closed: only periodic flush can deliver
	go ConsumeResults(out, router, 20*time.Millisecond, nil)

	// Hand an ordered item to the consumer; Router.Route accumulates it.
	out <- orderedResponse("20260328-0001")

	// Without periodic flush it would sit in router.pending until shutdown.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		got := delivered["/staging/20260328-0001"]
		mu.Unlock()
		if got {
			return // delivered by the periodic flush -- pass
		}
		select {
		case <-deadline:
			t.Fatal("ordered item was not delivered by the periodic flush (glovebox-v815)")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestWithTimeout_TimesOutSlowDelivery is the glovebox-lnzp guard: a delivery
// that hangs must not block the caller; it returns ErrDeliveryTimeout.
func TestWithTimeout_TimesOutSlowDelivery(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	var timedOut bool
	wrapped := WithTimeout(func(resp ScanResponse) error {
		<-release // simulate a wedged networked-mount file op
		return nil
	}, 50*time.Millisecond, func(resp ScanResponse) { timedOut = true })

	done := make(chan error, 1)
	go func() { done <- wrapped(unorderedResponse("stuck")) }()

	select {
	case err := <-done:
		if !errors.Is(err, ErrDeliveryTimeout) {
			t.Fatalf("err = %v, want ErrDeliveryTimeout", err)
		}
		if !timedOut {
			t.Error("onTimeout callback was not invoked")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WithTimeout did not return on a hung delivery (glovebox-lnzp)")
	}
}

// TestWithTimeout_PassesThroughFastDelivery: a fast delivery returns its own
// result and never trips the timeout.
func TestWithTimeout_PassesThroughFastDelivery(t *testing.T) {
	sentinel := errors.New("delivery failed")
	wrapped := WithTimeout(func(resp ScanResponse) error {
		return sentinel
	}, time.Second, func(resp ScanResponse) { t.Error("onTimeout should not fire for a fast delivery") })

	if err := wrapped(unorderedResponse("fast")); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel passthrough", err)
	}
}
