package main

// Sandbox adapter: DSH confines commands by running its platform chain's runner
// (Linux: bubblewrap, then Landlock) with a profile the package owns. A host can
// be unable to run either rung while still running bwrap fine under a slightly
// narrower profile — an LXC container, for example, refuses to mount a fresh
// /proc inside an unprivileged user namespace, which is exactly the
// `--unshare-pid --proc /proc` pair DSH's profile carries.
//
// The `sandbox` row accepts an operator-supplied `runnerCommand`, and DSH hands
// it the same bwrap profile arguments, then `--`, then the command. That makes a
// filter-and-exec adapter a *supported* configuration rather than a patched
// package: the file-effect policy (`--ro-bind / /`, `--dev /dev`, `--tmpfs
// /tmp`, `--bind <workspace> <workspace>`) passes through untouched and only the
// PID-namespace pair is dropped.
//
// What the operator gives up is PID isolation, not file confinement: commands
// still cannot write outside the workspace root and the sandbox's own temp
// directory, which is the guarantee DSH's prompt states. A sandboxed command
// runs as the same user either way, so it could already signal the DSH process.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// sandboxRunnerSubcommand is this launcher's own argv[1] when DSH invokes it as
// a sandbox runner.
const sandboxRunnerSubcommand = "sandbox-runner"

// sandboxRunnerModes are the accepted --sandbox-runner values.
const (
	sandboxRunnerNative      = "native"
	sandboxRunnerAuto        = "auto"
	sandboxRunnerBwrapNoProc = "bwrap-noproc"
)

// sandboxRunner is the `sandbox` row config that routes confinement through the
// adapter.
type sandboxRunner struct {
	Command           []string `yaml:"runnerCommand"`
	FailureSignatures []string `yaml:"runnerFailureSignatures"`
}

// bwrapProbeProfile is the profile DSH probes with, and the one the adapter
// filters. It is the read-only policy (the workspace-write policy adds
// `--tmpfs /tmp` and a writable bind of the workspace root).
func bwrapProbeProfile() []string {
	return []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
	}
}

// filterBwrapProfileArgs drops the PID-namespace pair from a bwrap profile,
// leaving everything else — including the `--` separator and the command — in
// order. Tokens after `--` belong to the command and are never touched.
func filterBwrapProfileArgs(argv []string) []string {
	filtered := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		switch {
		case argv[i] == "--":
			return append(filtered, argv[i:]...)
		case argv[i] == "--unshare-pid":
			// Dropped: the host refuses the namespace, not the profile.
		case argv[i] == "--proc" && i+1 < len(argv) && argv[i+1] == "/proc":
			i++
		default:
			filtered = append(filtered, argv[i])
		}
	}
	return filtered
}

// bwrapUsable reports whether bwrap can run profile on this host.
func bwrapUsable(profile []string, timeout time.Duration) bool {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := append(append([]string{}, profile...), "--", "true")
	command := exec.CommandContext(ctx, path, args...)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run() == nil
}

// resolveSandboxRunner turns --sandbox-runner into the config the overlay needs,
// or nil to leave DSH's own chain in charge.
func resolveSandboxRunner(log *logger, mode string, probeTimeout time.Duration) (*sandboxRunner, error) {
	switch mode {
	case "", sandboxRunnerNative:
		return nil, nil

	case sandboxRunnerBwrapNoProc:
		runner, err := adapterRunner()
		if err != nil {
			return nil, err
		}
		if _, err := exec.LookPath("bwrap"); err != nil {
			return nil, fmt.Errorf("--sandbox-runner %s needs bwrap on PATH: %w", mode, err)
		}
		log.warn("--sandbox-runner %s: commands keep the workspace file policy but lose PID isolation", mode)
		return runner, nil

	case sandboxRunnerAuto:
		if bwrapUsable(bwrapProbeProfile(), probeTimeout) {
			log.info("sandbox: bwrap runs DSH's full profile here; keeping the built-in chain")
			return nil, nil
		}
		if !bwrapUsable(filterBwrapProfileArgs(bwrapProbeProfile()), probeTimeout) {
			log.warn("sandbox: no usable bwrap profile on this host; keeping DSH's chain (workspace-write fails closed)")
			return nil, nil
		}
		runner, err := adapterRunner()
		if err != nil {
			return nil, err
		}
		log.warn("sandbox: bwrap cannot mount a fresh /proc in a user namespace here; using the adapter — workspace file policy kept, PID isolation dropped")
		return runner, nil

	default:
		return nil, fmt.Errorf("unknown --sandbox-runner %q (want %s, %s, or %s)",
			mode, sandboxRunnerNative, sandboxRunnerAuto, sandboxRunnerBwrapNoProc)
	}
}

// adapterRunner points the `sandbox` row at this launcher binary, which then
// execs bwrap with the filtered profile.
func adapterRunner() (*sandboxRunner, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve the launcher path for the sandbox adapter: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	return &sandboxRunner{
		Command: []string{executable, sandboxRunnerSubcommand},
		// bwrap's own fatal diagnostics; DSH classifies a command as a runner
		// failure rather than a denial on these.
		FailureSignatures: []string{"bwrap: "},
	}, nil
}
