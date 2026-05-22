package ingest

import (
	"context"
	"testing"
)

func TestWithDeliveredBy_RoundTrip(t *testing.T) {
	ctx := WithDeliveredBy(context.Background(), "recognizer")
	got, ok := DeliveredBy(ctx)
	if !ok || got != "recognizer" {
		t.Errorf("DeliveredBy = (%q, %v), want (recognizer, true)", got, ok)
	}
}

func TestDeliveredBy_AbsentReturnsFalse(t *testing.T) {
	if _, ok := DeliveredBy(context.Background()); ok {
		t.Error("DeliveredBy on bare ctx returned ok=true")
	}
}

func TestDeliveredBy_EmptyStringReturnsFalse(t *testing.T) {
	// Defensive: a ctx where the value was set to "" should still
	// report absent so callers can rely on the ok boolean alone.
	ctx := WithDeliveredBy(context.Background(), "")
	if got, ok := DeliveredBy(ctx); ok {
		t.Errorf("DeliveredBy(empty-string ctx) = (%q, %v), want (\"\", false)", got, ok)
	}
}

func TestBuildIdentity_FromContext(t *testing.T) {
	ctx := WithDeliveredBy(context.Background(), "recognizer")
	id := BuildIdentity(ctx)
	if id == nil {
		t.Fatal("BuildIdentity returned nil")
	}
	if id.Provider != "ingest" {
		t.Errorf("Provider=%q, want %q", id.Provider, "ingest")
	}
	if id.AuthMethod != "bearer_token" {
		t.Errorf("AuthMethod=%q, want %q", id.AuthMethod, "bearer_token")
	}
	if id.AccountID != "recognizer" {
		t.Errorf("AccountID=%q, want %q", id.AccountID, "recognizer")
	}
}

func TestBuildIdentity_AbsentReturnsNil(t *testing.T) {
	if id := BuildIdentity(context.Background()); id != nil {
		t.Errorf("BuildIdentity(bare ctx) = %+v, want nil", id)
	}
}
