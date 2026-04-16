package main

// Thread-safe Message-ID deduplication tracker for the mbox importer.
//
// V1 keeps the list of ingested Message-IDs inline in the import manifest
// (see docs/specs/09-mbox-importer-design.md §3.7). At runtime the list must
// be updated concurrently by the ingest worker pool (bead glovebox-1s3) while
// the parser thread also checks it for dedup decisions. This type owns both
// the set-membership map and the canonical slice, guarded by a single RWMutex.
//
// Concurrency contract:
//   - Seen/Len are read-heavy and take an RLock.
//   - Add takes the write lock; empty messageIDs are a no-op (the parser
//     emits "" for malformed messages).
//   - Snapshot takes the read lock and returns a copy of the underlying slice
//     so that callers (e.g. manifest writers) can serialize a consistent view
//     without holding the dedup lock across a disk write.
//
// The manifest code (manifest.go) is deliberately lock-free plain data; all
// concurrency coordination lives here and in the worker pool that embeds a
// DedupSet alongside a manifest-write mutex.

import "sync"

// DedupSet tracks which Message-IDs have been ingested during this import run
// (including any resumed prior run whose manifest seeded the set at startup).
type DedupSet struct {
	mu   sync.RWMutex
	seen map[string]struct{}
	list []string
}

// NewDedupSet constructs a tracker seeded from the given slice of
// already-ingested Message-IDs. Duplicates and empty strings in the initial
// slice are silently dropped so the canonical list is guaranteed to be a set
// of non-empty IDs.
//
// Typical call site: NewDedupSet(manifest.MessageIDsIngested) right after
// loading the manifest at startup. Callers should not retain the original
// slice; after this call the DedupSet owns the canonical copy and the
// manifest's slice should be refreshed from Snapshot() before each write.
func NewDedupSet(initial []string) *DedupSet {
	d := &DedupSet{
		seen: make(map[string]struct{}, len(initial)),
		list: make([]string, 0, len(initial)),
	}
	for _, id := range initial {
		if id == "" {
			continue
		}
		if _, exists := d.seen[id]; exists {
			continue
		}
		d.seen[id] = struct{}{}
		d.list = append(d.list, id)
	}
	return d
}

// Seen reports whether the given Message-ID has already been ingested in this
// run or a resumed prior run. The empty string is never considered seen.
func (d *DedupSet) Seen(messageID string) bool {
	if messageID == "" {
		return false
	}
	d.mu.RLock()
	_, ok := d.seen[messageID]
	d.mu.RUnlock()
	return ok
}

// Add marks the given Message-ID as ingested. Safe to call concurrently from
// multiple worker goroutines. Empty messageID is a no-op. Duplicate Adds of
// the same ID are silently ignored (no double-append to the internal list).
func (d *DedupSet) Add(messageID string) {
	if messageID == "" {
		return
	}
	d.mu.Lock()
	if _, exists := d.seen[messageID]; !exists {
		d.seen[messageID] = struct{}{}
		d.list = append(d.list, messageID)
	}
	d.mu.Unlock()
}

// Len returns the count of unique Message-IDs currently tracked. Safe for
// concurrent use.
func (d *DedupSet) Len() int {
	d.mu.RLock()
	n := len(d.seen)
	d.mu.RUnlock()
	return n
}

// Snapshot returns a copy of the ingested Message-IDs suitable for
// serialization into the manifest. The returned slice is independent of the
// DedupSet's internal state: subsequent Adds do not mutate it, and mutations
// to the returned slice do not affect the DedupSet.
//
// Manifest-write callers should assign the result to
// manifest.MessageIDsIngested immediately before writing.
func (d *DedupSet) Snapshot() []string {
	d.mu.RLock()
	out := make([]string, len(d.list))
	copy(out, d.list)
	d.mu.RUnlock()
	return out
}
