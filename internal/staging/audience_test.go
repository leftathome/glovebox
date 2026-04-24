package staging

import (
	"strings"
	"testing"
)

// makeAudience returns a slice of n copies of "household", for testing the
// max-entries cap in isolation from the duplicate-token check (length is
// checked before duplicates per ValidateAudience's ordering).
func makeAudience(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "household"
	}
	return out
}

func TestValidateAudience_ValidCombinations(t *testing.T) {
	cases := []struct {
		name           string
		audience       []string
		hasDataSubject bool
	}{
		{"subject-and-parents", []string{"subject", "parents"}, true},
		{"all-role-tokens", []string{"subject", "parents", "siblings"}, true},
		{"subject-only", []string{"subject"}, true},
		{"parents-only", []string{"parents"}, true},
		{"siblings-only", []string{"siblings"}, true},
		{"household-with-subject", []string{"household"}, true},
		{"household-without-subject", []string{"household"}, false},
		{"public-with-subject", []string{"public"}, true},
		{"public-without-subject", []string{"public"}, false},
		{"nil-with-subject", nil, true},
		{"nil-without-subject", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAudience(tc.audience, tc.hasDataSubject); err != nil {
				t.Errorf("expected valid, got error: %v", err)
			}
		})
	}
}

func TestValidateAudience_RejectedCombinations(t *testing.T) {
	cases := []struct {
		name           string
		audience       []string
		hasDataSubject bool
		wantSubstr     string
	}{
		{"unknown-token", []string{"grandparents"}, true, "unknown audience token"},
		{"empty-array", []string{}, true, "must be omitted"},
		{"duplicates", []string{"subject", "subject"}, true, "duplicate"},
		{"too-many", makeAudience(17), true, "too many"},
		{"public-with-subject-token", []string{"public", "subject"}, true, "public must appear alone"},
		{"public-with-household", []string{"public", "household"}, true, "public must appear alone"},
		{"household-with-parents", []string{"household", "parents"}, true, "household must appear alone"},
		{"household-with-subject-token", []string{"household", "subject"}, true, "household must appear alone"},
		{"subject-token-without-data-subject", []string{"subject"}, false, "requires data_subject"},
		{"parents-without-data-subject", []string{"parents"}, false, "requires data_subject"},
		{"role-plus-household-without-data-subject", []string{"siblings"}, false, "requires data_subject"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAudience(tc.audience, tc.hasDataSubject)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error %q did not contain %q", err.Error(), tc.wantSubstr)
			}
		})
	}
}

func TestEffectiveAudience_DefaultWhenNil(t *testing.T) {
	m := ItemMetadata{}
	got := EffectiveAudience(m)
	if len(got) != 1 || got[0] != AudienceHousehold {
		t.Errorf("expected [household] default, got %v", got)
	}
}

func TestEffectiveAudience_PassthroughWhenSet(t *testing.T) {
	m := ItemMetadata{Audience: []string{"subject", "parents"}}
	got := EffectiveAudience(m)
	if len(got) != 2 || got[0] != "subject" || got[1] != "parents" {
		t.Errorf("expected [subject parents], got %v", got)
	}
}
