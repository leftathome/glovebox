// Package journal is the record journal's store: schema migrations, Apply
// (spec 2026-10-16 section 4.4), household notes, reads, erase, rescan and
// held-snapshot decisions, over database/sql with the pgx driver. It is
// the only package that talks to PostgreSQL; the logic it runs lives in
// the database-free packages beside it (record, diff, rules, roster,
// access, policy).
package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version int
	name    string
	sql     string
	sum     []byte // sha256 of the file
}

var migrationFile = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		m := migrationFile.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("journal: migration file %q is not NNNN_name.sql", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(migrationFiles, "migrations/"+e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{version: v, name: m[2], sql: string(body), sum: sum[:]})
	}
	sort.Slice(out, func(i, k int) bool { return out[i].version < out[k].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("journal: migrations must be numbered from 0001 without gaps; found %04d at position %d", m.version, i+1)
		}
	}
	return out, nil
}

// Migrations lists the embedded migrations as "NNNN_name", in order.
func Migrations() ([]string, error) {
	ms, err := loadMigrations()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = fmt.Sprintf("%04d_%s", m.version, m.name)
	}
	return out, nil
}

const migrateLockKey = "glovebox_journal:migrate"

// Migrate brings the schema to the newest embedded version.
func Migrate(ctx context.Context, db *sql.DB) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	return MigrateTo(ctx, db, len(ms))
}

// MigrateTo applies the embedded migrations up to and including version
// target (0 applies none). It holds a session advisory lock, so replicas
// starting together migrate once; each migration runs in its own
// transaction together with its schema_migrations row.
func MigrateTo(ctx context.Context, db *sql.DB, target int) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	if target < 0 || target > len(ms) {
		return fmt.Errorf("journal: no migration %d (have %d)", target, len(ms))
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, migrateLockKey); err != nil {
		return fmt.Errorf("journal: migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, migrateLockKey)
	}()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    integer     PRIMARY KEY,
		name       text        NOT NULL,
		checksum   bytea       NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	var current int
	if err := conn.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return err
	}
	if current > len(ms) {
		return fmt.Errorf("journal: the database schema is at version %d, newer than this binary's %d; refusing to run against it", current, len(ms))
	}
	if err := verifyApplied(ctx, conn, ms); err != nil {
		return err
	}
	for _, m := range ms {
		if m.version <= current || m.version > target {
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		// No arguments: pgx sends this over the simple protocol, which
		// runs every statement in the file.
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("journal: migration %04d_%s: %w", m.version, m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, m.version, m.name, m.sum); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// verifyApplied fails when an applied migration no longer matches the
// embedded file of the same version: a migration is immutable once it has
// run anywhere; changes go in a new migration.
func verifyApplied(ctx context.Context, conn *sql.Conn, ms []migration) error {
	rows, err := conn.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v int
		var name string
		var sum []byte
		if err := rows.Scan(&v, &name, &sum); err != nil {
			return err
		}
		if v < 1 || v > len(ms) {
			continue // newer schemas are refused by the caller
		}
		m := ms[v-1]
		if name != m.name {
			return fmt.Errorf("journal: migration %04d was applied as %q but this binary names it %q; applied migrations must not be renamed", v, name, m.name)
		}
		if !bytes.Equal(sum, m.sum) {
			return fmt.Errorf("journal: migration %04d_%s was edited after it was applied (checksum mismatch); applied migrations are immutable, add a new one", v, name)
		}
	}
	return rows.Err()
}

// SchemaVersion is the newest migration applied to db (0 for none).
func SchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v int
	err := db.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}
