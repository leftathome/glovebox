package connector

import "sync"

// FetchLimits configures caps on items fetched per poll cycle.
// A zero value for either field means unlimited.
type FetchLimits struct {
	PerSource int `json:"per_source"` // max items per source per poll (0 = unlimited)
	PerPoll   int `json:"per_poll"`   // max items total across all sources (0 = unlimited)
}

// FetchStatus indicates whether a fetch attempt was allowed or denied and why.
type FetchStatus int

const (
	FetchAllowed     FetchStatus = iota // fetch is permitted
	FetchSourceLimit                    // per-source limit reached
	FetchPollLimit                      // aggregate poll limit reached
)

// Allowed returns true if the status permits fetching.
func (s FetchStatus) Allowed() bool {
	return s == FetchAllowed
}

// FetchCounter tracks and enforces fetch limits within a poll cycle.
// It is safe for concurrent use.
type FetchCounter struct {
	limits       FetchLimits
	mu           sync.Mutex
	totalCount   int
	sourceCounts map[string]int
}

// NewFetchCounter creates a FetchCounter with the given limits.
func NewFetchCounter(limits FetchLimits) *FetchCounter {
	return &FetchCounter{
		limits:       limits,
		sourceCounts: make(map[string]int),
	}
}

// TryFetch checks if fetching one more item from the given source is allowed.
// If allowed, it increments the counters and returns FetchAllowed.
// Otherwise it returns FetchSourceLimit or FetchPollLimit without changing counts.
func (fc *FetchCounter) TryFetch(source string) FetchStatus {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Check poll limit first (it blocks all sources).
	if fc.limits.PerPoll > 0 && fc.totalCount >= fc.limits.PerPoll {
		return FetchPollLimit
	}

	// Check per-source limit.
	if fc.limits.PerSource > 0 && fc.sourceCounts[source] >= fc.limits.PerSource {
		return FetchSourceLimit
	}

	// Allowed: increment counters.
	fc.totalCount++
	fc.sourceCounts[source]++
	return FetchAllowed
}

// Count returns the total number of items fetched so far in this poll cycle.
func (fc *FetchCounter) Count() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.totalCount
}

// Reset clears all counts for a new poll cycle.
func (fc *FetchCounter) Reset() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.totalCount = 0
	fc.sourceCounts = make(map[string]int)
}
