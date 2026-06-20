//go:build unix

package archives

import (
	"os"
	"syscall"
)

// statDev returns the Stat_t.Dev field for path, used by the spec 13 §5.4
// same-filesystem (st_dev) hardening check that archives/ and .tmp-archives/
// share a device so finalize's os.Rename is atomic. Unix implementation; see
// statdev_other.go for the non-Unix stub.
func statDev(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	sys := fi.Sys()
	if sys == nil {
		return 0, nil
	}
	if st, ok := sys.(*syscall.Stat_t); ok {
		return uint64(st.Dev), nil
	}
	return 0, nil
}
