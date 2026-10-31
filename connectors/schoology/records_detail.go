package schoology

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector/records"
	"github.com/leftathome/glovebox/internal/journal/journalapi"
	"github.com/leftathome/glovebox/internal/journal/record"
	schoologylib "github.com/leftathome/schoology-go"
)

// VerifiedDetail is one clean read of an assignment's detail for one kid
// (DetailDeps.OnVerified): the page passed the id, course and title checks
// against the kid's listing, parsed without error, and its comments were
// read (an empty or populated block; see commentsRead).
type VerifiedDetail struct {
	Kid      Kid
	Row      *schoologylib.Assignment
	Detail   *schoologylib.AssignmentDetail
	Comments schoologylib.AssignmentComments
	ReadAt   time.Time
}

// maxSnapshotBytes is the journal's body limit (spec 4.3); snapshotHeadroom
// leaves room for everything but the records.
const (
	maxSnapshotBytes = 8 << 20
	snapshotHeadroom = 64 << 10
)

// BuildAssignmentDetails builds the lms_assignment_detail snapshots of one
// kid's clean detail reads in a poll: scope (schoology, assignment_detail,
// subject), complete=false, key the assignment id. None when there are no
// reads.
//
// Usually one snapshot. A poll may read up to 200 details of up to 64 KiB
// of description and comments each, and the journal refuses a body over
// 8 MiB whole (permanently), so the records are split over as many
// snapshots of the same scope and observed_at as it takes. For a
// complete=false scope that says exactly what one snapshot would have.
//
// Fields carry the library's text (Fields.Text: only made valid for JSON
// and cut at 64 KiB, counted through onCut), not the item's cleaned,
// framed body; the journal scans them (spec 4.11).
func BuildAssignmentDetails(b Binding, reads []VerifiedDetail, observedAt time.Time, onCut func()) []journalapi.Snapshot {
	var out []journalapi.Snapshot
	var recs []journalapi.Record
	size := 0
	flush := func() {
		if len(recs) > 0 {
			out = append(out, records.NewSnapshot(TypeLMSAssignmentDetail, recordSchemaVersion,
				b.scope(detailSurface), false, nil, observedAt, recs))
		}
		recs, size = nil, 0
	}
	seen := map[int64]bool{}
	for _, r := range reads {
		if r.Detail == nil || r.Row == nil {
			continue
		}
		id := r.Row.ID
		if id == 0 {
			b.skip("no_id")
			continue
		}
		if seen[id] {
			b.skip("duplicate_key")
			continue
		}
		seen[id] = true
		f := records.NewFields(onCut)
		f.Text("description", r.Detail.DescriptionText)
		f.Text("comments", commentsText(r.Comments))
		rec := b.record(AssignmentRecordKey(id), f)
		raw, err := json.Marshal(rec)
		if err != nil {
			b.skip("encode")
			continue
		}
		if len(recs) > 0 && (len(recs) >= record.MaxRecords || size+len(raw)+1 > maxSnapshotBytes-snapshotHeadroom) {
			flush()
		}
		recs = append(recs, rec)
		size += len(raw) + 1
	}
	flush()
	return out
}

// commentsRead reports whether a comment block was read: empty or
// populated. Unknown and unavailable are not a reading of the comments.
func commentsRead(c schoologylib.AssignmentComments) bool {
	return c.State == schoologylib.CommentsStateEmpty || c.State == schoologylib.CommentsStatePopulated
}

// commentsText is the comments field (decision T3): a populated block's
// entries joined with a blank line, in page order, each as the library
// returned it; "" (null) otherwise. While the library reports the layout
// unmapped an entry is author, time and body run together -- still the
// teacher's text, which the record carries and the journal scans. The
// item's "count only" line is never used here.
func commentsText(c schoologylib.AssignmentComments) string {
	if c.State != schoologylib.CommentsStatePopulated {
		return ""
	}
	var parts []string
	for _, e := range c.Comments {
		if e != nil && e.Text != "" {
			parts = append(parts, e.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}
