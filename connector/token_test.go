package connector

import (
	"context"
	"testing"
)

func TestTokenSource_StaticReturnsConfiguredToken(t *testing.T) {
	src := NewStaticTokenSource("ghp_abc123")
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ghp_abc123" {
		t.Errorf("got %q, want %q", got, "ghp_abc123")
	}
}

func TestTokenSource_StaticMultipleCallsReturnSameToken(t *testing.T) {
	src := NewStaticTokenSource("tok_xyz")
	for i := 0; i < 5; i++ {
		got, err := src.Token(context.Background())
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if got != "tok_xyz" {
			t.Errorf("call %d: got %q, want %q", i, got, "tok_xyz")
		}
	}
}

func TestTokenSource_StaticEmptyTokenReturnsEmptyString(t *testing.T) {
	src := NewStaticTokenSource("")
	got, err := src.Token(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestTokenSource_StaticIgnoresCancelledContext(t *testing.T) {
	src := NewStaticTokenSource("fast_token")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	got, err := src.Token(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "fast_token" {
		t.Errorf("got %q, want %q", got, "fast_token")
	}
}
