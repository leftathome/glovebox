package journal

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/leftathome/glovebox/internal/journal/diff"
	"github.com/leftathome/glovebox/internal/journal/record"
	"github.com/leftathome/glovebox/internal/journal/roster"
	"github.com/leftathome/glovebox/internal/journal/rules"
)

// Recorder receives what the journal reports for metrics (Task 23).
type Recorder interface {
	Snapshot(recordType, status string)
	Event(recordType, kind, importance string)
	ScanWithheld(recordType string, n int)
	ApplyDuration(d time.Duration)
	VisibilityMismatch()
}

type nopRecorder struct{}

func (nopRecorder) Snapshot(string, string)      {}
func (nopRecorder) Event(string, string, string) {}
func (nopRecorder) ScanWithheld(string, int)     {}
func (nopRecorder) ApplyDuration(time.Duration)  {}
func (nopRecorder) VisibilityMismatch()          {}

// Options configure a Journal.
type Options struct {
	Registry *record.Registry
	Rules    *rules.Set // nil: every event is low importance
	Guard    diff.Guard
	Scanner  TextScanner
	// Roster returns the roster in use, nil before the first load.
	Roster  func() *roster.Roster
	Now     func() time.Time
	Metrics Recorder
}

// Journal is the record journal's store.
type Journal struct {
	db *sql.DB
	o  Options
	// visible overrides the Go half of the read visibility rule; tests only.
	visible func(grants []roster.Grant, subject string, audience []string) bool
	// beforeEraseLocks runs in every Erase attempt between the look-up of
	// the subjects to lock and taking their locks; tests only.
	beforeEraseLocks func()
}

// New wraps an open database.
func New(db *sql.DB, o Options) *Journal {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Metrics == nil {
		o.Metrics = nopRecorder{}
	}
	if o.Roster == nil {
		o.Roster = func() *roster.Roster { return nil }
	}
	return &Journal{db: db, o: o}
}

// Open opens the database lazily (nothing is dialled until first use). The
// password is read again for every new connection, so a rotated Secret is
// picked up without a restart.
func Open(dsn string, password func(context.Context) (string, error)) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("journal: database address: %w", err)
	}
	var opts []stdlib.OptionOpenDB
	if password != nil {
		opts = append(opts, stdlib.OptionBeforeConnect(func(ctx context.Context, cc *pgx.ConnConfig) error {
			p, err := password(ctx)
			if err != nil {
				return fmt.Errorf("journal: database password: %w", err)
			}
			cc.Password = p
			return nil
		}))
	}
	db := stdlib.OpenDB(*cfg, opts...)
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)
	return db, nil
}

// Migrate brings the schema up to date (see migrate.go).
func (j *Journal) Migrate(ctx context.Context) error { return Migrate(ctx, j.db) }

// SchemaVersion reports the applied schema version.
func (j *Journal) SchemaVersion(ctx context.Context) (int, error) { return SchemaVersion(ctx, j.db) }

// Close closes the database.
func (j *Journal) Close() error { return j.db.Close() }

func (j *Journal) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		// Covers a panic in fn as well as every error return: without it a
		// panic recovered upstream would leave the transaction, and its
		// advisory locks, open on a pooled connection.
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	committed = true
	return tx.Commit()
}

// lockSubject serialises every write for one data_subject (decision 2).
func lockSubject(ctx context.Context, tx *sql.Tx, subject string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "glovebox_journal:subject:"+subject)
	return err
}

// lockAuthor serializes the notes of one author with that author's erase:
// WriteNote takes it for its on_behalf_of, Erase for the erased member.
// Each transaction takes at most one, always before any subject lock, so
// the two kinds cannot form a cycle. Its key cannot equal a subject lock's
// (a different prefix).
func lockAuthor(ctx context.Context, tx *sql.Tx, member string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "glovebox_journal:author:"+member)
	return err
}

// lockEvents makes event sequence numbers become visible in order
// (decision 3): taken before the first insert, held to commit.
func lockEvents(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "glovebox_journal:events")
	return err
}
