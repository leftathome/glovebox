//go:build unix

package main

import "syscall"

// stagingCapacityBytes returns the total bytes of the filesystem hosting
// stagingRoot, used by the spec 13 §5.4 quota gauge. On any Statfs error
// (missing mountpoint at boot) we return 0 -- the quota goroutine treats zero
// as "unknown capacity" and skips the percentage trip but still records the
// raw gauge. Unix implementation; see statfs_other.go for the non-Unix stub.
func stagingCapacityBytes(stagingRoot string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(stagingRoot, &st); err != nil {
		return 0
	}
	// Bsize * Blocks = total bytes on the filesystem; Bavail would give the
	// user-available subset, but the spec 13 gauge is "fraction of the PVC
	// consumed" so Blocks is the right denominator.
	return int64(st.Bsize) * int64(st.Blocks)
}
