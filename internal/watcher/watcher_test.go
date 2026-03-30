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
