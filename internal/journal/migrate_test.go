package journal

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
)

func TestMigrateFromEmpty(t *testing.T) {
	db := journaltest.NewDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	names, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	v, err := SchemaVersion(ctx, db)
	if err != nil || v != len(names) {
		t.Fatalf("schema version %d, %v; want %d", v, err, len(names))
	}
	for _, table := range []string{"record_type", "snapshot", "record_current", "record_event", "erasure"} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

// Every intermediate version upgrades to exactly the schema a fresh
// database gets (spec 8: "migrations are tested from empty and from every
// previous version").
func TestMigrateFromEveryPreviousVersion(t *testing.T) {
	ctx := context.Background()
	names, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	fresh := journaltest.NewDB(t)
	if err := Migrate(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	want := schemaDump(t, fresh)
	for k := 0; k < len(names); k++ {
		db := journaltest.NewDB(t)
		if err := MigrateTo(ctx, db, k); err != nil {
			t.Fatalf("to %d: %v", k, err)
		}
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("from %d: %v", k, err)
		}
		if got := schemaDump(t, db); got != want {
			t.Errorf("from version %d the schema differs from a fresh one:\n%s\n---\n%s", k, got, want)
		}
	}
}

func TestMigrateTwiceAndConcurrentlyIsHarmless(t *testing.T) {
	db := journaltest.NewDB(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = Migrate(ctx, db) }(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	var rows, distinct int
	if err := db.QueryRow(`SELECT count(*), count(DISTINCT version) FROM schema_migrations`).Scan(&rows, &distinct); err != nil {
		t.Fatal(err)
	}
	if rows != distinct {
		t.Errorf("schema_migrations has %d rows for %d versions", rows, distinct)
	}
}

func TestMigrateRefusesANewerSchema(t *testing.T) {
	db := journaltest.NewDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_migrations (version, name) VALUES (9999, 'from_the_future')`); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Migrate against a newer schema: %v", err)
	}
}

// record_event is append-only: only the scan verdicts may change (rescan,
// 4.11), and rows go only through erase (4.13).
func TestRecordEventIsAppendOnly(t *testing.T) {
	db := journaltest.NewDB(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var seq int64
	err := db.QueryRow(`INSERT INTO record_event (record_type, data_subject, key, kind, schema_version, scope, audience,
		observed_at, approximate, actor, importance) VALUES ('t', 'e_child1', 'k', 'created', 1, 'source=x', '{subject}',
		now(), false, 'test', 'low') RETURNING seq`).Scan(&seq)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE record_event SET scan = '{"f": {"new": "pass"}}' WHERE seq = $1`, seq); err != nil {
		t.Errorf("updating scan: %v", err)
	}
	if _, err := db.Exec(`UPDATE record_event SET kind = 'removed' WHERE seq = $1`, seq); err == nil {
		t.Error("an event's kind was rewritten")
	}
	if _, err := db.Exec(`DELETE FROM record_event WHERE seq = $1`, seq); err != nil {
		t.Errorf("delete (erase) refused: %v", err)
	}
}

// schemaDump renders the public schema -- columns, constraints, indexes and
// triggers -- in a stable order.
func schemaDump(t *testing.T, db *sql.DB) string {
	t.Helper()
	var lines []string
	collect := func(q string) {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var a, b, c string
			if err := rows.Scan(&a, &b, &c); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, a+" | "+b+" | "+c)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	collect(`SELECT table_name, column_name, data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '')
		FROM information_schema.columns WHERE table_schema = 'public'`)
	collect(`SELECT conrelid::regclass::text, conname, pg_get_constraintdef(oid)
		FROM pg_constraint WHERE connamespace = 'public'::regnamespace`)
	collect(`SELECT tablename, indexname, indexdef FROM pg_indexes WHERE schemaname = 'public'`)
	collect(`SELECT tgrelid::regclass::text, tgname, pg_get_triggerdef(oid)
		FROM pg_trigger WHERE NOT tgisinternal`)
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}
