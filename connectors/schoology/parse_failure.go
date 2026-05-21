package schoology

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// ReceiptDedup tracks (parser, error_class) tuples observed during a
// single pollNow() invocation. Reset at the start of each poll. One
// receipt is emitted per unique (parser, error_class) per poll;
// subsequent occurrences only bump the affected-count.
type ReceiptDedup struct {
	mu    sync.Mutex
	seen  map[string]int
	bytes map[string][]byte
}

// NewReceiptDedup returns a fresh dedup tracker for one poll.
func NewReceiptDedup() *ReceiptDedup {
	return &ReceiptDedup{
		seen:  make(map[string]int),
		bytes: make(map[string][]byte),
	}
}

// Observe records a parse failure. Returns true if this is the FIRST
// occurrence of (parser, errorClass) in this poll (caller should emit
// a receipt). Subsequent calls return false but increment the
// affected-count.
func (r *ReceiptDedup) Observe(parser, errorClass string, sampleBytes []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := parser + "|" + errorClass
	count := r.seen[key]
	r.seen[key] = count + 1
	if count == 0 {
		r.bytes[key] = sampleBytes
		return true
	}
	return false
}

// AffectedCount returns the running count for a key.
func (r *ReceiptDedup) AffectedCount(parser, errorClass string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen[parser+"|"+errorClass]
}

// BuildReceiptOptions constructs ItemOptions for a parse-failure receipt
// staging item per spec 12 §11.3. The caller writes the sample bytes
// as the item content.
func BuildReceiptOptions(parser, errorClass, errorMsg, sourceURL, targetKid, targetContentType, libVersion string, sample []byte, affectedCount int) connector.ItemOptions {
	subject := fmt.Sprintf("[parse-failure] %s: %s (target: %s/%s)",
		parser, summarize(errorMsg, 80), pickTarget(targetKid), targetContentType)
	return connector.ItemOptions{
		Source:           "schoology-parse-failure",
		Sender:           "schoology-connector",
		Subject:          subject,
		Timestamp:        time.Now().UTC(),
		DestinationAgent: "school",
		ContentType:      "application/octet-stream",
		Tags: map[string]string{
			"parse_status":              "failure_receipt",
			"parser":                    parser,
			"error":                     truncate(errorMsg, 1024),
			"error_class":               errorClass,
			"source_url":                sourceURL,
			"target_kid":                targetKid,
			"target_content_type":       targetContentType,
			"schoology_library_version": libVersion,
			"affected_count":            fmt.Sprintf("%d", affectedCount),
		},
		Audience: []string{"guardians"},
	}
}

// BuildDegradedOptions adds parse_status=degraded tags to a base options
// (which the caller would have emitted on a successful parse) for a
// partial-failure item per spec 12 §11.3.
func BuildDegradedOptions(base connector.ItemOptions, errorMsg, missingFields, itemID, itemType string) connector.ItemOptions {
	if base.Tags == nil {
		base.Tags = make(map[string]string)
	}
	base.Tags["parse_status"] = "degraded"
	base.Tags["parse_error"] = truncate(errorMsg, 1024)
	base.Tags["parse_missing_fields"] = missingFields
	base.Tags["schoology_item_id"] = itemID
	base.Tags["schoology_item_type"] = itemType
	return base
}

// SchemaDriftCounter tracks consecutive polls that produced zero items
// while logging parse errors. Used to escalate persistent breakage to
// PermanentError per spec 12 §11.4.
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

func summarize(s string, max int) string {
	s = strings.SplitN(s, "\n", 2)[0]
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max-3] + "..."
	}
	return s
}
