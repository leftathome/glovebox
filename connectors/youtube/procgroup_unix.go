//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// setProcessGroupKill starts cmd in its own process group and, when its
// context is cancelled, kills the whole group: yt-dlp may leave children
// holding the output pipe.
func setProcessGroupKill(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
