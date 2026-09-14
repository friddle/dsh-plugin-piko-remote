package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRunner records invocations and can simulate the side effects of the real
// commands, which is how the install orchestration is tested without a network,
// a Node toolchain, or a real dsh.
type fakeRunner struct {
	calls [][]string
	onRun func(name string, args []string) error
}

func (f *fakeRunner) run(_ context.Context, _ *logger, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if f.onRun != nil {
		if err := f.onRun(name, args); err != nil {
			return "", err
		}
	}
	return "ok", nil
}

func (f *fakeRunner) ran(substring string) bool {
	for _, call := range f.calls {
		if strings.Contains(strings.Join(call, " "), substring) {
			return true
		}
	}
	return false
}

func testLogger() *logger {
	return &logger{mu: &sync.Mutex{}, out: io.Discard}
}

func TestExpandPluginSpec(t *testing.T) {
	cases := []struct {
		in       string
		wantSpec string
		wantKind pluginKind
	}{
		{"friddle/dsh-plugin-piko-remote", "github:friddle/dsh-plugin-piko-remote", kindGitHub},
		{"owner/repo#v1.2.3", "github:owner/repo#v1.2.3", kindGitHub},
		{"github:friddle/dsh-plugin-piko-remote", "github:friddle/dsh-plugin-piko-remote", kindGitHub},
		{"git+https://example.com/x.git", "git+https://example.com/x.git", kindGitHub},
		{"@deepseek-ai/dsh-tools", "@deepseek-ai/dsh-tools", kindNPM},
		{"@scope/pkg@0.1.5-rc.1", "@scope/pkg@0.1.5-rc.1", kindNPM},
		{"left-pad", "left-pad", kindNPM},
		{"left-pad@1.0.0", "left-pad@1.0.0", kindNPM},
		{"./local-plugin", "./local-plugin", kindLocal},
		{"/abs/local-plugin", "/abs/local-plugin", kindLocal},
		{"file:../sibling", "file:../sibling", kindLocal},
		{"~/plugins/mine", "~/plugins/mine", kindLocal},
		{"https://example.com/plugin.tgz", "https://example.com/plugin.tgz", kindURL},
		{"dsh-plugin-piko-remote-0.2.0.tgz", "dsh-plugin-piko-remote-0.2.0.tgz", kindNPM},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := expandPluginSpec(tc.in)
			if err != nil {
				t.Fatalf("expandPluginSpec(%q): %v", tc.in, err)
			}
			if got.Spec != tc.wantSpec {
				t.Fatalf("spec = %q, want %q", got.Spec, tc.wantSpec)
			}
			if got.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", got.Kind, tc.wantKind)
			}
		})
	}

	t.Run("a GitHub shorthand is recognised as this plugin", func(t *testing.T) {
		plugin, err := expandPluginSpec("friddle/dsh-plugin-piko-remote")
		if err != nil {
			t.Fatal(err)
		}
		if !plugin.isPikoRemote() {
			t.Fatal("expected the shorthand to be recognised as piko-remote")
		}
	})

	t.Run("whitespace is trimmed, empty and invalid specs are refused", func(t *testing.T) {
		got, err := expandPluginSpec("  owner/repo  ")
		if err != nil {
			t.Fatal(err)
		}
		if got.Spec != "github:owner/repo" {
			t.Fatalf("spec = %q", got.Spec)
		}
		if _, err := expandPluginSpec("   "); err == nil {
			t.Fatal("blank spec should fail")
		}
		if _, err := expandPluginSpec("two words"); err == nil {
			t.Fatal("spec with an inner space should fail")
		}
	})
}

func TestExpandPluginSpecsDedupes(t *testing.T) {
	plugins, err := expandPluginSpecs([]string{"owner/repo", "github:owner/repo", "@scope/pkg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 2 {
		t.Fatalf("got %d plugins, want 2: %+v", len(plugins), plugins)
	}
}

func TestNodeVersionSupported(t *testing.T) {
	cases := map[string]bool{
		"v22.19.0": true,
		"v22.20.1": true,
		"v24.0.0":  true,
		"v25.9.0":  true,
		"v22.18.0": false,
		"v20.20.0": false,
		"v23.5.0":  false,
		"v18.19.1": false,
		"nonsense": false,
		"":         false,
	}
	for input, want := range cases {
		if got := nodeVersionSupported(input); got != want {
			t.Errorf("nodeVersionSupported(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestParseNodeVersion(t *testing.T) {
	major, minor, patch, err := parseNodeVersion("v24.19.0\n")
	if err != nil {
		t.Fatal(err)
	}
	if major != 24 || minor != 19 || patch != 0 {
		t.Fatalf("got %d.%d.%d", major, minor, patch)
	}
	if _, _, _, err := parseNodeVersion("not a version"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestParseLocalURLAndToken(t *testing.T) {
	logText := "some other output\ndsh web: http://127.0.0.1:3080/?token=3HXi3Ga-4djTKQV7ErwvRXm2cU5ws5ZoSjyhpGY87MA\nmore\n"
	url, ok := parseLocalURL(logText)
	if !ok {
		t.Fatal("expected the dsh web line to be found")
	}
	if url != "http://127.0.0.1:3080/?token=3HXi3Ga-4djTKQV7ErwvRXm2cU5ws5ZoSjyhpGY87MA" {
		t.Fatalf("url = %q", url)
	}
	if token := tokenFromURL(url); token != "3HXi3Ga-4djTKQV7ErwvRXm2cU5ws5ZoSjyhpGY87MA" {
		t.Fatalf("token = %q", token)
	}
	if _, ok := parseLocalURL("nothing here"); ok {
		t.Fatal("expected no match")
	}
	if token := tokenFromURL("http://127.0.0.1:3080/"); token != "" {
		t.Fatalf("token = %q, want empty", token)
	}
}

func TestTunnelNoteFromLog(t *testing.T) {
	text := "[piko-remote] loaded; remote=https://x\n! [piko-remote] autoExpose ignored: allowDshUiExpose is false\n"
	note := tunnelNoteFromLog(text)
	if !strings.Contains(note, "allowDshUiExpose") {
		t.Fatalf("note = %q", note)
	}
	if tunnelNoteFromLog("nothing relevant") != "" {
		t.Fatal("expected no note")
	}
}

func TestRenderOverlay(t *testing.T) {
	body, err := renderOverlay(overlayConfig{
		Remote:            "https://clauded.friddle.me",
		EndpointPrefix:    "dsh",
		BasicAuth:         true,
		URLMode:           "subdomain",
		PreserveHost:      false,
		AllowDshUiExpose:  true,
		AutoExpose:        true,
		DefaultTTLMinutes: 480,
		CredentialsFile:   "/home/u/.local/share/dsh-piko-remote/access.json",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"id: piko-remote",
		"remote: https://clauded.friddle.me",
		"preserveHost: false",
		"allowDshUiExpose: true",
		"autoExpose: true",
		"defaultTtlMinutes: 480",
		"credentialsFile: /home/u/.local/share/dsh-piko-remote/access.json",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("overlay is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "endpoint:") {
		t.Errorf("an unset endpoint should be omitted:\n%s", text)
	}
	withEndpoint, err := renderOverlay(overlayConfig{Endpoint: "dsh-demo"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withEndpoint), "endpoint: dsh-demo") {
		t.Errorf("expected the fixed endpoint in the overlay:\n%s", withEndpoint)
	}
}

// fakeHarness writes a minimal npm-global DSH install: <prefix>/bin/dsh plus
// the harness package with the given bundled packages under its own
// node_modules — the layout `npm install --global --prefix <prefix>` produces.
func fakeHarness(t *testing.T, packages ...string) string {
	t.Helper()
	prefix := t.TempDir()
	binDir := filepath.Join(prefix, "bin")
	dshDir := filepath.Join(prefix, "lib", "node_modules", "@deepseek-ai", "dsh")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dshDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "dsh"), []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"@deepseek-ai/dsh","version":"0.1.5-rc.1"}`
	if err := os.WriteFile(filepath.Join(dshDir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, pkg := range packages {
		dir := filepath.Join(dshDir, "node_modules", filepath.FromSlash(pkg))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"name":"` + pkg + `","version":"0.1.5-rc.2"}`
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(binDir, "dsh")
}

func TestHarnessPackageRoot(t *testing.T) {
	dshPath := fakeHarness(t, "@deepseek-ai/dsh-tools")
	prefix := filepath.Dir(filepath.Dir(dshPath))
	// macOS resolves /var to /private/var, so compare resolved paths.
	want, err := filepath.EvalSymlinks(filepath.Join(prefix, "lib", "node_modules", "@deepseek-ai", "dsh"))
	if err != nil {
		t.Fatal(err)
	}
	if got := harnessPackageRoot(dshPath); got != want {
		t.Fatalf("harnessPackageRoot = %q, want %q", got, want)
	}
	if got := harnessPackageDir(dshPath, "@deepseek-ai/dsh-tools"); got != filepath.Join(want, "node_modules", "@deepseek-ai", "dsh-tools") {
		t.Fatalf("harnessPackageDir = %q", got)
	}
	if got := harnessPackageDir(dshPath, "@deepseek-ai/not-shipped"); got != "" {
		t.Fatalf("a package the harness does not ship must not resolve: %q", got)
	}
	if got := harnessPackageRoot(filepath.Join(t.TempDir(), "bin", "dsh")); got != "" {
		t.Fatalf("a dsh outside any install must not resolve: %q", got)
	}
}

// A second copy of a harness package is a second module identity, and DSH keys
// its scheduler by symbol: the copy is what made every web tool call fail with
// "Cannot read properties of undefined (reading 'prepare')".
func TestLinkHarnessPackagesReplacesCopies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	dshPath := fakeHarness(t, "@deepseek-ai/dsh-tools", "@deepseek-ai/cosmokit")

	modules := filepath.Join(home, "profiles", "dsh-piko", "node_modules")
	copyDir := filepath.Join(modules, "@deepseek-ai", "dsh-tools")
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "package.json"), []byte(`{"name":"@deepseek-ai/dsh-tools","version":"0.1.5-rc.1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A package the harness does not ship must be left alone.
	ownDir := filepath.Join(modules, "@deepseek-ai", "dsh-plugin-something")
	if err := os.MkdirAll(ownDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"dependencies":{"@deepseek-ai/dsh-tools":"^0.1.5-rc.1","dsh-plugin-piko-remote":"file:./x.tgz"}}`
	profileDir := filepath.Join(home, "profiles", "dsh-piko")
	if err := os.WriteFile(filepath.Join(profileDir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := linkHarnessPackages(testLogger(), dshPath, "dsh-piko"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(copyDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the copy should have been replaced by a symlink, mode = %v", info.Mode())
	}
	if !resolvesTo(copyDir, harnessPackageDir(dshPath, "@deepseek-ai/dsh-tools")) {
		t.Fatal("the symlink should point at the harness copy")
	}

	raw, err := os.ReadFile(filepath.Join(profileDir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	dependencies := parsed["dependencies"].(map[string]any)
	if got := dependencies["@deepseek-ai/dsh-tools"]; got != "link:"+harnessPackageDir(dshPath, "@deepseek-ai/dsh-tools") {
		t.Fatalf("manifest spec = %v, want a link: spec so pnpm keeps the symlink", got)
	}
	if got := dependencies["dsh-plugin-piko-remote"]; got != "file:./x.tgz" {
		t.Fatalf("unrelated dependencies must survive the rewrite: %v", got)
	}

	// Idempotent: a second pass leaves the linked tree untouched.
	runner := &fakeRunner{}
	if err := linkHarnessPackages(testLogger(), dshPath, "dsh-piko"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatal("the repair pass must not shell out")
	}
	if !resolvesTo(copyDir, harnessPackageDir(dshPath, "@deepseek-ai/dsh-tools")) {
		t.Fatal("a second pass must keep the link")
	}
}

func TestSatisfyPeersLinksHarnessPackages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	dshPath := fakeHarness(t, "@deepseek-ai/dsh-tools")

	modules := filepath.Join(home, "profiles", "dsh-piko", "node_modules")
	pluginDir := filepath.Join(modules, pikoRemotePackage)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"dsh-plugin-piko-remote","peerDependencies":{"@deepseek-ai/dsh-tools":"^0.1.5-rc.1"}}`
	if err := os.WriteFile(filepath.Join(pluginDir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{}
	if err := satisfyPeers(context.Background(), testLogger(), runner, dshPath, "dsh-piko", []string{pikoRemotePackage}); err != nil {
		t.Fatal(err)
	}
	want := "add link:" + harnessPackageDir(dshPath, "@deepseek-ai/dsh-tools")
	if !runner.ran(want) {
		t.Fatalf("a harness-shipped peer must be linked, not installed: %v", runner.calls)
	}
}

func TestApplyNoSandbox(t *testing.T) {
	base := []string{"PATH=/usr/bin"}

	if got := applyNoSandbox(testLogger(), nil, base, false); len(got) != 0 {
		t.Fatalf("a default launch must not touch the child environment: %v", got)
	}

	got := applyNoSandbox(testLogger(), nil, base, true)
	if len(got) != 1 || got[0] != "DSH_PERMISSION_MODE=danger-full-access" {
		t.Fatalf("--no-sandbox should pin the default mode: %v", got)
	}

	// An explicit choice wins, wherever it was made: inherited from the
	// launcher's own environment, or handed to the child with --env.
	inherited := applyNoSandbox(testLogger(), nil, []string{"PATH=/usr/bin", "DSH_PERMISSION_MODE=workspace-write"}, true)
	if len(inherited) != 0 {
		t.Fatalf("an inherited mode must win: %v", inherited)
	}
	explicit := applyNoSandbox(testLogger(), []string{"DSH_PERMISSION_MODE=read-only"}, base, true)
	if len(explicit) != 1 || explicit[0] != "DSH_PERMISSION_MODE=read-only" {
		t.Fatalf("an --env mode must survive untouched: %v", explicit)
	}
}

func TestHasEnvEntry(t *testing.T) {
	env := []string{"PATH=/usr/bin", "DSH_PERMISSION_MODE=read-only"}
	if !hasEnvEntry(env, "DSH_PERMISSION_MODE") {
		t.Fatal("expected the entry to be found")
	}
	if hasEnvEntry(env, "DSH_PERMISSION") {
		t.Fatal("a prefix of a key is not the key")
	}
	if hasEnvEntry(env, "DEEPSEEK_API_KEY") {
		t.Fatal("unexpected hit")
	}
}

func TestEnsureToken(t *testing.T) {
	if got := ensureToken("https://x.example/", "abc"); got != "https://x.example/?token=abc" {
		t.Fatalf("got %q", got)
	}
	if got := ensureToken("https://x.example/?a=1", "abc"); got != "https://x.example/?a=1&token=abc" {
		t.Fatalf("got %q", got)
	}
	if got := ensureToken("https://x.example/?token=abc", "abc"); got != "https://x.example/?token=abc" {
		t.Fatalf("got %q", got)
	}
}

func TestWaitReadyReportsTunnelNote(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dsh.log")
	body := "dsh web: http://127.0.0.1:3080/?token=tok\n! [piko-remote] autoExpose failed: connection refused\n"
	if err := os.WriteFile(logPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	state := &runState{PID: os.Getpid(), LogFile: logPath}
	if err := waitReady(context.Background(), testLogger(), state, true, 5*time.Second); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if state.LocalURL == "" || state.Token != "tok" {
		t.Fatalf("local URL/token not captured: %+v", state)
	}
	if !strings.Contains(state.TunnelNote, "connection refused") {
		t.Fatalf("tunnel note = %q", state.TunnelNote)
	}
}

func TestWaitReadyAcceptsAFreshAccessRecord(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dsh.log")
	accessPath := filepath.Join(dir, "access.json")
	if err := os.WriteFile(logPath, []byte("dsh web: http://127.0.0.1:41587/?token=tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := accessRecord{
		Endpoint:  "dsh-kyd4h7",
		RemoteURL: "https://dsh-kyd4h7.clauded.friddle.me/",
		AuthUser:  "friddle",
		AuthPass:  "sybran_20250807",
		WrittenAt: time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(accessPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	state := &runState{PID: os.Getpid(), LogFile: logPath, AccessFile: accessPath}
	if err := waitReady(context.Background(), testLogger(), state, true, 5*time.Second); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if state.RemoteURL != record.RemoteURL || state.AuthUser != "friddle" {
		t.Fatalf("fresh record not adopted: %+v", state)
	}
}

func TestWaitReadyIgnoresStaleAccessRecord(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dsh.log")
	accessPath := filepath.Join(dir, "access.json")
	if err := os.WriteFile(logPath, []byte("dsh web: http://127.0.0.1:41587/?token=tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := accessRecord{
		Endpoint:  "dsh-oldendp",
		RemoteURL: "https://dsh-oldendp.clauded.friddle.me/",
		AuthUser:  "someoneelse",
		WrittenAt: "2020-01-01T00:00:00Z",
	}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(accessPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	state := &runState{PID: os.Getpid(), LogFile: logPath, AccessFile: accessPath}
	if err := waitReady(context.Background(), testLogger(), state, true, 300*time.Millisecond); err != nil {
		t.Fatalf("waitReady: %v", err)
	}
	if state.RemoteURL != "" {
		t.Fatalf("a record from an earlier run must not be reported as this run's tunnel: %+v", state)
	}
	if !strings.Contains(state.TunnelNote, "timed out") {
		t.Fatalf("expected a timeout note, got %q", state.TunnelNote)
	}
}

func TestWaitReadyTimesOutWithoutWebLine(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dsh.log")
	if err := os.WriteFile(logPath, []byte("starting up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &runState{PID: os.Getpid(), LogFile: logPath}
	err := waitReady(context.Background(), testLogger(), state, false, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout error, got %v", err)
	}
}

func TestEnsureHelperBinaryPlantsEmbeddedHelper(t *testing.T) {
	name, err := helperFileName()
	if err != nil {
		t.Skipf("no helper name for this platform: %v", err)
	}
	if _, _, err := helperBinaries(name); err != nil {
		t.Skipf("this build carries no helper to plant: %v", err)
	}

	home := t.TempDir()
	t.Setenv("DSH_HOME", home)
	packageDir := filepath.Join(home, "profiles", "dsh-piko", "node_modules", pikoRemotePackage)
	if err := os.MkdirAll(packageDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ensureHelperBinary(testLogger(), "dsh-piko", pikoRemotePackage); err != nil {
		t.Fatalf("ensureHelperBinary: %v", err)
	}

	target := filepath.Join(packageDir, "bin", name)
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("helper not planted: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("planted helper is empty")
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("planted helper is not executable: %v", info.Mode())
	}
}

func TestExtractTarGzStripsTopLevelDirectory(t *testing.T) {
	// A real Node tarball wraps everything in `node-vX-os-arch/`; extraction
	// strips exactly that, which is why the caller must extract into a
	// version-named directory rather than straight into the install root.
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)

	files := map[string]string{
		"node-v1-linux-x64/bin/node":    "#!/bin/sh\necho v1\n",
		"node-v1-linux-x64/README.md":   "readme",
		"node-v1-linux-x64/lib/node.js": "// lib",
	}
	for name, body := range files {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if err := extractTarGz(bytes.NewReader(buffer.Bytes()), dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	for _, want := range []string{"bin/node", "README.md", "lib/node.js"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(want))); err != nil {
			t.Errorf("expected %s in the extraction root: %v", want, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "node-v1-linux-x64")); !os.IsNotExist(err) {
		t.Error("the top-level directory should have been stripped")
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestStripTopLevelAndSafeJoin(t *testing.T) {
	if name, ok := stripTopLevel("node-v24.19.0-linux-x64/bin/node"); !ok || name != "bin/node" {
		t.Fatalf("stripTopLevel = %q, %v", name, ok)
	}
	if _, ok := stripTopLevel("toplevelonly"); ok {
		t.Fatal("a single-element name has nothing to strip")
	}

	if _, err := safeJoin("/tmp/dest", "bin/node"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := safeJoin("/tmp/dest", "../../etc/passwd"); err == nil {
		t.Fatal("expected traversal to be refused")
	}
}

func TestSaveAndLoadState(t *testing.T) {
	dir := t.TempDir()
	path := statePath(dir)
	state := runState{
		Profile:   "dsh-piko",
		PID:       1234,
		PGID:      1234,
		LocalURL:  "http://127.0.0.1:3080/?token=abc",
		RemoteURL: "https://dsh-demo.clauded.friddle.me/",
		AuthUser:  "user",
		AuthPass:  "pass",
		Token:     "abc",
	}
	if err := saveState(path, state); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("state mode = %o, want 600 (it holds a live token and credentials)", mode)
	}

	loaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != state {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", loaded, state)
	}
}

func TestWithPathReplacesExistingPath(t *testing.T) {
	env := []string{"HOME=/home/u", "PATH=/usr/bin:/bin", "LANG=C"}
	got := withPath(env, "/opt/node/bin")

	pathEntries := 0
	for _, entry := range got {
		if strings.HasPrefix(entry, "PATH=") {
			pathEntries++
			if !strings.HasPrefix(entry, "PATH=/opt/node/bin"+string(os.PathListSeparator)) {
				t.Fatalf("managed node is not first on PATH: %q", entry)
			}
		}
	}
	if pathEntries != 1 {
		t.Fatalf("found %d PATH entries, want exactly 1", pathEntries)
	}
	if len(got) != len(env) {
		t.Fatalf("entry count changed: %d -> %d", len(env), len(got))
	}
}

func TestBuildChildEnv(t *testing.T) {
	base := []string{"HOME=/home/u", "PATH=/usr/bin:/bin", "DEEPSEEK_API_KEY=old"}

	t.Run("layers KEY=VALUE over the base environment", func(t *testing.T) {
		env, err := buildChildEnv(base, []string{"DEEPSEEK_API_KEY=sk-new", "EXTRA=1"}, "/opt/node/bin")
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(env, "\n")
		if strings.Contains(joined, "DEEPSEEK_API_KEY=old") {
			t.Error("the previous value should have been replaced")
		}
		if !strings.Contains(joined, "DEEPSEEK_API_KEY=sk-new") || !strings.Contains(joined, "EXTRA=1") {
			t.Errorf("overrides missing:\n%s", joined)
		}
		if !strings.Contains(joined, "PATH=/opt/node/bin"+string(os.PathListSeparator)+"/usr/bin:/bin") {
			t.Errorf("managed node must lead PATH:\n%s", joined)
		}
	})

	t.Run("refuses malformed entries and a PATH override", func(t *testing.T) {
		if _, err := buildChildEnv(base, []string{"NOEQUALS"}, "/opt/node/bin"); err == nil {
			t.Error("expected a malformed entry to be refused")
		}
		if _, err := buildChildEnv(base, []string{"PATH=/evil"}, "/opt/node/bin"); err == nil {
			t.Error("expected PATH to be protected")
		}
	})

	t.Run("without overrides it only fixes PATH", func(t *testing.T) {
		env, err := buildChildEnv(base, nil, "/opt/node/bin")
		if err != nil {
			t.Fatal(err)
		}
		if len(env) != len(base) {
			t.Fatalf("entry count changed: %d -> %d", len(base), len(env))
		}
	})
}

func TestOverlayCarriesFixedCredentials(t *testing.T) {
	body, err := renderOverlay(overlayConfig{
		Remote:        "https://clauded.friddle.me",
		BasicAuth:     true,
		BasicAuthUser: "friddle",
		BasicAuthPass: "sybran_20250807",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "basicAuthUser: friddle") || !strings.Contains(text, "basicAuthPass: sybran_20250807") {
		t.Fatalf("fixed credentials missing from the overlay:\n%s", text)
	}

	// Unset credentials must stay out of the overlay entirely, so the helper's
	// random generation remains the default.
	plain, err := renderOverlay(overlayConfig{Remote: "https://x", BasicAuth: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "basicAuthUser") || strings.Contains(string(plain), "basicAuthPass") {
		t.Fatalf("empty credentials should be omitted:\n%s", plain)
	}
}

func TestDefaultPlugins(t *testing.T) {
	if got := defaultPlugins(nil); len(got) != 1 || got[0] != defaultRemotePlugin {
		t.Fatalf("defaultPlugins(nil) = %v", got)
	}
	if got := defaultPlugins([]string{"owner/repo"}); len(got) != 1 || got[0] != "owner/repo" {
		t.Fatalf("explicit plugins must win, got %v", got)
	}
}

func TestEnsureProfileCreatesFromTemplate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)

	runner := &fakeRunner{onRun: func(_ string, args []string) error {
		// Simulate `dsh --profile X --from-default-profile web --dump-config`.
		if !contains(args, "--dump-config") {
			return nil
		}
		dir := filepath.Join(home, "profiles", "dsh-piko")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{}}`), 0o644)
	}}

	if err := ensureProfile(context.Background(), testLogger(), runner, "dsh", "dsh-piko"); err != nil {
		t.Fatal(err)
	}
	if !runner.ran("--from-default-profile web") {
		t.Fatalf("expected the profile to be initialised, calls: %v", runner.calls)
	}

	// A second call must be a no-op now that the profile exists.
	before := len(runner.calls)
	if err := ensureProfile(context.Background(), testLogger(), runner, "dsh", "dsh-piko"); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != before {
		t.Fatal("an existing profile must not be re-initialised")
	}
}

func TestInstallPluginsDetectsNewDependencies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)

	profileDir := filepath.Join(home, "profiles", "dsh-piko")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifest := func(deps map[string]string) {
		body, err := json.Marshal(map[string]any{"dependencies": deps})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profileDir, "package.json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(map[string]string{})

	runner := &fakeRunner{onRun: func(name string, args []string) error {
		if name != "dsh" || !contains(args, "add") {
			return nil
		}
		// Simulate pnpm resolving a GitHub spec to the package's real name.
		writeManifest(map[string]string{pikoRemotePackage: "github:friddle/dsh-plugin-piko-remote"})
		return nil
	}}

	plugins, err := expandPluginSpecs([]string{"friddle/dsh-plugin-piko-remote"})
	if err != nil {
		t.Fatal(err)
	}
	installed, err := installPlugins(context.Background(), testLogger(), runner, "dsh", "dsh-piko", plugins)
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0] != pikoRemotePackage {
		t.Fatalf("installed = %v, want [%s]", installed, pikoRemotePackage)
	}
	if !runner.ran("plugin --profile dsh-piko add github:friddle/dsh-plugin-piko-remote") {
		t.Fatalf("unexpected calls: %v", runner.calls)
	}
}

func TestSatisfyPeersInstallsMissingPeer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DSH_HOME", home)

	modules := filepath.Join(home, "profiles", "dsh-piko", "node_modules")
	pluginDir := filepath.Join(modules, pikoRemotePackage)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"dsh-plugin-piko-remote","peerDependencies":{"@deepseek-ai/dsh-tools":"^0.1.5-rc.1"}}`
	if err := os.WriteFile(filepath.Join(pluginDir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{}
	if err := satisfyPeers(context.Background(), testLogger(), runner, "dsh", "dsh-piko", []string{pikoRemotePackage}); err != nil {
		t.Fatal(err)
	}
	if !runner.ran("add @deepseek-ai/dsh-tools@^0.1.5-rc.1") {
		t.Fatalf("expected the missing peer to be installed, calls: %v", runner.calls)
	}

	// Once the peer exists, nothing more should be installed.
	if err := os.MkdirAll(filepath.Join(modules, "@deepseek-ai", "dsh-tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner.calls = nil
	if err := satisfyPeers(context.Background(), testLogger(), runner, "dsh", "dsh-piko", []string{pikoRemotePackage}); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("a satisfied peer must not be reinstalled: %v", runner.calls)
	}
}

func TestFilterBwrapProfileArgs(t *testing.T) {
	got := filterBwrapProfileArgs([]string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--unshare-pid",
		"--proc", "/proc",
		"--die-with-parent",
		"--tmpfs", "/tmp",
		"--bind", "/work", "/work",
		"--",
		"/bin/bash", "-c", "--unshare-pid --proc /proc",
	})
	want := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--die-with-parent",
		"--tmpfs", "/tmp",
		"--bind", "/work", "/work",
		"--",
		"/bin/bash", "-c", "--unshare-pid --proc /proc",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("filtered profile = %v, want %v", got, want)
	}

	// Everything after `--` belongs to the command and must survive verbatim.
	tail := filterBwrapProfileArgs([]string{"--", "--unshare-pid", "--proc", "/proc"})
	if strings.Join(tail, " ") != "-- --unshare-pid --proc /proc" {
		t.Fatalf("command arguments were rewritten: %v", tail)
	}
}

func TestResolveSandboxRunner(t *testing.T) {
	if runner, err := resolveSandboxRunner(testLogger(), sandboxRunnerNative, time.Second); err != nil || runner != nil {
		t.Fatalf("native = %v, %v; want the built-in chain untouched", runner, err)
	}
	if _, err := resolveSandboxRunner(testLogger(), "bwrap-noproc-lol", time.Second); err == nil {
		t.Fatal("an unknown mode must be refused")
	}
	// `auto` only installs the adapter when bwrap runs the reduced profile but
	// not DSH's own; on a host without bwrap it must stay out of the way.
	if runner, err := resolveSandboxRunner(testLogger(), sandboxRunnerAuto, time.Second); err != nil {
		t.Fatalf("auto: %v", err)
	} else if runner != nil {
		if len(runner.Command) != 2 || runner.Command[1] != sandboxRunnerSubcommand {
			t.Fatalf("auto adapter = %v", runner)
		}
	}
}

func TestAdapterRunnerKeepsDSHSignatures(t *testing.T) {
	runner, err := adapterRunner()
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.FailureSignatures) == 0 {
		t.Fatal("a custom runner needs failure signatures: DSH rejects the row without them")
	}
	// The denial text bwrap prints for a blocked write must stay recognizable;
	// DSH's own list for a custom runner is fixed, so only the fatal list is ours.
	if strings.Join(runner.FailureSignatures, " ") != "bwrap: " {
		t.Fatalf("failure signatures = %v", runner.FailureSignatures)
	}
}

func TestRenderOverlayAddsTheSandboxAdapter(t *testing.T) {
	body, err := renderOverlay(overlayConfig{Remote: "https://x"}, &sandboxRunner{
		Command:           []string{"/opt/dsh-piko-remote", sandboxRunnerSubcommand},
		FailureSignatures: []string{"bwrap: "},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"id: piko-remote",
		"id: sandbox",
		"runnerCommand:",
		"- /opt/dsh-piko-remote",
		"- sandbox-runner",
		"runnerFailureSignatures:",
		"- 'bwrap: '",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("overlay is missing %q:\n%s", want, text)
		}
	}
}
