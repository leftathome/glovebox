package schoology

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// ReceiptDedup tracks observations of parse failures within a single
// pollNow() invocation, keyed on (parser, error_class). Reset by
// constructing a new instance at the start of each poll. One receipt
// is emitted per unique key per poll; subsequent occurrences bump the
// affected count and append the affected item ID.
type ReceiptDedup struct {
	mu      sync.Mutex
	seen    map[string]int
	itemIDs map[string][]string
}

// NewReceiptDedup returns a fresh dedup tracker for one poll.
func NewReceiptDedup() *ReceiptDedup {
	return &ReceiptDedup{
		seen:    make(map[string]int),
		itemIDs: make(map[string][]string),
	}
}

// Observe records a parse failure. Returns true if this is the FIRST
// occurrence of (parser, errorClass) in this poll (caller should emit
// a receipt). Subsequent calls return false but increment the affected
// count and append the item ID.
//
// itemID may be empty when the failure occurred before any item ID was
// extracted (e.g. top-level HTML parse failure).
func (r *ReceiptDedup) Observe(parser, errorClass, itemID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := parser + "|" + errorClass
	count := r.seen[key]
	r.seen[key] = count + 1
	if itemID != "" {
		r.itemIDs[key] = append(r.itemIDs[key], itemID)
	}
	return count == 0
}

// AffectedCount returns the running count for a key.
func (r *ReceiptDedup) AffectedCount(parser, errorClass string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[parser+"|"+errorClass]
}

// AffectedItemIDs returns a copy of the item IDs recorded against a key.
func (r *ReceiptDedup) AffectedItemIDs(parser, errorClass string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := r.itemIDs[parser+"|"+errorClass]
	out := make([]string, len(ids))
	copy(out, ids)
	return out
}

// ReceiptInputs collects the per-receipt parameters for
// BuildReceiptOptions. Fields map 1:1 to spec 12 §11.3 tag values.
type ReceiptInputs struct {
	Parser            string
	ErrorClass        string
	ErrorMsg          string
	Trace             string
	SourceURL         string
	TargetKid         string // empty for parent-level (e.g. inbox)
	TargetContentType string
	LibVersion        string
	AffectedCount     int
	AffectedItemIDs   []string // joined comma-separated into the tag
}

// BuildReceiptOptions constructs ItemOptions for a parse-failure receipt
// staging item per spec 12 §11.3. The caller writes the raw sample bytes
// as the item content (which is why no sample is in the inputs -- the
// caller already has the bytes in scope).
func BuildReceiptOptions(in ReceiptInputs) connector.ItemOptions {
	subject := fmt.Sprintf("[parse-failure] %s: %s (target: %s/%s)",
		in.Parser, summarize(in.ErrorMsg, 80), pickTarget(in.TargetKid), in.TargetContentType)
	return connector.ItemOptions{
		Source:           "schoology-parse-failure",
		Sender:           "schoology-connector",
		Subject:          subject,
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "school",
		ContentType:      "application/octet-stream",
		Tags: map[string]string{
			"parse_status":              "failure_receipt",
			"parser":                    in.Parser,
			"error":                     truncate(in.ErrorMsg, 1024),
			"error_class":               in.ErrorClass,
			"source_url":                in.SourceURL,
			"target_kid":                in.TargetKid,
			"target_content_type":       in.TargetContentType,
			"schoology_library_version": in.LibVersion,
			"trace":                     truncate(in.Trace, 1024),
			"affected_count":            strconv.Itoa(in.AffectedCount),
			"affected_item_ids":         truncate(strings.Join(in.AffectedItemIDs, ","), 1024),
		},
		DataSubject: "", // explicit per spec §11.3 -- receipts have no kid scope
		Audience:    []string{"guardians"},
	}
}

// BuildDegradedOptions returns a copy of base with parse_status=degraded
// tags added (plus parse_error / missing_fields / item_id / item_type).
// Does NOT mutate base; the caller can safely reuse base for a separate
// non-degraded item.
func BuildDegradedOptions(base connector.ItemOptions, errorMsg, missingFields, itemID, itemType string) connector.ItemOptions {
	out := base
	out.Tags = copyTags(base.Tags)
	out.Tags["parse_status"] = "degraded"
	out.Tags["parse_error"] = truncate(errorMsg, 1024)
	out.Tags["parse_missing_fields"] = truncate(missingFields, 1024)
	out.Tags["schoology_item_id"] = itemID
	out.Tags["schoology_item_type"] = itemType
	return out
}

func copyTags(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+5)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SchemaDriftCounter tracks consecutive polls that produced zero items
// while logging parse errors. Used to escalate persistent breakage to
// PermanentError per spec 12 §11.4.
//
// Lifecycle: one instance per connector process. There is no Reset();
// after ShouldEscalate fires the connector is expected to exit with a
// PermanentError. To force a reset after manual intervention, restart
// the process (which constructs a fresh counter).
type SchemaDriftCounter struct {
	mu        sync.Mutex
	count     int
	threshold int
}

// NewSchemaDriftCounter returns a counter that escalates after
// `threshold` consecutive empty-with-errors polls.
func NewSchemaDriftCounter(threshold int) *SchemaDriftCounter {
	return &SchemaDriftCounter{threshold: threshold}
}

// RecordPoll updates the counter. Successful polls (items>0) reset to
// zero. Quiet polls (items=0, no errors) do not increment. Empty polls
// with errors logged increment.
func (s *SchemaDriftCounter) RecordPoll(itemsProduced int, errorsLogged bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if itemsProduced > 0 {
		s.count = 0
		return
	}
	if errorsLogged {
		s.count++
	}
}

// ShouldEscalate reports whether the counter has crossed the threshold.
func (s *SchemaDriftCounter) ShouldEscalate() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count >= s.threshold
}

// Threshold returns the configured threshold (for logging).
func (s *SchemaDriftCounter) Threshold() int { return s.threshold }

// Count returns the current count.
func (s *SchemaDriftCounter) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func pickTarget(kid string) string {
	if kid == "" {
		return "inbox"
	}
	return kid
}

// summarize collapses s to a single line (first newline-delimited
// segment) and truncates to at most `max` total bytes including the
// trailing ellipsis. Used for human-readable Subject lines.
func summarize(s string, max int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	return truncate(s, max)
}

// truncate returns s if it fits within max bytes; otherwise returns
// the first max-3 bytes followed by "..." so the total length is
// exactly max.
func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}
