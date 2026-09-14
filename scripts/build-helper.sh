#!/usr/bin/env bash
#
# Cross-compile the piko-expose helper into bin/.
#
# The plugin resolves the helper as bin/piko-expose-<os>-<arch>, so the names
# produced here must stay in that shape. bin/ is git-ignored on purpose: the
# binaries are build output, and a release is what ships them.
#
# Usage:
#   scripts/build-helper.sh                  # all default targets
#   TARGETS="linux/amd64" scripts/build-helper.sh
#
# Go needs a writable build cache. If the default one (GOCACHE) is not
# writable — a sandboxed or read-only environment — point it somewhere local:
#   GOCACHE=$PWD/.build/gocache GOMODCACHE=$PWD/.build/gomodcache scripts/build-helper.sh

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$root/bin"

# Default set: the platforms the plugin is expected to run on. windows/amd64 is
# included because DSH supports Windows hosts.
TARGETS="${TARGETS:-darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64}"

mkdir -p "$out"

export CGO_ENABLED=0
export GOFLAGS="${GOFLAGS:--mod=mod}"

# -trimpath keeps build paths out of the binary, and -s -w drops the symbol
# table: roughly halves the size of a binary that ships in an npm package.
ldflags="-s -w"

echo "building piko-expose -> $out"
for target in $TARGETS; do
  os="${target%%/*}"
  arch="${target##*/}"
  name="piko-expose-${os}-${arch}"
  [ "$os" = "windows" ] && name="${name}.exe"

  echo "  ${os}/${arch} -> bin/${name}"
  (
    cd "$root/go"
    GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$out/$name" .
  )
done

echo "done:"
ls -la "$out"
