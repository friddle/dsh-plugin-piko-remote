// Command piko-expose exposes one local port through a piko server.
//
// It is the tunnel engine behind dsh-plugin-piko-remote, and it is also usable
// on its own:
//
//	piko-expose --endpoint dsh-a1b2c3 --target 127.0.0.1:43120 --json
//
// The process holds exactly one outbound connection to the piko server (no
// inbound port is opened), reverse-proxies that endpoint's traffic to
// --target, optionally guards it with HTTP Basic Auth, and prints one JSON
// event per line on stdout so a supervisor can follow it:
//
//	{"event":"auth","user":"...","pass":"..."}
//	{"event":"ready","endpoint":"dsh-a1b2c3","target":"127.0.0.1:43120","remoteUrl":"https://dsh-a1b2c3.clauded.friddle.me/"}
//	{"event":"closed","reason":"signal"}
//	{"event":"error","message":"..."}
//
// `auth` precedes `ready` so a supervisor that waits for ready already knows
// the credentials.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// version is the helper's own version, reported by --version and used by
	// the plugin to spot a stale binary in bin/.
	version = "0.1.0"

	// defaultRemote matches opencode-piko-remote's DefaultRemote and the
	// public gotty-piko server.
	defaultRemote = "https://clauded.friddle.me"

	// autoStrip is the sentinel for "--strip-prefix not given": subdomain mode
	// strips nothing, path mode strips "/<endpoint>".
	autoStrip = "auto"
)

// endpointRE is the public server's subdomain rule, not a stylistic choice:
// the endpoint becomes a DNS label under the wildcard base, so it must be
// lowercase alphanumeric with inner dashes and at most 63 characters.
var endpointRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Ambiguous glyphs are left out: these credentials get read off a screen and
// typed into a phone.
const (
	userAlphabet = "abcdefghijkmnopqrstuvwxyz23456789"
	passAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
)

type options struct {
	remote       string
	endpoint     string
	target       string
	stripPrefix  string
	upstreamKey  string
	auth         bool
	authUser     string
	authPass     string
	preserveHost bool
	autoExitMin  int
	cacheMaxMB   int
	cacheTotalMB int
	noCache      bool
	jsonOut      bool
	urlMode      string
	localAddr    string
	insecure     bool
	showVersion  bool
}

func main() {
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintf(os.Stderr, "piko-expose: %v\n", err)
		os.Exit(2)
	}
	if opts.showVersion {
		fmt.Println("piko-expose " + version)
		return
	}

	rep := &reporter{out: os.Stdout, json: opts.jsonOut}
	if err := run(opts, rep); err != nil {
		rep.failure(err)
		os.Exit(1)
	}
}

// run wires the tunnel and blocks until the context is cancelled (signal or
// --auto-exit), the upstream dies, or shutdown completes.
func run(opts options, rep *reporter) error {
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()

	var cache *responseCache
	if !opts.noCache {
		cache = newResponseCache(
			int64(opts.cacheMaxMB)*1024*1024,
			int64(opts.cacheTotalMB)*1024*1024,
		)
	}

	handler, err := newHandler(proxyConfig{
		target:       opts.target,
		stripPrefix:  opts.stripPrefix,
		preserveHost: opts.preserveHost,
		authUser:     opts.authUser,
		authPass:     opts.authPass,
		cache:        cache,
	})
	if err != nil {
		return err
	}

	ln, err := dialUpstream(ctx, opts)
	if err != nil {
		return err
	}
	defer ln.Close()

	server := newHTTPServer(handler)
	serveErr := serve(server, ln)

	// --local-addr serves the same handler on loopback as well. It exists so
	// the proxy, the auth and the rewriting can be exercised without a piko
	// connection, which makes debugging a bad endpoint cheap.
	var localServer *http.Server
	if opts.localAddr != "" {
		localLn, err := net.Listen("tcp", opts.localAddr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", opts.localAddr, err)
		}
		localServer = newHTTPServer(handler)
		serve(localServer, localLn)
		rep.note("local debug listener on http://" + localLn.Addr().String())
	}

	remoteURL, err := opts.remoteURL()
	if err != nil {
		return err
	}
	// Auth is emitted before ready on purpose: the supervisor treats "ready" as
	// the signal that the tunnel is usable, and it must already know the
	// credentials by then so the caller can hand them to the user in one step.
	if opts.authPass != "" {
		rep.auth(opts.authUser, opts.authPass)
	}
	rep.ready(opts, remoteURL)

	var (
		mu     sync.Mutex
		reason = "signal"
	)
	setReason := func(r string) {
		mu.Lock()
		defer mu.Unlock()
		reason = r
	}

	if opts.autoExitMin > 0 {
		time.AfterFunc(time.Duration(opts.autoExitMin)*time.Minute, func() {
			setReason(fmt.Sprintf("auto-exit after %dm", opts.autoExitMin))
			cancel()
		})
	}

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve endpoint %q: %w", opts.endpoint, err)
		}
		setReason("upstream closed")
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = server.Shutdown(shutdownCtx)
	if localServer != nil {
		_ = localServer.Shutdown(shutdownCtx)
	}

	mu.Lock()
	finalReason := reason
	mu.Unlock()
	rep.closed(finalReason)
	return nil
}

// parseFlags parses and normalises the command line. Normalisation is part of
// parsing on purpose: the JSON protocol promises a ready-to-use remoteUrl, so
// a sloppy --target ("8080") or --remote ("clauded.friddle.me") must be fixed
// here rather than surfacing as a confusing failure later.
func parseFlags(args []string) (options, error) {
	fs := flag.NewFlagSet("piko-expose", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `piko-expose — expose a local port through a piko server

usage: piko-expose --endpoint <id> --target <host:port> [flags]

flags:
  --remote URL          piko server (default `+defaultRemote+`)
  --endpoint ID         endpoint to listen on, [a-z0-9-] (required)
  --target ADDR         local address to forward to, e.g. 127.0.0.1:43120 (required)
  --strip-prefix P      strip P from incoming paths; "auto" (default) means
                        "/<endpoint>" in path mode and nothing in subdomain mode
  --url-mode MODE       subdomain (default) or path; only affects the printed URL
  --upstream-key KEY    piko upstream API key, if the server requires one
  --auth                enable HTTP Basic Auth (default true)
  --auth-user USER      default: random
  --auth-pass PASS      default: random; empty disables auth
  --preserve-host       forward the browser Host instead of rewriting it (default true)
  --auto-exit MINUTES   exit after this long; 0 never exits (default 0)
  --cache-max-mb MB     largest response to buffer and re-serve (default 64)
  --cache-total-mb MB   total response cache budget (default 256)
  --no-cache            disable the response cache
  --local-addr ADDR     additionally serve the tunnel on a local address (debug)
  --insecure            skip TLS verification of the piko server (debug)
  --json                print one JSON event per line
  --version             print the version and exit
`)
	}

	var opts options
	fs.StringVar(&opts.remote, "remote", envOr("PIKO_REMOTE", defaultRemote), "piko server URL")
	fs.StringVar(&opts.endpoint, "endpoint", "", "endpoint to listen on")
	fs.StringVar(&opts.target, "target", "", "local address to forward to")
	fs.StringVar(&opts.stripPrefix, "strip-prefix", autoStrip, "path prefix to strip")
	fs.StringVar(&opts.urlMode, "url-mode", "subdomain", "subdomain or path")
	fs.StringVar(&opts.upstreamKey, "upstream-key", os.Getenv("PIKO_UPSTREAM_KEY"), "piko upstream API key")
	fs.BoolVar(&opts.auth, "auth", true, "enable HTTP Basic Auth")
	fs.StringVar(&opts.authUser, "auth-user", "", "Basic Auth user")
	fs.StringVar(&opts.authPass, "auth-pass", "", "Basic Auth password")
	fs.BoolVar(&opts.preserveHost, "preserve-host", true, "forward the browser Host")
	fs.IntVar(&opts.autoExitMin, "auto-exit", 0, "exit after this many minutes")
	fs.IntVar(&opts.cacheMaxMB, "cache-max-mb", 64, "largest response to buffer for re-serving")
	fs.IntVar(&opts.cacheTotalMB, "cache-total-mb", 256, "total response cache budget")
	fs.BoolVar(&opts.noCache, "no-cache", false, "disable the response cache")
	fs.StringVar(&opts.localAddr, "local-addr", "", "extra local debug listener")
	fs.BoolVar(&opts.insecure, "insecure", false, "skip piko server TLS verification")
	fs.BoolVar(&opts.jsonOut, "json", false, "emit JSON events")
	fs.BoolVar(&opts.showVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if opts.showVersion {
		return opts, nil
	}

	if opts.endpoint == "" {
		return options{}, errors.New("--endpoint is required")
	}
	opts.endpoint = strings.ToLower(strings.TrimSpace(opts.endpoint))
	if !endpointRE.MatchString(opts.endpoint) {
		return options{}, fmt.Errorf(
			"invalid --endpoint %q: use lowercase letters, digits and inner dashes (it becomes a DNS label)",
			opts.endpoint)
	}

	if opts.target == "" {
		return options{}, errors.New("--target is required")
	}
	if !strings.Contains(opts.target, ":") {
		// "--target 8080" is a port, not an address.
		opts.target = "127.0.0.1:" + opts.target
	}

	switch opts.urlMode {
	case "subdomain", "path":
	default:
		return options{}, fmt.Errorf("invalid --url-mode %q: want subdomain or path", opts.urlMode)
	}

	switch opts.stripPrefix {
	case autoStrip:
		if opts.urlMode == "subdomain" {
			opts.stripPrefix = ""
		} else {
			opts.stripPrefix = "/" + opts.endpoint
		}
	case "", "none", "-":
		opts.stripPrefix = ""
	}

	if !strings.Contains(opts.remote, "://") {
		opts.remote = "https://" + opts.remote
	}

	if opts.auth {
		var err error
		if opts.authUser == "" {
			if opts.authUser, err = randomString(8, userAlphabet); err != nil {
				return options{}, err
			}
		}
		if opts.authPass == "" {
			if opts.authPass, err = randomString(20, passAlphabet); err != nil {
				return options{}, err
			}
		}
	} else {
		opts.authUser, opts.authPass = "", ""
	}

	return opts, nil
}

// remoteURL is the address a human should open. It is derived rather than
// guessed, because the two routing modes produce different shapes:
// https://<endpoint>.<base>/ (subdomain) vs https://<base>/<endpoint>/ (path).
func (o options) remoteURL() (string, error) {
	u, err := url.Parse(o.remote)
	if err != nil {
		return "", fmt.Errorf("parse remote %q: %w", o.remote, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("remote %q has no host", o.remote)
	}
	if o.urlMode == "subdomain" {
		return fmt.Sprintf("%s://%s.%s/", u.Scheme, o.endpoint, u.Host), nil
	}
	return fmt.Sprintf("%s://%s/%s/", u.Scheme, u.Host, o.endpoint), nil
}

// reporter emits the stdout protocol. In JSON mode every event is one JSON
// object on one line, which is what the plugin parses; otherwise the same
// events are printed for a human running the helper by hand.
type reporter struct {
	out  io.Writer
	json bool
}

func (r *reporter) emit(event string, fields map[string]any) {
	if r.json {
		payload := make(map[string]any, len(fields)+1)
		payload["event"] = event
		for k, v := range fields {
			payload[k] = v
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintln(r.out, string(encoded))
		return
	}

	switch event {
	case "ready":
		fmt.Fprintf(r.out, "endpoint   %v\n", fields["endpoint"])
		fmt.Fprintf(r.out, "target     %v\n", fields["target"])
		fmt.Fprintf(r.out, "public url %v\n", fields["remoteUrl"])
	case "auth":
		fmt.Fprintf(r.out, "auth       %v / %v\n", fields["user"], fields["pass"])
	case "closed":
		fmt.Fprintf(r.out, "closed     %v\n", fields["reason"])
	case "error":
		fmt.Fprintf(r.out, "error      %v\n", fields["message"])
	default:
		fmt.Fprintf(r.out, "%s %v\n", event, fields)
	}
}

func (r *reporter) ready(opts options, remoteURL string) {
	r.emit("ready", map[string]any{
		"endpoint":  opts.endpoint,
		"target":    opts.target,
		"remoteUrl": remoteURL,
		"urlMode":   opts.urlMode,
	})
}

func (r *reporter) auth(user, pass string) {
	r.emit("auth", map[string]any{"user": user, "pass": pass})
}

func (r *reporter) closed(reason string) {
	r.emit("closed", map[string]any{"reason": reason})
}

func (r *reporter) failure(err error) {
	r.emit("error", map[string]any{"message": err.Error()})
}

// note is human-only chatter; it is suppressed in JSON mode so the protocol
// stream stays parseable.
func (r *reporter) note(message string) {
	if r.json {
		return
	}
	fmt.Fprintln(r.out, message)
}

// randomString draws n characters from alphabet using crypto/rand.
func randomString(n int, alphabet string) (string, error) {
	if len(alphabet) == 0 {
		return "", errors.New("empty alphabet")
	}
	limit := big.NewInt(int64(len(alphabet)))
	out := make([]byte, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("read random: %w", err)
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
