package main

// DSH install layout: the profile, its plugins, the helper binary, and the
// boot overlay.
//
// The launcher deliberately does NOT edit the user's own cordis.patch.yml. DSH
// accepts repeatable `--patch` overlays that are applied after the profile
// layer, so the tunnel configuration is written to a file the launcher owns and
// passed at boot. That keeps the user's file — comments and all — untouched,
// and makes the launcher's contribution obvious and removable.

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// launcherAssets carries piko-expose for this platform when the build script
// prepared it. `go build ./...` without that step still compiles: the directory
// always contains at least its README.
//
//go:embed assets
var launcherAssets embed.FS

// pikoRemotePackage is the npm package name of the plugin this launcher is
// built around.
const pikoRemotePackage = "dsh-plugin-piko-remote"

// defaultProfileName avoids colliding with the shipped `web` profile and with
// profiles a user may already run.
const defaultProfileName = "dsh-piko"

// dshHome resolves the DSH home directory, honouring $DSH_HOME.
func dshHome() (string, error) {
	if value := os.Getenv("DSH_HOME"); value != "" {
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".dsh"), nil
}

// profilePath is $DSH_HOME/profiles/<name>.
func profilePath(home, profile string) string {
	return filepath.Join(home, "profiles", profile)
}

// ensureProfile initialises the profile from the shipped `web` template when it
// does not exist yet. `--dump-config` composes the profile and exits, which is
// exactly "initialise but do not boot".
func ensureProfile(ctx context.Context, log *logger, runner commandRunner, dshPath, profile string) error {
	home, err := dshHome()
	if err != nil {
		return err
	}
	dir := profilePath(home, profile)
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		log.info("profile %q already exists at %s", profile, dir)
		return nil
	}

	log.info("creating profile %q from the shipped web template", profile)
	if _, err := runner.run(ctx, log, dshPath, "--profile", profile, "--from-default-profile", "web", "--dump-config"); err != nil {
		return fmt.Errorf("initialise profile %q: %w", profile, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return fmt.Errorf("profile %q was not created at %s", profile, dir)
	}
	return nil
}

// profileDependencies reads the profile's declared dependencies.
func profileDependencies(profile string) (map[string]string, error) {
	home, err := dshHome()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(profilePath(home, profile), "package.json"))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("parse profile package.json: %w", err)
	}
	if manifest.Dependencies == nil {
		manifest.Dependencies = map[string]string{}
	}
	return manifest.Dependencies, nil
}

// installPlugins installs every spec and returns the package names that
// appeared as a result.
//
// The names come from diffing the profile's dependency list rather than from
// parsing the spec: a GitHub spec installs under whatever name the repository's
// package.json declares, which the spec does not necessarily spell.
func installPlugins(ctx context.Context, log *logger, runner commandRunner, dshPath, profile string, plugins []expandedPlugin) ([]string, error) {
	before, err := profileDependencies(profile)
	if err != nil {
		return nil, err
	}

	for _, plugin := range plugins {
		log.info("installing plugin %s (%s)", plugin.Spec, plugin.Kind)
		if _, err := runner.run(ctx, log, dshPath, "plugin", "--profile", profile, "add", plugin.Spec); err != nil {
			return nil, fmt.Errorf("install plugin %s: %w", plugin.Spec, err)
		}
	}

	after, err := profileDependencies(profile)
	if err != nil {
		return nil, err
	}
	var installed []string
	for name := range after {
		if _, existed := before[name]; !existed {
			installed = append(installed, name)
		}
	}
	return installed, nil
}

// satisfyPeers provides any unresolvable DSH peer dependency of the given
// packages.
//
// This is not theoretical: the profile ships with `autoInstallPeers: false`, so
// a plugin that imports `@deepseek-ai/dsh-tools` fails to load with
// ERR_MODULE_NOT_FOUND until that peer is present.
//
// A peer the harness itself ships is linked, never installed: see
// linkHarnessPackages for why a second copy of a harness package is a
// correctness bug rather than a versioning preference.
func satisfyPeers(ctx context.Context, log *logger, runner commandRunner, dshPath, profile string, packages []string) error {
	home, err := dshHome()
	if err != nil {
		return err
	}
	modules := filepath.Join(profilePath(home, profile), "node_modules")

	for _, pkg := range packages {
		manifestPath := filepath.Join(modules, filepath.FromSlash(pkg), "package.json")
		raw, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var manifest struct {
			PeerDependencies map[string]string `json:"peerDependencies"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			log.warn("could not read %s: %v", manifestPath, err)
			continue
		}
		for peer, constraint := range manifest.PeerDependencies {
			if _, err := os.Stat(filepath.Join(modules, filepath.FromSlash(peer))); err == nil {
				continue
			}
			spec := peer + "@" + strings.TrimPrefix(constraint, "npm:")
			if shipped := harnessPackageDir(dshPath, peer); shipped != "" {
				spec = "link:" + shipped
				log.info("linking peer %s required by %s to the copy shipped inside dsh", peer, pkg)
			} else {
				log.info("installing missing peer %s@%s required by %s", peer, constraint, pkg)
			}
			if _, err := runner.run(ctx, log, dshPath, "plugin", "--profile", profile, "add", spec); err != nil {
				// Not fatal: the plugin may not need that peer at runtime, and
				// its own load error is the better diagnostic.
				log.warn("could not provide peer %s: %v", spec, err)
			}
		}
	}
	return nil
}

// dshPackageName is the harness package itself, used to walk a `bin/dsh` entry
// back to the install that owns it.
const dshPackageName = "@deepseek-ai/dsh"

// harnessPackageRoot resolves the install directory of the dsh package behind
// dshPath, or "" when it cannot be identified.
//
// Both layouts the launcher produces are covered: a `bin/dsh` that lives inside
// the package (so EvalSymlinks walks into it), and the npm global prefix shape
// where the package sits at <prefix>/lib/node_modules/@deepseek-ai/dsh.
func harnessPackageRoot(dshPath string) string {
	starts := make([]string, 0, 2)
	if resolved, err := filepath.EvalSymlinks(dshPath); err == nil {
		starts = append(starts, filepath.Dir(resolved))
	}
	starts = append(starts, filepath.Dir(dshPath))

	for _, start := range starts {
		dir := start
		for depth := 0; depth < 8; depth++ {
			if readPackageName(filepath.Join(dir, "package.json")) == dshPackageName {
				return dir
			}
			for _, candidate := range []string{
				filepath.Join(dir, "lib", "node_modules", "@deepseek-ai", "dsh"),
				filepath.Join(dir, "node_modules", "@deepseek-ai", "dsh"),
			} {
				if readPackageName(filepath.Join(candidate, "package.json")) == dshPackageName {
					return candidate
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return ""
}

// harnessPackageDir is the harness's own copy of pkg, or "" when the harness
// does not ship it.
func harnessPackageDir(dshPath, pkg string) string {
	root := harnessPackageRoot(dshPath)
	if root == "" {
		return ""
	}
	dir := filepath.Join(root, "node_modules", filepath.FromSlash(pkg))
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return ""
}

// readPackageName returns the "name" field of a package.json, or "" when the
// file is missing or unreadable.
func readPackageName(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return ""
	}
	return manifest.Name
}

// resolvesTo reports whether path already resolves to the same directory as
// target.
func resolvesTo(path, target string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	expected, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	return resolved == expected
}

// setProfileDependency rewrites one dependency spec in the profile manifest,
// preserving every other field.
func setProfileDependency(profile, pkg, spec string) error {
	home, err := dshHome()
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(profilePath(home, profile), "package.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("parse profile package.json: %w", err)
	}
	dependencies, _ := manifest["dependencies"].(map[string]any)
	if dependencies == nil {
		dependencies = map[string]any{}
	}
	dependencies[pkg] = spec
	manifest["dependencies"] = dependencies
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, append(body, '\n'), 0o644)
}

// linkHarnessPackages replaces profile copies of packages the harness itself
// ships with symlinks to the harness's copies.
//
// This is a correctness requirement, not housekeeping. Harness packages pass
// values across module boundaries by symbol: `@deepseek-ai/dsh-tools` hands the
// agent loop a `Symbol()` key for its execution scheduler. Node keys module
// identity by resolved path, so a second copy of that package in the profile is
// a second symbol — the agent loop then reads an undefined scheduler and every
// tool call fails with `Cannot read properties of undefined (reading
// 'prepare')`. Linking keeps one physical copy, and therefore one symbol, per
// package for the whole process.
//
// The pass also repairs profiles an earlier launcher left with copies, which is
// why it scans the installed tree instead of trusting the dependency list.
func linkHarnessPackages(log *logger, dshPath, profile string) error {
	root := harnessPackageRoot(dshPath)
	if root == "" {
		log.warn("cannot locate the dsh install behind %s; leaving profile copies of harness packages in place", dshPath)
		return nil
	}
	home, err := dshHome()
	if err != nil {
		return err
	}
	scope := filepath.Join(profilePath(home, profile), "node_modules", "@deepseek-ai")
	entries, err := os.ReadDir(scope)
	if err != nil {
		return nil // no scoped dependencies in this profile
	}

	for _, entry := range entries {
		name := entry.Name()
		source := filepath.Join(root, "node_modules", "@deepseek-ai", name)
		if info, err := os.Stat(source); err != nil || !info.IsDir() {
			continue
		}
		target := filepath.Join(scope, name)
		if resolvesTo(target, source) {
			continue
		}
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("remove duplicate %s: %w", name, err)
		}
		if err := os.Symlink(source, target); err != nil {
			return fmt.Errorf("link %s: %w", name, err)
		}
		pkg := "@deepseek-ai/" + name
		if err := setProfileDependency(profile, pkg, "link:"+source); err != nil {
			log.warn("linked %s but could not rewrite the profile manifest: %v", pkg, err)
			continue
		}
		log.info("linked %s to the copy shipped inside dsh", pkg)
	}
	return nil
}

// helperFileName is the platform-specific piko-expose name used in bin/.
func helperFileName() (string, error) {
	var osName string
	switch goos() {
	case "linux":
		osName = "linux"
	case "darwin":
		osName = "darwin"
	case "windows":
		osName = "windows"
	default:
		return "", fmt.Errorf("no piko-expose build is defined for %s", goos())
	}
	var arch string
	switch goarch() {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("no piko-expose build is defined for %s", goarch())
	}
	name := fmt.Sprintf("piko-expose-%s-%s", osName, arch)
	if osName == "windows" {
		name += ".exe"
	}
	return name, nil
}

// ensureHelperBinary plants piko-expose in the installed plugin when it is
// missing.
//
// A GitHub or npm install of the plugin does not carry bin/ — the binaries are
// build output — so without this step `dsh plugin add friddle/...` produces a
// plugin that loads and then cannot open a tunnel.
func ensureHelperBinary(log *logger, profile, packageName string) error {
	name, err := helperFileName()
	if err != nil {
		return err
	}
	home, err := dshHome()
	if err != nil {
		return err
	}
	packageDir := filepath.Join(profilePath(home, profile), "node_modules", filepath.FromSlash(packageName))
	if _, err := os.Stat(packageDir); err != nil {
		return fmt.Errorf("plugin %s is not installed in profile %s", packageName, profile)
	}

	target := filepath.Join(packageDir, "bin", name)
	if info, err := os.Stat(target); err == nil && info.Size() > 0 {
		log.info("helper already present at %s", target)
		return nil
	}

	data, source, err := helperBinaries(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(target, data, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	log.info("planted piko-expose (%s, %d bytes) at %s", source, len(data), target)
	return nil
}

// helperBinaries finds piko-expose for this platform: embedded in the launcher
// first, then next to the launcher binary.
func helperBinaries(name string) ([]byte, string, error) {
	if data, err := launcherAssets.ReadFile("assets/" + name); err == nil && len(data) > 0 {
		return data, "embedded", nil
	}

	if executable, err := os.Executable(); err == nil {
		dir := filepath.Dir(executable)
		candidates := []string{
			filepath.Join(dir, name),
			filepath.Join(dir, "..", "bin", name),
		}
		for _, candidate := range candidates {
			if data, err := os.ReadFile(candidate); err == nil && len(data) > 0 {
				return data, candidate, nil
			}
		}
	}

	return nil, "", fmt.Errorf(
		"cannot find %s to install into the plugin: this launcher carries no embedded build for this platform, "+
			"and there is no copy next to it. Build the pair with scripts/build-helper.sh", name)
}

// overlayRow is one loader patch entry: a config override targeting the row id
// the plugin's own cordis.patch.yml inserts.
type overlayRow struct {
	ID     string        `yaml:"id"`
	Config overlayConfig `yaml:"config"`
}

// overlayConfig is the full plugin config. DSH replaces a targeted row's config
// wholesale rather than merging, so every key the launcher cares about is
// written explicitly.
type overlayConfig struct {
	Remote            string `yaml:"remote"`
	EndpointPrefix    string `yaml:"endpointPrefix"`
	BasicAuth         bool   `yaml:"basicAuth"`
	BasicAuthUser     string `yaml:"basicAuthUser,omitempty"`
	BasicAuthPass     string `yaml:"basicAuthPass,omitempty"`
	URLMode           string `yaml:"urlMode"`
	PreserveHost      bool   `yaml:"preserveHost"`
	AllowDshUiExpose  bool   `yaml:"allowDshUiExpose"`
	AutoExpose        bool   `yaml:"autoExpose"`
	DefaultTTLMinutes int    `yaml:"defaultTtlMinutes"`
	CredentialsFile   string `yaml:"credentialsFile"`
	Endpoint          string `yaml:"endpoint,omitempty"`
}

// overlaySandboxRow routes confinement through an operator-supplied runner. DSH
// replaces a targeted row's whole config, so the row restates every key the
// adapter owns; the omitted ones keep their schema defaults.
type overlaySandboxRow struct {
	ID     string        `yaml:"id"`
	Config sandboxRunner `yaml:"config"`
}

// renderOverlay renders the boot overlay. adapter, when non-nil, is the sandbox
// runner resolved from --sandbox-runner.
func renderOverlay(config overlayConfig, adapter *sandboxRunner) ([]byte, error) {
	rows := []any{overlayRow{ID: "piko-remote", Config: config}}
	if adapter != nil {
		rows = append(rows, overlaySandboxRow{ID: "sandbox", Config: *adapter})
	}
	body, err := yaml.Marshal(rows)
	if err != nil {
		return nil, err
	}
	header := "# Written by dsh-piko-remote. Applied as a --patch overlay at boot;\n" +
		"# the profile's own cordis.patch.yml is left untouched.\n"
	if adapter != nil {
		header += "# The sandbox row runs bwrap through this launcher: same file policy, no PID namespace.\n"
	}
	return append([]byte(header), body...), nil
}
