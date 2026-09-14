package main

// Detached DSH process management and readiness detection.
//
// `up` has to survive the shell that started it (people run this over SSH) and
// has to report the two things that only exist after boot: the DSH web URL with
// its access token, and the tunnel URL with its generated credentials. Both are
// read from files the child writes, never scraped from a terminal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// runState is what `status`, `logs` and `down` need, persisted after `up`.
type runState struct {
	Profile    string `json:"profile"`
	PID        int    `json:"pid"`
	PGID       int    `json:"pgid"`
	LogFile    string `json:"logFile"`
	DataDir    string `json:"dataDir"`
	LocalURL   string `json:"localUrl"`
	Token      string `json:"token"`
	RemoteURL  string `json:"remoteUrl"`
	Endpoint   string `json:"endpoint"`
	AuthUser   string `json:"authUser"`
	AuthPass   string `json:"authPass"`
	ExpiresAt  string `json:"expiresAt"`
	AccessFile string `json:"accessFile"`
	StartedAt  string `json:"startedAt"`
	// TunnelNote explains why there is no public URL, when there is none.
	TunnelNote string `json:"tunnelNote,omitempty"`
}

// dshWebLineRE matches the boot line DSH prints with its loopback URL and token.
var dshWebLineRE = regexp.MustCompile(`dsh web:\s*(http://\S+)`)

// tunnelFailureRE matches the plugin's own complaints about auto-exposing.
var tunnelFailureRE = regexp.MustCompile(`\[piko-remote\] (autoExpose failed:[^\n]*|autoExpose ignored:[^\n]*)`)

// accessRecord mirrors the JSON the plugin's credentialsFile holds.
type accessRecord struct {
	Endpoint  string `json:"endpoint"`
	RemoteURL string `json:"remoteUrl"`
	LocalPort int    `json:"localPort"`
	AuthUser  string `json:"authUser"`
	AuthPass  string `json:"authPass"`
	ExpiresAt string `json:"expiresAt"`
}

// parseLocalURL finds the DSH web URL in the log.
func parseLocalURL(logText string) (string, bool) {
	match := dshWebLineRE.FindStringSubmatch(logText)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// tokenFromURL extracts the `token` query parameter.
func tokenFromURL(raw string) string {
	index := strings.Index(raw, "token=")
	if index < 0 {
		return ""
	}
	token := raw[index+len("token="):]
	if cut := strings.IndexAny(token, "&# \n"); cut >= 0 {
		token = token[:cut]
	}
	return token
}

// readAccessFile loads the plugin's credentials file, if it exists yet.
func readAccessFile(path string) (accessRecord, bool) {
	if path == "" {
		return accessRecord{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return accessRecord{}, false
	}
	var record accessRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return accessRecord{}, false
	}
	return record, record.RemoteURL != ""
}

// tunnelNoteFromLog reports the plugin's reason for not exposing, if it gave one.
func tunnelNoteFromLog(logText string) string {
	match := tunnelFailureRE.FindStringSubmatch(logText)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

// processAlive reports whether a pid still exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 performs the permission and existence checks without delivering.
	return process.Signal(syscall.Signal(0)) == nil
}

// startDetached launches dsh in its own session so it outlives this command.
//
// Setsid is what makes the difference over a plain background child: the new
// session has no controlling terminal, so closing the SSH connection that
// started the launcher does not take DSH down with it.
func startDetached(dshPath string, args []string, env []string, logPath, workDir string) (int, int, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return 0, 0, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, 0, err
	}
	defer logFile.Close()

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, 0, err
	}
	defer devNull.Close()

	command := exec.Command(dshPath, args...)
	command.Env = env
	command.Dir = workDir
	command.Stdin = devNull
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := command.Start(); err != nil {
		return 0, 0, err
	}
	pid := command.Process.Pid
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = pid
	}
	// The child is intentionally not waited on: this process exits shortly and
	// the child is reparented, which is exactly the detached behaviour wanted.
	_ = command.Process.Release()
	return pid, pgid, nil
}

// stopProcessGroup asks the whole group to stop, then insists.
//
// The group, not just the pid: DSH spawns piko-expose, and signalling only the
// parent would leave the tunnel helper running and the endpoint routed.
func stopProcessGroup(pid, pgid int, timeout time.Duration) error {
	target := pgid
	if target <= 0 {
		target = pid
	}
	if target <= 0 {
		return errors.New("no recorded process to stop")
	}

	if err := syscall.Kill(-target, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		// Fall back to the single pid when the group is gone but the process is
		// somehow still there.
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := syscall.Kill(-target, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	time.Sleep(200 * time.Millisecond)
	if processAlive(pid) {
		return fmt.Errorf("process %d is still alive after SIGKILL", pid)
	}
	return nil
}

// waitReady polls until DSH is serving and, when a tunnel is expected, until
// the plugin has published its access record.
//
// A tunnel that never appears is reported in state.TunnelNote rather than as a
// failure: the local URL is still useful, and the log line explaining why is
// more actionable than an error the caller has to go digging for.
func waitReady(ctx context.Context, log *logger, state *runState, expectTunnel bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	reportEvery := 5 * time.Second
	lastReport := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		raw, _ := os.ReadFile(state.LogFile)
		text := string(raw)

		if state.LocalURL == "" {
			if url, ok := parseLocalURL(text); ok {
				state.LocalURL = url
				state.Token = tokenFromURL(url)
				log.info("DSH web is up at %s", url)
			}
		}

		if expectTunnel && state.RemoteURL == "" {
			if record, ok := readAccessFile(state.AccessFile); ok {
				state.RemoteURL = record.RemoteURL
				state.Endpoint = record.Endpoint
				state.AuthUser = record.AuthUser
				state.AuthPass = record.AuthPass
				state.ExpiresAt = record.ExpiresAt
				log.info("tunnel is up at %s", record.RemoteURL)
			} else if note := tunnelNoteFromLog(text); note != "" {
				state.TunnelNote = note
				log.warn("tunnel was not exposed: %s", note)
				expectTunnel = false
			}
		}

		if state.LocalURL != "" && (!expectTunnel || state.RemoteURL != "") {
			return nil
		}

		if !processAlive(state.PID) {
			return fmt.Errorf("dsh exited during startup; last log lines:\n%s", tailText(text, 15))
		}

		if time.Now().After(deadline) {
			if state.LocalURL == "" {
				return fmt.Errorf("timed out after %s waiting for the DSH web server; last log lines:\n%s",
					timeout, tailText(text, 15))
			}
			if expectTunnel && state.TunnelNote == "" {
				state.TunnelNote = "timed out waiting for the piko tunnel"
			}
			return nil
		}

		if time.Since(lastReport) >= reportEvery {
			log.info("waiting for DSH to come up…")
			lastReport = time.Now()
		}
		time.Sleep(400 * time.Millisecond)
	}
}

// tailText returns the last n lines of text.
func tailText(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// statePath is where runState is persisted inside the data directory.
func statePath(dataDir string) string {
	return filepath.Join(dataDir, "state.json")
}

// saveState writes the state file, owner-readable only: it holds a live URL,
// an access token and Basic Auth credentials.
func saveState(path string, state runState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// loadState reads a previously saved state file.
func loadState(path string) (runState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return runState{}, err
	}
	var state runState
	if err := json.Unmarshal(raw, &state); err != nil {
		return runState{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return state, nil
}
