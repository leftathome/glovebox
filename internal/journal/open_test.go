package journal

import (
	"errors"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
)

func TestOpenIsLazyAndReadsThePasswordPerConnection(t *testing.T) {
	admin := journaltest.AdminURL(t)
	// Lazy: an unreachable address opens without dialling.
	db, err := Open("postgres://nobody@127.0.0.1:1/none", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open("::not a url::", nil); err == nil {
		t.Error("a malformed address opened")
	}

	calls := 0
	db, err = Open(admin, func() (string, error) { calls++; return "", errors.New("vault down") })
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
