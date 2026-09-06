package connector

import "testing"

func TestTierValid(t *testing.T) {
	for _, tc := range []struct {
		name string
		tier Tier
		want bool
	}{
		{"feed", TierFeed, true},
		{"personal", TierPersonal, true},

		// Unset is NOT valid. A connector must state its tier: the whole point
		// of moving the decision here is that a new connector cannot be
		// forgotten, and a permissive zero value would silently restore the
		// central-list failure mode (openclaw-iw1s).
		{"unset", Tier(""), false},

		// Matching is exact. Downstream (openclaw triage) compares string
		// literals, so a near-miss must be rejected at the producer rather
		// than silently falling back on the consumer.
		{"wrong case", Tier("Feed"), false},
		{"plural", Tier("feeds"), false},
		{"padded", Tier(" feed"), false},
		{"unknown", Tier("durable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.tier.Valid(); got != tc.want {
				t.Fatalf("Tier(%q).Valid() = %v, want %v", string(tc.tier), got, tc.want)
			}
		})
	}
}

// The wire values are a cross-repo contract with openclaw's triage
// (image/cmd/triage/tier.go compares against these exact literals). Changing
// them silently re-tiers every item, so pin them here.
func TestTierWireValues(t *testing.T) {
	if string(TierFeed) != "feed" {
		t.Fatalf("TierFeed = %q, want \"feed\"; this is a cross-repo contract with openclaw triage", TierFeed)
	}
	if string(TierPersonal) != "personal" {
		t.Fatalf("TierPersonal = %q, want \"personal\"; this is a cross-repo contract with openclaw triage", TierPersonal)
	}
}
