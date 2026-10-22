package journal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/leftathome/glovebox/internal/journal/record"
)

// SyncTypes writes the configured record types into record_type. A stored
// version must still be configured, unchanged; a new version is inserted
// and, when it renames fields, record_current of that type is rewritten
// under the new names in the same transaction, without events (4.7).
//
// Rolling deploys: activation rewrites rows to the new names, and nothing
// stops an old replica that still runs the previous configuration from
// applying a v1 snapshot afterwards and writing v1 names back. Activating
// a version with renames therefore requires the old replicas to be gone
// first: run the journal with one replica or the Recreate strategy for
// that release.
func (j *Journal) SyncTypes(ctx context.Context) error {
	if j.o.Registry == nil {
		return fmt.Errorf("journal: no record type registry")
	}
	return j.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, migrateLockKey); err != nil {
			return fmt.Errorf("journal: sync types: lock: %w", err)
		}
		stored, err := loadStoredTypes(ctx, tx)
		if err != nil {
			return fmt.Errorf("journal: sync types: load: %w", err)
		}
		keys := make([]string, 0, len(stored))
		for k := range stored {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			t := stored[key]
			want, ok := j.o.Registry.Version(t.Name, t.SchemaVersion)
			if !ok {
				return fmt.Errorf("journal: sync types %s: the type is in the database but not in the configuration; a version, once used, stays declared", key)
			}
			if !bytes.Equal(want.Canonical(), t.Canonical()) {
				return fmt.Errorf("journal: sync types %s: differs from the stored definition; changing a type means a new schema_version", key)
			}
		}
		// firstRenamed is, per type, the oldest new version that renames
		// fields: the rows are at the version before it.
		firstRenamed := map[string]int{}
		var order []string
		for _, t := range j.o.Registry.Types() {
			if _, ok := stored[typeKey(t)]; ok {
				continue
			}
			if err := insertType(ctx, tx, t); err != nil {
				return fmt.Errorf("journal: sync types %s: %w", typeKey(t), err)
			}
			if _, seen := firstRenamed[t.Name]; len(t.Renames) > 0 && !seen {
				firstRenamed[t.Name] = t.SchemaVersion
				order = append(order, t.Name)
			}
		}
		for _, name := range order {
			if err := rewriteFields(ctx, tx, j.o.Registry, name, firstRenamed[name]-1); err != nil {
				return fmt.Errorf("journal: sync types %s v%d: %w", name, firstRenamed[name], err)
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

// rewriteFields moves record_current of one type from the field names of
// version from to the active names, through the registry's own Forward
// (so the result never depends on map order and matches how a late snapshot
// is renamed), and recomputes the hash.
func rewriteFields(ctx context.Context, tx *sql.Tx, reg *record.Registry, name string, from int) error {
	active, ok := reg.Active(name)
	if !ok {
		return fmt.Errorf("no active version")
	}
	type row struct {
		subject, key string
		fields       record.Fields
		scan         record.ScanMap
	}
	rows, err := tx.QueryContext(ctx, `SELECT data_subject, key, fields, scan FROM record_current WHERE record_type = $1 ORDER BY data_subject, key FOR UPDATE`, name)
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
		fields, err := record.Reload(active, reg.Forward(name, from, r.fields).JSON())
		if err != nil {
			return err
		}
		scan := record.ScanMap{}
		for k, v := range r.scan {
			scan[reg.ForwardName(name, from, k)] = v
		}
		sj, err := json.Marshal(scan)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE record_current SET fields = $4, scan = $5, content_hash = $6
			WHERE record_type = $1 AND data_subject = $2 AND key = $3`,
			name, r.subject, r.key, fields.JSON(), sj, fields.Hash()); err != nil {
			return err
		}
	}
	return nil
}
