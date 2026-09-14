//go:build windows

package main

import "errors"

// runSandboxRunner has no Windows counterpart: the adapter works around a Linux
// mount-namespace restriction, while Windows confines through the ACL runner.
func runSandboxRunner([]string) error {
	return errors.New("sandbox-runner: the bwrap adapter is Linux-only")
}
