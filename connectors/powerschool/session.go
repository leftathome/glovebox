package powerschool

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	pslib "github.com/leftathome/powerschool-go"
)

// LoadSession reads the saved PowerSchool session the connector runs on.
//
// The connector never holds the account's username or password and never
// logs in: login needs a browser and a POST, neither of which the read
// path is allowed (see Transport). A separate login workload produces the
// session with powerschool-go's Client.ExportSession and stores its JSON
// form ({"cookies":[...],"expires_at":"..."}); this function reads that
// JSON back.
//
// Two delivery forms are accepted, file first:
//
//	POWERSCHOOL_SESSION_FILE  path to the JSON document
//	POWERSCHOOL_SESSION       the JSON document itself (envFrom a Secret)
//
// getenv is injected so tests need not mutate the process environment.
func LoadSession(getenv func(string) string, now time.Time) (*pslib.SessionExport, error) {
	var raw []byte
	switch {
	case getenv("POWERSCHOOL_SESSION_FILE") != "":
		data, err := os.ReadFile(getenv("POWERSCHOOL_SESSION_FILE"))
		if err != nil {
			return nil, fmt.Errorf("read session file: %w", err)
		}
		raw = data
	case getenv("POWERSCHOOL_SESSION") != "":
		raw = []byte(getenv("POWERSCHOOL_SESSION"))
	default:
		return nil, fmt.Errorf("neither POWERSCHOOL_SESSION_FILE nor POWERSCHOOL_SESSION is set")
	}
	return ParseSession(raw, now)
}

// ParseSession decodes and validates a session document. Errors never
// include the document: it is a live credential.
func ParseSession(raw []byte, now time.Time) (*pslib.SessionExport, error) {
	var s pslib.SessionExport
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &s); err != nil {
		return nil, fmt.Errorf("session is not valid JSON")
	}
	usable := s.Cookies[:0]
	for _, c := range s.Cookies {
		if c != nil && c.Name != "" && c.Value != "" {
			usable = append(usable, c)
		}
	}
	s.Cookies = usable
	if len(s.Cookies) == 0 {
		return nil, fmt.Errorf("session has no cookies")
	}
	if s.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("session has no expires_at")
	}
	if !s.ExpiresAt.After(now) {
		return nil, fmt.Errorf("session expired at %s", s.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return &s, nil
}
