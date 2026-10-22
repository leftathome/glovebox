package journal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/leftathome/glovebox/internal/journal/record"
)

// SyncTypes writes the configured record types into record_type. A stored
// version must still be configured, unchanged; a new version is inserted
// and, when it renames fields, record_current of that type is rewritten
// under the new names in the same transaction, without events (4.7).
func (j *Journal) SyncTypes(ctx context.Context) error {
	if j.o.Registry == nil {
		return fmt.Errorf("journal: no record type registry")
	}
	return j.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, migrateLockKey); err != nil {
			return err
		}
		stored, err := loadStoredTypes(ctx, tx)
		if err != nil {
			return err
		}
		for key, t := range stored {
			want, ok := j.o.Registry.Version(t.Name, t.SchemaVersion)
			if !ok {
				return fmt.Errorf("journal: record type %s is in the database but not in the configuration; a version, once used, stays declared", key)
			}
			if !bytes.Equal(want.Canonical(), t.Canonical()) {
				return fmt.Errorf("journal: record type %s differs from the stored definition; changing a type means a new schema_version", key)
			}
		}
		for _, t := range j.o.Registry.Types() {
			if _, ok := stored[typeKey(t)]; ok {
				continue
			}
			if err := insertType(ctx, tx, t); err != nil {
				return err
			}
			if len(t.Renames) > 0 {
				if err := rewriteFields(ctx, tx, t); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func typeKey(t record.Type) string { return fmt.Sprintf("%s v%d", t.Name, t.SchemaVersion) }

func loadStoredTypes(ctx context.Context, tx *sql.Tx) (map[string]record.Type, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, schema_version, owner, fields, renames,
		to_jsonb(default_audience), coalesce(to_jsonb(fixed_audience), '[]'::jsonb), allow_no_subject FROM record_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]record.Type{}
	for rows.Next() {
		var t record.Type
		var fields, renames, def, fixed []byte
		if err := rows.Scan(&t.Name, &t.SchemaVersion, &t.Owner, &fields, &renames, &def, &fixed, &t.AllowNoSubject); err != nil {
			return nil, err
		}
		for _, d := range []struct {
			raw []byte
			to  any
		}{{fields, &t.Fields}, {renames, &t.Renames}, {def, &t.DefaultAudience}, {fixed, &t.FixedAudience}} {
			if err := json.Unmarshal(d.raw, d.to); err != nil {
				return nil, err
			}
		}
		out[typeKey(t)] = t
	}
	return out, rows.Err()
}

func insertType(ctx context.Context, tx *sql.Tx, t record.Type) error {
	fields, _ := json.Marshal(t.Fields)
	renames, _ := json.Marshal(t.Renames)
	if t.Renames == nil {
		renames = []byte(`{}`)
	}
	def := t.DefaultAudience
	if def == nil {
		def = []string{}
	}
	var fixed []string
	if len(t.FixedAudience) > 0 {
		fixed = t.FixedAudience
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO record_type (name, schema_version, owner, fields, renames,
		default_audience, fixed_audience, allow_no_subject) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		t.Name, t.SchemaVersion, t.Owner, fields, renames, def, fixed, t.AllowNoSubject)
	return err
}

// rewriteFields renames record_current fields (and scan keys) of type t's
// previous version to t's names and recomputes the hash.
func rewriteFields(ctx context.Context, tx *sql.Tx, t record.Type) error {
	type row struct {
		subject, key string
		fields       record.Fields
		scan         record.ScanMap
	}
	rows, err := tx.QueryContext(ctx, `SELECT data_subject, key, fields, scan FROM record_current WHERE record_type = $1 FOR UPDATE`, t.Name)
	if err != nil {
		return err
	}
	var all []row
	for rows.Next() {
		var r row
		var f, s []byte
		if err := rows.Scan(&r.subject, &r.key, &f, &s); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(f, &r.fields); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal(s, &r.scan); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range all {
		fields := record.Fields{}
		for k, v := range r.fields {
			if to, ok := t.Renames[k]; ok {
				k = to
			}
			fields[k] = v
		}
		fields, err := record.Reload(t, fields.JSON())
		if err != nil {
			return err
		}
		scan := record.ScanMap{}
		for k, v := range r.scan {
			if to, ok := t.Renames[k]; ok {
				k = to
			}
			scan[k] = v
		}
		sj, _ := json.Marshal(scan)
		if _, err := tx.ExecContext(ctx, `UPDATE record_current SET fields = $4, scan = $5, content_hash = $6
			WHERE record_type = $1 AND data_subject = $2 AND key = $3`,
			t.Name, r.subject, r.key, fields.JSON(), sj, fields.Hash()); err != nil {
			return err
		}
	}
	return nil
}
