//go:build linux

package main

import "syscall"

// prctl(2) options not in package syscall.
const (
	prGetDumpable = 3
	prSetDumpable = 4
)

// hardenProcess marks this process non-dumpable (prctl PR_SET_DUMPABLE 0).
//
// Why it matters here: the account's password arrives in the environment,
// and os.Unsetenv does not change /proc/<pid>/environ, which keeps showing
// the environment the process was started with. A browser the PowerSchool
// driver starts runs as the same user, without Chromium's own sandbox, and
// parses whatever the site sends; code running in it could read
// /proc/<broker pid>/environ and /proc/<broker pid>/mem. For a non-dumpable
// process the kernel gives those files to root and refuses same-user access
// to them (ptrace access checks), so the password is out of that reach.
//
// What this does NOT cover is said in internal/loginbroker/DESIGN.md: files
// the browser's user can read anyway, the ServiceAccount token first among
// them.
//
// The flag is per process and is reset by execve, so the browser itself is
// unaffected.
func hardenProcess() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

// processDumpable reports the current setting (1: dumpable).
func processDumpable() (int, error) {
	v, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prGetDumpable, 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(v), nil
}
