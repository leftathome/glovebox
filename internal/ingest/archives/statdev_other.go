//go:build !unix

package archives

// statDev returns 0 on non-Unix platforms. The same-filesystem (st_dev) check
// is a Linux production hardening, not a portability concern, so the device
// comparison always passes off-Unix (both sides report 0). See statdev_unix.go
// for the real implementation.
func statDev(_ string) (uint64, error) {
	return 0, nil
}
