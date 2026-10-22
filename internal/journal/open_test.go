package journal

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
)

func TestOpenIsLazy(t *testing.T) {
	// An unreachable address opens without dialling.
	db, err := Open("postgres://nobody@127.0.0.1:1/none", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open("::not a url::", nil); err == nil {
		t.Error("a malformed address opened")
	}
}

func TestOpenPasswordSourceFailureFailsTheConnection(t *testing.T) {
	admin := journaltest.AdminURL(t)
	calls := 0
	db, err := Open(admin, func(context.Context) (string, error) { calls++; return "", errors.New("vault down") })
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err == nil || calls == 0 {
		t.Errorf("a failing password source connected (calls=%d, err=%v)", calls, err)
	}
	if got := db.Stats().MaxOpenConnections; got != 8 {
		t.Errorf("max open connections %d", got)
	}
}

// A rotated Secret must be seen by the next connection, not cached from the
// first: with no idle connections every use dials again.
func TestOpenReadsThePasswordForEveryNewConnection(t *testing.T) {
	admin := journaltest.AdminURL(t)
	var seen []string
	db, err := Open(admin, func(c context.Context) (string, error) {
		if c == nil || c.Err() != nil {
			return "", errors.New("no live connect context")
		}
		p := fmt.Sprintf("secret-%d", len(seen)+1)
		seen = append(seen, p)
		return p, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxIdleConns(0)
	for i := 0; i < 2; i++ {
		if err := db.PingContext(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] == seen[1] {
		t.Errorf("the password source was asked %v; want a fresh value per connection", seen)
	}
}
