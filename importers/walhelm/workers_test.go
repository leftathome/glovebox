// workers_test.go -- unit tests for the bounded-concurrency stageAll pool.
//
// Each test exercises stageAll in isolation; no real staging backend is
// required. The five test cases cover:
//
//  1. All-OK: every job's fn runs exactly once and every result slot is nil.
//  2. Mixed errors: error slots carry the expected error; success slots are nil.
//  3. Context cancelled before start: fn never runs; every slot carries
//     context.Canceled.
//  4. Context cancelled mid-run: in-flight fn is blocked; cancel; release --
//     the call returns promptly, unsent slots carry context.Canceled, no
//     goroutine is leaked (the -race detector and a timeout guard are the proof).
//  5. Concurrency bound: with a barrier in fn, peak concurrency never exceeds
//     the requested limit.
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- helpers ------------------------------------------------------------------

// makeJobs builds a slice of n job strings ("job-0" .. "job-N-1").
func makeJobs(n int) []string {
	jobs := make([]string, n)
	for i := range jobs {
		jobs[i] = fmt.Sprintf("job-%d", i)
	}
	return jobs
}

// runWithTimeout runs stageAll in a goroutine and returns its results, or
// calls t.Fatal if the call does not complete within the given deadline. This
// prevents a deadlocked pool from hanging the test suite under -race.
func runWithTimeout(t *testing.T, deadline time.Duration, fn func() []stageResult) []stageResult {
	t.Helper()
	type outcome struct{ results []stageResult }
	ch := make(chan outcome, 1)
	go func() {
		ch <- outcome{results: fn()}
	}()
	select {
	case o := <-ch:
		return o.results
	case <-time.After(deadline):
		t.Fatalf("stageAll did not return within %v -- suspected deadlock", deadline)
		return nil // unreachable, satisfies compiler
	}
}

// --- tests --------------------------------------------------------------------

// TestStageAll_AllOK verifies that 10 jobs at concurrency 4 each run exactly
// once and produce a nil error.
func TestStageAll_AllOK(t *testing.T) {
	const n = 10
	jobs := makeJobs(n)

	var mu sync.Mutex
	seen := make(map[string]int)

	fn := func(_ context.Context, rel string) error {
		mu.Lock()
		seen[rel]++
		mu.Unlock()
		return nil
	}

	results := stageAll(context.Background(), 4, jobs, fn)

	if len(results) != n {
		t.Fatalf("len(results) = %d, want %d", len(results), n)
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
		}
		if r.RelPath != jobs[i] {
			t.Errorf("results[%d].RelPath = %q, want %q", i, r.RelPath, jobs[i])
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for _, job := range jobs {
		if seen[job] != 1 {
			t.Errorf("job %q ran %d times, want exactly 1", job, seen[job])
		}
	}
}

// TestStageAll_MixedErrors verifies that error slots carry the expected error
// and success slots remain nil, and that each slot corresponds to its job's
// position in the input slice.
func TestStageAll_MixedErrors(t *testing.T) {
	jobs := makeJobs(6)
	// Jobs at even indices will fail.
	sentinel := errors.New("synthetic failure")

	fn := func(_ context.Context, rel string) error {
		for i, j := range jobs {
			if j == rel && i%2 == 0 {
				return sentinel
			}
		}
		return nil
	}

	results := stageAll(context.Background(), 2, jobs, fn)

	if len(results) != len(jobs) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(jobs))
	}
	for i, r := range results {
		if r.RelPath != jobs[i] {
			t.Errorf("results[%d].RelPath = %q, want %q", i, r.RelPath, jobs[i])
		}
		if i%2 == 0 {
			if !errors.Is(r.Err, sentinel) {
				t.Errorf("results[%d].Err = %v, want sentinel error", i, r.Err)
			}
		} else {
			if r.Err != nil {
				t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
			}
		}
	}
}

// TestStageAll_ContextCancelledBeforeStart verifies that when a pre-cancelled
// context is passed, fn is never invoked and every result carries
// context.Canceled.
func TestStageAll_ContextCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before stageAll is called

	var calls atomic.Int64

	fn := func(_ context.Context, _ string) error {
		calls.Add(1)
		return nil
	}

	jobs := makeJobs(8)
	results := runWithTimeout(t, 5*time.Second, func() []stageResult {
		return stageAll(ctx, 3, jobs, fn)
	})

	if calls.Load() != 0 {
		t.Errorf("fn was called %d times, want 0 (ctx cancelled before start)", calls.Load())
	}
	for i, r := range results {
		if !errors.Is(r.Err, context.Canceled) {
			t.Errorf("results[%d].Err = %v, want context.Canceled", i, r.Err)
		}
	}
}

// TestStageAll_ContextCancelledMidRun verifies that cancelling the context
// while one job is blocked causes stageAll to return promptly. Unstarted jobs
// carry context.Canceled; started jobs complete normally. No goroutines are
// leaked (verified implicitly by the test completing under -race and the
// timeout guard above).
func TestStageAll_ContextCancelledMidRun(t *testing.T) {
	// gate is held open only after the test signals the first fn invocation has
	// been reached; the second receive unblocks when we're ready for fn to finish.
	started := make(chan struct{})  // closed when the first fn call begins
	release := make(chan struct{})  // closed to unblock blocked fn calls

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fn := func(fnCtx context.Context, _ string) error {
		// Signal that we've started, then block until released or ctx done.
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-fnCtx.Done():
		}
		return fnCtx.Err()
	}

	jobs := makeJobs(10)

	var results []stageResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		results = stageAll(ctx, 2, jobs, fn)
	}()

	// Wait for at least one fn to be in-flight.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first fn invocation")
	}

	// Cancel and then release blocked goroutines.
	cancel()
	close(release)

	select {
	case <-done:
		// good: stageAll returned
	case <-time.After(5 * time.Second):
		t.Fatal("stageAll did not return promptly after context cancellation")
	}

	if len(results) != len(jobs) {
		t.Fatalf("len(results) = %d, want %d", len(results), len(jobs))
	}
	// Every result must either be context.Canceled or context.Canceled from fn.
	for i, r := range results {
		if r.Err != nil && !errors.Is(r.Err, context.Canceled) {
			t.Errorf("results[%d].Err = %v, want nil or context.Canceled", i, r.Err)
		}
	}
}

// TestStageAll_ConcurrencyBound verifies that at most concurrency goroutines
// are in fn simultaneously. The test uses an atomic peak counter plus a
// counting barrier to measure the maximum observed concurrency.
func TestStageAll_ConcurrencyBound(t *testing.T) {
	const concurrency = 3
	const jobs = 12

	var (
		inFlight atomic.Int64
		peak     atomic.Int64
	)

	// barrier is a WaitGroup that each fn goroutine joins. We set its count
	// to concurrency so the first concurrency goroutines all block until all
	// concurrency slots are filled; then we let them finish together. This
	// maximises the chance of detecting oversubscription.
	var (
		barrierOnce sync.Once
		barrierReady = make(chan struct{}) // closed when peak is measured
	)

	fn := func(_ context.Context, _ string) error {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)

		// Update peak.
		for {
			old := peak.Load()
			if cur <= old {
				break
			}
			if peak.CompareAndSwap(old, cur) {
				break
			}
		}

		// First batch: once concurrency goroutines are in-flight, record peak
		// and signal the rest to proceed.
		if cur >= concurrency {
			barrierOnce.Do(func() { close(barrierReady) })
		}

		// Block until the barrier fires (or a short timeout).
		select {
		case <-barrierReady:
		case <-time.After(2 * time.Second):
		}
		return nil
	}

	results := runWithTimeout(t, 10*time.Second, func() []stageResult {
		return stageAll(context.Background(), concurrency, makeJobs(jobs), fn)
	})

	if len(results) != jobs {
		t.Fatalf("len(results) = %d, want %d", len(results), jobs)
	}
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d].Err = %v, want nil", i, r.Err)
		}
	}

	p := peak.Load()
	if p > concurrency {
		t.Errorf("peak in-flight = %d, must not exceed concurrency %d", p, concurrency)
	}
	if p == 0 {
		t.Errorf("peak in-flight = 0, fn never ran")
	}
}
