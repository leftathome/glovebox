// Tests for the upload-id store: per-source concurrent cap, global
// concurrent cap, source-id binding (no cross-source existence leak),
// upload-id uniqueness, and Remove idempotency.
//
// Spec 13 references: §4.1 (upload-id format and binding), §4.4
// (per-upload mutex semantics — the Store does not lock it, but
// ErrUploadBusy is exposed for callers), §5.4 (concurrent caps).

package archives

import (
	"errors"
	"regexp"
	"sync"
	"testing"
)

// hexIDRegexp matches the spec-required upload-id format: 32
// lowercase hex chars (128 bits, hex-encoded).
var hexIDRegexp = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestStoreCreateReturnsHexUploadID verifies that newly created
// upload-ids match the 32-lowercase-hex format required by spec 13
// §4.1 and that the returned UploadState is populated with the
// bound source-id, archive-id, and timestamps.
func TestStoreCreateReturnsHexUploadID(t *testing.T) {
	t.Parallel()
	s := NewStore(StoreConfig{PerSourceMaxConcurrent: 4, GlobalMaxConcurrent: 32})

	meta := &Metadata{ArchiveID: "archive-1"}
	st, err := s.Create("source-A", meta, 1024)
	if err != nil {
		t.Fatalf("Create: unexpected error: %v", err)
	}
	if st == nil {
		t.Fatal("Create: returned nil state without error")
	}
	if !hexIDRegexp.MatchString(st.ID) {
		t.Fatalf("upload-id %q does not match ^[0-9a-f]{32}$", st.ID)
	}
	if st.SourceID != "source-A" {
		t.Errorf("SourceID = %q, want source-A", st.SourceID)
	}
	if st.ArchiveID != "archive-1" {
		t.Errorf("ArchiveID = %q, want archive-1", st.ArchiveID)
	}
	if st.UploadLength != 1024 {
		t.Errorf("UploadLength = %d, want 1024", st.UploadLength)
	}
	if st.Hasher == nil {
		t.Error("Hasher is nil; expected sha256 hasher")
	}
	if st.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero; expected wall-clock time")
	}
	if st.LastActivity.IsZero() {
		t.Error("LastActivity is zero; expected wall-clock time at creation")
	}
}

// TestStorePerSourceConcurrentCap verifies that the per-source cap is
// enforced: the 5th concurrent Create from one source-id (cap=4)
// returns ErrConcurrencyPerSource. After Remove of one upload, a new
// Create from the same source succeeds.
func TestStorePerSourceConcurrentCap(t *testing.T) {
	t.Parallel()
	s := NewStore(StoreConfig{PerSourceMaxConcurrent: 4, GlobalMaxConcurrent: 32})

	var ids []string
	for i := 0; i < 4; i++ {
		st, err := s.Create("source-A", &Metadata{ArchiveID: "a"}, 1024)
		if err != nil {
			t.Fatalf("Create #%d: unexpected error: %v", i, err)
		}
		ids = append(ids, st.ID)
	}

	// 5th create from same source MUST fail with per-source cap.
	_, err := s.Create("source-A", &Metadata{ArchiveID: "a"}, 1024)
	if !errors.Is(err, ErrConcurrencyPerSource) {
		t.Fatalf("Create #5: want ErrConcurrencyPerSource, got %v", err)
	}

	// A different source-id is still allowed.
	if _, err := s.Create("source-B", &Metadata{ArchiveID: "b"}, 1024); err != nil {
		t.Fatalf("Create from source-B: unexpected error: %v", err)
	}

	// After Removing one source-A upload, another Create succeeds.
	s.Remove(ids[0])
	if _, err := s.Create("source-A", &Metadata{ArchiveID: "a"}, 1024); err != nil {
		t.Fatalf("Create after Remove: unexpected error: %v", err)
	}
}

// TestStoreGlobalConcurrentCap verifies that the global cap is
// enforced and is checked BEFORE the per-source cap (so distinct
// source-ids cannot collectively exceed the global cap by each
// staying under their individual per-source caps).
func TestStoreGlobalConcurrentCap(t *testing.T) {
	t.Parallel()
	// Global cap of 3, per-source cap of 2. Fill via two source-ids
	// (1+2 = 3 total). A 4th create from ANY source must fail with
	// ErrConcurrencyGlobal, not ErrConcurrencyPerSource.
	s := NewStore(StoreConfig{PerSourceMaxConcurrent: 2, GlobalMaxConcurrent: 3})

	if _, err := s.Create("source-A", &Metadata{ArchiveID: "a1"}, 1024); err != nil {
		t.Fatalf("Create #1 (source-A): %v", err)
	}
	if _, err := s.Create("source-B", &Metadata{ArchiveID: "b1"}, 1024); err != nil {
		t.Fatalf("Create #2 (source-B): %v", err)
	}
	if _, err := s.Create("source-B", &Metadata{ArchiveID: "b2"}, 1024); err != nil {
		t.Fatalf("Create #3 (source-B): %v", err)
	}

	// At this point: 3 in-flight uploads total. source-A has 1
	// (under its per-source cap of 2). A new Create from source-A
	// MUST fail with ErrConcurrencyGlobal (not ErrConcurrencyPerSource),
	// because the global cap is checked first.
	_, err := s.Create("source-A", &Metadata{ArchiveID: "a2"}, 1024)
	if !errors.Is(err, ErrConcurrencyGlobal) {
		t.Fatalf("Create #4 (source-A): want ErrConcurrencyGlobal, got %v", err)
	}

	// A Create from a brand-new source-C also fails globally.
	_, err = s.Create("source-C", &Metadata{ArchiveID: "c1"}, 1024)
	if !errors.Is(err, ErrConcurrencyGlobal) {
		t.Fatalf("Create #5 (source-C): want ErrConcurrencyGlobal, got %v", err)
	}
}

// TestStoreGlobalCapCheckedBeforePerSource is a sharper version of
// the above: it constructs the case where a source is AT its
// per-source cap AND the global cap is also full. The implementation
// MUST return ErrConcurrencyGlobal (not ErrConcurrencyPerSource).
// This is the explicit ordering decision recorded in the task plan.
func TestStoreGlobalCapCheckedBeforePerSource(t *testing.T) {
	t.Parallel()
	s := NewStore(StoreConfig{PerSourceMaxConcurrent: 2, GlobalMaxConcurrent: 2})

	if _, err := s.Create("source-A", &Metadata{ArchiveID: "a1"}, 1024); err != nil {
		t.Fatalf("Create #1: %v", err)
	}
	if _, err := s.Create("source-A", &Metadata{ArchiveID: "a2"}, 1024); err != nil {
		t.Fatalf("Create #2: %v", err)
	}

	// source-A is at per-source cap (2/2) AND the global cap is full
	// (2/2). The implementation MUST report the global cap.
	_, err := s.Create("source-A", &Metadata{ArchiveID: "a3"}, 1024)
	if !errors.Is(err, ErrConcurrencyGlobal) {
		t.Fatalf("want ErrConcurrencyGlobal (global is checked first), got %v", err)
	}
}

// TestStoreGetBindsToSourceID verifies that Get returns the state
// only when the caller presents the binding source-id, and returns
// ErrUploadNotFound otherwise. Crucially, the SAME error is returned
// for both "id doesn't exist" and "id exists but wrong source" --
// this prevents cross-source existence-leak fingerprinting per
// spec 13 §4.1.
func TestStoreGetBindsToSourceID(t *testing.T) {
	t.Parallel()
	s := NewStore(StoreConfig{PerSourceMaxConcurrent: 4, GlobalMaxConcurrent: 32})

	st, err := s.Create("source-A", &Metadata{ArchiveID: "a"}, 1024)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Correct source-id: returns the state.
	got, err := s.Get(st.ID, "source-A")
	if err != nil {
		t.Fatalf("Get with correct source-id: %v", err)
	}
	if got != st {
		t.Errorf("Get returned a different *UploadState pointer")
	}

	// Wrong source-id: returns ErrUploadNotFound (NOT a different
	// "wrong owner" error). This is the no-leak rule.
	_, err = s.Get(st.ID, "source-B")
	if !errors.Is(err, ErrUploadNotFound) {
		t.Errorf("Get with wrong source-id: want ErrUploadNotFound, got %v", err)
	}

	// Non-existent upload-id: also returns ErrUploadNotFound (same
	// error as wrong-source — externally indistinguishable, which is
	// the entire point of the binding rule).
	_, err = s.Get("00000000000000000000000000000000", "source-A")
	if !errors.Is(err, ErrUploadNotFound) {
		t.Errorf("Get with non-existent id: want ErrUploadNotFound, got %v", err)
	}
}

// TestStoreRemoveIdempotent verifies that calling Remove on a
// non-existent upload-id is a silent no-op (no panic, no error),
// and that calling Remove twice on the same upload-id is also safe.
// This simplifies cleanup-after-finalize paths.
func TestStoreRemoveIdempotent(t *testing.T) {
	t.Parallel()
	s := NewStore(StoreConfig{PerSourceMaxConcurrent: 4, GlobalMaxConcurrent: 32})

	// Remove on never-existed id: must not panic.
	s.Remove("nonexistent")

	st, err := s.Create("source-A", &Metadata{ArchiveID: "a"}, 1024)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.Remove(st.ID)
	s.Remove(st.ID) // second Remove also a no-op

	// Verify the source-id is also freed from the perSrc map after
	// Remove (no leak of zero-count source-ids).
	s.mu.Lock()
	if _, present := s.perSrc["source-A"]; present {
		t.Errorf("perSrc[source-A] still present after Remove; expected entry deleted at count 0")
	}
	s.mu.Unlock()
}

// TestStoreUploadIDUniqueness exercises a tight loop of Create calls
// and asserts no upload-id collisions. With 128-bit random IDs a
// collision is statistically impossible at this scale; the test is
// a sanity check that newUploadID actually uses 128 bits of entropy
// (as opposed to, say, a time-based or counter-based scheme).
func TestStoreUploadIDUniqueness(t *testing.T) {
	t.Parallel()
	// A large enough budget to catch a broken 16- or 24-bit ID
	// scheme by birthday paradox; small enough to keep the test fast.
	const n = 10_000

	s := NewStore(StoreConfig{
		PerSourceMaxConcurrent: n + 1,
		GlobalMaxConcurrent:    n + 1,
	})

	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		st, err := s.Create("source-A", &Metadata{ArchiveID: "a"}, 1024)
		if err != nil {
			t.Fatalf("Create #%d: %v", i, err)
		}
		if _, dup := seen[st.ID]; dup {
			t.Fatalf("upload-id collision at iteration %d: %q", i, st.ID)
		}
		seen[st.ID] = struct{}{}
		if !hexIDRegexp.MatchString(st.ID) {
			t.Fatalf("malformed upload-id %q", st.ID)
		}
	}
}

// TestStoreNewUploadIDDirect calls newUploadID directly (no Store
// involved) to confirm the hex format and that successive calls
// return distinct values.
func TestStoreNewUploadIDDirect(t *testing.T) {
	t.Parallel()
	id1, err := newUploadID()
	if err != nil {
		t.Fatalf("newUploadID: %v", err)
	}
	if !hexIDRegexp.MatchString(id1) {
		t.Fatalf("id1 %q is not 32 lowercase hex chars", id1)
	}
	id2, err := newUploadID()
	if err != nil {
		t.Fatalf("newUploadID #2: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("two successive newUploadID calls returned the same value %q (would happen only with a broken RNG)", id1)
	}
}

// TestStoreConcurrentCreate exercises Create from many goroutines to
// confirm the store-level mutex serializes map access without
// data-race detector complaints. Run with `-race` in CI.
func TestStoreConcurrentCreate(t *testing.T) {
	t.Parallel()
	const goroutines = 32
	const perGoroutine = 8

	s := NewStore(StoreConfig{
		PerSourceMaxConcurrent: goroutines * perGoroutine,
		GlobalMaxConcurrent:    goroutines * perGoroutine,
	})

	var wg sync.WaitGroup
	wg.Add(goroutines)
	errCh := make(chan error, goroutines*perGoroutine)

	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if _, err := s.Create("source-A", &Metadata{ArchiveID: "a"}, 1024); err != nil {
					errCh <- err
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Create: %v", err)
	}

	// Confirm the final count matches expectation (no lost
	// increments).
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.uploads) != goroutines*perGoroutine {
		t.Errorf("got %d uploads, want %d", len(s.uploads), goroutines*perGoroutine)
	}
	if s.perSrc["source-A"] != goroutines*perGoroutine {
		t.Errorf("perSrc counter = %d, want %d", s.perSrc["source-A"], goroutines*perGoroutine)
	}
}
