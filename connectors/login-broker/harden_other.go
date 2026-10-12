//go:build !linux

package main

import "errors"

// hardenProcess is Linux-only (see harden_linux.go). The service runs in a
// Linux container; elsewhere the binary is built for tests and tooling.
func hardenProcess() error { return errors.New("not supported on this platform") }

func processDumpable() (int, error) { return -1, errors.New("not supported on this platform") }
