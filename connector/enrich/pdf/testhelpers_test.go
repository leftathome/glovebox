package pdf

import (
	"context"
	"testing"
)

// testCtx returns a context that is cancelled when the test ends. Tests
// that need a deadline should derive from this.
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
