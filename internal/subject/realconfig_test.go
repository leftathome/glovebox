package subject

import (
	"testing"
)

// shippedRegistryPath is the subjects.json that ships in the PUBLIC glovebox
// Helm chart. It must stay a NEUTRAL example: a real household roster belongs
// only in operator-controlled values (gitops / the live ConfigMap), never baked
// into a public artifact (glovebox-0nzk). These tests are the regression guard
// against re-introducing identity defaults here.
const shippedRegistryPath = "../../charts/glovebox/subjects.json"

// TestShippedRegistry_IsNeutral guards the committed chart subjects.json: it
// must load, must NOT enforce against a baked roster, and must carry no
// subjects. An unconfigured install therefore falls to the safe household
// default rather than routing a stranger's data to a specific entity_id.
func TestShippedRegistry_IsNeutral(t *testing.T) {
	reg, err := Load(shippedRegistryPath)
	if err != nil {
		t.Fatalf("Load(%q): %v", shippedRegistryPath, err)
	}

	if reg.Enforce() {
		t.Error("committed chart subjects.json must ship with enforce=false " +
			"(a real roster + enforcement belongs in operator values, not the public chart)")
	}

	// No identity may be baked into the public chart. Any entity_id-shaped
	// string that resolves here is a re-introduced household default.
	for _, eid := range []string{"e_111111", "e_222222", "e_333333", "e_444444"} {
		if _, ok := reg.Resolve(eid); ok {
			t.Errorf("shipped subjects.json resolves %q -- the public chart must not bake a household roster", eid)
		}
	}
}

// TestShippedRegistry_UnregisteredSubjectUnresolved confirms the fail-closed
// posture still holds with the neutral file: an unregistered principal does not
// resolve, so the quarantine gate stays closed by default.
func TestShippedRegistry_UnregisteredSubjectUnresolved(t *testing.T) {
	reg, err := Load(shippedRegistryPath)
	if err != nil {
		t.Fatalf("Load(%q): %v", shippedRegistryPath, err)
	}

	const unknown = "walhelm:not-a-registered-subject"
	if got, ok := reg.Resolve(unknown); ok {
		t.Errorf("Resolve(%q) = (%q,true), want not found", unknown, got)
	}
}
