package journal

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// tryLock reports whether fn's lock can be taken at once in a new
// transaction (a held lock makes the statement wait, so a short deadline
// tells the two apart).
func tryLock(t *testing.T, j *Journal, fn func(context.Context, *sql.Tx) error) bool {
	t.Helper()
	c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(c, tx) == nil
}

func TestSubjectAndEventLocksExcludeOnlyTheirOwn(t *testing.T) {
	r := newRig(t)
	holder, err := r.j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockSubject(ctx, holder, "e_child1"); err != nil {
		t.Fatal(err)
	}
	subj := func(s string) func(context.Context, *sql.Tx) error {
		return func(c context.Context, tx *sql.Tx) error { return lockSubject(c, tx, s) }
	}
	if tryLock(t, r.j, subj("e_child1")) {
		t.Error("a second writer for the same subject was not excluded")
	}
	if !tryLock(t, r.j, subj("e_child2")) {
		t.Error("a writer for another subject was excluded")
	}
	if !tryLock(t, r.j, lockEvents) {
		t.Error("the subject lock excluded the event lock")
	}
	if err := lockEvents(ctx, holder); err != nil {
		t.Fatal(err)
	}
	if tryLock(t, r.j, lockEvents) {
		t.Error("the event lock did not exclude a second holder")
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if !tryLock(t, r.j, subj("e_child1")) || !tryLock(t, r.j, lockEvents) {
		t.Error("locks outlived the transaction")
	}
}
