package journal

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// bounded is a context that expires soon: a lock that cannot be taken
// fails the test within seconds instead of hanging until the test timeout.
func bounded(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	t.Cleanup(cancel)
	return c
}

// tryLock reports whether fn's lock can be taken at once in a new
// transaction (a held lock makes the statement wait, so a short deadline
// tells the two apart). The probe runs on its own connection: the holder's
// transaction pins the connection it holds.
func tryLock(t *testing.T, j *Journal, fn func(context.Context, *sql.Tx) error) bool {
	t.Helper()
	c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	tx, err := j.db.BeginTx(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(c, tx) == nil
}

// advisoryLocks counts the advisory locks held in this database. Both lock
// helpers are transaction-scoped, so none may remain once a transaction
// ends; a session-scoped lock would survive on the pooled connection and a
// probe from the same connection could not see it (advisory locks are
// re-entrant within a session).
func advisoryLocks(t *testing.T, j *Journal) int {
	t.Helper()
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var n int
	if err := j.db.QueryRowContext(c, `SELECT count(*) FROM pg_locks WHERE locktype = 'advisory'
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSubjectAndEventLocksExcludeOnlyTheirOwn(t *testing.T) {
	r := newRig(t)
	holder, err := r.j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if err := lockSubject(bounded(t), holder, "e_child1"); err != nil {
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
	if err := lockEvents(bounded(t), holder); err != nil {
		t.Fatal(err)
	}
	if tryLock(t, r.j, lockEvents) {
		t.Error("the event lock did not exclude a second holder")
	}
	if n := advisoryLocks(t, r.j); n != 2 {
		t.Errorf("%d advisory locks held, want 2", n)
	}
	if err := holder.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := advisoryLocks(t, r.j); n != 0 {
		t.Errorf("%d advisory locks outlived the transaction", n)
	}
	if !tryLock(t, r.j, subj("e_child1")) || !tryLock(t, r.j, lockEvents) {
		t.Error("locks outlived the transaction")
	}
}

func TestLocksAreReleasedByRollbackToo(t *testing.T) {
	r := newRig(t)
	tx, err := r.j.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := lockSubject(bounded(t), tx, "e_child1"); err != nil {
		t.Fatal(err)
	}
	if err := lockEvents(bounded(t), tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := advisoryLocks(t, r.j); n != 0 {
		t.Errorf("%d advisory locks outlived a rollback", n)
	}
}

// A panic inside inTx, recovered by the caller, must not leave the
// transaction (and the locks it took) open on a pooled connection.
func TestInTxRollsBackWhenTheFunctionPanics(t *testing.T) {
	r := newRig(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("inTx swallowed the panic")
			}
		}()
		_ = r.j.inTx(ctx, func(tx *sql.Tx) error {
			if err := lockEvents(ctx, tx); err != nil {
				t.Error(err)
			}
			panic("boom")
		})
	}()
	if n := advisoryLocks(t, r.j); n != 0 {
		t.Errorf("%d advisory locks left after a panic in inTx", n)
	}
	if !tryLock(t, r.j, lockEvents) {
		t.Error("the events lock is still held after a panic in inTx")
	}
}

func TestInTxRollsBackOnErrorAndCommitsOnSuccess(t *testing.T) {
	r := newRig(t)
	if _, err := r.j.db.Exec(`CREATE TABLE intx_probe (n int)`); err != nil {
		t.Fatal(err)
	}
	errBoom := sql.ErrNoRows
	err := r.j.inTx(ctx, func(tx *sql.Tx) error {
		_, _ = tx.Exec(`INSERT INTO intx_probe VALUES (1)`)
		return errBoom
	})
	if err != errBoom {
		t.Fatalf("error %v", err)
	}
	if r.count("intx_probe") != 0 {
		t.Error("a failed transaction was committed")
	}
	if err := r.j.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO intx_probe VALUES (2)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r.count("intx_probe") != 1 {
		t.Error("a successful transaction was not committed")
	}
}
