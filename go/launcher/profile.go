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

// satisfyPeers installs any unresolvable DSH peer dependency of the given
// packages.
//
// This is not theoretical: the profile ships with `autoInstallPeers: false`, so
// a plugin that imports `@deepseek-ai/dsh-tools` fails to load with
// ERR_MODULE_NOT_FOUND until that peer is installed explicitly.
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
			log.info("installing missing peer %s@%s required by %s", peer, constraint, pkg)
			spec := peer + "@" + strings.TrimPrefix(constraint, "npm:")
			if _, err := runner.run(ctx, log, dshPath, "plugin", "--profile", profile, "add", spec); err != nil {
				// Not fatal: the plugin may not need that peer at runtime, and
				// its own load error is the better diagnostic.
				log.warn("could not install peer %s: %v", spec, err)
			}
		}
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

// renderOverlay renders the boot overlay.
func renderOverlay(config overlayConfig) ([]byte, error) {
	body, err := yaml.Marshal([]overlayRow{{ID: "piko-remote", Config: config}})
	if err != nil {
		return nil, err
	}
	header := "# Written by dsh-piko-remote. Applied as a --patch overlay at boot;\n" +
		"# the profile's own cordis.patch.yml is left untouched.\n"
	return append([]byte(header), body...), nil
}
