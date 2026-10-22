package journal

import (
	"bytes"
	"strings"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
)

func TestSyncTypesWritesEachVersionOnce(t *testing.T) {
	r := newRig(t)
	if got := r.count("record_type"); got != len(journaltest.Types()) {
		t.Fatalf("record_type rows %d", got)
	}
	if err := r.j.SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	if got := r.count("record_type"); got != len(journaltest.Types()) {
		t.Errorf("second sync added rows: %d", got)
	}
}

func TestSyncTypesRefusesAChangedDefinition(t *testing.T) {
	r := newRig(t)
	types := journaltest.Types()
	types[0].Fields["title"] = record.FieldDef{Type: record.Text}
	reg, err := record.NewRegistry(types)
	if err != nil {
		t.Fatal(err)
	}
	j2 := New(r.j.db, Options{Registry: reg, Scanner: r.scanner})
	if err := j2.SyncTypes(ctx); err == nil || !strings.Contains(err.Error(), "new schema_version") {
		t.Errorf("a changed version synced: %v", err)
	}
	reg, _ = record.NewRegistry(types[1:])
	j3 := New(r.j.db, Options{Registry: reg, Scanner: r.scanner})
	if err := j3.SyncTypes(ctx); err == nil {
		t.Error("a configuration without a stored type synced")
	}
}

func assignmentV2() record.Type {
	t := journaltest.Types()[0]
	t.SchemaVersion = 2
	t.Fields = map[string]record.FieldDef{"name": {Type: record.String}, "due": {Type: record.Date},
		"score": {Type: record.Number}, "missing": {Type: record.Bool}, "teacher_comment": {Type: record.Text}}
	t.Renames = map[string]string{"title": "name", "comment": "teacher_comment"}
	return t
}

// 4.7: activating a version rewrites current fields (and their scan keys
// and hash) under the new names, with no events.
func TestActivationRewritesCurrentFields(t *testing.T) {
	r := newRig(t)
	_, err := r.j.db.Exec(`INSERT INTO record_current (record_type, data_subject, key, scope, audience, fields,
		first_seen_at, last_seen_at, last_confirmed_at, content_hash, last_event_seq, scan)
		VALUES ('assignment', 'e_child1', 'k', 's', '{guardians,subject}', '{"title": "W", "comment": "ok"}',
		now(), now(), now(), '\x00', 1, '{"comment": {"verdict": "pass", "scanner_version": "scanner-1"}}')`)
	if err != nil {
		t.Fatal(err)
	}
	reg := journaltest.Registry(t, assignmentV2())
	j2 := New(r.j.db, Options{Registry: reg, Scanner: r.scanner})
	if err := j2.SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	r.reg = reg
	c, _ := r.current("assignment", "e_child1", "k")
	if string(c.Fields["name"]) != `"W"` || string(c.Fields["teacher_comment"]) != `"ok"` || c.Fields["title"] != nil {
		t.Errorf("fields %v", c.Fields)
	}
	if c.Scan["teacher_comment"].Verdict != "pass" {
		t.Errorf("scan %v", c.Scan)
	}
	var hash []byte
	_ = r.j.db.QueryRow(`SELECT content_hash FROM record_current WHERE key = 'k'`).Scan(&hash)
	if !bytes.Equal(hash, c.Fields.Hash()) {
		t.Error("the hash was not recomputed")
	}
	if r.count("record_event") != 0 {
		t.Error("activation wrote events")
	}
	if err := j2.SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	c2, _ := r.current("assignment", "e_child1", "k")
	if string(c2.Fields["name"]) != `"W"` {
		t.Error("a second sync renamed again")
	}
}
