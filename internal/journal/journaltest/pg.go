// Package journaltest gives journal tests a fresh PostgreSQL database each,
// and the fixtures they share.
//
// JOURNAL_TEST_DATABASE_URL names a server and a superuser connection to it
// (scripts/journal-test-db.sh starts one: the CloudNativePG image at the
// PostgreSQL version production runs). NewDB creates a database owned by
// the non-superuser role journal_app -- the shape of the CNPG Database the
// gitops change creates -- connects as that role, and drops the database
// when the test ends.
//
// Without the variable a database test is skipped, unless
// JOURNAL_TEST_REQUIRE_DB is set (CI), in which case it fails: a database
// job that silently skipped every database test would be green and
// worthless.
package journaltest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver
)

// AppRole is the role every test database belongs to.
const AppRole = "journal_app"

var (
	serverOnce sync.Once
	serverErr  error
)

// appPassword is the test-only password of AppRole. It protects nothing (the
// server is a throwaway container) but gives the role a password, so servers
// that require password authentication (scram), such as a stock PostgreSQL
// CI service container, work as well as trust-auth ones. It is the same in
// every test binary, so binaries running in parallel never invalidate each
// other's connections.
const appPassword = "journal-test-only"

// AdminURL returns JOURNAL_TEST_DATABASE_URL, skipping or failing the test
// when it is not set.
func AdminURL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv("JOURNAL_TEST_DATABASE_URL")
	if admin == "" {
		if os.Getenv("JOURNAL_TEST_REQUIRE_DB") != "" {
			t.Fatal("JOURNAL_TEST_REQUIRE_DB is set but JOURNAL_TEST_DATABASE_URL is not")
		}
		t.Skip("JOURNAL_TEST_DATABASE_URL is not set; run scripts/journal-test-db.sh")
	}
	return admin
}

// NewDB returns a connection to a new, empty database owned by AppRole.
func NewDB(t testing.TB) *sql.DB {
	t.Helper()
	admin := AdminURL(t)
	serverOnce.Do(func() { serverErr = prepareServer(admin) })
	if serverErr != nil {
		t.Fatalf("test database server: %v", serverErr)
	}
	adb, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer adb.Close()
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "jt_" + hex.EncodeToString(b[:])
	if _, err := adb.Exec("CREATE DATABASE " + name + " OWNER " + AppRole); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(AppRole, appPassword)
	u.Path = "/" + name
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		a, err := sql.Open("pgx", admin)
		if err != nil {
			return
		}
		defer a.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = a.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return db
}

// prepareServer waits for the server (a CI service container may still be
// running initdb) and creates the application role once.
func prepareServer(admin string) error {
	db, err := sql.Open("pgx", admin)
	if err != nil {
		return err
	}
	defer db.Close()
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = db.PingContext(ctx)
		cancel()
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return err
	}
	// Several test binaries start at once and share the role; a transaction
	// lock serialises whoever creates it.
	_, err = db.Exec(fmt.Sprintf(`DO $$ BEGIN
		PERFORM pg_advisory_xact_lock(7347001);
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'journal_app') THEN
			CREATE ROLE journal_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE;
		END IF;
		ALTER ROLE journal_app PASSWORD '%s';
	END $$`, appPassword))
	return err
}
