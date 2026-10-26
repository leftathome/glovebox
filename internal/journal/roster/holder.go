package roster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Holder is the roster in use. The file is a mounted ConfigMap (no
// subPath, so the kubelet's updates reach it); Reload swaps atomically,
// and a file that fails to parse leaves the previous roster in place.
type Holder struct {
	path, format string
	cur          atomic.Pointer[Roster]
	mu           sync.Mutex // serialises Reload so an older read never overwrites a newer one
	failures     atomic.Uint64
	failing      atomic.Bool
}

// Failures counts every failed Reload (unreadable or unparsable file).
func (h *Holder) Failures() uint64 { return h.failures.Load() }

// Failing reports whether the latest Reload failed. Every Reload sets it --
// true on an unreadable or unparsable file, false on success or an
// unchanged file -- so, unlike Watch's reports, which name a repeated error
// once, it stays true for as long as the file stays broken. It is the
// source of the glovebox_journal_roster_reload_failing gauge
// (jmetrics.Metrics.SetRosterReloadFailingSource).
func (h *Holder) Failing() bool { return h.failing.Load() }

// NewHolder holds no roster until the first successful Reload.
func NewHolder(path, format string) *Holder {
	return &Holder{path: path, format: format}
}

// Current is the roster in use, or nil before the first successful load.
func (h *Holder) Current() *Roster { return h.cur.Load() }

// Reload reads the file. changed is true when a different roster is now in
// use. On error the previous roster stays.
func (h *Holder) Reload() (changed bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer func() {
		if err != nil {
			h.failures.Add(1)
		}
		h.failing.Store(err != nil)
	}()
	raw, err := os.ReadFile(h.path)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(raw)
	if cur := h.cur.Load(); cur != nil && cur.sha == hex.EncodeToString(sum[:]) {
		return false, nil
	}
	r, err := Parse(h.format, raw)
	if err != nil {
		return false, err
	}
	h.cur.Store(r)
	return true, nil
}

// Watch reloads every interval until ctx ends, reporting each attempt
// except that an error identical to the previous attempt's is not repeated.
// Every failure is still counted (Failures). A non-positive interval is
// refused with no reload.
func (h *Holder) Watch(ctx context.Context, every time.Duration, report func(r *Roster, changed bool, err error)) {
	if every <= 0 {
		report(h.Current(), false, errors.New("roster: watch interval must be positive"))
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var lastErr string
	for {
		changed, err := h.Reload()
		switch {
		case err == nil:
			lastErr = ""
			report(h.Current(), changed, nil)
		case err.Error() != lastErr:
			lastErr = err.Error()
			report(h.Current(), changed, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
