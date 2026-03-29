package pipeline

import (
	"path/filepath"
	"sort"
)

type DeliveryFunc func(resp ScanResponse) error

// Router receives scan responses and delivers them. Unordered items are
// delivered immediately. Ordered items are accumulated and delivered in
// FIFO order (by directory name) when Flush is called.
type Router struct {
	deliver DeliveryFunc
	pending map[string][]ScanResponse
}

func NewRouter(deliver DeliveryFunc) *Router {
	return &Router{
		deliver: deliver,
		pending: make(map[string][]ScanResponse),
	}
}

func (r *Router) Route(resp ScanResponse) error {
	if !resp.Item.Metadata.Ordered {
		return r.deliver(resp)
	}

	dest := resp.Item.Metadata.DestinationAgent
	r.pending[dest] = append(r.pending[dest], resp)
	return nil
}

// Flush delivers all accumulated ordered items in FIFO order per destination.
func (r *Router) Flush() error {
	for dest, items := range r.pending {
		sort.Slice(items, func(i, j int) bool {
			return filepath.Base(items[i].Item.DirPath) < filepath.Base(items[j].Item.DirPath)
		})
		for _, resp := range items {
			if err := r.deliver(resp); err != nil {
				return err
			}
		}
		delete(r.pending, dest)
	}
	return nil
}
