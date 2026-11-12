//go:build !unix

package main

import "os/exec"

// setProcessGroupKill on platforms without POSIX process groups (Windows)
// keeps exec's default: cancelling the context kills the process itself.
// cmd.WaitDelay still bounds the wait for any children holding the pipes.
// The connector ships as a Linux image; this exists so the module
// cross-compiles for the release binaries.
func setProcessGroupKill(cmd *exec.Cmd) {}
