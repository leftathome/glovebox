package watcher

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// writeReadyItem creates a staging item directory with metadata.json so
// the watcher's readiness gate considers it dispatchable.
func writeReadyItem(t *testing.T, dir, name string) {
	t.Helper()
	itemDir := filepath.Join(dir, name)
	os.MkdirAll(itemDir, 0755)
	os.WriteFile(filepath.Join(itemDir, "metadata.json"), []byte(`{}`), 0644)
}

func TestWatcher_DetectsNewDirectory(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var detected []string

	w := New(dir, 100*time.Millisecond, func(path string) {
		mu.Lock()
		detected = append(detected, filepath.Base(path))
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	time.Sleep(50 * time.Millisecond)
	writeReadyItem(t, dir, "20260328-item1")
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(detected) == 0 {
		t.Error("expected at least one item detected")
	}
}

func TestWatcher_FIFOOrder(t *testing.T) {
	dir := t.TempDir()

	// Pre-create directories with metadata.json before starting watcher
	writeReadyItem(t, dir, "20260328-0003-ccc")
	writeReadyItem(t, dir, "20260328-0001-aaa")
	writeReadyItem(t, dir, "20260328-0002-bbb")

	var mu sync.Mutex
	var detected []string

	w := New(dir, 50*time.Millisecond, func(path string) {
		mu.Lock()
		detected = append(detected, filepath.Base(path))
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	time.Sleep(150 * time.Millisecond)
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if len(detected) != 3 {
		t.Fatalf("detected %d items, want 3", len(detected))
	}
	if detected[0] != "20260328-0001-aaa" {
		t.Errorf("first = %q, want 20260328-0001-aaa", detected[0])
	}
	if detected[1] != "20260328-0002-bbb" {
		t.Errorf("second = %q, want 20260328-0002-bbb", detected[1])
	}
	if detected[2] != "20260328-0003-ccc" {
		t.Errorf("third = %q, want 20260328-0003-ccc", detected[2])
	}
}

func TestWatcher_PollingDetectsItems(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var detected []string

	// Use polling with short interval
	w := New(dir, 50*time.Millisecond, func(path string) {
		mu.Lock()
		detected = append(detected, filepath.Base(path))
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.runPolling(ctx)

	time.Sleep(30 * time.Millisecond)
	writeReadyItem(t, dir, "20260328-poll-item")
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, d := range detected {
		if d == "20260328-poll-item" {
			found = true
		}
	}
	if !found {
		t.Error("polling should detect new directory")
	}
}

func TestWatcher_Stoppable(t *testing.T) {
	dir := t.TempDir()

	w := New(dir, 50*time.Millisecond, func(path string) {})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop on context cancellation")
	}
}

func TestWatcher_IgnoresFiles(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var detected []string

	w := New(dir, 50*time.Millisecond, func(path string) {
		mu.Lock()
		detected = append(detected, filepath.Base(path))
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.runPolling(ctx)

	os.WriteFile(filepath.Join(dir, "not-a-directory.txt"), []byte("file"), 0644)
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(detected) != 0 {
		t.Errorf("should not detect files, got %v", detected)
	}
}

func TestWatcher_SkipsItemWithoutMetadata(t *testing.T) {
	dir := t.TempDir()

	var mu sync.Mutex
	var detected []string

	w := New(dir, 50*time.Millisecond, func(path string) {
		mu.Lock()
		detected = append(detected, filepath.Base(path))
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.runPolling(ctx)

	// Create a directory WITHOUT metadata.json (simulates mount visibility delay)
	os.MkdirAll(filepath.Join(dir, "20260328-not-ready"), 0755)
	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	if len(detected) != 0 {
		t.Errorf("should not dispatch item without metadata.json, got %v", detected)
	}
	mu.Unlock()

	// Now write metadata.json -- next poll should pick it up
	os.WriteFile(filepath.Join(dir, "20260328-not-ready", "metadata.json"), []byte(`{}`), 0644)
	time.Sleep(120 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, d := range detected {
		if d == "20260328-not-ready" {
			found = true
		}
	}
	if !found {
		t.Error("item should be dispatched after metadata.json appears")
	}
}

// TestWatcher_DispatchDoesNotBlockDiscovery is the regression guard for
// glovebox-rbpt: discovery must not be coupled to handler/pool backpressure.
// A handler that blocks (simulating a saturated scan-worker pool) must NOT
// freeze the discovery path -- otherwise the watcher loop stops draining
// fsnotify events (-> overflow) and, after the polling fallback, pollOnce
// wedges on the first un-dispatchable item and the queue never drains.
func TestWatcher_DispatchDoesNotBlockDiscovery(t *testing.T) {
	dir := t.TempDir()
	writeReadyItem(t, dir, "20260328-item")

	blocked := make(chan struct{})
	defer close(blocked)
	w := New(dir, 50*time.Millisecond, func(path string) {
		<-blocked // simulate a saturated pool.Input() send
	})

	// dispatchIfNew runs on the discovery goroutine; it must return promptly
	// even though the handler blocks indefinitely.
	done := make(chan struct{})
	go func() {
		w.dispatchIfNew(filepath.Join(dir, "20260328-item"))
		close(done)
	}()

	select {
	case <-done:
		// good: discovery decoupled from the blocking handler
	case <-time.After(2 * time.Second):
		t.Fatal("dispatchIfNew blocked on a slow handler -- discovery would freeze (glovebox-rbpt)")
	}
}

// TestWatcher_DrainsBacklogWhileHandlerBlocksThenRecovers verifies that a
// temporary handler stall does not permanently freeze discovery: while the
// first dispatch is blocked, the watcher keeps polling and discovers later
// items, and once the handler unblocks the whole backlog drains.
func TestWatcher_DrainsBacklogWhileHandlerBlocksThenRecovers(t *testing.T) {
	dir := t.TempDir()

	gate := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var handled []string

	w := New(dir, 20*time.Millisecond, func(path string) {
		// Block only on the very first dispatched item, then recover.
		once.Do(func() { <-gate })
		mu.Lock()
		handled = append(handled, filepath.Base(path))
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.runPolling(ctx)

	// Seed the first item (will block the feeder), then add more while blocked.
	writeReadyItem(t, dir, "20260328-0001")
	time.Sleep(60 * time.Millisecond)
	writeReadyItem(t, dir, "20260328-0002")
	writeReadyItem(t, dir, "20260328-0003")
	time.Sleep(60 * time.Millisecond)

	// Release the stalled handler; the full backlog must now drain.
	close(gate)

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(handled)
		mu.Unlock()
		if n == 3 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("backlog did not drain after handler recovered: handled %d/3", n)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestWatcher_NoDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeReadyItem(t, dir, "20260328-item")

	var mu sync.Mutex
	count := 0

	w := New(dir, 50*time.Millisecond, func(path string) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.runPolling(ctx)

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if count != 1 {
		t.Errorf("handler called %d times, want 1 (no duplicates)", count)
	}
}
