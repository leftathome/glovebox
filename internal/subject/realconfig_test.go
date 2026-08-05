package subject

import (
	"os"
	"strings"
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

// chartConfigMapPath is the template that renders the three registry files into
// the glovebox ConfigMap.
const chartConfigMapPath = "../../charts/glovebox/templates/configmap.yaml"

// TestChartTemplate_RegistryFilesAreOverridable is the counterpart to
// TestShippedRegistry_IsNeutral. Shipping a neutral roster is only safe if an
// operator can actually supply their own WITHOUT forking the chart: the files
// are rendered from the chart via .Files.Get, which no value can override, so
// before glovebox-pyb9 a customized registry lived only in the live ConfigMap
// and a chart upgrade silently replaced it with the neutral default -- turning
// subject enforcement off and dropping every registered entity_id.
//
// This guards that each registry file keeps BOTH halves of the contract: a
// values override, and the baked file as the fallback when it is unset.
func TestChartTemplate_RegistryFilesAreOverridable(t *testing.T) {
	raw, err := os.ReadFile(chartConfigMapPath)
	if err != nil {
		t.Fatalf("read %q: %v -- the chart ConfigMap template is the only place "+
			"the registry files are rendered; if it moved, update chartConfigMapPath",
			chartConfigMapPath, err)
	}
	tmpl := string(raw)

	for _, tc := range []struct {
		file  string
		value string
	}{
		{"rules.json", ".Values.config.rulesJson"},
		{"subjects.json", ".Values.config.subjectsJson"},
		{"sources.json", ".Values.config.sourcesJson"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			// Scope to this file's block so a reference under a sibling key
			// cannot satisfy the assertion.
			start := strings.Index(tmpl, "  "+tc.file+": |")
			if start < 0 {
				t.Fatalf("chart template does not render %q; expected a %q key in %s",
					tc.file, tc.file, chartConfigMapPath)
			}
			block := tmpl[start:]
			if end := strings.Index(block, "{{- end }}"); end > 0 {
				block = block[:end]
			}

			if !strings.Contains(block, tc.value) {
				t.Errorf("%s is not operator-overridable: no %s branch. "+
					"A neutral shipped roster is only safe if operators can supply their own "+
					"via values; restore the override or a chart upgrade will wipe a live registry "+
					"(glovebox-pyb9).", tc.file, tc.value)
			}
			if !strings.Contains(block, `.Files.Get "`+tc.file+`"`) {
				t.Errorf("%s lost its baked-file fallback; an install that sets no override "+
					"must still get the neutral shipped file", tc.file)
			}
		})
	}
}
