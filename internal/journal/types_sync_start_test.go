package journal

import (
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
)

// v2 (stored, renames title->name), v3 (stored,
// re-adds "title" as a new field), then v4 renames score->points. The
// rewrite must start at v3: replaying v2's renames would fold the new
// "title" into "name".
func TestRenameRewriteStartsAfterTheNewestStoredVersion(t *testing.T) {
	v3 := assignmentV2()
	v3.SchemaVersion = 3
	v3.Renames = nil
	v3.Fields = map[string]record.FieldDef{"name": {Type: record.String}, "title": {Type: record.String},
		"due": {Type: record.Date}, "score": {Type: record.Number}, "missing": {Type: record.Bool},
		"teacher_comment": {Type: record.Text}}
	r := newRig(t, func(o *Options) { o.Registry = journaltest.Registry(t, assignmentV2(), v3) })
	if _, err := r.j.db.Exec(`INSERT INTO record_current (record_type, data_subject, key, scope, audience, fields,
		first_seen_at, last_seen_at, last_confirmed_at, content_hash, last_event_seq)
		VALUES ('assignment', 'e_child1', 'k', 's', '{subject}', '{"name": "N", "title": "T", "score": 3}',
		now(), now(), now(), '\x00', 1)`); err != nil {
		t.Fatal(err)
	}
	v4 := v3
	v4.SchemaVersion = 4
	v4.Fields = map[string]record.FieldDef{"name": {Type: record.String}, "title": {Type: record.String},
		"due": {Type: record.Date}, "points": {Type: record.Number}, "missing": {Type: record.Bool},
		"teacher_comment": {Type: record.Text}}
	v4.Renames = map[string]string{"score": "points"}
	reg := journaltest.Registry(t, assignmentV2(), v3, v4)
	if err := New(r.j.db, Options{Registry: reg, Scanner: r.scanner}).SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	var f string
	if err := r.j.db.QueryRow(`SELECT fields::text FROM record_current WHERE key = 'k'`).Scan(&f); err != nil {
		t.Fatal(err)
	}
	if f != `{"name": "N", "title": "T", "points": 3}` {
		t.Errorf("fields %s", f)
	}
}
