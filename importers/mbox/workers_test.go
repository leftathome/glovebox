package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// makeMessages returns n synthetic *Message values with unique
// byte-offsets so tests can assert per-message routing without
// fuller header construction.
func makeMessages(n int) []*Message {
	out := make([]*Message, n)
	for i := 0; i < n; i++ {
		out[i] = &Message{
			ByteOffset: int64(i * 1024),
			MessageID:  fmt.Sprintf("<msg-%d@test>", i),
			Size:       1024,
		}
	}
	return out
}

// runPool is the standard test driver: starts the pool, spawns a
// collector that drains Results to natural close (so workers never
// backpressure even with small QueueSize defaults), pushes every
// msg on Jobs, closes Jobs, then waits for the collector to finish.
// Returns the collected per-message results in arrival order.
func runPool(t *testing.T, ctx context.Context, pool *WorkerPool, msgs []*Message) []WorkerResult {
	t.Helper()
	pool.Start(ctx)

	doneCh := make(chan []WorkerResult, 1)
	go func() {
		var got []WorkerResult
		for r := range pool.Results() {
			got = append(got, r)
		}
		doneCh <- got
	}()

	for _, m := range msgs {
		pool.Jobs() <- m
	}
	close(pool.Jobs())

	select {
	case got := <-doneCh:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("collector never finished; pool deadlocked")
		return nil
	}
}

// Test_WorkerPool_AllOK -- happy path: every IngestFunc returns nil.
// Every message yields one StatusOK result; total count matches.
func Test_WorkerPool_AllOK(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{
		Concurrency: 4,
		Ingest:      func(context.Context, *Message) error { return nil },
	})
	got := runPool(t, context.Background(), pool, makeMessages(50))
	if len(got) != 50 {
		t.Fatalf("results count = %d, want 50", len(got))
	}
	for _, r := range got {
		if r.Status != StatusOK {
			t.Errorf("result for %s: status=%v, want StatusOK", r.Message.MessageID, r.Status)
		}
	}
}

// Test_WorkerPool_MixedOKAndFailed -- IngestFunc returns nil for
// even-indexed messages and an error for odd. Result count is
// preserved; per-message status matches the script.
func Test_WorkerPool_MixedOKAndFailed(t *testing.T) {
	errOdd := errors.New("scripted failure")
	pool := NewWorkerPool(WorkerPoolConfig{
		Concurrency: 4,
		Ingest: func(_ context.Context, msg *Message) error {
			if (msg.ByteOffset/1024)%2 == 1 {
				return errOdd
			}
			return nil
		},
	})
	got := runPool(t, context.Background(), pool, makeMessages(20))
	if len(got) != 20 {
		t.Fatalf("results = %d, want 20", len(got))
	}
	var okCount, failCount int
	for _, r := range got {
		switch r.Status {
		case StatusOK:
			okCount++
		case StatusFailed:
			failCount++
			if r.Err == nil {
				t.Errorf("StatusFailed result has nil Err for %s", r.Message.MessageID)
			}
		default:
			t.Errorf("unexpected status %v for %s", r.Status, r.Message.MessageID)
		}
	}
	if okCount != 10 || failCount != 10 {
		t.Errorf("ok=%d failed=%d, want 10/10", okCount, failCount)
	}
}

// Test_WorkerPool_ConcurrencyCap -- with Concurrency=3 and an
// IngestFunc that sleeps 50ms, the peak active worker count must
// never exceed 3 even when the producer pushes 30 messages.
func Test_WorkerPool_ConcurrencyCap(t *testing.T) {
	const cap = 3
	pool := NewWorkerPool(WorkerPoolConfig{
		Concurrency: cap,
		Ingest: func(context.Context, *Message) error {
			time.Sleep(50 * time.Millisecond)
			return nil
		},
	})
	_ = runPool(t, context.Background(), pool, makeMessages(30))
	if peak := pool.Peak(); peak > cap {
		t.Errorf("peak in-flight workers = %d, want <= %d", peak, cap)
	} else if peak == 0 {
		t.Error("peak never recorded a non-zero value (instrumentation regression?)")
	}
}

// Test_WorkerPool_ContextCancelDuringIngest -- cancel the context
// mid-stream. The pool must NOT drop messages on the floor: every
// pushed Message must produce exactly one WorkerResult (some OK if
// they raced past the cancel, the rest StatusCancelled).
func Test_WorkerPool_ContextCancelDuringIngest(t *testing.T) {
	var started atomic.Int32

	ctx, cncl := context.WithCancel(context.Background())
	defer cncl()

	pool := NewWorkerPool(WorkerPoolConfig{
		Concurrency: 2,
		QueueSize:   32,
		Ingest: func(c context.Context, _ *Message) error {
			started.Add(1)
			<-c.Done()
			return c.Err()
		},
	})
	pool.Start(ctx)

	doneCh := make(chan []WorkerResult, 1)
	go func() {
		var got []WorkerResult
		for r := range pool.Results() {
			got = append(got, r)
		}
		doneCh <- got
	}()

	const N = 20
	msgs := makeMessages(N)
	for _, m := range msgs {
		pool.Jobs() <- m
	}
	close(pool.Jobs())

	// Wait until both workers are parked in IngestFunc, then cancel.
	for started.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	cncl()

	var got []WorkerResult
	select {
	case got = <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("collector never finished; Results did not close after cancel")
	}

	if len(got) != N {
		t.Errorf("results = %d, want %d (pool dropped messages on cancel)", len(got), N)
	}

	var cancelledCount, okCount int
	for _, r := range got {
		switch r.Status {
		case StatusCancelled:
			cancelledCount++
		case StatusOK:
			okCount++
		default:
			t.Errorf("unexpected status %v for %s", r.Status, r.Message.MessageID)
		}
	}
	if cancelledCount == 0 {
		t.Errorf("expected at least one StatusCancelled, got none (all %d were OK?)", okCount)
	}
}

// Test_WorkerPool_DefaultConcurrencyIsAtLeastOne -- guard against a
// misconfigured zero-concurrency pool deadlocking (workers=0 means
// the producer would block forever).
func Test_WorkerPool_DefaultConcurrencyIsAtLeastOne(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{
		Concurrency: 0,
		Ingest:      func(context.Context, *Message) error { return nil },
	})
	got := runPool(t, context.Background(), pool, makeMessages(1))
	if len(got) != 1 {
		t.Fatalf("results = %d, want 1 (zero-concurrency should default to >= 1)", len(got))
	}
}

// Test_WorkerPool_ResultsClosesAfterDrain -- once Jobs is closed and
// every queued message has emitted a result, Results MUST close
// (the closer goroutine guarantees this). Range-over-Results will
// terminate naturally; this asserts that contract.
func Test_WorkerPool_ResultsClosesAfterDrain(t *testing.T) {
	pool := NewWorkerPool(WorkerPoolConfig{
		Concurrency: 2,
		Ingest:      func(context.Context, *Message) error { return nil },
	})
	// runPool's collector ranges Results to natural close; if Results
	// never closes, the runPool helper's t.Fatal triggers via its
	// 10-second timeout. Reaching the assertion below proves the
	// closer-goroutine contract: Jobs closed -> workers exit ->
	// closer closes Results -> collector returns.
	got := runPool(t, context.Background(), pool, makeMessages(5))
	if len(got) != 5 {
		t.Fatalf("results = %d, want 5", len(got))
	}
}
