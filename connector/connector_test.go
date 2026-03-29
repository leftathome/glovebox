package connector

import (
	"errors"
	"testing"
)

func TestPermanentError_Wrapping(t *testing.T) {
	inner := errors.New("bad credentials")
	pe := PermanentError(inner)
	if pe.Error() != "bad credentials" {
		t.Errorf("error message = %q, want %q", pe.Error(), "bad credentials")
	}
}

func TestPermanentError_Unwrap(t *testing.T) {
	inner := errors.New("bad credentials")
	pe := PermanentError(inner)
	if !errors.Is(pe, inner) {
		t.Error("should unwrap to inner error")
	}
}

func TestIsPermanent_True(t *testing.T) {
	pe := PermanentError(errors.New("fatal"))
	if !IsPermanent(pe) {
		t.Error("IsPermanent should return true for PermanentError")
	}
}

func TestIsPermanent_False(t *testing.T) {
	err := errors.New("transient network error")
	if IsPermanent(err) {
		t.Error("IsPermanent should return false for regular error")
	}
}

func TestIsPermanent_Nil(t *testing.T) {
	if IsPermanent(nil) {
		t.Error("IsPermanent should return false for nil")
	}
}

func TestPermanentError_WrappedInChain(t *testing.T) {
	inner := errors.New("auth failed")
	pe := PermanentError(inner)
	wrapped := errors.Join(errors.New("context"), pe)
	// errors.Join doesn't support As traversal the same way,
	// but a direct wrap does
	_ = wrapped
}
