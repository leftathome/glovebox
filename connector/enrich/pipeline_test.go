package enrich

import (
	"context"
	"errors"
	"testing"

	"github.com/leftathome/glovebox/internal/staging"
)

// fakeEnricher is a test-only Enricher implementation used to exercise
// Registry semantics without depending on any real enricher.
type fakeEnricher struct {
	name      string
	appliesFn func(meta staging.ItemMetadata, sourcePath string) bool
	enrichFn  func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error)

	appliesCalls int
	enrichCalls  int
}

func (f *fakeEnricher) Name() string { return f.name }

func (f *fakeEnricher) Applies(meta staging.ItemMetadata, sourcePath string) bool {
	f.appliesCalls++
	if f.appliesFn == nil {
		return true
	}
	return f.appliesFn(meta, sourcePath)
}

func (f *fakeEnricher) Enrich(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error) {
	f.enrichCalls++
	if f.enrichFn == nil {
		return nil, nil
	}
	return f.enrichFn(ctx, sourcePath, meta, outputDir)
}

func TestApplyAll_EmptyRegistry(t *testing.T) {
	r := NewRegistry()
	arts, errs := r.ApplyAll(context.Background(), "any-path", staging.ItemMetadata{}, "anywhere")
	if len(arts) != 0 {
		t.Errorf("expected 0 artifacts, got %d", len(arts))
	}
	if len(errs) != 0 {
		t.Errorf("expected 0 errors, got %d", len(errs))
	}
}

func TestApplyAll_AppliesTrue_Enriches(t *testing.T) {
	want := Artifact{Filename: "content.extracted.md", Kind: "extracted-text", Producer: "fake"}
	fe := &fakeEnricher{
		name:      "fake",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return true },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error) {
			return []Artifact{want}, nil
		},
	}

	r := NewRegistry()
	r.Register(fe)

	arts, errs := r.ApplyAll(context.Background(), "source.txt", staging.ItemMetadata{}, "out")
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %+v", len(errs), errs)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(arts))
	}
	if arts[0] != want {
		t.Errorf("artifact mismatch: got %+v want %+v", arts[0], want)
	}
	if fe.enrichCalls != 1 {
		t.Errorf("Enrich called %d times, want 1", fe.enrichCalls)
	}
}

func TestApplyAll_AppliesFalse_NotCalled(t *testing.T) {
	fe := &fakeEnricher{
		name:      "fake",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return false },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error) {
			t.Fatalf("Enrich must not be called when Applies returns false")
			return nil, nil
		},
	}

	r := NewRegistry()
	r.Register(fe)

	arts, errs := r.ApplyAll(context.Background(), "source.txt", staging.ItemMetadata{}, "out")
	if len(arts) != 0 {
		t.Errorf("expected 0 artifacts, got %d", len(arts))
	}
	if len(errs) != 0 {
		t.Errorf("expected 0 errors, got %d", len(errs))
	}
	if fe.enrichCalls != 0 {
		t.Errorf("Enrich called %d times, want 0", fe.enrichCalls)
	}
}

func TestApplyAll_EnrichError_CollectedAndContinues(t *testing.T) {
	failErr := errors.New("boom")
	failing := &fakeEnricher{
		name:      "failing",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return true },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error) {
			return nil, failErr
		},
	}
	want := Artifact{Filename: "ok.md", Kind: "extracted-text", Producer: "ok"}
	succeeding := &fakeEnricher{
		name:      "ok",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return true },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error) {
			return []Artifact{want}, nil
		},
	}

	r := NewRegistry()
	r.Register(failing)
	r.Register(succeeding)

	arts, errs := r.ApplyAll(context.Background(), "source.txt", staging.ItemMetadata{}, "out")

	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact (from succeeding), got %d", len(arts))
	}
	if arts[0] != want {
		t.Errorf("artifact mismatch: got %+v want %+v", arts[0], want)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 EnricherError, got %d: %+v", len(errs), errs)
	}
	if errs[0].Producer != "failing" {
		t.Errorf("EnricherError.Producer = %q, want %q", errs[0].Producer, "failing")
	}
	if !errors.Is(errs[0].Err, failErr) {
		t.Errorf("EnricherError.Err = %v, want %v", errs[0].Err, failErr)
	}
	if succeeding.enrichCalls != 1 {
		t.Errorf("succeeding.Enrich called %d times, want 1 (iteration must continue after error)", succeeding.enrichCalls)
	}
}

func TestApplyAll_MultipleEnrichersMatch(t *testing.T) {
	artA := Artifact{Filename: "a.md", Kind: "extracted-text", Producer: "a"}
	artB := Artifact{Filename: "b.md", Kind: "extracted-text", Producer: "b"}
	eA := &fakeEnricher{
		name:      "a",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return true },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error) {
			return []Artifact{artA}, nil
		},
	}
	eB := &fakeEnricher{
		name:      "b",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return true },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error) {
			return []Artifact{artB}, nil
		},
	}

	r := NewRegistry()
	r.Register(eA)
	r.Register(eB)

	arts, errs := r.ApplyAll(context.Background(), "source.txt", staging.ItemMetadata{}, "out")
	if len(errs) != 0 {
		t.Fatalf("expected 0 errors, got %d: %+v", len(errs), errs)
	}
	if len(arts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d: %+v", len(arts), arts)
	}

	// Order matches registration order.
	if arts[0] != artA {
		t.Errorf("arts[0] = %+v, want %+v", arts[0], artA)
	}
	if arts[1] != artB {
		t.Errorf("arts[1] = %+v, want %+v", arts[1], artB)
	}
}

func TestRegister_AddsEnricher(t *testing.T) {
	r := NewRegistry()

	// Before registration: no enrichers consulted, nothing happens.
	arts, errs := r.ApplyAll(context.Background(), "x", staging.ItemMetadata{}, "y")
	if len(arts) != 0 || len(errs) != 0 {
		t.Fatalf("empty registry should be silent: arts=%d errs=%d", len(arts), len(errs))
	}

	fe := &fakeEnricher{
		name:      "fake",
		appliesFn: func(meta staging.ItemMetadata, sourcePath string) bool { return true },
		enrichFn: func(ctx context.Context, sourcePath string, meta staging.ItemMetadata, outputDir string) ([]Artifact, error) {
			return []Artifact{{Filename: "f.md", Kind: "extracted-text", Producer: "fake"}}, nil
		},
	}
	r.Register(fe)

	arts, errs = r.ApplyAll(context.Background(), "x", staging.ItemMetadata{}, "y")
	if len(errs) != 0 {
		t.Fatalf("unexpected errs: %+v", errs)
	}
	if len(arts) != 1 {
		t.Fatalf("expected 1 artifact after Register, got %d", len(arts))
	}
	if fe.appliesCalls == 0 {
		t.Errorf("registered enricher's Applies was never consulted")
	}
}

func TestDefault_IsNonNilEmptyRegistry(t *testing.T) {
	// Sanity check: the package-level Default registry exists and is empty
	// at process start. We don't register into it from tests (that would
	// leak state across the test binary's lifetime).
	if Default == nil {
		t.Fatal("Default registry is nil")
	}
	arts, errs := Default.ApplyAll(context.Background(), "x", staging.ItemMetadata{}, "y")
	if len(arts) != 0 || len(errs) != 0 {
		t.Errorf("Default registry should be empty at process start: arts=%d errs=%d", len(arts), len(errs))
	}
}
