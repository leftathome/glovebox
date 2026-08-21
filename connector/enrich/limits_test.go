package enrich

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leftathome/glovebox/internal/staging"
)

func TestLimitedWriter_UnderLimit(t *testing.T) {
	w := NewLimitedWriter(100)
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if w.Exceeded() {
		t.Error("Exceeded() true under the limit")
	}
	if got := w.String(); got != "hello" {
		t.Errorf("String() = %q, want %q", got, "hello")
	}
	if err := w.Err("test"); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

// The cap must hold across many small writes, which is how a child process
// streaming output actually arrives.
func TestLimitedWriter_AccumulatesToLimit(t *testing.T) {
	w := NewLimitedWriter(10)
	for i := 0; i < 100; i++ {
		if _, err := w.Write([]byte("xxxx")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if !w.Exceeded() {
		t.Fatal("Exceeded() false after writing 400 bytes to a 10-byte writer")
	}
	if w.Len() > 10 {
		t.Errorf("retained %d bytes, want at most 10", w.Len())
	}
	err := w.Err("test")
	if !errors.Is(err, ErrOutputTooLarge) {
		t.Errorf("Err() = %v, want ErrOutputTooLarge", err)
	}
	if !strings.Contains(err.Error(), "enrich/test") {
		t.Errorf("Err() should name the producer, got %q", err)
	}
}

// Writes keep reporting success after the cap so the child is not killed by
// a broken pipe before its stderr can be read for a better diagnostic.
func TestLimitedWriter_KeepsAcceptingAfterLimit(t *testing.T) {
	w := NewLimitedWriter(4)
	w.Write([]byte("aaaaaaaa"))
	n, err := w.Write([]byte("more"))
	if err != nil {
		t.Errorf("Write after limit returned %v, want nil", err)
	}
	if n != 4 {
		t.Errorf("Write after limit reported %d bytes, want 4", n)
	}
}

func TestNewLimitedWriter_DefaultLimit(t *testing.T) {
	if w := NewLimitedWriter(0); w.limit != DefaultMaxOutputBytes {
		t.Errorf("limit = %d, want %d", w.limit, DefaultMaxOutputBytes)
	}
	if w := NewLimitedWriter(-1); w.limit != DefaultMaxOutputBytes {
		t.Errorf("negative limit = %d, want the default", w.limit)
	}
}

// slowEnricher blocks until its context is done, standing in for a wedged
// pandoc or tesseract.
type slowEnricher struct {
	saw chan error
}

func (s *slowEnricher) Name() string { return "slow" }
func (s *slowEnricher) Applies(staging.ItemMetadata, string) bool {
	return true
}
func (s *slowEnricher) Enrich(ctx context.Context, _ string, _ staging.ItemMetadata, _ string) ([]Artifact, error) {
	<-ctx.Done()
	s.saw <- ctx.Err()
	return nil, ctx.Err()
}

// The production caller passes context.Background(), so without a
// registry-side timeout exec.CommandContext bounds nothing and a wedged
// child holds Commit open indefinitely.
func TestApplyAll_BoundsEnricherWithoutCallerDeadline(t *testing.T) {
	r := NewRegistry()
	se := &slowEnricher{saw: make(chan error, 1)}
	r.Register(se)
	r.SetTimeout(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		r.ApplyAll(context.Background(), "/tmp/x", staging.ItemMetadata{}, t.TempDir())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyAll did not return: a wedged enricher was not bounded")
	}

	select {
	case err := <-se.saw:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("enricher saw %v, want DeadlineExceeded", err)
		}
	default:
		t.Error("enricher never observed a cancelled context")
	}
}

// A caller with a shorter deadline must still win.
func TestApplyAll_RespectsEarlierCallerDeadline(t *testing.T) {
	r := NewRegistry()
	se := &slowEnricher{saw: make(chan error, 1)}
	r.Register(se)
	r.SetTimeout(10 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	r.ApplyAll(ctx, "/tmp/x", staging.ItemMetadata{}, t.TempDir())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("ApplyAll took %v; the caller's shorter deadline was ignored", elapsed)
	}
}
