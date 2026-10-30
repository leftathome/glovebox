package schoology

import (
	"strconv"
	"time"

	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	schoologylib "github.com/leftathome/schoology-go"
)

// AssignmentRecordKey is the record key of an assignment, in both record
// types: the decimal id, which is also what the item path's seen set holds
// (SaveLastSeenID). Exported for the seed (S15).
func AssignmentRecordKey(id int64) string {
	return strconv.FormatInt(id, 10)
}

// BuildAssignmentListing builds the lms_assignment snapshot of one kid's
// overdue/upcoming listing, for a listing that answered (spec 5.2).
//
// The listing is a window, not the kid's whole assignment list, so the
// snapshot is complete=false: it adds and updates, and never removes. An
// assignment that left the window keeps its last values in the journal
// (in_latest_snapshot=false). Rows the library could not parse are not in
// rows and are simply absent, which a windowed scope tolerates. An empty
// listing gives an empty snapshot: it confirms nothing and removes nothing,
// and it lets the journal's staleness check see the scope.
//
// A nil row is skipped; a row without an id is skipped and counted
// (no_id); a repeated id keeps the first row and is counted
// (duplicate_key), as the journal refuses a repeated key.
func BuildAssignmentListing(b Binding, rows []*schoologylib.Assignment, observedAt time.Time) journalapi.Snapshot {
	recs := []journalapi.Record{}
	seen := map[int64]bool{}
	for _, a := range rows {
		if a == nil {
			continue
		}
		if a.ID == 0 {
			b.skip("no_id")
			continue
		}
		if seen[a.ID] {
			b.skip("duplicate_key")
			continue
		}
		seen[a.ID] = true
		f := records.NewFields(nil)
		f.String("title", a.Title)
		f.String("course", a.CourseTitle)
		// Decision T4: the library's own values -- the item's status tag
		// (overdue / upcoming) and the path-only link of its Link: line.
		f.String("status", string(a.Status))
		f.String("link", a.URL)
		f.Timestamp("due", a.DueAt)
		recs = append(recs, b.record(AssignmentRecordKey(a.ID), f))
	}
	return records.NewSnapshot(TypeLMSAssignment, recordSchemaVersion, b.scope("assignments"), false, nil, observedAt, recs)
}
