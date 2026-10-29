package records

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// Log lines carry only the scope prefix, the record type, codes, reasons
// and counts: never a subject id, key, scope value, field value, file name
// or the token.
func TestLogsCarryNoValuesSubjectsKeysOrToken(t *testing.T) {
	_, logs := exerciseAll(t)
	if logs == "" {
		t.Fatal("the exercise logged nothing: the check would be vacuous")
	}
	for _, secret := range []string{"secretsubj", "secretkey", "secretvalue", "secretcourse", "secret", "tok-1", ".json", "e_"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain %q:\n%s", secret, logs)
		}
	}
	for _, want := range []string{`"record_type":"t1"`, `"scope":"source=src/surface=s/"`, `"reason":"schema"`, `"reason":"other"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s:\n%s", want, logs)
		}
	}
}

// logKeyAllowlist is every attribute a spool log line may carry, besides
// slog's own time, level and msg. A new key is reviewed for privacy here.
var logKeyAllowlist = map[string]bool{
	"record_type": true, "scope": true, "code": true, "reason": true, "error": true,
	"files": true, "bytes": true, "max_bytes": true, "failed": true, "total": true,
}

func TestLogLinesCarryOnlyAllowedKeys(t *testing.T) {
	_, logs := exerciseAll(t)
	lines := 0
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var attrs map[string]any
		if err := json.Unmarshal([]byte(line), &attrs); err != nil {
			t.Fatalf("not a JSON log line: %q", line)
		}
		lines++
		for k := range attrs {
			if k != slog.TimeKey && k != slog.LevelKey && k != slog.MessageKey && !logKeyAllowlist[k] {
				t.Errorf("log key %q is not in the allowlist: %s", k, line)
			}
		}
	}
	if lines < 5 {
		t.Fatalf("%d log lines: the check would be vacuous", lines)
	}
}
