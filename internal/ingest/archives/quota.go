// Package archives -- storage quota measurement (spec 13 §5.4).
//
// This file owns the background storage-measurement loop and the
// QuotaState type that the C2 handler reads via its QuotaProvider
// interface. The goroutine ticks every `interval`, sums archives/ +
// .tmp-archives/ bytes, publishes the totals to the Telemetry gauges,
// and toggles the hardCapBlock flag with hysteresis: trip at
// >= hardTripPct (default 95%), lift at <= hardLiftPct (default 85%).
//
// Threading model: every field of QuotaState is updated and read via
// sync/atomic primitives. ShouldBlock is wait-free; the handler hot
// path never blocks on the measurement goroutine. The hysteresis
// transition uses a small mutex only to serialize the trip/lift
// decision against itself, never against the readers.
package archives

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// QuotaState is the in-memory representation of the storage-quota
// computation. The handler's QuotaProvider interface is satisfied by
// *QuotaState; the C3 background goroutine updates the fields on a tick.
//
// All four fields are read concurrently with their writers; pct and
// perSource are swapped via atomic.Pointer, hardCapBlock is a plain
// atomic.Bool, and the mu mutex serializes hysteresis transitions
// against themselves so a CAS-loop-style trip/lift never races itself.
type QuotaState struct {
	// pct is the most recent (archives/ + .tmp-archives/) / capacity
	// reading, expressed as a percentage in the 0..100 range. Stored
	// as a heap-allocated float64 pointer so atomic.Pointer can swap
	// values without tearing.
	pct atomic.Pointer[float64]

	// hardCapBlock is true when the spec §5.4 503 hard cap is in
	// effect. Set when pct crosses hardTripPct upward, cleared when
	// pct crosses hardLiftPct downward (hysteresis).
	hardCapBlock atomic.Bool

	// perSource maps source-id -> bytes attributable to that source.
	// Stored as a heap-allocated map pointer so reads return a stable
	// snapshot even while the writer is assembling the next one.
	perSource atomic.Pointer[map[string]int64]

	// mu serializes hysteresis transitions (trip vs lift) so two
	// closely-spaced measurements can't simultaneously flip the bool.
	// Readers never acquire this lock; only the goroutine's transition
	// step does, and the critical section is two atomic loads + at
	// most one atomic store.
	mu sync.Mutex
}

// NewQuotaState returns a zeroed QuotaState ready to be passed to
// StartQuotaGoroutine and to the handler as a QuotaProvider. Before
// the first measurement tick, ShouldBlock returns false and Pct
// returns 0.
func NewQuotaState() *QuotaState {
	q := &QuotaState{}
	zero := 0.0
	q.pct.Store(&zero)
	empty := map[string]int64{}
	q.perSource.Store(&empty)
	return q
}

// ShouldBlock satisfies the handler's QuotaProvider interface. Returns
// true when the spec §5.4 hard cap is currently tripped (95% by
// default; lifts at 85%).
func (q *QuotaState) ShouldBlock() bool {
	return q.hardCapBlock.Load()
}

// Pct returns the most recent measurement as a percentage in the 0..100
// range. Returns 0 before the first measurement.
func (q *QuotaState) Pct() float64 {
	p := q.pct.Load()
	if p == nil {
		return 0
	}
	return *p
}

// PerSourceBytes returns a copy of the most recent per-source-id usage
// map. The returned map is independent of the internal pointer; callers
// may mutate it without affecting subsequent reads.
func (q *QuotaState) PerSourceBytes() map[string]int64 {
	m := q.perSource.Load()
	if m == nil {
		return nil
	}
	out := make(map[string]int64, len(*m))
	for k, v := range *m {
		out[k] = v
	}
	return out
}

// QuotaConfig parameterizes StartQuotaGoroutine. Zero values pick
// reasonable production defaults per spec 13 §5.4.
type QuotaConfig struct {
	// StagingRoot is the absolute path under which archives/ and
	// .tmp-archives/ live.
	StagingRoot string

	// CapacityBytes is the PVC's total capacity in bytes. The
	// percentage gauge divides measured bytes by this value. A zero
	// or negative value causes the percentage to be reported as 0.0
	// (no division by zero); the hardCapBlock cannot trip in that
	// case.
	CapacityBytes int64

	// Interval is the measurement tick interval (default 60s per
	// spec §5.4).
	Interval time.Duration

	// HardTripPct is the upper hysteresis bound (default 95.0). When
	// the measured pct crosses this threshold upward and the bool is
	// currently false, the trip fires.
	HardTripPct float64

	// HardLiftPct is the lower hysteresis bound (default 85.0). When
	// the measured pct crosses this threshold downward and the bool
	// is currently true, the lift fires.
	HardLiftPct float64

	// Telemetry is the gauge-publication target. May be nil; nil-safe
	// methods on *Telemetry no-op.
	Telemetry *Telemetry

	// Logger is the goroutine's slog. Defaults to slog.Default()
	// when nil.
	Logger *slog.Logger
}

// StartQuotaGoroutine runs the storage-measurement loop until ctx is
// cancelled. The function blocks; callers MUST invoke it via `go`.
// One measurement runs synchronously before the first tick so the
// state isn't all-zeros when the handler first calls ShouldBlock.
//
// Per spec 13 §5.4: pct = (archives/ + .tmp-archives/) / capacity * 100.
// The hardCapBlock flag trips at >= HardTripPct and lifts at
// <= HardLiftPct (default 95 / 85).
func StartQuotaGoroutine(ctx context.Context, cfg QuotaConfig, q *QuotaState) {
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.HardTripPct <= 0 {
		cfg.HardTripPct = 95.0
	}
	if cfg.HardLiftPct <= 0 {
		cfg.HardLiftPct = 85.0
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	measureOnce(ctx, cfg, q)

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			measureOnce(ctx, cfg, q)
		}
	}
}

// measureOnce performs one measurement pass: sums bytes, updates the
// QuotaState atomics, publishes to Telemetry, and runs the hysteresis
// transition. Extracted so the startup-once call and the tick path
// share one implementation. Defaults the logger when nil so direct
// callers (tests) don't have to remember to set it.
func measureOnce(ctx context.Context, cfg QuotaConfig, q *QuotaState) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	archivesRoot := filepath.Join(cfg.StagingRoot, "archives")
	tmpRoot := filepath.Join(cfg.StagingRoot, ".tmp-archives")

	archBytes := dirBytes(archivesRoot)
	tmpBytesN := dirBytes(tmpRoot)
	total := archBytes + tmpBytesN

	var pct float64
	if cfg.CapacityBytes > 0 {
		pct = float64(total) / float64(cfg.CapacityBytes) * 100.0
	}
	q.pct.Store(&pct)

	cfg.Telemetry.SetStorageBytes(ctx, total)
	cfg.Telemetry.SetStoragePct(ctx, pct)

	perSrc := perSourceBytes(archivesRoot)
	q.perSource.Store(&perSrc)
	for sid, n := range perSrc {
		cfg.Telemetry.SetStorageSourceBytes(ctx, sid, n)
	}

	// Hysteresis transition. The mutex prevents two ticks running
	// near-simultaneously (e.g. from a manual measureOnce call in
	// tests) from racing the trip/lift decision; readers of
	// hardCapBlock never acquire it.
	q.mu.Lock()
	defer q.mu.Unlock()
	currentlyBlocking := q.hardCapBlock.Load()
	switch {
	case !currentlyBlocking && pct >= cfg.HardTripPct:
		q.hardCapBlock.Store(true)
		cfg.Logger.Error(EventStorageHardCap,
			"used_bytes", total,
			"capacity_bytes", cfg.CapacityBytes,
			"pct", pct,
			"hard_trip_pct", cfg.HardTripPct)
	case currentlyBlocking && pct <= cfg.HardLiftPct:
		q.hardCapBlock.Store(false)
		cfg.Logger.Info(EventStorageNearCap,
			"used_bytes", total,
			"capacity_bytes", cfg.CapacityBytes,
			"pct", pct,
			"hard_lift_pct", cfg.HardLiftPct,
			"transition", "lift")
	}
}

// dirBytes recursively sums regular-file sizes under root. Errors are
// silently ignored on a per-entry basis: a permission error on one
// subdirectory should not tank the whole measurement, and the goroutine
// runs every minute so transient ENOENT (a tmp file deleted mid-walk by
// finalize) is self-healing on the next tick.
//
// If root itself doesn't exist, returns 0 without logging.
func dirBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

// perSourceBytes attributes archives/<id>/ usage to source-id via the
// finalized metadata.json's delivered_by field. Entries under
// archives/.done/ or any other dot-prefixed subdir are skipped (those
// are operator/retention dirs, not active archives bound to a source).
//
// .tmp-archives/ entries don't have a finalized metadata.json so they
// are NOT attributed here; they show up in the total via dirBytes
// (called from measureOnce) but not in the per-source breakdown. A
// future revision could read the in-memory upload-id table to map
// tmp files to source-ids, but spec §5.4 only requires per-source
// attribution for finalized archives.
func perSourceBytes(archivesRoot string) map[string]int64 {
	out := map[string]int64{}
	entries, err := os.ReadDir(archivesRoot)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip dot-prefixed entries (e.g. .done/, .tmp/, .lock).
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sub := filepath.Join(archivesRoot, e.Name())
		metaBytes, rerr := os.ReadFile(filepath.Join(sub, "metadata.json"))
		if rerr != nil {
			continue
		}
		var meta struct {
			DeliveredBy string `json:"delivered_by"`
		}
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			continue
		}
		sid := meta.DeliveredBy
		if sid == "" {
			sid = "unknown"
		}
		out[sid] += dirBytes(sub)
	}
	return out
}
