#!/usr/bin/env bash
#
# Cross-compile the Go binaries into bin/.
#
# Two binaries per platform:
#   piko-expose-<os>-<arch>       the tunnel engine the plugin spawns
#   dsh-piko-remote-<os>-<arch>   the one-click launcher
#
# The launcher embeds piko-expose for its own platform, so it can plant the
# helper into a plugin installed from GitHub or npm — where bin/ is absent,
# because binaries are build output. That means the helper has to be built
# *first* for each target and copied into go/launcher/assets/ before the
# launcher is compiled for the same target.
#
# bin/ and the assets copies are git-ignored on purpose: these are build outputs,
# and a release is what ships them.
#
# Usage:
#   scripts/build-helper.sh                  # all default targets
#   TARGETS="linux/amd64" scripts/build-helper.sh
#
# Go needs a writable build cache. If the default one (GOCACHE) is not writable
# — a sandboxed or read-only environment — point it somewhere local:
#   GOCACHE=$PWD/.build/gocache GOMODCACHE=$PWD/.build/gomodcache scripts/build-helper.sh

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$root/bin"
assets="$root/go/launcher/assets"

# Default set: the platforms the plugin and launcher are expected to run on.
TARGETS="${TARGETS:-darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64}"

mkdir -p "$out" "$assets"

export CGO_ENABLED=0
export GOFLAGS="${GOFLAGS:--mod=mod}"

# -trimpath keeps build paths out of the binary, and -s -w drops the symbol
# table: roughly halves the size of binaries that ship in an npm package.
ldflags="-s -w"

helper_name() {
  local os="$1" arch="$2"
  local name="piko-expose-${os}-${arch}"
  [ "$os" = "windows" ] && name="${name}.exe"
  echo "$name"
}

launcher_name() {
  local os="$1" arch="$2"
  local name="dsh-piko-remote-${os}-${arch}"
  [ "$os" = "windows" ] && name="${name}.exe"
  echo "$name"
}

build_helper() {
  local os="$1" arch="$2" name
  name="$(helper_name "$os" "$arch")"
  echo "  piko-expose   ${os}/${arch} -> bin/${name}"
  ( cd "$root/go" && GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$out/$name" . )
}

build_launcher() {
  local os="$1" arch="$2" name
  name="$(launcher_name "$os" "$arch")"
  echo "  launcher      ${os}/${arch} -> bin/${name}"
  # Stage this target's helper where //go:embed can see it.
  rm -f "$assets"/piko-expose-*
  cp "$out/$(helper_name "$os" "$arch")" "$assets/$(helper_name "$os" "$arch")"
  ( cd "$root/go" && GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$out/$name" ./launcher )
}

echo "building into $out"
for target in $TARGETS; do
  os="${target%%/*}"
  arch="${target##*/}"
  build_helper "$os" "$arch"
  build_launcher "$os" "$arch"
done

# Leave assets/ holding this host's helper, so a plain `go build ./...` in go/
# embeds the build that can actually run here.
case "$(uname -s)" in
  Darwin) host_os=darwin ;;
  Linux) host_os=linux ;;
  *) host_os="" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) host_arch=amd64 ;;
  arm64 | aarch64) host_arch=arm64 ;;
  *) host_arch="" ;;
esac

if [ -n "$host_os" ] && [ -n "$host_arch" ]; then
  host_helper="$(helper_name "$host_os" "$host_arch")"
  if [ ! -f "$out/$host_helper" ]; then
    build_helper "$host_os" "$host_arch"
  fi
  rm -f "$assets"/piko-expose-*
  cp "$out/$host_helper" "$assets/$host_helper"
  echo "embedded helper for this host: $host_helper"
fi

echo "done:"
ls -la "$out"
