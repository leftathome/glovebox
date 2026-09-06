package connector

// Tier is a connector's declaration of what kind of channel it is. It travels
// on every staged item as metadata.json's `tier`, and openclaw's triage uses it
// to decide whether an item is diverted to caro's feed store or written into
// audiences/<group>/inbox/ where it enters per-agent ambient recall.
//
// WHY THE CONNECTOR DECLARES THIS (openclaw-iw1s). triage previously kept a
// hardcoded allowlist of feed-class sources -- literally {"rss": true}. Any
// connector added after it was written fell through to the audiences tree and
// polluted per-agent recall, which is the exact condition caro was built to
// remove (measured 2026-07-31: feed content was 89% of main's memory index and
// ~100% of Tessera's). The list failed because it lived where the connector
// author had no reason to look. Declaring the tier here puts the decision next
// to the connector, stated once by the person who knows the answer.
//
// The tier is a property of the CHANNEL, not of the item: it says whether this
// source produces high-volume ephemeral content or durable person-relevant
// correspondence. It is not a judgement about whether one particular item
// happens to be interesting.
type Tier string

const (
	// TierFeed marks a high-volume public or semi-public stream whose items are
	// individually disposable: news, releases, papers, social timelines.
	// Diverted to caro; agents reach it deliberately via search_items.
	TierFeed Tier = "feed"

	// TierPersonal marks durable, person-relevant content that belongs in
	// ambient recall: correspondence, calendars, documents, coursework.
	// Routed into audiences/<group>/inbox/ as before.
	TierPersonal Tier = "personal"
)

// Valid reports whether t is a recognised declaration.
//
// The zero value is deliberately INVALID. NewFramework refuses to start a
// connector that has not declared a tier, so adding a connector without
// making this decision is a startup failure rather than a silent
// misclassification discovered months later in an index-pollution
// measurement. That is the entire safety property this type exists for.
//
// Matching is exact: openclaw's triage compares these strings literally and
// treats an unrecognised value as "undeclared", falling back to its frozen
// legacy allowlist. Rejecting near-misses here means that fallback is only ever
// exercised by genuinely old producers, never by a typo in a new one.
func (t Tier) Valid() bool {
	return t == TierFeed || t == TierPersonal
}
