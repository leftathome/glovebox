package diff

import (
	"sort"

	"github.com/leftathome/glovebox/internal/journal/record"
)

// Identity is a record's identity (4.2).
type Identity struct {
	RecordType, DataSubject, Key string
}

// FoldEvent is the part of a record_event row the fold reads.
type FoldEvent struct {
	ID            Identity
	Kind          Kind
	SchemaVersion int
	Scope         string
	Audience      []string
	Fields        record.Fields     // created, restored
	Changed       map[string]Change // updated
}

// FoldRow is the event-derived part of a record_current row.
type FoldRow struct {
	Scope    string
	Audience []string
	Fields   record.Fields
	Removed  bool
}

// Rename maps a field name written at a schema version of a record type to
// the active version's name.
type Rename func(recordType string, version int, field string) string

// Fold replays events, in the order given (seq order), into current state.
// It never mutates the events.
func Fold(events []FoldEvent, rename Rename) map[Identity]FoldRow {
	out := map[Identity]FoldRow{}
	for _, e := range events {
		row := out[e.ID]
		switch e.Kind {
		case Created, Restored:
			fields := record.Fields{}
			for _, k := range renameOrder(e.Fields, e.ID.RecordType, e.SchemaVersion, rename) {
				fields[rename(e.ID.RecordType, e.SchemaVersion, k)] = e.Fields[k]
			}
			row = FoldRow{Scope: e.Scope, Audience: e.Audience, Fields: fields}
		case Updated:
			fields := row.Fields.Clone()
			keys := make([]string, 0, len(e.Changed))
			for k := range e.Changed {
				keys = append(keys, k)
			}
			for _, k := range orderKeys(keys, e.ID.RecordType, e.SchemaVersion, rename) {
				name := rename(e.ID.RecordType, e.SchemaVersion, k)
				if c := e.Changed[k]; c.New == nil {
					delete(fields, name)
				} else {
					fields[name] = c.New
				}
			}
			row = FoldRow{Scope: e.Scope, Audience: e.Audience, Fields: fields}
		case Removed:
			row.Removed = true
			row.Scope = e.Scope
			row.Audience = e.Audience
		}
		out[e.ID] = row
	}
	return out
}

func renameOrder(f record.Fields, rt string, v int, rename Rename) []string {
	return orderKeys(f.Keys(), rt, v, rename)
}

// orderKeys sorts keys so the result of applying renames does not depend on
// map order: names that are not renamed first, then renamed ones, so a
// renamed value beats an undeclared key of the same final name (as
// record.Forward does).
func orderKeys(keys []string, rt string, v int, rename Rename) []string {
	sort.Slice(keys, func(i, j int) bool {
		ri := rename(rt, v, keys[i]) != keys[i]
		rj := rename(rt, v, keys[j]) != keys[j]
		if ri != rj {
			return !ri
		}
		return keys[i] < keys[j]
	})
	return keys
}
