//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// runSandboxRunner re-execs bwrap with DSH's profile minus the PID-namespace
// pair. exec replaces this process, so bwrap stays DSH's direct child: no
// wrapper to orphan on a timeout, and DSH's teardown reaches the command it
// started.
func runSandboxRunner(argv []string) error {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return fmt.Errorf("sandbox-runner: bwrap is not on PATH: %w", err)
	}
	execArgv := append([]string{"bwrap"}, filterBwrapProfileArgs(argv)...)
	if err := syscall.Exec(bwrap, execArgv, os.Environ()); err != nil {
		return fmt.Errorf("sandbox-runner: exec %s: %w", bwrap, err)
	}
	return nil
}
