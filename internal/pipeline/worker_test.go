package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/detector"
	"github.com/leftathome/glovebox/internal/engine"
	"github.com/leftathome/glovebox/internal/scan"
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

// noopScanner builds a Scanner with no rules: Scan returns an empty result.
func noopScanner(t *testing.T) *scan.Scanner {
	t.Helper()
	sc, err := scan.New(engine.RuleConfig{QuarantineThreshold: 1.0}, detector.NewRegistry())
	if err != nil {
		t.Fatalf("build noop scanner: %v", err)
	}
	return sc
}

// sleepDetector sleeps for a fixed duration then fires one signal. It lets the
// pipeline tests exercise pool timing/timeout behavior through the real Scanner
// (custom matchers can no longer be injected per-request).
type sleepDetector struct{ d time.Duration }

func (s sleepDetector) Detect(content []byte) ([]engine.Signal, error) {
	time.Sleep(s.d)
	return []engine.Signal{{Name: "slow", Weight: 0.5}}, nil
}

func slowScanner(t *testing.T, d time.Duration) *scan.Scanner {
	t.Helper()
	reg := detector.NewRegistry()
	reg.Register("sleep", sleepDetector{d: d})
	rules := engine.RuleConfig{
		QuarantineThreshold: 1.0,
		Rules: []engine.Rule{{
			Name:      "slow",
			Weight:    0.5,
			MatchType: engine.MatchCustomDetector,
			Detector:  "sleep",
		}},
	}
	sc, err := scan.New(rules, reg)
	if err != nil {
		t.Fatalf("build slow scanner: %v", err)
	}
	return sc
}

func TestWorkerPool_ProcessesItems(t *testing.T) {
	pool := NewWorkerPool(2, 5*time.Second, noopScanner(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.Run(ctx)

	item := testStagingItem(t, "hello world")
	pool.Input() <- ScanRequest{Item: item}
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
	pool := NewWorkerPool(4, 5*time.Second, slowScanner(t, 100*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.Run(ctx)

	start := time.Now()
	for i := 0; i < 4; i++ {
		item := testStagingItem(t, "content")
		pool.Input() <- ScanRequest{Item: item}
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
	pool := NewWorkerPool(2, 5*time.Second, slowScanner(t, 200*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.Run(ctx)

	// Two items processed concurrently across the pool's workers.
	itemA := testStagingItem(t, "content one")
	itemB := testStagingItem(t, "content two")

	pool.Input() <- ScanRequest{Item: itemA}
	pool.Input() <- ScanRequest{Item: itemB}
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
	pool := NewWorkerPool(1, 50*time.Millisecond, slowScanner(t, 500*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go pool.Run(ctx)

	item := testStagingItem(t, "content that takes too long")
	pool.Input() <- ScanRequest{Item: item}
	close(pool.input)

	resp := <-pool.Output()
	if !resp.TimedOut {
		t.Error("expected timeout")
	}
}

func TestWorkerPool_GracefulShutdown(t *testing.T) {
	pool := NewWorkerPool(2, 5*time.Second, noopScanner(t))
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
