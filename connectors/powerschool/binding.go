package powerschool

import (
	"github.com/leftathome/glovebox/connector"
)

// A kid is bound to one scores-page student id (the library's
// StudentAPIID), and every scores page read for that kid must carry it.
//
// Why this id. The portal keeps "which student is selected" on its side of
// the session and renders scores.html for THAT student, whatever course the
// URL names: asking for child B's course while child A is selected returns
// a page carrying child A's id (observed live). The id is distinct per
// child. So the id on a scores page is direct evidence of which child the
// session had selected at the moment that page was served -- evidence the
// home page's marker cannot give for anything read between two looks at it.
// A selection that flips to the other child after the kid's turn starts and
// flips back before it ends passes both marker checks; it does not pass
// this one.
//
// Where the bound id comes from:
//   - student_api_id in the kid's config, if set; or
//   - learned: the first time a kid's turn completes with the marker
//     confirmed before and after and every scores page agreeing on one id
//     that no other kid is bound to, that id is saved in the checkpoint
//     (apiIDKey) and required from then on, across restarts.
//
// The learning poll itself is the one poll the binding cannot protect: it
// has only the marker checks and the pages' agreement with each other.
// Setting student_api_id in config closes that too.

// apiIDKey is the checkpoint key holding a kid's learned scores-page id.
func apiIDKey(kid string) string {
	return "meta:student_api_id:" + kid
}

// studentBinding is the binding state for one kid's turn in one poll.
type studentBinding struct {
	// want is the kid's bound id; "" while it has none.
	want string
	// others holds the ids bound to the OTHER kids: a page carrying one of
	// those is known to be another child's, bound or not.
	others map[string]bool
	// seen is the id the scores pages of this turn agreed on, while want
	// is empty. It is what gets learned.
	seen string
}

// newStudentBinding builds kid's binding from config and checkpoint.
func newStudentBinding(kids []Kid, cp connector.Checkpoint, kid Kid) *studentBinding {
	b := &studentBinding{want: boundAPIID(cp, kid), others: map[string]bool{}}
	for _, other := range kids {
		if other.Name == kid.Name {
			continue
		}
		if id := boundAPIID(cp, other); id != "" {
			b.others[id] = true
		}
	}
	return b
}

// boundAPIID returns kid's bound id: config first, then the checkpoint.
func boundAPIID(cp connector.Checkpoint, kid Kid) string {
	if kid.StudentAPIID != "" {
		return kid.StudentAPIID
	}
	if cp == nil {
		return ""
	}
	id, _ := cp.Load(apiIDKey(kid.Name))
	return id
}

// check puts one scores page's student id to the binding. Any error means
// the page is not usable as this kid's and the kid's poll is discarded.
func (b *studentBinding) check(apiID string) error {
	switch {
	case apiID == "":
		// A page that names no student is not evidence of anything.
		return ErrSelectionChanged
	case b.others[apiID]:
		return ErrSelectionChanged
	case b.want != "":
		if apiID != b.want {
			return ErrSelectionChanged
		}
		return nil
	case b.seen == "":
		b.seen = apiID
		return nil
	case apiID != b.seen:
		return ErrSelectionChanged
	}
	return nil
}

// learn persists the id this turn's scores pages agreed on, if the kid has
// no bound id yet. Call it only after the end-of-turn marker check passed.
// It reports whether a new binding was saved.
func (b *studentBinding) learn(cp connector.Checkpoint, kid Kid) (learned bool, err error) {
	if b.want != "" || b.seen == "" {
		return false, nil
	}
	if b.others[b.seen] {
		return false, ErrStudentAPIIDConflict
	}
	if err := cp.Save(apiIDKey(kid.Name), b.seen); err != nil {
		return false, err
	}
	b.want = b.seen
	return true, nil
}
