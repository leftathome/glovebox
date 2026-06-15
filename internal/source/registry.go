// Package source holds the ingest source-policy registry (glovebox-9s60).
//
// It maps an AUTHENTICATED ingest source-id (the bearer-token identity
// carried on UploadState.SourceID, never a producer-asserted field) to the
// lane policy glovebox applies on finalize: the connector kind, the default
// data_subject, and the default audience marker. This is the "connector
// registry" of the operator-scanner-lane design §4.2 and the home of the
// per-connector data_subject_default knob (Steve | household).
//
// It deliberately mirrors internal/subject: a JSON file (sources.json) loaded
// at boot, an enforce flag, and validation via the staging audience rules so a
// malformed registry fails the process rather than silently mis-routing.
package source

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/leftathome/glovebox/internal/staging"
)

// sourcePolicy is one authorized source's lane policy. Unexported so external
// packages cannot mutate registry internals; reads go through the Policy view.
type sourcePolicy struct {
	sourceID           string
	kind               string
	dataSubjectDefault string
	audienceDefault    []string
}

// Policy is a read-only view of one source's lane policy, returned by Lookup.
type Policy struct{ p *sourcePolicy }

// Kind returns the connector kind (e.g. "recognizer-scanner").
func (v Policy) Kind() string { return v.p.kind }

// DataSubjectDefault returns the per-connector default data_subject, or "".
func (v Policy) DataSubjectDefault() string { return v.p.dataSubjectDefault }

// AudienceDefault returns a copy of the per-connector default audience marker.
func (v Policy) AudienceDefault() []string {
	if v.p.audienceDefault == nil {
		return nil
	}
	out := make([]string, len(v.p.audienceDefault))
	copy(out, v.p.audienceDefault)
	return out
}

// Registry maps authorized source-ids to their lane policies.
type Registry struct {
	enforce    bool
	bySourceID map[string]*sourcePolicy
}

// Enforce reports whether source enforcement is active. Nil-receiver safe.
func (r *Registry) Enforce() bool { return r != nil && r.enforce }

// Lookup returns the lane Policy for an authenticated source-id and whether it
// is registered. Nil-receiver safe (returns false).
func (r *Registry) Lookup(sourceID string) (Policy, bool) {
	if r == nil {
		return Policy{}, false
	}
	p, ok := r.bySourceID[sourceID]
	if !ok {
		return Policy{}, false
	}
	return Policy{p: p}, true
}

// wireSource is the JSON wire format for one source entry.
type wireSource struct {
	SourceID           string   `json:"source_id"`
	Kind               string   `json:"kind"`
	DataSubjectDefault string   `json:"data_subject_default"`
	AudienceDefault    []string `json:"audience_default"`
}

// wireRegistry is the JSON wire format for the top-level registry file.
type wireRegistry struct {
	Enforce bool         `json:"enforce"`
	Sources []wireSource `json:"sources"`
}

// Load reads a Registry from the JSON file at path. An empty path returns an
// empty, non-enforcing registry with no error (the no-sources-configured case).
// A malformed file, an empty/duplicate source_id, or an audience_default that
// violates the staging rules is a hard error so a bad config fails boot.
func Load(path string) (*Registry, error) {
	if path == "" {
		return &Registry{enforce: false, bySourceID: make(map[string]*sourcePolicy)}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("source registry: open %q: %w", path, err)
	}
	defer f.Close()

	var wire wireRegistry
	if err := json.NewDecoder(f).Decode(&wire); err != nil {
		return nil, fmt.Errorf("source registry: decode %q: %w", path, err)
	}

	reg := &Registry{
		enforce:    wire.Enforce,
		bySourceID: make(map[string]*sourcePolicy, len(wire.Sources)),
	}

	for i, ws := range wire.Sources {
		if ws.SourceID == "" {
			return nil, fmt.Errorf("source registry: entry %d: source_id must not be empty", i)
		}
		if _, dup := reg.bySourceID[ws.SourceID]; dup {
			return nil, fmt.Errorf("source registry: duplicate source_id %q", ws.SourceID)
		}
		// Validate audience_default against the staging rules; hasDataSubject
		// is true when a data_subject_default is set (so a subject-relative
		// default is only legal when a default subject backs it).
		if err := staging.ValidateAudience(ws.AudienceDefault, ws.DataSubjectDefault != ""); err != nil {
			return nil, fmt.Errorf("source registry: source_id %q: audience_default: %w", ws.SourceID, err)
		}
		reg.bySourceID[ws.SourceID] = &sourcePolicy{
			sourceID:           ws.SourceID,
			kind:               ws.Kind,
			dataSubjectDefault: ws.DataSubjectDefault,
			audienceDefault:    ws.AudienceDefault,
		}
	}

	return reg, nil
}
