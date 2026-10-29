package journal

import (
	"bytes"
	"testing"

	"github.com/leftathome/glovebox/internal/journal/journaltest"
	"github.com/leftathome/glovebox/internal/journal/record"
)

func TestSyncTypesRefusesEveryKindOfChange(t *testing.T) {
	r := newRig(t)
	mutate := map[string]func(ts []record.Type){
		"default audience": func(ts []record.Type) { ts[0].DefaultAudience = []string{"guardians"} },
		"owner":            func(ts []record.Type) { ts[0].Owner = "schoology" },
		"required":         func(ts []record.Type) { ts[0].Fields["title"] = record.FieldDef{Type: record.String, Required: true} },
		"fixed audience":   func(ts []record.Type) { ts[4].FixedAudience = []string{"guardians"} },
		"no subject":       func(ts []record.Type) { ts[0].AllowNoSubject = true },
	}
	for name, fn := range mutate {
		ts := journaltest.Types()
		fn(ts)
		reg, err := record.NewRegistry(ts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := New(r.j.db, Options{Registry: reg, Scanner: r.scanner}).SyncTypes(ctx); err == nil {
			t.Errorf("%s: a changed definition synced", name)
		}
	}
	if err := New(r.j.db, Options{Scanner: r.scanner}).SyncTypes(ctx); err == nil {
		t.Error("sync without a registry succeeded")
	}
}

func TestActivationTouchesOnlyRenamedTypes(t *testing.T) {
	r := newRig(t)
	for _, row := range []struct{ typ, key, fields string }{
		{"assignment", "a", `{"title": "A"}`},
		{"lms_assignment", "b", `{"title": "B"}`},
	} {
		if _, err := r.j.db.Exec(`INSERT INTO record_current (record_type, data_subject, key, scope, audience, fields,
			first_seen_at, last_seen_at, last_confirmed_at, content_hash, last_event_seq, removed)
			VALUES ($1, 'e_child1', $2, 's', '{subject}', $3, now(), now(), now(), '\x00', 1, $2 = 'a')`,
			row.typ, row.key, row.fields); err != nil {
			t.Fatal(err)
		}
	}
	// A version without renames rewrites nothing.
	v2 := journaltest.Types()[3]
	v2.SchemaVersion = 2
	v2.Fields = map[string]record.FieldDef{"title": {Type: record.String}, "status": {Type: record.String}, "extra": {Type: record.String}}
	if err := New(r.j.db, Options{Registry: journaltest.Registry(t, v2), Scanner: r.scanner}).SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	var hash []byte
	_ = r.j.db.QueryRow(`SELECT content_hash FROM record_current WHERE key = 'b'`).Scan(&hash)
	if !bytes.Equal(hash, []byte{0}) {
		t.Error("a version without renames rewrote rows")
	}
	// A renaming version rewrites its own type, removed rows included, and no other.
	if err := New(r.j.db, Options{Registry: journaltest.Registry(t, assignmentV2(), v2), Scanner: r.scanner}).SyncTypes(ctx); err != nil {
		t.Fatal(err)
	}
	var f string
	var removed bool
	if err := r.j.db.QueryRow(`SELECT fields::text, removed FROM record_current WHERE key = 'a'`).Scan(&f, &removed); err != nil {
		t.Fatal(err)
	}
	if !removed || f != `{"name": "A"}` {
		t.Errorf("removed row not rewritten: %s removed=%v", f, removed)
	}
	_ = r.j.db.QueryRow(`SELECT content_hash FROM record_current WHERE key = 'b'`).Scan(&hash)
	if !bytes.Equal(hash, []byte{0}) {
		t.Error("another type's row was rewritten")
	}
}
