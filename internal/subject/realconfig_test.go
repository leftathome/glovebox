package subject

import (
	"slices"
	"testing"

	"github.com/leftathome/glovebox/internal/staging"
)

// realRegistryPath is the operator-maintained registry that ships in the
// glovebox Helm chart and is mounted at /etc/glovebox/subjects.json in
// production. These tests guard the committed file itself (glovebox-ptst):
// a typo or a botched enforce flag here would silently change routing posture
// for the whole household, so the real artifact -- not a synthetic fixture --
// is the unit under test.
const realRegistryPath = "../../charts/glovebox/subjects.json"

// wantSubjects is the household model confirmed with the operator
// (2026-06-12). entity_ids are opaque (spec 15 §5.1); display labels live in
// the file for legibility but are deliberately not asserted here -- they must
// never be load-bearing.
var wantSubjects = map[string][]string{
	"e_111111": {"subject"},              // Steve  -- private
	"e_222222": {"subject"},              // Guardian   -- private
	"e_333333": {"subject", "guardians"}, // Child 1
	"e_444444": {"subject", "guardians"}, // Casey
}

func TestRealRegistry_EnforceOnAndResolvesEntities(t *testing.T) {
	reg, err := Load(realRegistryPath)
	if err != nil {
		t.Fatalf("Load(%q): %v", realRegistryPath, err)
	}
	if !reg.Enforce() {
		t.Fatal("committed subjects.json must have enforce=true (openclaw glovebox->memory prereq)")
	}

	for eid, wantAud := range wantSubjects {
		// Each entity_id must pass-through-resolve to itself (spec 15 §5.2
		// step 3): connectors stamp the entity_id directly, and the gate
		// accepts it as an already-registered subject.
		got, ok := reg.Resolve(eid)
		if !ok {
			t.Errorf("Resolve(%q): not found; entity_id must resolve to itself", eid)
			continue
		}
		if got != eid {
			t.Errorf("Resolve(%q) = %q, want pass-through to itself", eid, got)
		}
		if da := reg.DefaultAudience(eid); !slices.Equal(da, wantAud) {
			t.Errorf("DefaultAudience(%q) = %v, want %v", eid, da, wantAud)
		}
	}
}

func TestRealRegistry_UnregisteredSubjectUnresolved(t *testing.T) {
	reg, err := Load(realRegistryPath)
	if err != nil {
		t.Fatalf("Load(%q): %v", realRegistryPath, err)
	}

	// A principal nobody registered (e.g. a walhelm subject not yet mapped)
	// must NOT resolve -- this is what drives the fail-closed quarantine gate.
	const unknown = "walhelm:not-a-registered-subject"
	if got, ok := reg.Resolve(unknown); ok {
		t.Errorf("Resolve(%q) = (%q,true), want not found", unknown, got)
	}

	meta := &staging.ItemMetadata{DataSubject: unknown}
	if got := ResolveItem(meta, reg); got != OutcomeUnresolved {
		t.Errorf("ResolveItem(%q) = %v, want OutcomeUnresolved", unknown, got)
	}
	// A string that merely *looks* like an entity_id but isn't registered is
	// still unresolved -- it must not slip through the pass-through path.
	const lookalike = "e_deadbe"
	if _, ok := reg.Resolve(lookalike); ok {
		t.Errorf("Resolve(%q): unregistered entity_id-shaped string must not resolve", lookalike)
	}
}
