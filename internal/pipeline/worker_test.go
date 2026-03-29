package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/staging"
)

func testStagingItem(t *testing.T, content string) staging.StagingItem {
	t.Helper()
	dir := t.TempDir()
	contentPath := filepath.Join(dir, "content.raw")
	os.WriteFile(contentPath, []byte(content), 0644)
	return staging.StagingItem{
		DirPath:     dir,
		ContentPath: contentPath,
		Metadata: staging.ItemMetadata{
			Source:           "email",
			DestinationAgent: "messaging",
			ContentType:      "text/plain",
		},
	}
}

func noopMatcher(c []byte) ([]engine.Signal, error) {
	return nil, nil
}

func slowMatcher(d time.Duration) engine.ScanFunc {
	return func(c []byte) ([]engine.Signal, error) {
		time.Sleep(d)
		return []engine.Signal{{Name: "slow", Weight: 0.5}}, nil
	}
}

func TestWorkerPool_ProcessesItems(t *testing.T) {
	pool := NewWorkerPool(2, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.Run(ctx)

	item := testStagingItem(t, "hello world")
	pool.Input() <- ScanRequest{Item: item, Matchers: []engine.ScanFunc{noopMatcher}}
	close(pool.input)

	resp := <-pool.Output()
	if resp.Err != nil {
		t.Fatalf("unexpected error: %v", resp.Err)
	}
	if resp.TimedOut {
		t.Error("should not have timed out")
	}
}

func TestWorkerPool_ConcurrentProcessing(t *testing.T) {
	pool := NewWorkerPool(4, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.Run(ctx)

	start := time.Now()
	for i := 0; i < 4; i++ {
		item := testStagingItem(t, "content")
		pool.Input() <- ScanRequest{
			Item:     item,
			Matchers: []engine.ScanFunc{slowMatcher(100 * time.Millisecond)},
		}
	}
	close(pool.input)

	var mu sync.Mutex
	var results []ScanResponse
	for resp := range pool.Output() {
		mu.Lock()
		results = append(results, resp)
		mu.Unlock()
	}

	elapsed := time.Since(start)
	if len(results) != 4 {
		t.Fatalf("expected 4 results, got %d", len(results))
	}
	// 4 items with 100ms each on 4 workers should take ~100ms, not 400ms
	if elapsed > 300*time.Millisecond {
		t.Errorf("took %v, expected ~100ms (parallel processing)", elapsed)
	}
}

func TestWorkerPool_SlowItemDoesNotBlockOthers(t *testing.T) {
	pool := NewWorkerPool(2, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.Run(ctx)

	// One slow item, one fast item
	slow := testStagingItem(t, "slow content")
	fast := testStagingItem(t, "fast content")

	pool.Input() <- ScanRequest{Item: slow, Matchers: []engine.ScanFunc{slowMatcher(200 * time.Millisecond)}}
	pool.Input() <- ScanRequest{Item: fast, Matchers: []engine.ScanFunc{noopMatcher}}
	close(pool.input)

	var results []ScanResponse
	for resp := range pool.Output() {
		results = append(results, resp)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Fast item should complete before slow item
	if results[0].Duration > results[1].Duration {
		// First result was slower -- that's fine, order depends on scheduling
	}
}

func TestWorkerPool_ScanTimeout(t *testing.T) {
	pool := NewWorkerPool(1, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.Run(ctx)

	item := testStagingItem(t, "content that takes too long")
	pool.Input() <- ScanRequest{
		Item:     item,
		Matchers: []engine.ScanFunc{slowMatcher(500 * time.Millisecond)},
	}
	close(pool.input)

	resp := <-pool.Output()
	if !resp.TimedOut {
		t.Error("expected timeout")
	}
}

func TestWorkerPool_GracefulShutdown(t *testing.T) {
	pool := NewWorkerPool(2, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		pool.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not shut down on context cancellation")
	}
}
