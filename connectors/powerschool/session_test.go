package powerschool

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pslib "github.com/leftathome/powerschool-go"
)

// syntheticSession returns the JSON a login workload would store, built
// with the library's own type so the two cannot drift.
func syntheticSession(t *testing.T, expires time.Time, cookies ...*http.Cookie) string {
	t.Helper()
	data, err := json.Marshal(pslib.SessionExport{Cookies: cookies, ExpiresAt: expires})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	return string(data)
}

func TestParseSession(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	good := &http.Cookie{Name: "JSESSIONID", Value: "synthetic-secret-value"}
	cases := []struct {
		name    string
		raw     string
		wantErr string // substring; empty = valid
		wantN   int
	}{
		{name: "valid", raw: syntheticSession(t, now.Add(time.Hour), good), wantN: 1},
		{name: "valid with surrounding whitespace", raw: "\n " + syntheticSession(t, now.Add(time.Hour), good) + "\n", wantN: 1},
		{
			name:  "unusable cookies are dropped",
			raw:   syntheticSession(t, now.Add(time.Hour), good, &http.Cookie{Name: "empty"}, &http.Cookie{Value: "nameless"}),
			wantN: 1,
		},
		{name: "not JSON", raw: "JSESSIONID=synthetic-secret-value", wantErr: "not valid JSON"},
		{name: "empty", raw: "", wantErr: "not valid JSON"},
		{name: "no cookies", raw: syntheticSession(t, now.Add(time.Hour)), wantErr: "no cookies"},
		{name: "only unusable cookies", raw: syntheticSession(t, now.Add(time.Hour), &http.Cookie{Name: "empty"}), wantErr: "no cookies"},
		{name: "no expiry", raw: `{"cookies":[{"Name":"JSESSIONID","Value":"synthetic-secret-value"}]}`, wantErr: "no expires_at"},
		{name: "expired", raw: syntheticSession(t, now.Add(-time.Minute), good), wantErr: "session expired at"},
		{name: "expires exactly now", raw: syntheticSession(t, now, good), wantErr: "session expired at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseSession([]byte(tc.raw), now)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseSession: %v", err)
				}
				if len(s.Cookies) != tc.wantN {
					t.Errorf("cookies = %d, want %d", len(s.Cookies), tc.wantN)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			// The document is a credential: no error may echo it.
			if strings.Contains(err.Error(), "synthetic-secret-value") {
				t.Errorf("error leaks the session: %v", err)
			}
		})
	}
}

func TestLoadSession(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	doc := syntheticSession(t, now.Add(time.Hour), &http.Cookie{Name: "JSESSIONID", Value: "from-env"})
	fileDoc := syntheticSession(t, now.Add(time.Hour), &http.Cookie{Name: "JSESSIONID", Value: "from-file"})
	path := filepath.Join(t.TempDir(), "ps-export.json")
	if err := os.WriteFile(path, []byte(fileDoc), 0o600); err != nil {
		t.Fatalf("write session file: %v", err)
	}
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	cases := []struct {
		name      string
		env       map[string]string
		wantValue string
		wantErr   bool
	}{
		{name: "env document", env: map[string]string{"POWERSCHOOL_SESSION": doc}, wantValue: "from-env"},
		{name: "file", env: map[string]string{"POWERSCHOOL_SESSION_FILE": path}, wantValue: "from-file"},
		{name: "file wins over env", env: map[string]string{"POWERSCHOOL_SESSION_FILE": path, "POWERSCHOOL_SESSION": doc}, wantValue: "from-file"},
		{name: "neither set", env: map[string]string{}, wantErr: true},
		{name: "missing file", env: map[string]string{"POWERSCHOOL_SESSION_FILE": filepath.Join(t.TempDir(), "absent.json")}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := LoadSession(env(tc.env), now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && s.Cookies[0].Value != tc.wantValue {
				t.Errorf("cookie value = %q, want %q", s.Cookies[0].Value, tc.wantValue)
			}
		})
	}
}
