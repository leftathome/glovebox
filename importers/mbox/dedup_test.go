package main

import (
	"fmt"
	"sync"
	"testing"
)

func TestNewDedupSet_Empty(t *testing.T) {
	d := NewDedupSet(nil)
	if got := d.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
	if d.Seen("<anything>") {
		t.Fatalf("Seen on empty set returned true")
	}
	snap := d.Snapshot()
	if len(snap) != 0 {
		t.Fatalf("Snapshot() len = %d, want 0", len(snap))
	}
}

func TestNewDedupSet_SeedsUniqueEntries(t *testing.T) {
	// Initial slice contains duplicates, an empty string, and unique IDs.
	// The set should hold only the unique non-empty IDs.
	initial := []string{
		"<a@example.com>",
		"<b@example.com>",
		"<a@example.com>", // duplicate
		"",                // empty, must be dropped
		"<c@example.com>",
		"<b@example.com>", // duplicate
	}
	d := NewDedupSet(initial)

	if got, want := d.Len(), 3; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
	for _, id := range []string{"<a@example.com>", "<b@example.com>", "<c@example.com>"} {
		if !d.Seen(id) {
			t.Errorf("Seen(%q) = false, want true", id)
		}
	}
	if d.Seen("") {
		t.Errorf("Seen(\"\") = true, want false")
	}
	if d.Seen("<unseen@example.com>") {
		t.Errorf("Seen(unseen) = true, want false")
	}
}

func TestDedupSet_AddAndSeen(t *testing.T) {
	d := NewDedupSet(nil)

	if d.Seen("<x@example.com>") {
		t.Fatalf("Seen on unset ID returned true")
	}
	d.Add("<x@example.com>")
	if !d.Seen("<x@example.com>") {
		t.Fatalf("Seen after Add returned false")
	}
	if got, want := d.Len(), 1; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}

	// Adding the same ID again must not change Len.
	d.Add("<x@example.com>")
	if got, want := d.Len(), 1; got != want {
		t.Fatalf("Len() after duplicate Add = %d, want %d", got, want)
	}
}

func TestDedupSet_AddEmptyIsNoop(t *testing.T) {
	d := NewDedupSet(nil)
	d.Add("")
	if got := d.Len(); got != 0 {
		t.Fatalf("Len() after Add(\"\") = %d, want 0", got)
	}
	if d.Seen("") {
		t.Fatalf("Seen(\"\") = true, want false")
	}

	// Empty string mixed with real adds still must not appear.
	d.Add("<real@example.com>")
	d.Add("")
	if got, want := d.Len(), 1; got != want {
		t.Fatalf("Len() = %d, want %d", got, want)
	}
}

func TestDedupSet_SnapshotIsCopy(t *testing.T) {
	d := NewDedupSet([]string{"<a@example.com>", "<b@example.com>"})
	snap := d.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("Snapshot len = %d, want 2", len(snap))
	}

	// Mutating the returned slice must not affect the set.
	snap[0] = "<mutated@example.com>"
	if !d.Seen("<a@example.com>") {
		t.Errorf("set lost original ID after snapshot mutation")
	}
	if d.Seen("<mutated@example.com>") {
		t.Errorf("set gained ID from snapshot mutation")
	}

	// Subsequent Adds must not appear in the already-returned snapshot.
	d.Add("<c@example.com>")
	if len(snap) != 2 {
		t.Errorf("snapshot length changed after Add: got %d", len(snap))
	}

	snap2 := d.Snapshot()
	if len(snap2) != 3 {
		t.Fatalf("second Snapshot len = %d, want 3", len(snap2))
	}
}

func TestDedupSet_SnapshotPreservesInsertionOrder(t *testing.T) {
	// Not required by the API contract per se, but useful for deterministic
	// manifest output: the slice mirrors the order of first Add/seed.
	d := NewDedupSet([]string{"<a@example.com>", "<b@example.com>"})
	d.Add("<c@example.com>")
	d.Add("<a@example.com>") // duplicate; must not reorder
	d.Add("<d@example.com>")

	got := d.Snapshot()
	want := []string{
		"<a@example.com>",
		"<b@example.com>",
		"<c@example.com>",
		"<d@example.com>",
	}
	if len(got) != len(want) {
		t.Fatalf("Snapshot len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Snapshot[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDedupSet_ConcurrentAdd exercises Add and Seen from many goroutines to
// catch data races under `go test -race`. After all goroutines finish, the
// set must contain exactly the unique IDs drawn from the shared pool.
func TestDedupSet_ConcurrentAdd(t *testing.T) {
	const (
		poolSize       = 5000
		goroutineCount = 100
		addsPerRoutine = 1000
	)

	// Build the shared pool of IDs.
	pool := make([]string, poolSize)
	for i := range pool {
		pool[i] = fmt.Sprintf("<msg-%05d@example.com>", i)
	}

	d := NewDedupSet(nil)

	var wg sync.WaitGroup
	wg.Add(goroutineCount)
	for g := 0; g < goroutineCount; g++ {
		g := g
		go func() {
			defer wg.Done()
			// Each goroutine picks IDs deterministically from the shared pool
			// so there's heavy overlap across routines. Also exercises Seen
			// and Len concurrently to catch races on those paths.
			for i := 0; i < addsPerRoutine; i++ {
				idx := (g*addsPerRoutine + i) % poolSize
				id := pool[idx]
				d.Add(id)
				// Mix in some reads.
				if i%17 == 0 {
					_ = d.Seen(id)
					_ = d.Len()
				}
			}
		}()
	}
	wg.Wait()

	// Every goroutine walks every ID in the pool at least once across all
	// iterations (goroutineCount * addsPerRoutine = 100_000 adds, pool size
	// 5_000, so each pool entry is hit 20 times on average). Len must equal
	// poolSize.
	if got := d.Len(); got != poolSize {
		t.Fatalf("Len() after concurrent Adds = %d, want %d", got, poolSize)
	}
	snap := d.Snapshot()
	if len(snap) != poolSize {
		t.Fatalf("Snapshot len after concurrent Adds = %d, want %d", len(snap), poolSize)
	}

	// Spot-check a few IDs from the pool.
	for _, idx := range []int{0, 1, poolSize / 2, poolSize - 1} {
		if !d.Seen(pool[idx]) {
			t.Errorf("Seen(%q) = false after concurrent Adds", pool[idx])
		}
	}
}
