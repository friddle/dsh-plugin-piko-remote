#!/usr/bin/env bash
#
# End-to-end smoke test for the piko-expose helper.
#
# It starts a throwaway HTTP target, points the helper at it through the real
# piko server, and then fetches the public URL twice: once without credentials
# (must be 401) and once with them (must be the marker). That exercises the
# whole chain — upstream connection, endpoint routing, prefix/host rewriting,
# Basic Auth — rather than just the process starting up.
#
# Usage:
#   scripts/smoke-test.sh
#   PIKO_REMOTE=https://clauded.friddle.me ENDPOINT=smoke-abc123 scripts/smoke-test.sh
#
# Requirements: python3 (target server), curl, and a built helper in bin/.

set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "unsupported OS: $(uname -s)" >&2; exit 2 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 2 ;;
esac

helper="${HELPER:-$root/bin/piko-expose-${os}-${arch}}"
remote="${PIKO_REMOTE:-https://clauded.friddle.me}"
# Random suffix via python3 rather than `tr </dev/urandom | head`, which trips
# SIGPIPE under `set -o pipefail` and kills the script before it prints anything.
endpoint="${ENDPOINT:-smoke-$(python3 -c 'import random,string; print("".join(random.choices(string.ascii_lowercase+string.digits,k=6)))')}"
marker="piko-expose-smoke-$$"
target_port="${TARGET_PORT:-18080}"

if [ ! -x "$helper" ]; then
  echo "helper not found at $helper — run scripts/build-helper.sh first" >&2
  exit 2
fi

workdir="$(mktemp -d)"
helper_log="$workdir/helper.jsonl"
helper_pid=""
target_pid=""

cleanup() {
  [ -n "$helper_pid" ] && kill "$helper_pid" 2>/dev/null || true
  [ -n "$target_pid" ] && kill "$target_pid" 2>/dev/null || true
  rm -rf "$workdir"
}
trap cleanup EXIT

echo "$marker" >"$workdir/index.html"
( cd "$workdir" && exec python3 -m http.server "$target_port" --bind 127.0.0.1 ) >"$workdir/target.log" 2>&1 &
target_pid=$!

# Wait for the target to accept connections before the helper dials it.
for _ in $(seq 1 50); do
  if curl -fsS -m 1 -o /dev/null "http://127.0.0.1:${target_port}/" 2>/dev/null; then break; fi
  sleep 0.1
done

"$helper" \
  --remote "$remote" \
  --endpoint "$endpoint" \
  --target "127.0.0.1:${target_port}" \
  --json >"$helper_log" 2>"$workdir/helper.err" &
helper_pid=$!

ready=""
for _ in $(seq 1 100); do
  if grep -q '"event":"ready"' "$helper_log" 2>/dev/null; then
    ready=$(grep -m1 '"event":"ready"' "$helper_log")
    break
  fi
  if ! kill -0 "$helper_pid" 2>/dev/null; then
    echo "helper exited early:" >&2
    cat "$helper_log" "$workdir/helper.err" >&2
    exit 1
  fi
  sleep 0.1
done

if [ -z "$ready" ]; then
  echo "helper never reported ready:" >&2
  cat "$helper_log" "$workdir/helper.err" >&2
  exit 1
fi

public_url=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["remoteUrl"])' "$ready")
auth_line=""
for _ in $(seq 1 50); do
  if grep -q '"event":"auth"' "$helper_log" 2>/dev/null; then
    auth_line=$(grep -m1 '"event":"auth"' "$helper_log")
    break
  fi
  sleep 0.1
done
auth_user=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["user"])' "$auth_line")
auth_pass=$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["pass"])' "$auth_line")

echo "endpoint   $endpoint"
echo "public url $public_url"
echo "auth       $auth_user / $auth_pass"

fail=0

# The endpoint is registered server-side the moment the upstream connects, but
# give the edge a moment before the first fetch.
status=""
for _ in $(seq 1 50); do
  status=$(curl -sS -m 10 -o "$workdir/anon.html" -w '%{http_code}' "$public_url" 2>/dev/null || true)
  [ "$status" = "401" ] && break
  sleep 0.2
done

if [ "$status" = "401" ]; then
  echo "PASS  unauthenticated request is rejected with 401"
else
  echo "FAIL  unauthenticated request returned ${status:-no response}, want 401"
  fail=1
fi

status=$(curl -sS -m 15 -u "$auth_user:$auth_pass" -o "$workdir/auth.html" -w '%{http_code}' "$public_url" 2>/dev/null || true)
if [ "$status" = "200" ] && grep -q "$marker" "$workdir/auth.html"; then
  echo "PASS  authenticated request reaches the target through the tunnel"
else
  echo "FAIL  authenticated request returned ${status:-no response} (body follows)"
  head -c 300 "$workdir/auth.html" 2>/dev/null || true
  echo
  fail=1
fi

# Path passthrough: subdomain mode must not strip or prefix anything.
status=$(curl -sS -m 15 -u "$auth_user:$auth_pass" -o /dev/null -w '%{http_code}' "${public_url}index.html" 2>/dev/null || true)
if [ "$status" = "200" ]; then
  echo "PASS  path passthrough works"
else
  echo "FAIL  ${public_url}index.html returned ${status:-no response}, want 200"
  fail=1
fi

exit "$fail"
