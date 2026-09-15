#!/usr/bin/env bash
#
# Boot `dsh web` on loopback, capture its per-boot session token, wait until it
# serves the UI, then hand the container over to the reverse proxy (0.0.0.0).
#
# `dsh web` gates *everything* behind a token it prints at boot:
#     dsh web: http://127.0.0.1:3080/?token=<random>
# and answers 401 otherwise. The proxy cannot guess it, so we scrape it here and
# pass it on as DSH_WEB_TOKEN; the proxy then transparently walks browsers
# through `/?token=` once (dsh sets its session cookie and the UI proceeds).
#
# Mounted config (read-only) is copied into place first:
#   $DSH_CONFIG_DIR/.credentials.yaml -> $DSH_HOME/.credentials.yaml
#   $DSH_CONFIG_DIR/settings.yaml     -> $DSH_HOME/settings.yaml
# Mounting the whole secret at $DSH_HOME would hide $DSH_HOME/profiles, which is
# baked into the image, hence the copy.

set -euo pipefail

DSH_HOME="${DSH_HOME:-/root/.dsh}"
DSH_PORT="${DSH_PORT:-3080}"
DSH_PROXY_PORT="${DSH_PROXY_PORT:-8080}"
DSH_CONFIG_DIR="${DSH_CONFIG_DIR:-/etc/dsh-credentials}"
WORKSPACE_DIR="${WORKSPACE_DIR:-/workspace}"
DSH_TRUSTED_HOST="${DSH_TRUSTED_HOST:-}"
DSH_PROXY_BIND="${DSH_PROXY_BIND:-0.0.0.0}"
DSH_LOG="${DSH_LOG:-/tmp/dsh-web.log}"
PATCH_DIR="${PATCH_DIR:-/opt/dsh/patches}"
PATCH_RENDER_DIR="${PATCH_RENDER_DIR:-/tmp/dsh-patches}"
export DSH_HOME DSH_PORT DSH_PROXY_PORT DSH_PROXY_BIND

# --- persistent DSH_HOME -------------------------------------------------
# The image bakes the warmed profile at /root/.dsh, but the container's own
# filesystem is thrown away on every pod restart — which loses sessions/ and
# storages/ and leaves open browser tabs pointing at a session the server no
# longer has (they look "stuck" until a refresh). When DSH_HOME points at a
# mounted volume, seed it once from the baked skeleton and keep using the volume.
DSH_SEED_DIR="${DSH_SEED_DIR:-/root/.dsh}"
if [[ "$DSH_HOME" != "$DSH_SEED_DIR" ]]; then
	mkdir -p "$DSH_HOME"
	# profiles/ is IMAGE-owned (plugins are installed at build time), so it is
	# re-synced from the image on every boot — otherwise an existing volume would
	# permanently shadow newer images and image upgrades would never arrive.
	# sessions/ storages/ and the credential files stay volume-owned: never
	# deleted here.
	if [[ -d "$DSH_SEED_DIR/profiles" ]]; then
		mkdir -p "$DSH_HOME/profiles"
		rsync -a --delete "$DSH_SEED_DIR/profiles/" "$DSH_HOME/profiles/"
		echo "[entrypoint] synced the image profile into $DSH_HOME/profiles"
	fi
fi

mkdir -p "$DSH_HOME" "$WORKSPACE_DIR"

# --- mounted credentials ------------------------------------------------
for f in .credentials.yaml settings.yaml; do
	if [[ -f "$DSH_CONFIG_DIR/$f" ]]; then
		install -m 0600 "$DSH_CONFIG_DIR/$f" "$DSH_HOME/$f"
		echo "[entrypoint] seeded $DSH_HOME/$f from $DSH_CONFIG_DIR/$f"
	fi
done

# --- mounted kubeconfig / ssh sanity -----------------------------------
if [[ -f /root/.kube/config ]]; then
	echo "[entrypoint] kubeconfig: $(grep -c . /root/.kube/config) lines from the mounted secret"
else
	echo "[entrypoint] warning: /root/.kube/config is missing — kubectl will not work" >&2
fi
if [[ -d /root/.ssh ]]; then
	chmod 0700 /root/.ssh 2>/dev/null || true
	chmod 0600 /root/.ssh/* 2>/dev/null || true
fi

# --- dsh web -----------------------------------------------------------
# Invoked as `dsh --profile web …` rather than the `dsh web` alias: the alias
# rejects the parent's --patch flag ("web takes none of parent --patch"), and we
# need --patch for the baked plugin config.
launcher_args=(--profile web)
app_args=(--host 127.0.0.1 --port "$DSH_PORT" --no-open)
if [[ -n "$DSH_TRUSTED_HOST" ]]; then
	IFS=',' read -r -a trusted <<< "$DSH_TRUSTED_HOST"
	for host in "${trusted[@]}"; do
		host="${host#"${host%%[![:space:]]*}"}"
		host="${host%"${host##*[![:space:]]}"}"
		[[ -n "$host" ]] && app_args+=(--trusted-host "$host")
	done
fi

# Render the baked patch overlays (plugin config). The chrome-driverless service
# is the one running in this cluster; manageContainer must stay off (no docker).
DSH_CHROME_BASE_URL="${DSH_CHROME_BASE_URL:-http://chrome-driverless-mp2.management.svc.cluster.local}"
if [[ -d "$PATCH_DIR" ]]; then
	mkdir -p "$PATCH_RENDER_DIR"
	for template in "$PATCH_DIR"/*.yml.tmpl; do
		[[ -e "$template" ]] || continue
		rendered="$PATCH_RENDER_DIR/$(basename "${template%.tmpl}")"
		sed "s|@DSH_CHROME_BASE_URL@|${DSH_CHROME_BASE_URL}|g" "$template" >"$rendered"
		launcher_args+=(--patch "$rendered")
		echo "[entrypoint] patch $(basename "$rendered") (chrome baseUrl=${DSH_CHROME_BASE_URL})"
	done
fi

echo "[entrypoint] starting: dsh ${launcher_args[*]} ${app_args[*]}"
: >"$DSH_LOG"
(cd "$WORKSPACE_DIR" && dsh "${launcher_args[@]}" "${app_args[@]}") >"$DSH_LOG" 2>&1 &
DSH_PID=$!

shutdown() {
	kill "$DSH_PID" "${PROXY_PID:-}" "${TAIL_PID:-}" 2>/dev/null || true
}
trap shutdown TERM INT

echo "[entrypoint] waiting for dsh web on 127.0.0.1:${DSH_PORT} ..."
token=""
ready=0
for _ in $(seq 1 180); do
	if ! kill -0 "$DSH_PID" 2>/dev/null; then
		echo "[entrypoint] dsh exited before becoming ready" >&2
		cat "$DSH_LOG" >&2 || true
		wait "$DSH_PID" || exit $?
	fi
	if [[ -z "$token" ]]; then
		token="$(sed -n 's/.*token=\([A-Za-z0-9._-]*\).*/\1/p' "$DSH_LOG" | head -1)"
	fi
	if [[ -n "$token" ]]; then
		# 401 without the token, 303 with it: both prove the server is up.
		# --noproxy: a proxy env var leaked into the image must not swallow loopback.
		code="$(curl -s -o /dev/null -w '%{http_code}' --noproxy '*' --max-time 5 \
			"http://127.0.0.1:${DSH_PORT}/?token=${token}" || true)"
		if [[ "$code" == 2?? || "$code" == 3?? ]]; then
			ready=1
			break
		fi
	fi
	sleep 1
done
if [[ "$ready" != 1 ]]; then
	echo "[entrypoint] dsh web did not become ready within 180s" >&2
	cat "$DSH_LOG" >&2 || true
	kill "$DSH_PID" 2>/dev/null || true
	exit 1
fi

# Surface dsh's own output (its banner contains the token URL) from now on.
cat "$DSH_LOG"
tail -n 0 -f "$DSH_LOG" &
TAIL_PID=$!

echo "[entrypoint] dsh web is ready (session token captured, ${#token} chars); starting auth proxy on ${DSH_PROXY_BIND}:${DSH_PROXY_PORT}"

export DSH_WEB_TOKEN="$token"
node /usr/local/lib/dsh-proxy/auth-proxy.mjs &
PROXY_PID=$!

# If either side dies, tear the other down and mirror its exit code.
status=0
wait -n "$DSH_PID" "$PROXY_PID" || status=$?
shutdown
wait 2>/dev/null || true
exit "$status"
