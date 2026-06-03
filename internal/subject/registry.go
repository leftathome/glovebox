package subject

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/leftathome/glovebox/internal/staging"
)

// subjectEntry is one known subject. display is intentionally unexported and
// has no accessor: it is at-rest operator legibility only and must never reach
// an item, route, or audit record (spec 15 sec 5.1 PHI firewall).
// subjectEntry is also intentionally unexported to prevent external packages
// from holding direct references to registry internals.
type subjectEntry struct {
	EntityID        string
	display         string
	Principals      []string
	DefaultAudience []string
}

// SubjectRegistry maps principals and entity_ids to opaque entity_ids.
type SubjectRegistry struct {
	enforce  bool
	entities map[string]*subjectEntry // entity_id -> entry
	byPrinc  map[string]*subjectEntry // principal  -> entry
}

// Enforce reports whether subject enforcement is active.
func (r *SubjectRegistry) Enforce() bool { return r != nil && r.enforce }

// Resolve maps a principal token OR a registered entity_id to the canonical
// entity_id. Returns ("", false) if s is not found in either index.
func (r *SubjectRegistry) Resolve(s string) (string, bool) {
	if r == nil {
		return "", false
	}
	// Check principal index first.
	if e, ok := r.byPrinc[s]; ok {
		return e.EntityID, true
	}
	// Fall through to entity_id direct look-up (pass-through for callers that
	// already hold a canonical entity_id, per spec 15 sec 5.2 step 3).
	if e, ok := r.entities[s]; ok {
		return e.EntityID, true
	}
	return "", false
}

// DefaultAudience returns the default_audience for a known entity_id, or nil
// if the entity_id is not registered.
func (r *SubjectRegistry) DefaultAudience(entityID string) []string {
	if r == nil {
		return nil
	}
	if e, ok := r.entities[entityID]; ok {
		return e.DefaultAudience
	}
	return nil
}

// wireEntry is the JSON wire format for one subject entry.
type wireEntry struct {
	EntityID        string   `json:"entity_id"`
	Display         string   `json:"display"`
	Principals      []string `json:"principals"`
	DefaultAudience []string `json:"default_audience"`
}

// wireRegistry is the JSON wire format for the top-level registry file.
type wireRegistry struct {
	Enforce  bool        `json:"enforce"`
	Subjects []wireEntry `json:"subjects"`
}

// Load reads a SubjectRegistry from the JSON file at path. If path is empty,
// Load returns an empty (non-enforcing) registry with no error.
func Load(path string) (*SubjectRegistry, error) {
	if path == "" {
		return &SubjectRegistry{
			enforce:  false,
			entities: make(map[string]*subjectEntry),
			byPrinc:  make(map[string]*subjectEntry),
		}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("subject registry: open %q: %w", path, err)
	}
	defer f.Close()

	var wire wireRegistry
	if err := json.NewDecoder(f).Decode(&wire); err != nil {
		return nil, fmt.Errorf("subject registry: decode %q: %w", path, err)
	}

	reg := &SubjectRegistry{
		enforce:  wire.Enforce,
		entities: make(map[string]*subjectEntry, len(wire.Subjects)),
		byPrinc:  make(map[string]*subjectEntry),
	}

	for i, ws := range wire.Subjects {
		if ws.EntityID == "" {
			return nil, fmt.Errorf("subject registry: entry %d: entity_id must not be empty", i)
		}
		if _, dup := reg.entities[ws.EntityID]; dup {
			return nil, fmt.Errorf("subject registry: duplicate entity_id %q", ws.EntityID)
		}

		// Validate default_audience via staging rules; for registry entries the
		// entity IS the subject, so subject/siblings tokens are legal.
		if err := staging.ValidateAudience(ws.DefaultAudience, true); err != nil {
			return nil, fmt.Errorf("subject registry: entity_id %q: default_audience: %w", ws.EntityID, err)
		}

		entry := &subjectEntry{
			EntityID:        ws.EntityID,
			display:         ws.Display,
			Principals:      ws.Principals,
			DefaultAudience: ws.DefaultAudience,
		}
		reg.entities[ws.EntityID] = entry

		for _, p := range ws.Principals {
			if existing, dup := reg.byPrinc[p]; dup {
				return nil, fmt.Errorf("subject registry: principal %q claimed by both %q and %q",
					p, existing.EntityID, ws.EntityID)
			}
			reg.byPrinc[p] = entry
		}
	}

	// Second pass: reject any principal that equals a registered entity_id.
	// This must run after the entities map is fully built so that ordering of
	// entries in the file does not affect the outcome.
	for _, ws := range wire.Subjects {
		for _, p := range ws.Principals {
			if _, clash := reg.entities[p]; clash {
				return nil, fmt.Errorf("subject registry: principal %q collides with a registered entity_id", p)
			}
		}
	}

	return reg, nil
}
