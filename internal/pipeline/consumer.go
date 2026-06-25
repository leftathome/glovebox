package pipeline

import (
	"errors"
	"fmt"
	"time"
)

// ErrDeliveryTimeout is returned by a WithTimeout-wrapped DeliveryFunc when the
// underlying delivery does not complete within the configured budget.
var ErrDeliveryTimeout = errors.New("delivery timed out")

// ConsumeResults reads scan responses from output and routes them, flushing
// accumulated ordered items every flushInterval rather than only at shutdown
// (glovebox-v815: Router.Flush was previously SIGTERM-only, so ordered items
// were held in memory and never delivered in a long-running scanner).
//
// Route and Flush are both driven from this single goroutine, so the Router's
// pending map is never accessed concurrently. ConsumeResults returns when
// output is closed (draining any buffered responses first); the caller should
// still call Router.Flush once afterwards to deliver the final batch. A
// flushInterval <= 0 disables periodic flushing.
func ConsumeResults(output <-chan ScanResponse, router *Router, flushInterval time.Duration, onError func(error)) {
	report := func(err error) {
		if err != nil && onError != nil {
			onError(err)
		}
	}

	var tickC <-chan time.Time
	if flushInterval > 0 {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		tickC = t.C
	}

	for {
		select {
		case resp, ok := <-output:
			if !ok {
				return
			}
			report(router.Route(resp))
		case <-tickC:
			report(router.Flush())
		}
	}
}

// WithTimeout wraps a DeliveryFunc so a single delivery cannot block the result
// consumer indefinitely (glovebox-lnzp: a wedged file op on a networked staging
// mount would otherwise stall the lone consumer goroutine, back-pressuring the
// whole scan pipeline into a deadlock). On timeout it invokes onTimeout (for
// metrics/logging) and returns ErrDeliveryTimeout so the consumer logs it and
// moves on; the orphaned delivery goroutine is consistent with the worker
// scan-timeout pattern (it completes if/when the op unwedges). A timeout <= 0
// returns the delivery unchanged.
func WithTimeout(deliver DeliveryFunc, timeout time.Duration, onTimeout func(ScanResponse)) DeliveryFunc {
	if timeout <= 0 {
		return deliver
	}
	return func(resp ScanResponse) error {
		done := make(chan error, 1) // buffered so the goroutine never leaks on the send
		go func() { done <- deliver(resp) }()

		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case err := <-done:
			return err
		case <-timer.C:
			if onTimeout != nil {
				onTimeout(resp)
			}
			return fmt.Errorf("%w after %s: %s", ErrDeliveryTimeout, timeout, resp.Item.DirPath)
		}
	}
}
