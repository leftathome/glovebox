// Package diff turns one observation of a scope into planned change events
// (spec 2026-10-16 section 4.4 steps 7-8), decides the removal guard (4.5)
// and folds events back into current state (4.2's invariant). It touches
// no database.
package diff

import (
	"bytes"
	"encoding/json"
	"sort"
	"time"

	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// Kind is an event kind.
type Kind string

const (
	Created  Kind = "created"
	Updated  Kind = "updated"
	Removed  Kind = "removed"
	Restored Kind = "restored"
)

// Current is a record_current row as the diff needs it.
type Current struct {
	Key             string
	Scope           string
	Audience        []string // sorted, distinct
	Fields          record.Fields
	Removed         bool
	Hash            []byte
	LastSeenAt      time.Time
	LastConfirmedAt time.Time
	Scan            record.ScanMap // carried for the writer; the diff ignores it
}

// Change is one changed field: nil means null (absent).
type Change struct {
	Old, New json.RawMessage
}

// Event is one planned change.
type Event struct {
	Kind         Kind
	Key          string
	Scope        string
	PrevScope    string   // set when the record moves scope
	Audience     []string // after the event
	PrevAudience []string // set when the audience changes
	// After: the record's fields after the event (created, restored,
	// updated). Before: before it (updated, restored, removed).
	After, Before  record.Fields
	Changed        map[string]Change // updated only
	PrevObservedAt *time.Time        // nil for created
}

// Input is one snapshot against the current rows of its subject and type.
type Input struct {
	Scope      string // canonical
	Complete   bool
	Range      *record.Range
	ObservedAt time.Time
	Current    map[string]Current // every row of the subject and type, by key
	Records    []record.NormRecord
}

// Plan is what applying the snapshot would do.
type Plan struct {
	Events      []Event  // created, updated, restored: in snapshot order
	Removals    []Event  // candidate removals, by key; the guard decides
	Confirm     []string // present and unchanged: observation columns only
	Stale       []string // confirmed later by another scope: left alone
	LiveInRange int      // live keys of this scope (in range) before the snapshot
}

// Stats counts the plan as journalapi reports it.
func (p Plan) Stats() journalapi.Stats {
	var s journalapi.Stats
	for _, e := range p.Events {
		switch e.Kind {
		case Created:
			s.Created++
		case Updated:
			s.Updated++
		case Restored:
			s.Restored++
		}
	}
	s.Removed = len(p.Removals)
	s.Unchanged = len(p.Confirm)
	s.Stale = len(p.Stale)
	return s
}

// Diff plans the events of one snapshot.
func Diff(in Input) Plan {
	var p Plan
	present := make(map[string]bool, len(in.Records))
	for _, r := range in.Records {
		present[r.Key] = true
		c, ok := in.Current[r.Key]
		if !ok {
			p.Events = append(p.Events, Event{Kind: Created, Key: r.Key, Scope: in.Scope, Audience: r.Audience, After: r.Fields})
			continue
		}
		if c.LastConfirmedAt.After(in.ObservedAt) {
			p.Stale = append(p.Stale, r.Key)
			continue
		}
		prev := c.LastConfirmedAt
		e := Event{Key: r.Key, Scope: in.Scope, Audience: r.Audience, After: r.Fields, Before: c.Fields, PrevObservedAt: &prev}
		if c.Scope != in.Scope {
			e.PrevScope = c.Scope
		}
		audChanged := !equalStrings(c.Audience, r.Audience)
		if audChanged {
			e.PrevAudience = c.Audience
		}
		if c.Removed {
			e.Kind = Restored
			p.Events = append(p.Events, e)
			continue
		}
		if bytes.Equal(c.Hash, r.Hash) && e.PrevScope == "" && !audChanged {
			p.Confirm = append(p.Confirm, r.Key)
			continue
		}
		e.Kind = Updated
		e.Changed = changes(c.Fields, r.Fields)
		p.Events = append(p.Events, e)
	}
	if !in.Complete {
		return p
	}
	keys := make([]string, 0, len(in.Current))
	for k := range in.Current {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		c := in.Current[k]
		if c.Removed || c.Scope != in.Scope {
			continue
		}
		if in.Range != nil && !in.Range.Contains(c.Fields[in.Range.Field]) {
			continue
		}
		p.LiveInRange++
		if present[k] || c.LastConfirmedAt.After(in.ObservedAt) {
			continue
		}
		prev := c.LastConfirmedAt
		p.Removals = append(p.Removals, Event{Kind: Removed, Key: k, Scope: c.Scope, Audience: c.Audience,
			Before: c.Fields, PrevObservedAt: &prev})
	}
	return p
}

func changes(before, after record.Fields) map[string]Change {
	out := map[string]Change{}
	for k, v := range before {
		if !bytes.Equal(v, after[k]) {
			out[k] = Change{Old: v, New: after[k]}
		}
	}
	for k, v := range after {
		if _, ok := before[k]; !ok {
			out[k] = Change{Old: nil, New: v}
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
