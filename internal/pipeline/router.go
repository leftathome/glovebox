package pipeline

import (
	"path/filepath"
	"sort"
)

type DeliveryFunc func(resp ScanResponse) error

// Router receives scan responses and delivers them. Unordered items are
// delivered immediately. Ordered items are accumulated and delivered in
// sorted order (by directory name, which is timestamp-prefixed) when
// Flush is called.
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

// Flush delivers all accumulated ordered items in sorted order per destination.
func (r *Router) Flush() error {
	for _, items := range r.pending {
		// Pre-compute sort keys to avoid filepath.Base in comparator
		type keyed struct {
			key  string
			resp ScanResponse
		}
		sorted := make([]keyed, len(items))
		for i, resp := range items {
			sorted[i] = keyed{key: filepath.Base(resp.Item.DirPath), resp: resp}
		}
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].key < sorted[j].key
		})

		for _, k := range sorted {
			if err := r.deliver(k.resp); err != nil {
				return err
			}
		}
	}
	r.pending = make(map[string][]ScanResponse)
	return nil
}
