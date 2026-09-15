package main

// Instance bookkeeping for one machine.
//
// DSH keeps sessions in `$DSH_HOME/sessions`, and a session is owned by the
// process that has it open. Two instances sharing one DSH home therefore fight
// over the same sessions — and the loser does not merely lose a race: the Web
// client's command directory asks the host to resume the current session, the
// resume fails with `SessionAlreadyOwnedError`, and the slash-command catalog
// never loads. The visible symptom is that `/` shows nothing and `/compact`
// "fails", with no server-side trace at all because no turn ever starts.
//
// That is why `up` treats the profile as its own: it stops a previous run of
// the same profile before starting, and warns about other profiles that share
// the home.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// liveDshProcesses lists running dsh processes, filtered to one profile when
// `profile` is non-empty. PIDs are returned in `ps` order.
//
// `ps` is used rather than reading /proc so the same code works on macOS, where
// the launcher is also a first-class target.
func liveDshProcesses(profile string) []int {
	output, err := exec.Command("ps", "-eo", "pid=,args=").Output()
	if err != nil {
		return nil
	}
	return parseDshProcesses(string(output), profile, os.Getpid())
}

// parseDshProcesses is the pure half of {@link liveDshProcesses}, so the
// matching rules can be tested without a process table.
//
// A row counts only when dsh is the program being run — `argv[0]` or `argv[1]`
// must be named `dsh`, which is the shape of the launcher's own start
// (`<node> <dsh> --profile …`) and of a directly executed dsh binary.
//
// Matching on the `--profile` flag alone is not enough, and neither is a
// substring test for "dsh": the shell that invokes the launcher carries both
// `--profile piko` and the dsh path in its own argv, so a looser rule makes the
// launcher stop its own caller.
//
// @param psOutput - `ps -eo pid=,args=` output.
// @param profile - profile to match; empty matches every dsh instance.
// @param self - this process's pid, never reported.
// @returns matching pids.
func parseDshProcesses(psOutput, profile string, self int) []int {
	var pids []int
	for _, line := range strings.Split(psOutput, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == self {
			continue
		}
		args := strings.Join(fields[1:], " ")
		name, ok := dshProfileOf(args)
		if !ok {
			continue
		}
		if profile != "" && name != profile {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// dshProfileOf reports whether one argv is a dsh CLI invocation and which
// profile it names.
//
// @param args - the process's argv, space-joined.
// @returns the profile name and whether the row is a dsh invocation.
func dshProfileOf(args string) (string, bool) {
	tokens := strings.Fields(args)
	if len(tokens) < 2 {
		return "", false
	}

	// `<node> <dsh> …` (the launcher's shape) or `<dsh> …` (a binary entry).
	entry := 0
	switch {
	case filepath.Base(tokens[0]) == "dsh":
	case filepath.Base(tokens[1]) == "dsh":
		entry = 1
	default:
		return "", false
	}

	for index := entry + 1; index < len(tokens); index++ {
		if tokens[index] == "--profile" && index+1 < len(tokens) {
			return tokens[index+1], true
		}
		if value, found := strings.CutPrefix(tokens[index], "--profile="); found {
			return value, true
		}
	}
	return "", false
}

// stopPreviousRuns makes this machine single-instance for one profile.
//
// @param log - logger for the operator-facing explanation.
// @param profile - the profile this run is about to start.
// @returns the pids that were asked to stop.
func stopPreviousRuns(log *logger, profile string) []int {
	previous := liveDshProcesses(profile)
	for _, pid := range previous {
		log.warn("stopping the previous dsh for profile %q (pid %d); two instances on one DSH home break session ownership", profile, pid)
		if err := stopProcessGroup(pid, pid, 5*time.Second); err != nil {
			log.warn("could not stop pid %d: %v", pid, err)
		}
	}

	// Other profiles on the same home are the same hazard, but they may be
	// deliberate (a second profile is a legitimate way to run another surface),
	// so they are reported rather than stopped.
	if others := liveDshProcesses(""); len(others) > 0 {
		log.warn(
			"%d other dsh instance(s) still run on this machine (%v); if they share DSH home %s, the sessions they hold cannot be resumed here and the UI's command menu will fail",
			len(others), others, dshHomeOrUnknown(),
		)
	}
	return previous
}

// dshHomeOrUnknown resolves the DSH home for a message, tolerating failure.
func dshHomeOrUnknown() string {
	home, err := dshHome()
	if err != nil {
		return "(unknown)"
	}
	return home
}
