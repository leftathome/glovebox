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
// It never mutates the events, and the rows it returns share no map or
// slice with them (field values are immutable json.RawMessage bytes).
//
// Fold assumes a well-formed history, the kind the store's Apply writes. On
// an impossible sequence it does not fail; it behaves as follows:
//   - created and restored are the same operation: they replace the row
//     wholesale and make it live, whether or not the row existed or was live;
//   - updated starts from the row's fields (empty if there is no row), applies
//     its changes, and always yields a live row, even when the row was removed
//     or never created;
//   - removed marks the row removed and takes the event's scope and audience,
//     keeping the fields; removing an absent row yields a removed row with
//     no fields; removing a removed row changes nothing but scope and audience.
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
			row = FoldRow{Scope: e.Scope, Audience: cloneStrings(e.Audience), Fields: fields}
		case Updated:
			fields := row.Fields.Clone()
			keys := make([]string, 0, len(e.Changed))
			for k := range e.Changed {
				keys = append(keys, k)
			}
			keys = orderKeys(keys, e.ID.RecordType, e.SchemaVersion, rename)
			// Deletions first, then sets, so deleting a renamed key cannot
			// drop an undeclared value set in the same event.
			for _, k := range keys {
				if e.Changed[k].New == nil {
					delete(fields, rename(e.ID.RecordType, e.SchemaVersion, k))
				}
			}
			for _, k := range keys {
				if c := e.Changed[k]; c.New != nil {
					fields[rename(e.ID.RecordType, e.SchemaVersion, k)] = c.New
				}
			}
			row = FoldRow{Scope: e.Scope, Audience: cloneStrings(e.Audience), Fields: fields}
		case Removed:
			row.Removed = true
			row.Scope = e.Scope
			row.Audience = cloneStrings(e.Audience)
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

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	return append([]string{}, s...)
}
