package subject

import (
	"encoding"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeRegistry writes JSON to a temp file and returns the path.
func writeRegistry(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	f, err := os.CreateTemp(t.TempDir(), "registry-*.json")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.Write(b); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	return f.Name()
}

func TestLoad_ParsesValidRegistry(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Example Subject",
				"principals":       []string{"walhelm:9f2a"},
				"default_audience": []string{"subject"},
			},
		},
	})

	reg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if reg == nil {
		t.Fatal("Load returned nil registry")
	}
	if !reg.Enforce() {
		t.Error("expected enforce=true")
	}
}

func TestLoad_EmptyPath_ReturnsEmptyRegistry(t *testing.T) {
	reg, err := Load("")
	if err != nil {
		t.Fatalf("Load empty path: unexpected error: %v", err)
	}
	if reg == nil {
		t.Fatal("Load returned nil registry")
	}
	if reg.Enforce() {
		t.Error("expected enforce=false for empty registry")
	}
}

func TestResolve_ByPrincipal(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Example Subject",
				"principals":       []string{"walhelm:9f2a"},
				"default_audience": []string{"subject"},
			},
		},
	})

	reg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	entityID, ok := reg.Resolve("walhelm:9f2a")
	if !ok {
		t.Fatal("Resolve(walhelm:9f2a): expected ok=true")
	}
	if entityID != "e_1" {
		t.Errorf("Resolve(walhelm:9f2a): got %q, want %q", entityID, "e_1")
	}
}

func TestResolve_ByEntityID_Passthrough(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Example Subject",
				"principals":       []string{"walhelm:9f2a"},
				"default_audience": []string{"subject"},
			},
		},
	})

	reg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A registered entity_id resolves to itself.
	entityID, ok := reg.Resolve("e_1")
	if !ok {
		t.Fatal("Resolve(e_1): expected ok=true")
	}
	if entityID != "e_1" {
		t.Errorf("Resolve(e_1): got %q, want %q", entityID, "e_1")
	}
}

func TestResolve_UnknownPrincipal(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Example Subject",
				"principals":       []string{"walhelm:9f2a"},
				"default_audience": []string{"subject"},
			},
		},
	})

	reg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	entityID, ok := reg.Resolve("walhelm:zzz")
	if ok {
		t.Errorf("Resolve(walhelm:zzz): expected ok=false, got entity_id=%q", entityID)
	}
	if entityID != "" {
		t.Errorf("Resolve(walhelm:zzz): expected empty string, got %q", entityID)
	}
}

func TestResolve_UnknownEntityID(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Example Subject",
				"principals":       []string{"walhelm:9f2a"},
				"default_audience": []string{"subject"},
			},
		},
	})

	reg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	entityID, ok := reg.Resolve("e_unknown")
	if ok {
		t.Errorf("Resolve(e_unknown): expected ok=false, got entity_id=%q", entityID)
	}
	if entityID != "" {
		t.Errorf("Resolve(e_unknown): expected empty string, got %q", entityID)
	}
}

func TestLoad_DuplicatePrincipal_ReturnsError(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Alice",
				"principals":       []string{"walhelm:aaaa"},
				"default_audience": []string{"subject"},
			},
			{
				"entity_id":        "e_2",
				"display":          "Bob",
				"principals":       []string{"walhelm:aaaa"},
				"default_audience": []string{"subject"},
			},
		},
	})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with duplicate principal: expected error, got nil")
	}
}

func TestLoad_BadAudienceToken_ReturnsError(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Example Subject",
				"principals":       []string{"walhelm:9f2a"},
				"default_audience": []string{"admin"},
			},
		},
	})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with bad audience token: expected error, got nil")
	}
}

func TestLoad_PublicWithSubject_ReturnsError(t *testing.T) {
	// public must appear alone; combining with subject is invalid per spec 11 §3.5.
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Example Subject",
				"principals":       []string{"walhelm:9f2a"},
				"default_audience": []string{"public", "subject"},
			},
		},
	})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with [public, subject] audience: expected error, got nil")
	}
}

func TestDefaultAudience(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Example Subject",
				"principals":       []string{"walhelm:9f2a"},
				"default_audience": []string{"subject"},
			},
		},
	})

	reg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := reg.DefaultAudience("e_1")
	want := []string{"subject"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultAudience(e_1): got %v, want %v", got, want)
	}
}

func TestDefaultAudience_UnknownEntity_ReturnsNil(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Example Subject",
				"principals":       []string{"walhelm:9f2a"},
				"default_audience": []string{"subject"},
			},
		},
	})

	reg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := reg.DefaultAudience("e_unknown")
	if got != nil {
		t.Errorf("DefaultAudience(e_unknown): expected nil, got %v", got)
	}
}

// TestDisplayFieldUnreachable is a compile-time assertion: the subjectEntry
// type must not expose a Display getter or an exported Display field. This
// test ensures no public API surfaces the display name (spec 15 sec 5.1
// PHI firewall). If subjectEntry had an exported Display field or a
// Display() method, this file would not compile (or a reflection check
// would catch it at test time). It also guards against fmt.Stringer and
// encoding.TextMarshaler, either of which could leak the display field
// through formatting or serialization.
func TestDisplayFieldUnreachable(t *testing.T) {
	// Verify at runtime via reflection: subjectEntry must have no exported
	// field named "Display" and no exported method named "Display".
	var e subjectEntry
	rt := reflect.TypeOf(e)
	for i := range rt.NumField() {
		f := rt.Field(i)
		if f.Name == "Display" {
			t.Errorf("subjectEntry must not have an exported field named Display (spec 15 sec 5.1 PHI firewall)")
		}
	}
	for i := range reflect.TypeOf(&e).NumMethod() {
		m := reflect.TypeOf(&e).Method(i)
		if m.Name == "Display" {
			t.Errorf("subjectEntry must not have a Display() method (spec 15 sec 5.1 PHI firewall)")
		}
	}

	// Guard against fmt.Stringer or encoding.TextMarshaler, which could leak
	// the display field via formatting or serialization (PHI firewall).
	if _, ok := any(&e).(fmt.Stringer); ok {
		t.Error("subjectEntry must not implement fmt.Stringer (PHI firewall)")
	}
	if _, ok := any(&e).(encoding.TextMarshaler); ok {
		t.Error("subjectEntry must not implement encoding.TextMarshaler (PHI firewall)")
	}
}

// TestLoad_PrincipalCollidesWithEntityID_ReturnsError checks that a principal
// equal to any registered entity_id is rejected at load time (spec 15 sec 5.2
// namespace-collision guard). This includes the degenerate self-principal case.
func TestLoad_PrincipalCollidesWithEntityID_ReturnsError(t *testing.T) {
	// Entry A has entity_id "e_1"; entry B lists "e_1" as a principal.
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Alice",
				"principals":       []string{"walhelm:aaaa"},
				"default_audience": []string{"subject"},
			},
			{
				"entity_id":        "e_2",
				"display":          "Bob",
				"principals":       []string{"e_1"}, // collides with Alice's entity_id
				"default_audience": []string{"subject"},
			},
		},
	})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with principal colliding entity_id: expected error, got nil")
	}
}

// TestLoad_SelfPrincipalCollidesWithEntityID_ReturnsError checks the
// degenerate case where an entry lists its own entity_id as a principal.
func TestLoad_SelfPrincipalCollidesWithEntityID_ReturnsError(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Alice",
				"principals":       []string{"e_1"}, // self-collision
				"default_audience": []string{"subject"},
			},
		},
	})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with self-principal collision: expected error, got nil")
	}
}

// TestLoad_EmptyEntityID_ReturnsError checks that empty entity_id is rejected.
func TestLoad_EmptyEntityID_ReturnsError(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "",
				"display":          "Example Subject",
				"principals":       []string{"walhelm:9f2a"},
				"default_audience": []string{"subject"},
			},
		},
	})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with empty entity_id: expected error, got nil")
	}
}

// TestLoad_DuplicateEntityID_ReturnsError checks that duplicate entity_ids are rejected.
func TestLoad_DuplicateEntityID_ReturnsError(t *testing.T) {
	path := writeRegistry(t, map[string]any{
		"enforce": true,
		"subjects": []map[string]any{
			{
				"entity_id":        "e_1",
				"display":          "Alice",
				"principals":       []string{"walhelm:aaaa"},
				"default_audience": []string{"subject"},
			},
			{
				"entity_id":        "e_1",
				"display":          "Bob",
				"principals":       []string{"walhelm:bbbb"},
				"default_audience": []string{"subject"},
			},
		},
	})

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load with duplicate entity_id: expected error, got nil")
	}
}

// TestLoad_FileNotFound returns an error for a non-existent file.
func TestLoad_FileNotFound_ReturnsError(t *testing.T) {
	nonExistent := filepath.Join(t.TempDir(), "no-such-file.json")
	_, err := Load(nonExistent)
	if err == nil {
		t.Fatal("Load nonexistent file: expected error, got nil")
	}
}
