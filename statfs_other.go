//go:build !unix

package main

// stagingCapacityBytes returns 0 on non-Unix platforms. The spec 13 §5.4 quota
// gauge is a Linux production concern; 0 is treated as "unknown capacity" by
// the quota goroutine (the percentage trip is skipped). See statfs_unix.go for
// the real implementation.
func stagingCapacityBytes(_ string) int64 {
	return 0
}
