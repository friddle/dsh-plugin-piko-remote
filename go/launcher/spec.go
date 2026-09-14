package main

// Plugin spec handling.
//
// Everything a user can point the launcher at: a GitHub shorthand, an npm
// package, a local directory or tarball, or a URL. `dsh plugin add` already
// understands npm-style specs, so the only real work here is turning the
// shorthand people actually type — `friddle/dsh-plugin-piko-remote` — into the
// `github:` spec it expects, and refusing the inputs that are ambiguous.

import (
	"fmt"
	"regexp"
	"strings"
)

// pluginKind is what a spec resolves to, kept for logging and for the checks
// that only apply to some kinds (a GitHub install never carries bin/).
type pluginKind string

const (
	kindGitHub pluginKind = "github"
	kindNPM    pluginKind = "npm"
	kindLocal  pluginKind = "local"
	kindURL    pluginKind = "url"
)

// githubShorthand matches `owner/repo` and `owner/repo#ref`. The single slash
// is what separates it from an npm scoped name (`@scope/pkg`) and from a path
// (`./dir`, `/abs/dir`).
var githubShorthand = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(#.+)?$`)

// expandedPlugin is one plugin to install.
type expandedPlugin struct {
	// Raw is what the user typed, kept for messages.
	Raw string
	// Spec is what `dsh plugin add` receives.
	Spec string
	// Kind is how the spec was classified.
	Kind pluginKind
}

// isPikoRemote reports whether this spec installs this repository's plugin,
// which is what decides if the launcher also writes the tunnel overlay and
// plants the helper binary.
func (p expandedPlugin) isPikoRemote() bool {
	return strings.Contains(p.Spec, "piko-remote")
}

// expandPluginSpec classifies and normalises one plugin spec.
func expandPluginSpec(raw string) (expandedPlugin, error) {
	spec := strings.TrimSpace(raw)
	if spec == "" {
		return expandedPlugin{}, fmt.Errorf("empty plugin spec")
	}
	if spec != raw {
		// Whitespace in a spec is always a mistake, but trimming it is what the
		// user meant, so only the trimmed value is used.
		spec = strings.TrimSpace(raw)
	}

	switch {
	case strings.HasPrefix(spec, "github:"):
		return expandedPlugin{Raw: raw, Spec: spec, Kind: kindGitHub}, nil
	case strings.HasPrefix(spec, "git+"):
		return expandedPlugin{Raw: raw, Spec: spec, Kind: kindGitHub}, nil
	case strings.HasPrefix(spec, "@"):
		// An npm scope, e.g. @deepseek-ai/dsh-tools.
		return expandedPlugin{Raw: raw, Spec: spec, Kind: kindNPM}, nil
	case strings.HasPrefix(spec, "./"), strings.HasPrefix(spec, "../"), strings.HasPrefix(spec, "/"),
		strings.HasPrefix(spec, "file:"), strings.HasPrefix(spec, "~/"), strings.HasPrefix(spec, "."+string('/')):
		return expandedPlugin{Raw: raw, Spec: spec, Kind: kindLocal}, nil
	case strings.HasPrefix(spec, "http://"), strings.HasPrefix(spec, "https://"):
		return expandedPlugin{Raw: raw, Spec: spec, Kind: kindURL}, nil
	case githubShorthand.MatchString(spec):
		return expandedPlugin{Raw: raw, Spec: "github:" + spec, Kind: kindGitHub}, nil
	case spec == "." || spec == "..":
		return expandedPlugin{Raw: raw, Spec: spec, Kind: kindLocal}, nil
	default:
		// A plain npm package name (possibly with @version).
		if strings.ContainsAny(spec, " \t") {
			return expandedPlugin{}, fmt.Errorf("invalid plugin spec %q: contains whitespace", raw)
		}
		return expandedPlugin{Raw: raw, Spec: spec, Kind: kindNPM}, nil
	}
}

// expandPluginSpecs expands a list, preserving order and rejecting duplicates.
func expandPluginSpecs(raw []string) ([]expandedPlugin, error) {
	seen := make(map[string]bool, len(raw))
	out := make([]expandedPlugin, 0, len(raw))
	for _, item := range raw {
		plugin, err := expandPluginSpec(item)
		if err != nil {
			return nil, err
		}
		if seen[plugin.Spec] {
			continue
		}
		seen[plugin.Spec] = true
		out = append(out, plugin)
	}
	return out, nil
}
