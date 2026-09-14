// Command dsh-piko-remote is the one-shot launcher for this plugin.
//
// It is to DSH what opencode-piko-remote is to opencode: one command that makes
// sure the runtime exists, installs the plugin into a DSH profile, boots the web
// UI with the tunnel configuration applied, and prints the public URL with the
// credentials it generated.
//
//	dsh-piko-remote up                                  # the default
//	dsh-piko-remote up --plugin someone/their-plugin     # any plugin, GitHub shorthand
//	dsh-piko-remote status | logs | down
//
// Everything it installs goes into its own data directory (Node, npm globals) or
// into a dedicated DSH profile; it never edits the user's own profile patches.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is the launcher's own version.
const version = "0.1.0"

// defaultDshVersion is pinned rather than `latest`: the plugin declares its peer
// range against this line, and "it worked yesterday" is worth more than new.
const defaultDshVersion = "0.1.5-rc.1"

// defaultRemotePlugin is what `up` installs when no --plugin is given.
const defaultRemotePlugin = "github:friddle/dsh-plugin-piko-remote"

// defaultTTLMinutes keeps an exposed DSH UI from living forever by accident.
const defaultTTLMinutes = 480

// defaultRemote is the public gotty-piko server.
const defaultRemote = "https://clauded.friddle.me"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "dsh-piko-remote: %v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	// Global help before command dispatch, so `--help` shows this tool's usage
	// rather than the flag package's dump for whichever subcommand it guessed.
	if len(argv) > 0 {
		switch argv[0] {
		case "-h", "--help", "-help", "help":
			usage(os.Stdout)
			return nil
		}
	}

	command := "up"
	if len(argv) > 0 && !strings.HasPrefix(argv[0], "-") {
		command = argv[0]
		argv = argv[1:]
	}

	switch command {
	case "up":
		return cmdUp(argv)
	case "down":
		return cmdDown(argv)
	case "status":
		return cmdStatus(argv)
	case "logs":
		return cmdLogs(argv)
	case "version", "-V", "--version":
		fmt.Println("dsh-piko-remote " + version)
		return nil
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage(out io.Writer) {
	fmt.Fprint(out, `dsh-piko-remote — install and boot DSH with the piko-remote tunnel

usage:
  dsh-piko-remote [up] [flags]     install everything and start (default command)
  dsh-piko-remote status [flags]   show the running instance and its URLs
  dsh-piko-remote logs [flags]     print the tail of the DSH log
  dsh-piko-remote down [flags]     stop DSH and close the tunnel

up flags:
  --plugin SPEC         plugin to install, repeatable (default `+defaultRemotePlugin+`)
                        owner/repo            -> github:owner/repo
                        @scope/pkg[@version]  -> npm
                        ./dir | /abs/dir      -> local
                        https://host/x.tgz    -> url
  --profile NAME        DSH profile to create/use (default `+defaultProfileName+`)
  --remote URL          piko server (default `+defaultRemote+`)
  --endpoint NAME       fixed endpoint name; default: random per boot
  --ttl MINUTES         tunnel lifetime (default `+fmt.Sprint(defaultTTLMinutes)+`; 0 = never expire)
  --basic-auth          keep Basic Auth on the tunnel (default true)
  --expose-dsh-ui       allow exposing the DSH Web UI itself (default true; needs Basic Auth)
  --port PORT           DSH web port (default 0: the OS picks one, which avoids
                        colliding with another DSH already on this machine)
  --credentials-file F  where the tunnel access record is written
  --dsh PATH            use this dsh binary instead of installing one
  --dsh-version V       dsh version to install (default `+defaultDshVersion+`)
  --dsh-arg ARG         extra argument for dsh, repeatable
  --env KEY=VALUE       environment entry for dsh, repeatable (e.g. the model key)
  --auth-user USER      fixed tunnel Basic Auth user (default: random)
  --auth-pass PASS      fixed tunnel Basic Auth password (default: random)
  --node PATH           use this node instead of installing one
  --node-version V      Node version to install when needed (default `+defaultNodeVersion+`)
  --registry URL        npm registry for installs
  --data-dir DIR        launcher state (default: XDG data dir)
  --dsh-home DIR        DSH home (default $DSH_HOME or ~/.dsh)
  --timeout SECONDS     how long to wait for boot (default 180)
  --no-sandbox          start new sessions with the danger-full-access policy:
                        commands run unwrapped and approvals are off. Use it on
                        a host whose sandbox has no usable backend; the
                        per-session permission picker still overrides it
  --force               reinstall node/dsh even when a usable one exists
  --json                print one JSON result object on stdout
`)
}

// logger writes progress to stderr in JSON mode so stdout stays one object.
//
// The mutex is a pointer so the struct can be copied by value into helpers
// without tripping the copylocks vet check.
type logger struct {
	mu   *sync.Mutex
	out  io.Writer
	json bool
}

func newLogger(jsonMode bool) *logger {
	out := io.Writer(os.Stdout)
	if jsonMode {
		out = os.Stderr
	}
	return &logger{mu: &sync.Mutex{}, out: out, json: jsonMode}
}

func (l *logger) write(prefix, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(l.out, prefix+format+"\n", args...)
}

func (l *logger) info(format string, args ...any)  { l.write("• ", format, args...) }
func (l *logger) warn(format string, args ...any)  { l.write("! ", format, args...) }
func (l *logger) plain(format string, args ...any) { l.write("", format, args...) }

// commandRunner runs one external command and returns its combined output.
//
// It exists as an interface so the install orchestration can be tested against
// a fake dsh and pnpm, without a network or a Node toolchain.
type commandRunner interface {
	run(ctx context.Context, log *logger, name string, args ...string) (string, error)
}

// execRunner runs commands with a fixed environment.
type execRunner struct {
	env []string
	dir string
}

func (r execRunner) run(ctx context.Context, log *logger, name string, args ...string) (string, error) {
	log.info("$ %s %s", name, strings.Join(args, " "))
	command := exec.CommandContext(ctx, name, args...)
	command.Env = r.env
	if r.dir != "" {
		command.Dir = r.dir
	}
	output, err := command.CombinedOutput()
	text := string(output)
	if err != nil {
		return text, fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, tailText(text, 10))
	}
	return text, nil
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(value string) error {
	*m = append(*m, value)
	return nil
}

// upOptions is the parsed `up` command line.
type upOptions struct {
	plugins     multiFlag
	dshArgs     multiFlag
	childEnv    multiFlag
	authUser    string
	authPass    string
	profile     string
	remote      string
	endpoint    string
	ttlMinutes  int
	basicAuth   bool
	exposeDshUI bool
	port        int
	credentials string
	dshPath     string
	dshVersion  string
	nodePath    string
	nodeVersion string
	registry    string
	dataDir     string
	dshHome     string
	timeout     time.Duration
	force       bool
	noSandbox   bool
	jsonOut     bool
}

// defaultDataDir follows the XDG convention, falling back to ~/.local/share.
func defaultDataDir() string {
	if value := os.Getenv("XDG_DATA_HOME"); value != "" {
		return filepath.Join(value, "dsh-piko-remote")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "dsh-piko-remote")
	}
	return filepath.Join(home, ".local", "share", "dsh-piko-remote")
}

func parseUpFlags(argv []string) (*upOptions, error) {
	opts := &upOptions{}
	flags := flag.NewFlagSet("up", flag.ContinueOnError)
	flags.Usage = func() { usage(flags.Output()) }
	flags.Var(&opts.plugins, "plugin", "plugin to install (repeatable)")
	flags.Var(&opts.dshArgs, "dsh-arg", "extra dsh argument (repeatable)")
	flags.Var(&opts.childEnv, "env", "environment entry KEY=VALUE for dsh (repeatable)")
	flags.StringVar(&opts.authUser, "auth-user", "", "fixed tunnel Basic Auth user")
	flags.StringVar(&opts.authPass, "auth-pass", "", "fixed tunnel Basic Auth password")
	flags.StringVar(&opts.profile, "profile", defaultProfileName, "DSH profile name")
	flags.StringVar(&opts.remote, "remote", defaultRemote, "piko server URL")
	flags.StringVar(&opts.endpoint, "endpoint", "", "fixed endpoint name")
	flags.IntVar(&opts.ttlMinutes, "ttl", defaultTTLMinutes, "tunnel lifetime in minutes")
	flags.BoolVar(&opts.basicAuth, "basic-auth", true, "require Basic Auth on the tunnel")
	flags.BoolVar(&opts.exposeDshUI, "expose-dsh-ui", true, "allow exposing the DSH Web UI")
	flags.IntVar(&opts.port, "port", 0, "DSH web port (0 = let the OS pick)")
	flags.StringVar(&opts.credentials, "credentials-file", "", "tunnel access record path")
	flags.StringVar(&opts.dshPath, "dsh", "", "existing dsh binary")
	flags.StringVar(&opts.dshVersion, "dsh-version", defaultDshVersion, "dsh version to install")
	flags.StringVar(&opts.nodePath, "node", "", "existing node binary")
	flags.StringVar(&opts.nodeVersion, "node-version", defaultNodeVersion, "Node version to install")
	flags.StringVar(&opts.registry, "registry", "", "npm registry")
	flags.StringVar(&opts.dataDir, "data-dir", "", "launcher data directory")
	flags.StringVar(&opts.dshHome, "dsh-home", "", "DSH home directory")
	seconds := flags.Int("timeout", 180, "boot timeout in seconds")
	flags.BoolVar(&opts.force, "force", false, "reinstall node and dsh")
	flags.BoolVar(&opts.noSandbox, "no-sandbox", false, "default new sessions to the danger-full-access policy (no sandbox wrapper, no approvals)")
	flags.BoolVar(&opts.jsonOut, "json", false, "print one JSON result object")

	if err := flags.Parse(argv); err != nil {
		return nil, err
	}

	if opts.dataDir == "" {
		opts.dataDir = defaultDataDir()
	}
	if opts.credentials == "" {
		opts.credentials = filepath.Join(opts.dataDir, "access.json")
	}
	opts.timeout = time.Duration(*seconds) * time.Second
	if opts.dshHome != "" {
		if err := os.Setenv("DSH_HOME", opts.dshHome); err != nil {
			return nil, err
		}
	}
	if opts.authUser != "" && opts.authPass == "" {
		// The helper generates a password whenever one is not supplied, so a
		// fixed user with a random password is a footgun worth naming.
		fmt.Fprintln(os.Stderr, "warning: --auth-user without --auth-pass keeps a randomly generated password")
	}
	if !opts.basicAuth && opts.exposeDshUI {
		return nil, errors.New("--expose-dsh-ui with --basic-auth=false would publish this machine's agent with no credential at all; keep Basic Auth on")
	}
	return opts, nil
}

func cmdUp(argv []string) error {
	opts, err := parseUpFlags(argv)
	if err != nil {
		return err
	}
	log := newLogger(opts.jsonOut)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if opts.force {
		log.warn("--force: reinstalling Node and dsh")
	}
	opts.childEnv = applyNoSandbox(log, opts.childEnv, os.Environ(), opts.noSandbox)

	plugins, err := expandPluginSpecs(defaultPlugins(opts.plugins))
	if err != nil {
		return err
	}
	log.info("plugins: %s", describePlugins(plugins))

	nodePath, nodeVersion, err := ensureNode(ctx, log, opts)
	if err != nil {
		return err
	}
	env, err := buildChildEnv(os.Environ(), opts.childEnv, filepath.Dir(nodePath))
	if err != nil {
		return err
	}
	runner := execRunner{env: env}

	dshPath, dshVersion, err := ensureDsh(ctx, log, opts, nodePath, runner)
	if err != nil {
		return err
	}
	log.info("dsh %s at %s", dshVersion, dshPath)

	if err := ensureProfile(ctx, log, runner, dshPath, opts.profile); err != nil {
		return err
	}
	installed, err := installPlugins(ctx, log, runner, dshPath, opts.profile, plugins)
	if err != nil {
		return err
	}
	if err := satisfyPeers(ctx, log, runner, dshPath, opts.profile, installed); err != nil {
		return err
	}
	// Peer resolution can leave a second copy of a harness package behind, and
	// an earlier run may already have installed one; both break symbol identity
	// across module boundaries.
	if err := linkHarnessPackages(log, dshPath, opts.profile); err != nil {
		return err
	}

	// The tunnel configuration only makes sense when the plugin that reads it is
	// actually in the profile — a plugin already installed earlier counts too.
	wantsTunnel := false
	if dependencies, err := profileDependencies(opts.profile); err == nil {
		_, wantsTunnel = dependencies[pikoRemotePackage]
	}

	args := []string{"--profile", opts.profile}
	if wantsTunnel {
		overlayPath := filepath.Join(opts.dataDir, "piko-remote.overlay.yml")
		overlay, err := renderOverlay(overlayConfig{
			Remote:            opts.remote,
			EndpointPrefix:    "dsh",
			BasicAuth:         opts.basicAuth,
			BasicAuthUser:     opts.authUser,
			BasicAuthPass:     opts.authPass,
			URLMode:           "subdomain",
			PreserveHost:      false,
			AllowDshUiExpose:  opts.exposeDshUI,
			AutoExpose:        opts.exposeDshUI,
			DefaultTTLMinutes: opts.ttlMinutes,
			CredentialsFile:   opts.credentials,
			Endpoint:          opts.endpoint,
		})
		if err != nil {
			return err
		}
		if err := os.MkdirAll(opts.dataDir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
			return fmt.Errorf("write overlay: %w", err)
		}
		log.info("wrote tunnel overlay %s", overlayPath)

		if err := ensureHelperBinary(log, opts.profile, pikoRemotePackage); err != nil {
			return err
		}
		args = append(args, "--patch", overlayPath)
	} else {
		log.warn("profile has no %s; starting DSH without tunnel configuration", pikoRemotePackage)
	}

	// The port is always passed, defaulting to 0 (OS-assigned). A fixed default
	// collides with any other DSH already running — including a second profile on
	// the same machine — and the port is irrelevant to the caller anyway: the
	// tunnel URL and its target port come from the running server.
	// Extra args come before the app-level flags: parent-level options such as
	// --patch must precede the app's own positionals, or the app's parser sees
	// them and rejects them as unknown.
	args = append(args, opts.dshArgs...)
	args = append(args, "--no-open", "--port", fmt.Sprint(opts.port))

	state := runState{
		Profile:    opts.profile,
		LogFile:    filepath.Join(opts.dataDir, "logs", "dsh-"+opts.profile+".log"),
		DataDir:    opts.dataDir,
		AccessFile: opts.credentials,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	pid, pgid, err := startDetached(dshPath, args, env, state.LogFile, opts.dataDir)
	if err != nil {
		return fmt.Errorf("start dsh: %w", err)
	}
	state.PID, state.PGID = pid, pgid
	log.info("dsh started (pid %d); log at %s", pid, state.LogFile)

	if err := waitReady(ctx, log, &state, wantsTunnel, opts.timeout); err != nil {
		_ = stopProcessGroup(state.PID, state.PGID, 5*time.Second)
		return err
	}
	if err := saveState(statePath(opts.dataDir), state); err != nil {
		log.warn("could not save state: %v", err)
	}

	reportReady(log, opts, nodeVersion, dshVersion, installed, state)
	return nil
}

// defaultPlugins substitutes the built-in plugin when nothing was requested.
func defaultPlugins(requested []string) []string {
	if len(requested) == 0 {
		return []string{defaultRemotePlugin}
	}
	return requested
}

func describePlugins(plugins []expandedPlugin) string {
	parts := make([]string, 0, len(plugins))
	for _, plugin := range plugins {
		parts = append(parts, string(plugin.Kind)+":"+plugin.Spec)
	}
	return strings.Join(parts, ", ")
}

// reportReady prints the result: the local URL, the public URL and the
// credentials, in whichever shape the caller asked for.
func reportReady(log *logger, opts *upOptions, nodeVersion, dshVersion string, installed []string, state runState) {
	localURL := state.LocalURL
	if state.Token != "" {
		localURL = ensureToken(state.LocalURL, state.Token)
	}
	publicURL := state.RemoteURL
	if publicURL != "" && state.Token != "" {
		publicURL = ensureToken(publicURL, state.Token)
	}

	if opts.jsonOut {
		payload := map[string]any{
			"event":       "ready",
			"profile":     state.Profile,
			"pid":         state.PID,
			"localUrl":    localURL,
			"remoteUrl":   publicURL,
			"endpoint":    state.Endpoint,
			"authUser":    state.AuthUser,
			"authPass":    state.AuthPass,
			"expiresAt":   state.ExpiresAt,
			"logFile":     state.LogFile,
			"nodeVersion": nodeVersion,
			"dshVersion":  dshVersion,
			"plugins":     installed,
		}
		if state.TunnelNote != "" {
			payload["tunnelNote"] = state.TunnelNote
		}
		encoded, err := json.Marshal(payload)
		if err == nil {
			fmt.Fprintln(os.Stdout, string(encoded))
		}
		return
	}

	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "  profile      %s\n", state.Profile)
	fmt.Fprintf(os.Stdout, "  node         %s\n", nodeVersion)
	fmt.Fprintf(os.Stdout, "  dsh          %s\n", dshVersion)
	if len(installed) > 0 {
		fmt.Fprintf(os.Stdout, "  installed    %s\n", strings.Join(installed, ", "))
	}
	fmt.Fprintf(os.Stdout, "  local url    %s\n", localURL)
	if publicURL != "" {
		fmt.Fprintf(os.Stdout, "  public url   %s\n", publicURL)
		fmt.Fprintf(os.Stdout, "  auth         %s / %s\n", state.AuthUser, state.AuthPass)
		if state.ExpiresAt != "" {
			fmt.Fprintf(os.Stdout, "  expires      %s\n", state.ExpiresAt)
		}
	} else if state.TunnelNote != "" {
		fmt.Fprintf(os.Stdout, "  tunnel       not exposed: %s\n", state.TunnelNote)
	}
	fmt.Fprintf(os.Stdout, "  log          %s\n", state.LogFile)
	fmt.Fprintf(os.Stdout, "  stop         dsh-piko-remote down --data-dir %s\n", state.DataDir)
	fmt.Fprintln(os.Stdout)
}

// ensureToken appends the DSH access token to a URL when it is missing.
func ensureToken(raw, token string) string {
	if raw == "" || token == "" || strings.Contains(raw, "token=") {
		return raw
	}
	separator := "?"
	if strings.Contains(raw, "?") {
		separator = "&"
	}
	return raw + separator + "token=" + token
}

// buildChildEnv layers explicit KEY=VALUE entries over base, then puts dir first
// on PATH.
//
// PATH is rebuilt last and cannot be overridden: `dsh` is a JS shim, so the
// managed Node has to stay findable even if someone passes --env PATH=...
func buildChildEnv(base, extra []string, dir string) ([]string, error) {
	if len(extra) == 0 {
		return withPath(base, dir), nil
	}

	overrides := make(map[string]string, len(extra))
	for _, entry := range extra {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" {
			return nil, fmt.Errorf("--env %q is not KEY=VALUE", entry)
		}
		if key == "PATH" {
			return nil, errors.New("--env PATH=... is not allowed: the managed Node must stay on PATH")
		}
		overrides[key] = value
	}

	merged := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; replaced {
			continue
		}
		merged = append(merged, entry)
	}
	for key, value := range overrides {
		merged = append(merged, key+"="+value)
	}
	return withPath(merged, dir), nil
}

// applyNoSandbox implements --no-sandbox.
//
// The flag pins the *default policy mode* to `danger-full-access` instead of
// swapping the executor plugin. That is the only supported way to run
// unwrapped: the sandboxed executor short-circuits to the local one in that
// mode, while an unconfined executor cannot be mounted at all — the permission
// presets reject one that publishes no `sandboxMode`, and `fs-sandbox`,
// `api-workspace-files`, and the deliverables UI all require the policy
// service the sandbox row provides.
//
// The mode is what an operator wants anyway: `danger-full-access` also turns
// approvals off, so a host whose sandbox has no usable backend stops answering
// every command with a denial and an escalation prompt.
func applyNoSandbox(log *logger, childEnv, base []string, noSandbox bool) []string {
	if !noSandbox {
		return childEnv
	}
	if hasEnvEntry(base, permissionModeEnv) || hasEnvEntry(childEnv, permissionModeEnv) {
		log.warn("--no-sandbox ignored: an explicit %s already decides whether commands are wrapped", permissionModeEnv)
		return childEnv
	}
	log.info("--no-sandbox: new sessions default to %s=danger-full-access", permissionModeEnv)
	return append(childEnv, permissionModeEnv+"=danger-full-access")
}

// permissionModeEnv is the environment variable the host composition reads as
// the default sandbox mode for new sessions.
const permissionModeEnv = "DSH_PERMISSION_MODE"

// hasEnvEntry reports whether env already carries key, in either the
// os.Environ or the --env KEY=VALUE shape.
func hasEnvEntry(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

// withPath returns env with dir first on PATH, replacing any existing PATH
// entry: a duplicated PATH would leave the child's lookup order to the OS.
func withPath(env []string, dir string) []string {
	existing := ""
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, "PATH=") {
			// Taken from env rather than os.Getenv so the function depends only
			// on what it was handed, which is what makes it testable.
			existing = strings.TrimPrefix(entry, "PATH=")
			continue
		}
		out = append(out, entry)
	}
	path := dir
	if existing != "" {
		path = dir + string(os.PathListSeparator) + existing
	}
	return append(out, "PATH="+path)
}

// ensureNode uses a usable node when there is one, and installs its own when
// there is not.
func ensureNode(ctx context.Context, log *logger, opts *upOptions) (string, string, error) {
	if opts.nodePath != "" {
		version, ok := inspectNode(opts.nodePath)
		if !ok {
			return "", "", fmt.Errorf("--node %s is not a usable node (need ^22.19.0 || >=24.0.0)", opts.nodePath)
		}
		log.info("using node %s at %s", version, opts.nodePath)
		return opts.nodePath, version, nil
	}

	if !opts.force {
		if path, version, ok := findNodeOnPath(); ok {
			log.info("using node %s at %s", version, path)
			return path, version, nil
		}
	}

	// A managed Node from an earlier run is reused: re-downloading a toolchain on
	// every `up` would make the second run worse than the first.
	if !opts.force {
		if nodeDir, err := managedNodeDir(opts.dataDir, opts.nodeVersion); err == nil {
			if version, ok := inspectNode(nodeExecutable(nodeDir)); ok {
				log.info("using managed node %s at %s", version, nodeExecutable(nodeDir))
				return nodeExecutable(nodeDir), version, nil
			}
		}
	}

	log.info("no usable node found; installing Node %s into %s", opts.nodeVersion, opts.dataDir)
	nodeDir, err := installNode(ctx, log, filepath.Join(opts.dataDir, "node"), opts.nodeVersion)
	if err != nil {
		return "", "", err
	}
	path := nodeExecutable(nodeDir)
	version, ok := inspectNode(path)
	if !ok {
		return "", "", fmt.Errorf("installed node at %s is not usable", path)
	}
	log.info("installed node %s at %s", version, path)
	return path, version, nil
}

// ensureDsh reuses an existing dsh, or installs one into the managed Node prefix.
func ensureDsh(ctx context.Context, log *logger, opts *upOptions, nodePath string, runner execRunner) (string, string, error) {
	if opts.dshPath != "" {
		log.info("using dsh at %s", opts.dshPath)
		return opts.dshPath, runVersion(ctx, runner.env, opts.dshPath), nil
	}

	managed := filepath.Join(filepath.Dir(nodePath), "dsh")
	if !opts.force {
		if info, err := os.Stat(managed); err == nil && info.Mode()&0o111 != 0 {
			log.info("using managed dsh at %s", managed)
			return managed, runVersion(ctx, runner.env, managed), nil
		}
		if found, err := exec.LookPath("dsh"); err == nil {
			log.info("using dsh from PATH at %s", found)
			return found, runVersion(ctx, runner.env, found), nil
		}
	}

	spec := "@deepseek-ai/dsh@" + opts.dshVersion
	log.info("installing %s and pnpm (this also brings the Node dependencies DSH needs)", spec)
	if err := installNodeGlobals(ctx, log, runner, nodePath, opts.registry, spec, "pnpm"); err != nil {
		return "", "", err
	}

	version := runVersion(ctx, runner.env, managed)
	if version == "" {
		return "", "", fmt.Errorf("installed %s but %s did not run", spec, managed)
	}
	log.info("installed dsh %s at %s", version, managed)
	return managed, version, nil
}

// installNodeGlobals installs npm packages into the managed Node prefix.
func installNodeGlobals(ctx context.Context, log *logger, runner execRunner, nodePath, registry string, packages ...string) error {
	npm := filepath.Join(filepath.Dir(nodePath), "npm")
	if _, err := os.Stat(npm); err != nil {
		return fmt.Errorf("no npm next to %s", nodePath)
	}
	nodeRoot := filepath.Dir(filepath.Dir(nodePath))
	args := []string{"install", "--global", "--prefix", nodeRoot, "--no-fund", "--no-audit"}
	if registry != "" {
		args = append(args, "--registry", registry)
	}
	args = append(args, packages...)
	if _, err := runner.run(ctx, log, npm, args...); err != nil {
		return fmt.Errorf("install %s: %w", strings.Join(packages, " "), err)
	}
	return nil
}

// runVersion asks a binary for its version, tolerating failure.
func runVersion(ctx context.Context, env []string, binary string) string {
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, binary, "--version")
	command.Env = env
	output, err := command.Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(output))
}

func cmdDown(argv []string) error {
	opts, err := parseUpFlags(argv)
	if err != nil {
		return err
	}
	log := newLogger(opts.jsonOut)

	state, err := loadState(statePath(opts.dataDir))
	if err != nil {
		log.warn("no recorded instance in %s", opts.dataDir)
		return nil
	}

	if !processAlive(state.PID) {
		log.info("dsh (pid %d) is not running; closing the tunnel if it is still open", state.PID)
	} else {
		log.info("stopping dsh (pid %d, process group %d)", state.PID, state.PGID)
		if err := stopProcessGroup(state.PID, state.PGID, 10*time.Second); err != nil {
			return err
		}
	}

	if err := os.Remove(statePath(opts.dataDir)); err != nil && !os.IsNotExist(err) {
		log.warn("could not remove state file: %v", err)
	}
	log.info("stopped")
	return nil
}

func cmdStatus(argv []string) error {
	opts, err := parseUpFlags(argv)
	if err != nil {
		return err
	}
	log := newLogger(opts.jsonOut)

	state, err := loadState(statePath(opts.dataDir))
	if err != nil {
		if opts.jsonOut {
			fmt.Fprintln(os.Stdout, `{"event":"status","running":false}`)
			return nil
		}
		log.info("no instance recorded in %s", opts.dataDir)
		return nil
	}
	running := processAlive(state.PID)

	if opts.jsonOut {
		payload := map[string]any{
			"event":     "status",
			"running":   running,
			"profile":   state.Profile,
			"pid":       state.PID,
			"localUrl":  state.LocalURL,
			"remoteUrl": state.RemoteURL,
			"endpoint":  state.Endpoint,
			"authUser":  state.AuthUser,
			"authPass":  state.AuthPass,
			"expiresAt": state.ExpiresAt,
			"logFile":   state.LogFile,
			"startedAt": state.StartedAt,
		}
		if state.TunnelNote != "" {
			payload["tunnelNote"] = state.TunnelNote
		}
		encoded, err := json.Marshal(payload)
		if err == nil {
			fmt.Fprintln(os.Stdout, string(encoded))
		}
		return nil
	}

	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "  running      %v\n", running)
	fmt.Fprintf(os.Stdout, "  profile      %s\n", state.Profile)
	fmt.Fprintf(os.Stdout, "  pid          %d\n", state.PID)
	fmt.Fprintf(os.Stdout, "  started      %s\n", state.StartedAt)
	if state.LocalURL != "" {
		fmt.Fprintf(os.Stdout, "  local url    %s\n", ensureToken(state.LocalURL, state.Token))
	}
	if state.RemoteURL != "" {
		fmt.Fprintf(os.Stdout, "  public url   %s\n", state.RemoteURL)
		fmt.Fprintf(os.Stdout, "  auth         %s / %s\n", state.AuthUser, state.AuthPass)
	}
	if state.TunnelNote != "" {
		fmt.Fprintf(os.Stdout, "  tunnel       %s\n", state.TunnelNote)
	}
	fmt.Fprintf(os.Stdout, "  log          %s\n", state.LogFile)
	fmt.Fprintln(os.Stdout)
	return nil
}

func cmdLogs(argv []string) error {
	opts := &upOptions{}
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	flags.StringVar(&opts.dataDir, "data-dir", "", "launcher data directory")
	flags.BoolVar(&opts.jsonOut, "json", false, "print one JSON object")
	lines := flags.Int("lines", 40, "number of lines to print")
	if err := flags.Parse(argv); err != nil {
		return err
	}
	if opts.dataDir == "" {
		opts.dataDir = defaultDataDir()
	}

	state, err := loadState(statePath(opts.dataDir))
	if err != nil {
		return fmt.Errorf("no recorded instance in %s", opts.dataDir)
	}
	raw, err := os.ReadFile(state.LogFile)
	if err != nil {
		return fmt.Errorf("read %s: %w", state.LogFile, err)
	}
	text := tailText(string(raw), *lines)
	if opts.jsonOut {
		encoded, err := json.Marshal(map[string]any{"event": "logs", "logFile": state.LogFile, "text": text})
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, string(encoded))
		return nil
	}
	fmt.Fprintln(os.Stdout, text)
	return nil
}

// goos/goarch are wrapped so the platform-dependent naming is easy to find.
func goos() string   { return runtime.GOOS }
func goarch() string { return runtime.GOARCH }
