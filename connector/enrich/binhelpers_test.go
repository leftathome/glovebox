package enrich

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/staging"
)

// stubEnricher is a minimal Enricher for registry assertions.
type stubEnricher struct{ name string }

func (s stubEnricher) Name() string                              { return s.name }
func (s stubEnricher) Applies(staging.ItemMetadata, string) bool { return false }
func (s stubEnricher) Enrich(context.Context, string, staging.ItemMetadata, string) ([]Artifact, error) {
	return nil, nil
}

func TestRegisterIfAvailable_BinaryPresent_Registers(t *testing.T) {
	reg := NewRegistry()
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	var gotPath string

	ok := RegisterIfAvailable(reg, "mybin",
		func(p string) Enricher { gotPath = p; return stubEnricher{name: "stub"} },
		func(string) (string, error) { return "/usr/bin/mybin", nil },
		logger, "DISABLED MSG")

	if !ok {
		t.Fatal("RegisterIfAvailable = false, want true when binary present")
	}
	if gotPath != "/usr/bin/mybin" {
		t.Errorf("make got path %q, want the resolved path", gotPath)
	}
	if len(reg.enrichers) != 1 {
		t.Errorf("registry has %d enrichers, want 1", len(reg.enrichers))
	}
	if buf.Len() != 0 {
		t.Errorf("logged on success: %q", buf.String())
	}
}

func TestRegisterIfAvailable_BinaryMissing_LogsAndSkips(t *testing.T) {
	reg := NewRegistry()
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)

	ok := RegisterIfAvailable(reg, "mybin",
		func(string) Enricher { t.Fatal("make called when binary missing"); return nil },
		func(string) (string, error) { return "", errors.New("not found") },
		logger, "WHAT: disabled\n  CHECK: x\n  FIX: y")

	if ok {
		t.Fatal("RegisterIfAvailable = true, want false when binary missing")
	}
	if len(reg.enrichers) != 0 {
		t.Errorf("registry has %d enrichers, want 0", len(reg.enrichers))
	}
	out := buf.String()
	if !strings.Contains(out, "WHAT: disabled") || !strings.Contains(out, "not found") {
		t.Errorf("log %q missing disabled message or LookPath error", out)
	}
}
