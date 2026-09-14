package main

// Node and package-manager installation.
//
// DSH needs Node ^22.19.0 || >=24.0.0. Rather than trust whatever `node` is on
// PATH, the launcher checks the version and, when nothing suitable exists,
// downloads a Node release into its own data directory and installs `dsh` and
// `pnpm` into that private prefix. Nothing outside that directory is touched,
// and a Node the user already has is reused when it is good enough.

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// defaultNodeVersion is the release the launcher installs when it has to
// provide Node itself. It is a version known to satisfy DSH's engine range.
const defaultNodeVersion = "v24.19.0"

// nodeMirrors are tried in order. The npmmirror one exists because the common
// failure mode for this install is a Chinese network that cannot reach
// nodejs.org — which is exactly where the launcher is most useful.
var nodeMirrors = []string{
	"https://npmmirror.com/mirrors/node",
	"https://nodejs.org/dist",
}

var nodeVersionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// parseNodeVersion extracts the numeric parts of a `node -v` string.
func parseNodeVersion(output string) (major, minor, patch int, err error) {
	match := nodeVersionRE.FindStringSubmatch(strings.TrimSpace(output))
	if match == nil {
		return 0, 0, 0, fmt.Errorf("unrecognised node version %q", strings.TrimSpace(output))
	}
	if major, err = strconv.Atoi(match[1]); err != nil {
		return 0, 0, 0, err
	}
	if minor, err = strconv.Atoi(match[2]); err != nil {
		return 0, 0, 0, err
	}
	if patch, err = strconv.Atoi(match[3]); err != nil {
		return 0, 0, 0, err
	}
	return major, minor, patch, nil
}

// nodeVersionSupported mirrors DSH's `engines` field: ^22.19.0 || >=24.0.0.
func nodeVersionSupported(output string) bool {
	major, minor, _, err := parseNodeVersion(output)
	if err != nil {
		return false
	}
	switch {
	case major == 22:
		return minor >= 19
	case major >= 24:
		return true
	default:
		return false
	}
}

// nodeReleaseName is the platform tag used in Node's download file names.
func nodeReleaseName() (string, error) {
	var osName string
	switch runtime.GOOS {
	case "linux":
		osName = "linux"
	case "darwin":
		osName = "darwin"
	case "windows":
		osName = "win"
	default:
		return "", fmt.Errorf("no Node release naming is defined for %s", runtime.GOOS)
	}
	var arch string
	switch runtime.GOARCH {
	case "amd64":
		arch = "x64"
	case "arm64":
		arch = "arm64"
	case "386":
		arch = "x86"
	default:
		return "", fmt.Errorf("no Node release naming is defined for %s", runtime.GOARCH)
	}
	return fmt.Sprintf("%s-%s", osName, arch), nil
}

// nodeBinaryName is the executable name inside a Node release.
func nodeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "node.exe"
	}
	return "node"
}

// nodeExecutable returns <nodeDir>/bin/node (or the Windows equivalent).
func nodeExecutable(nodeDir string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(nodeDir, nodeBinaryName())
	}
	return filepath.Join(nodeDir, "bin", nodeBinaryName())
}

// inspectNode runs `<path> -v` and reports whether the version is usable.
func inspectNode(path string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-v").Output()
	if err != nil {
		return "", false
	}
	version := strings.TrimSpace(string(out))
	return version, nodeVersionSupported(version)
}

// findNodeOnPath looks for a usable `node` in PATH.
func findNodeOnPath() (string, string, bool) {
	path, err := exec.LookPath("node")
	if err != nil {
		return "", "", false
	}
	version, ok := inspectNode(path)
	return path, version, ok
}

// normalizeNodeVersion accepts "24.19.0" or "v24.19.0" and returns the "v" form.
func normalizeNodeVersion(version string) string {
	if version == "" {
		return defaultNodeVersion
	}
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

// managedNodeDir is where a managed Node release lives inside the data directory.
func managedNodeDir(dataDir, version string) (string, error) {
	platform, err := nodeReleaseName()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataDir, "node", fmt.Sprintf("node-%s-%s", normalizeNodeVersion(version), platform)), nil
}

// installNode downloads and unpacks a Node release into destDir, returning the
// directory that contains bin/node.
//
// The archive layout differs by platform (tar.gz on POSIX, zip on Windows) and
// every release wraps its payload in a top-level directory, which is stripped
// so callers get a predictable root.
func installNode(ctx context.Context, log *logger, destDir, version string) (string, error) {
	version = normalizeNodeVersion(version)

	platform, err := nodeReleaseName()
	if err != nil {
		return "", err
	}
	extension := "tar.gz"
	if runtime.GOOS == "windows" {
		extension = "zip"
	}
	archive := fmt.Sprintf("node-%s-%s.%s", version, platform, extension)

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", destDir, err)
	}

	// Extraction strips the archive's own top-level directory, so the target has
	// to carry the version. Extracting straight into destDir would scatter bin/,
	// lib/ and include/ at its root, and a second version would merge into the
	// first.
	nodeDir := filepath.Join(destDir, fmt.Sprintf("node-%s-%s", version, platform))
	if err := os.MkdirAll(nodeDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", nodeDir, err)
	}

	var lastErr error
	for _, mirror := range nodeMirrors {
		url := fmt.Sprintf("%s/%s/%s", strings.TrimSuffix(mirror, "/"), version, archive)
		log.info("downloading Node %s from %s", version, url)
		err := downloadAndExtract(ctx, url, nodeDir)
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		log.warn("download from %s failed: %v", mirror, err)
	}
	if lastErr != nil {
		return "", fmt.Errorf("could not download Node %s: %w", version, lastErr)
	}

	binary := nodeExecutable(nodeDir)
	if _, err := os.Stat(binary); err != nil {
		return "", fmt.Errorf("downloaded Node archive did not contain %s", binary)
	}
	return nodeDir, nil
}

// downloadAndExtract fetches url and unpacks it under destDir, stripping the
// archive's single top-level directory.
func downloadAndExtract(ctx context.Context, url, destDir string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 15 * time.Minute}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}

	if strings.HasSuffix(url, ".zip") {
		return extractZip(response.Body, response.ContentLength, destDir)
	}
	return extractTarGz(response.Body, destDir)
}

// stripTopLevel removes the first path element from an archive member name.
func stripTopLevel(name string) (string, bool) {
	cleaned := strings.TrimPrefix(filepath.ToSlash(name), "./")
	index := strings.Index(cleaned, "/")
	if index < 0 {
		return "", false
	}
	rest := cleaned[index+1:]
	if rest == "" {
		return "", false
	}
	return rest, true
}

// safeJoin refuses archive entries that would escape destDir.
func safeJoin(destDir, name string) (string, error) {
	target := filepath.Join(destDir, filepath.FromSlash(name))
	if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}
	return target, nil
}

func extractTarGz(source io.Reader, destDir string) error {
	gzipReader, err := gzip.NewReader(source)
	if err != nil {
		return err
	}
	defer gzipReader.Close()

	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name, ok := stripTopLevel(header.Name)
		if !ok {
			continue
		}
		target, err := safeJoin(destDir, name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(header.Mode)
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, reader); err != nil { //nolint:gosec // trusted release archive
				file.Close()
				return err
			}
			file.Close()
		case tar.TypeSymlink:
			// Node's tarball links bin/npm -> ../lib/node_modules/npm/bin/npm-cli.js
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return err
			}
		}
	}
}

func extractZip(source io.Reader, size int64, destDir string) error {
	// archive/zip needs random access, so the body is buffered to a temp file.
	tempFile, err := os.CreateTemp("", "piko-node-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	if _, err := io.Copy(tempFile, source); err != nil { //nolint:gosec // trusted release archive
		return err
	}
	info, err := tempFile.Stat()
	if err != nil {
		return err
	}

	reader, err := zip.NewReader(tempFile, info.Size())
	if err != nil {
		return err
	}
	for _, entry := range reader.File {
		name, ok := stripTopLevel(entry.Name)
		if !ok {
			continue
		}
		target, err := safeJoin(destDir, name)
		if err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		source, err := entry.Open()
		if err != nil {
			return err
		}
		destination, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			source.Close()
			return err
		}
		if _, err := io.Copy(destination, source); err != nil { //nolint:gosec // trusted release archive
			source.Close()
			destination.Close()
			return err
		}
		source.Close()
		destination.Close()
	}
	return nil
}
