package subject

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/leftathome/glovebox/internal/staging"
)

// loadRegJSON writes raw JSON to a temp file and calls Load.
func loadRegJSON(t *testing.T, raw string) *SubjectRegistry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "reg.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write temp registry: %v", err)
	}
	reg, err := Load(path)
	if err != nil {
		t.Fatalf("Load %q: %v", path, err)
	}
	return reg
}

const testRegistryJSON = `{
	"enforce": true,
	"subjects": [
		{
			"entity_id": "ent-alice",
			"display": "Alice",
			"principals": ["alice@example.com", "alice"],
			"default_audience": ["subject", "siblings"]
		}
	]
}`

func TestResolveItem(t *testing.T) {
	reg := loadRegJSON(t, testRegistryJSON)

	t.Run("empty DataSubject returns OutcomeSubjectless and leaves meta unchanged", func(t *testing.T) {
		meta := &staging.ItemMetadata{
			DataSubject: "",
			Audience:    []string{"some-agent"},
		}
		got := ResolveItem(meta, reg)
		if got != OutcomeSubjectless {
			t.Errorf("outcome = %v, want OutcomeSubjectless", got)
		}
		if len(meta.Audience) != 1 || meta.Audience[0] != "some-agent" {
			t.Errorf("Audience mutated unexpectedly: %v", meta.Audience)
		}
	})

	t.Run("principal registered rewrites DataSubject to entity_id", func(t *testing.T) {
		meta := &staging.ItemMetadata{
			DataSubject: "alice@example.com",
			Audience:    nil,
		}
		got := ResolveItem(meta, reg)
		if got != OutcomeResolved {
			t.Errorf("outcome = %v, want OutcomeResolved", got)
		}
		if meta.DataSubject != "ent-alice" {
			t.Errorf("DataSubject = %q, want %q", meta.DataSubject, "ent-alice")
		}
		// Audience was empty, so registry default should be applied.
		if len(meta.Audience) == 0 {
			t.Errorf("Audience should have been filled from registry default, got empty")
		}
	})

	t.Run("principal registered leaves non-empty Audience unchanged", func(t *testing.T) {
		meta := &staging.ItemMetadata{
			DataSubject: "alice",
			Audience:    []string{"custom-agent"},
		}
		got := ResolveItem(meta, reg)
		if got != OutcomeResolved {
			t.Errorf("outcome = %v, want OutcomeResolved", got)
		}
		if meta.DataSubject != "ent-alice" {
			t.Errorf("DataSubject = %q, want %q", meta.DataSubject, "ent-alice")
		}
		if len(meta.Audience) != 1 || meta.Audience[0] != "custom-agent" {
			t.Errorf("Audience was mutated, got %v", meta.Audience)
		}
	})

	t.Run("registered entity_id passed directly is a pass-through", func(t *testing.T) {
		meta := &staging.ItemMetadata{
			DataSubject: "ent-alice",
			Audience:    []string{"existing-agent"},
		}
		got := ResolveItem(meta, reg)
		if got != OutcomeResolved {
			t.Errorf("outcome = %v, want OutcomeResolved", got)
		}
		if meta.DataSubject != "ent-alice" {
			t.Errorf("DataSubject = %q, want %q", meta.DataSubject, "ent-alice")
		}
		if len(meta.Audience) != 1 || meta.Audience[0] != "existing-agent" {
			t.Errorf("Audience was mutated, got %v", meta.Audience)
		}
	})

	t.Run("unknown principal returns OutcomeUnresolved and leaves meta unchanged", func(t *testing.T) {
		meta := &staging.ItemMetadata{
			DataSubject: "unknown@example.com",
			Audience:    []string{"some-agent"},
		}
		got := ResolveItem(meta, reg)
		if got != OutcomeUnresolved {
			t.Errorf("outcome = %v, want OutcomeUnresolved", got)
		}
		if meta.DataSubject != "unknown@example.com" {
			t.Errorf("DataSubject mutated to %q", meta.DataSubject)
		}
		if len(meta.Audience) != 1 || meta.Audience[0] != "some-agent" {
			t.Errorf("Audience mutated unexpectedly: %v", meta.Audience)
		}
	})

	t.Run("nil registry with non-empty subject returns OutcomeUnresolved (fail closed)", func(t *testing.T) {
		meta := &staging.ItemMetadata{
			DataSubject: "alice@example.com",
			Audience:    nil,
		}
		got := ResolveItem(meta, nil)
		if got != OutcomeUnresolved {
			t.Errorf("outcome = %v, want OutcomeUnresolved", got)
		}
		if meta.DataSubject != "alice@example.com" {
			t.Errorf("DataSubject mutated: %q", meta.DataSubject)
		}
	})

	t.Run("nil meta returns OutcomeSubjectless", func(t *testing.T) {
		got := ResolveItem(nil, reg)
		if got != OutcomeSubjectless {
			t.Errorf("outcome = %v, want OutcomeSubjectless", got)
		}
	})
}
