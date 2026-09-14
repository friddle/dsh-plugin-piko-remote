#!/usr/bin/env bash
#
# Publish this package to the public npm registry.
#
# Two traps this script exists to avoid:
#
#   1. The registry is pinned. A machine whose default registry is a read-only
#      mirror (registry.npmmirror.com is a common one) fails `npm publish` with a
#      confusing auth error, because it is the *mirror* rejecting the write.
#   2. The helper binaries are build output and git-ignored, so a fresh clone
#      publishes a package that cannot open a tunnel. The preflight below refuses
#      to publish an empty bin/.
#
# Usage:
#   npm login --registry https://registry.npmjs.org   # once per machine
#   scripts/publish.sh
#
set -euo pipefail

registry="${NPM_REGISTRY:-https://registry.npmjs.org}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

name="$(node -p "require('./package.json').name")"
version="$(node -p "require('./package.json').version")"

echo "==> preflight: helpers in bin/"
helpers="$(ls bin/piko-expose-* 2>/dev/null || true)"
if [ -z "$helpers" ]; then
  echo "bin/ holds no piko-expose build. Run scripts/build-helper.sh first:" >&2
  echo "  scripts/build-helper.sh            # all default targets" >&2
  echo "  TARGETS=\"linux/amd64\" scripts/build-helper.sh" >&2
  exit 1
fi
echo "$helpers" | sed 's/^/    /'
echo "    (the launcher binaries in bin/ are deliberately excluded by package.json files)"

echo "==> preflight: npm test"
npm test

echo "==> preflight: what would be published"
npm pack --dry-run 2>&1 | grep -E "package size|unpacked size|total files|piko-expose" || true

echo "==> auth"
npm whoami --registry "$registry"

echo "==> publishing $name@$version"
npm publish --registry "$registry" --access public

echo "==> verifying the registry sees it"
npm view "$name@$version" version --registry "$registry"
