package journal

import (
	"testing"

	"github.com/leftathome/glovebox/internal/journal/record"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
)

// Two renaming versions that first appear together are applied as a chain
// to rows still under v1 names.
func TestActivationChainsRenamesAcrossNewVersions(t *testing.T) {
	r := newRig(t)
	if _, err := r.j.db.Exec(`INSERT INTO record_current (record_type, data_subject, key, scope, audience, fields,
		first_seen_at, last_seen_at, last_confirmed_at, content_hash, last_event_seq, scan)
		VALUES ('assignment', 'e_child1', 'k', 's', '{subject}', '{"title": "W", "comment": "ok"}',
		now(), now(), now(), '\x00', 1, '{"comment": {"verdict": "pass", "scanner_version": "s"}}')`); err != nil {
		t.Fatal(err)
	}
	v3 := assignmentV2()
	v3.SchemaVersion = 3
	v3.Fields = map[string]record.FieldDef{"label": {Type: record.String}, "due": {Type: record.Date},
		"score": {Type: record.Number}, "missing": {Type: record.Bool}, "teacher_comment": {Type: record.Text}}
	v3.Renames = map[string]string{"name": "label"}
	reg := journaltest.Registry(t, assignmentV2(), v3)
	if err := New(r.j.db, Options{Registry: reg, Scanner: r.scanner}).SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	r.reg = reg
	c, _ := r.current("assignment", "e_child1", "k")
	if string(c.Fields["label"]) != `"W"` || string(c.Fields["teacher_comment"]) != `"ok"` || len(c.Fields) != 2 {
		t.Errorf("fields %v", c.Fields)
	}
	if c.Scan["teacher_comment"].Verdict != "pass" || len(c.Scan) != 1 {
		t.Errorf("scan %v", c.Scan)
	}
}
