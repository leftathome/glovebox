package records

import (
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
