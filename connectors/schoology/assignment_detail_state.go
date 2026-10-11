package schoology

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/leftathome/glovebox/connector"
)

// Checkpoint state for the assignment_detail surface. Two keys per kid,
// both separate from every other surface's keys:
//
//	assignment_detail:<kid>:seen  what has been staged (long memory)
//	assignment_detail:<kid>:rows  what the list looked like last poll and
//	                              when each detail was last read (short
//	                              memory: what is listed now, plus rows
//	                              that left within the last few polls)
//
// The other surfaces keep sets of int64 IDs under "<surface>:<scope>:seen".
// Detail keys live under their own checkpoint key, so however many of them
// accumulate they can never push an assignment, feed or message ID out of
// those sets.

// detailSeen maps an assignment id to the content hash of the detail item
// last staged for it. It is stored as a list of keys of the form
//
//	<kid>|detail|<assignment_id>|<content-hash>
//
// with at most one key per assignment: staging a changed detail replaces
// the old key. Only the latest hash matters -- the question each poll asks
// is "does the page differ from what downstream was last told", and a page
// that goes A -> B -> A must be staged a third time for downstream to know
// it is A again.
type detailSeen map[int64]string

// detailSeenEntry renders one seen-set key.
func detailSeenEntry(kid string, id int64, hash string) string {
	return kid + "|detail|" + strconv.FormatInt(id, 10) + "|" + hash
}

// loadDetailSeen reads a kid's seen-set. A missing key is an empty set. An
// unreadable value is an error; the caller starts a fresh set (as
// SaveLastSeenID does), at the cost of re-staging what is listed now.
func loadDetailSeen(cp connector.Checkpoint, kid string) (detailSeen, error) {
	key := seenKey(detailSurface, kid)
	seen := detailSeen{}
	v, ok := cp.Load(key)
	if !ok || v == "" {
		return seen, nil
	}
	var entries []string
	if err := json.Unmarshal([]byte(v), &entries); err != nil {
		return detailSeen{}, fmt.Errorf("checkpoint parse error key=%s: %w", key, err)
	}
	prefix := kid + "|detail|"
	for _, e := range entries {
		rest, found := strings.CutPrefix(e, prefix)
		if !found {
			return detailSeen{}, fmt.Errorf("checkpoint parse error key=%s: entry for another scope", key)
		}
		idStr, hash, found := strings.Cut(rest, "|")
		if !found || hash == "" {
			return detailSeen{}, fmt.Errorf("checkpoint parse error key=%s: malformed entry", key)
		}
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			return detailSeen{}, fmt.Errorf("checkpoint parse error key=%s: %w", key, err)
		}
		seen[id] = hash
	}
	return seen, nil
}

// boundDetailSeen trims seen to at most limit entries, in place.
//
// The other surfaces drop their smallest IDs on overflow. That is not safe
// here: a detail key is consulted again on every refresh for as long as its
// assignment is listed, and dropping one makes the next refresh look like a
// change and stages the item again. So assignments in listed (what Schoology
// lists for this kid right now) are dropped last; among the rest the
// smallest IDs go first, since those are the oldest and the least likely to
// be listed again. The limit is hard: if listed alone exceeds it, the
// smallest listed IDs go too.
func boundDetailSeen(seen detailSeen, listed map[int64]bool, limit int) {
	over := len(seen) - limit
	if over <= 0 {
		return
	}
	var unlisted, kept []int64
	for id := range seen {
		if listed[id] {
			kept = append(kept, id)
		} else {
			unlisted = append(unlisted, id)
		}
	}
	sort.Slice(unlisted, func(i, j int) bool { return unlisted[i] < unlisted[j] })
	sort.Slice(kept, func(i, j int) bool { return kept[i] < kept[j] })
	for _, id := range append(unlisted, kept...) {
		if over == 0 {
			return
		}
		delete(seen, id)
		over--
	}
}

// saveDetailSeen bounds and writes a kid's seen-set. MUST be called only
// after the item's Commit() returned nil (spec 05 section 3.2).
func saveDetailSeen(cp connector.Checkpoint, kid string, seen detailSeen, listed map[int64]bool) error {
	boundDetailSeen(seen, listed, maxSeenIDs)
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	entries := make([]string, len(ids))
	for i, id := range ids {
		entries[i] = detailSeenEntry(kid, id, seen[id])
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return cp.Save(seenKey(detailSurface, kid), string(data))
}

// detailRow is what the connector remembers about one listed assignment
// between polls.
type detailRow struct {
	// Fingerprint is the hash of the list row when its detail was last
	// read. A different row means the detail is read again.
	Fingerprint string `json:"fp"`
	// FetchedAt is when the detail was last read, unix seconds. It drives
	// the slow refresh. Zero means never: the row is only waiting.
	FetchedAt int64 `json:"at"`
	// Since is when a row the per-poll cap deferred first needed reading,
	// unix seconds, so it goes ahead of rows that turn up later. Zero once
	// the row has been read.
	Since int64 `json:"since,omitempty"`
	// Missing counts consecutive successful list reads the assignment was
	// absent from.
	Missing int `json:"missing,omitempty"`
}

// detailRowGracePolls is how many consecutive listings an assignment must
// be absent from before its row is forgotten. Schoology has been seen to
// return a short or empty list for one poll; forgetting on the first
// absence would cost a re-read of everything when the list comes back.
const detailRowGracePolls = 3

// detailRows maps an assignment id to its remembered row.
type detailRows map[int64]detailRow

func detailRowsKey(kid string) string {
	return detailSurface + ":" + kid + ":rows"
}

// loadDetailRows reads a kid's row map. Missing is empty; unreadable is an
// error and the caller starts fresh, which costs one detail read per listed
// assignment and stages nothing that is unchanged.
func loadDetailRows(cp connector.Checkpoint, kid string) (detailRows, error) {
	rows := detailRows{}
	v, ok := cp.Load(detailRowsKey(kid))
	if !ok || v == "" {
		return rows, nil
	}
	if err := json.Unmarshal([]byte(v), &rows); err != nil {
		return detailRows{}, fmt.Errorf("checkpoint parse error key=%s: %w", detailRowsKey(kid), err)
	}
	return rows, nil
}

// saveDetailRows writes a kid's row map.
func saveDetailRows(cp connector.Checkpoint, kid string, rows detailRows) error {
	data, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return cp.Save(detailRowsKey(kid), string(data))
}

// ageDetailRows notes which remembered assignments are absent from this
// (successful) listing, forgets the ones that have been absent for
// detailRowGracePolls listings in a row, and reports whether anything
// changed. This is what keeps the row map small: it holds the current
// listing plus a few polls of leavers. An assignment that is forgotten and
// comes back costs one detail read and, because the seen-set still holds
// its hash, stages nothing unless it changed.
func ageDetailRows(rows detailRows, listed map[int64]bool) bool {
	changed := false
	for id, row := range rows {
		switch {
		case listed[id] && row.Missing != 0:
			row.Missing = 0
			rows[id] = row
			changed = true
		case listed[id]:
		case row.Missing+1 >= detailRowGracePolls:
			delete(rows, id)
			changed = true
		default:
			row.Missing++
			rows[id] = row
			changed = true
		}
	}
	return changed
}

// fetchedAt returns the row's last-read time.
func (r detailRow) fetchedAt() time.Time {
	return time.Unix(r.FetchedAt, 0)
}
