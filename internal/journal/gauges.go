package journal

import (
	"context"
	"database/sql"
	"time"
)

// Gauges reads what two gauges need: held snapshots, and the newest
// received_at under each configured scope prefix (decision 8). A prefix
// with no snapshot is absent from the map.
func (j *Journal) Gauges(ctx context.Context, prefixes []string) (int64, map[string]time.Time, error) {
	var held int64
	if err := j.db.QueryRowContext(ctx, `SELECT count(*) FROM snapshot WHERE status = 'held'`).Scan(&held); err != nil {
		return 0, nil, err
	}
	last := map[string]time.Time{}
	for _, p := range prefixes {
		var t sql.NullTime
		if err := j.db.QueryRowContext(ctx, `SELECT max(received_at) FROM snapshot WHERE starts_with(scope, $1)`, p).Scan(&t); err != nil {
			return 0, nil, err
		}
		if t.Valid {
			last[p] = t.Time
		}
	}
	return held, last, nil
}
