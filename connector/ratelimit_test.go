package connector

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestRateLimitNoHeaders(t *testing.T) {
	rl := NewRateLimiter()
	ctx := context.Background()

	start := time.Now()
	err := rl.Wait(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("Wait took %v, expected immediate return", elapsed)
	}
}

func TestRateLimitRemainingZeroBlocks(t *testing.T) {
	rl := NewRateLimiter()
	ctx := context.Background()

	// Use a reset time 2 seconds in the future. Unix timestamps have
	// 1-second granularity so very short durations can round to zero.
	resetTime := time.Now().Add(2 * time.Second)
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"X-Ratelimit-Remaining": []string{"0"},
			"X-Ratelimit-Reset":     []string{fmt.Sprintf("%d", resetTime.Unix())},
		},
	}
	rl.Update(resp)

	// Cancel the context after 200ms so we don't wait the full 2s.
	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := rl.Wait(waitCtx)
	elapsed := time.Since(start)

	// We expect the context to expire while blocking.
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded (proving Wait blocked), got %v", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("Wait returned too quickly (%v), expected blocking for ~200ms", elapsed)
	}
}

func TestRateLimitRetryAfter429(t *testing.T) {
	rl := NewRateLimiter()
	ctx := context.Background()

	resp := &http.Response{
		StatusCode: 429,
		Header: http.Header{
			"Retry-After": []string{"1"},
		},
	}
	rl.Update(resp)

	start := time.Now()
	err := rl.Wait(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("Wait returned too quickly (%v), expected ~1s", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Wait took too long (%v), expected ~1s", elapsed)
	}
}

func TestRateLimitRetryAfterCapped(t *testing.T) {
	rl := NewRateLimiter()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Retry-After of 600 seconds exceeds maxRateLimitWait (5 min = 300s).
	// The wait should be capped at 5 minutes, and since our context times
	// out in 500ms, we should get a context error quickly.
	resp := &http.Response{
		StatusCode: 429,
		Header: http.Header{
			"Retry-After": []string{"600"},
		},
	}
	rl.Update(resp)

	err := rl.Wait(ctx)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}

	// Verify the internal retryAt was capped: it should be at most
	// maxRateLimitWait from now, not 600s from now.
	rl.mu.Lock()
	retryAt := rl.retryAt
	rl.mu.Unlock()
	maxAllowed := time.Now().Add(maxRateLimitWait + 5*time.Second)
	if retryAt.After(maxAllowed) {
		t.Fatalf("retryAt %v exceeds capped max %v", retryAt, maxAllowed)
	}
}

func TestRateLimitXRateLimitResetUnixTimestamp(t *testing.T) {
	rl := NewRateLimiter()

	resetTime := time.Now().Add(2 * time.Second)
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"X-Ratelimit-Remaining": []string{"0"},
			"X-Ratelimit-Reset":     []string{fmt.Sprintf("%d", resetTime.Unix())},
		},
	}
	rl.Update(resp)

	rl.mu.Lock()
	gotReset := rl.resetAt
	gotRemaining := rl.remaining
	rl.mu.Unlock()

	if gotRemaining != 0 {
		t.Fatalf("expected remaining=0, got %d", gotRemaining)
	}
	// resetAt should be within 2 seconds of the intended time
	// (1-second granularity from Unix timestamp conversion).
	diff := gotReset.Sub(resetTime)
	if diff < -2*time.Second || diff > 2*time.Second {
		t.Fatalf("resetAt %v not close to expected %v (diff %v)", gotReset, resetTime, diff)
	}
}

func TestRateLimitIETFResetSeconds(t *testing.T) {
	rl := NewRateLimiter()

	before := time.Now()
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"Ratelimit-Remaining": []string{"5"},
			"Ratelimit-Reset":     []string{"10"},
		},
	}
	rl.Update(resp)
	after := time.Now()

	rl.mu.Lock()
	gotReset := rl.resetAt
	gotRemaining := rl.remaining
	rl.mu.Unlock()

	if gotRemaining != 5 {
		t.Fatalf("expected remaining=5, got %d", gotRemaining)
	}
	// resetAt should be approximately now + 10 seconds.
	expectedLow := before.Add(10 * time.Second)
	expectedHigh := after.Add(10*time.Second + time.Second)
	if gotReset.Before(expectedLow) || gotReset.After(expectedHigh) {
		t.Fatalf("resetAt %v not in expected range [%v, %v]", gotReset, expectedLow, expectedHigh)
	}
}

func TestRateLimitContextCancellation(t *testing.T) {
	rl := NewRateLimiter()

	// Set up a rate limit that won't expire for a while.
	resetTime := time.Now().Add(10 * time.Second)
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"X-Ratelimit-Remaining": []string{"0"},
			"X-Ratelimit-Reset":     []string{fmt.Sprintf("%d", resetTime.Unix())},
		},
	}
	rl.Update(resp)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := rl.Wait(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Wait did not respect context cancellation (took %v)", elapsed)
	}
}

func TestRateLimitPreemptiveSlowdown(t *testing.T) {
	rl := NewRateLimiter()
	ctx := context.Background()

	resetTime := time.Now().Add(60 * time.Second)
	resp := &http.Response{
		StatusCode: 200,
		Header: http.Header{
			"X-Ratelimit-Remaining": []string{"5"},
			"X-Ratelimit-Reset":     []string{fmt.Sprintf("%d", resetTime.Unix())},
		},
	}
	rl.Update(resp)

	start := time.Now()
	err := rl.Wait(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	// remaining=5 < 10 should trigger 100ms pre-emptive delay.
	if elapsed < 90*time.Millisecond {
		t.Fatalf("expected pre-emptive delay (~100ms), got %v", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("delay too long (%v), expected ~100ms", elapsed)
	}
}

func TestRateLimitConcurrentUpdateWait(t *testing.T) {
	rl := NewRateLimiter()
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 200)

	// Hammer Update and Wait concurrently to detect races.
	for i := 0; i < 100; i++ {
		wg.Add(2)

		go func(n int) {
			defer wg.Done()
			resp := &http.Response{
				StatusCode: 200,
				Header: http.Header{
					"X-Ratelimit-Remaining": []string{fmt.Sprintf("%d", 50+n)},
					"X-Ratelimit-Reset":     []string{fmt.Sprintf("%d", time.Now().Add(time.Minute).Unix())},
				},
			}
			rl.Update(resp)
		}(i)

		go func() {
			defer wg.Done()
			if err := rl.Wait(ctx); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("Wait returned error during concurrent access: %v", err)
	}
}
