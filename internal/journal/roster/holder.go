package roster

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync/atomic"
	"time"
)

// Holder is the roster in use. The file is a mounted ConfigMap (no
// subPath, so the kubelet's updates reach it); Reload swaps atomically,
// and a file that fails to parse leaves the previous roster in place.
type Holder struct {
	path, format string
	cur          atomic.Pointer[Roster]
}

// NewHolder holds no roster until the first successful Reload.
func NewHolder(path, format string) *Holder {
	return &Holder{path: path, format: format}
}

// Current is the roster in use, or nil before the first successful load.
func (h *Holder) Current() *Roster { return h.cur.Load() }

// Reload reads the file. changed is true when a different roster is now in
// use. On error the previous roster stays.
func (h *Holder) Reload() (changed bool, err error) {
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

// Watch reloads every interval until ctx ends, reporting each attempt.
func (h *Holder) Watch(ctx context.Context, every time.Duration, report func(r *Roster, changed bool, err error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		changed, err := h.Reload()
		report(h.Current(), changed, err)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
