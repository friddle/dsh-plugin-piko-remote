#!/usr/bin/env bash
#
# Build the DSH image, smoke-test it, and push it to the cluster-visible registry.
#
#   ./deploy/build.sh              # build + smoke test + push
#   PUSH=0 ./deploy/build.sh       # build + smoke test only
#   TAG=0.2.0 ./deploy/build.sh
#
# Two tags are used on purpose: the LAN alias pushes from this Mac, the
# registry.code27.co name is what the cluster pulls.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

TAG="${TAG:-0.1.0}"
DSH_VERSION="${DSH_VERSION:-0.1.5-rc.1}"
GO_VERSION="${GO_VERSION:-1.27.1}"
KUBECTL_VERSION="${KUBECTL_VERSION:-1.33.9}"
# Docker Hub is unreachable from the OrbStack build VM. dockermirror.service.code27.cn
# also serves this image but truncates its 211MB layer ("short read: unexpected
# EOF"), so the public daocloud mirror is the default.
NODE_IMAGE="${NODE_IMAGE:-docker.m.daocloud.io/library/node:24-bookworm}"
NPM_REGISTRY="${NPM_REGISTRY:-https://registry.npmjs.org}"
PLATFORM="${PLATFORM:-linux/amd64}"
LAN_IMAGE="${LAN_IMAGE:-registrylan.service.code27.cn/app/dsh-bi-interface}"
CLUSTER_IMAGE="${CLUSTER_IMAGE:-registry.code27.co/app/dsh-bi-interface}"
SMOKE_PORT="${SMOKE_PORT:-18080}"
INGRESS_HOST="${INGRESS_HOST:-dsh-bi-interface.management.code27.co}"
PUSH="${PUSH:-1}"
SMOKE_USER="${SMOKE_USER:-smoke}"
SMOKE_PASS="${SMOKE_PASS:-smoke}"

echo "==> building $LAN_IMAGE:$TAG (platform $PLATFORM, dsh $DSH_VERSION)"
# The Docker client config carries a "proxies" block pointing at the host's
# localhost proxy, which the daemon VM cannot reach; clear them for the build.
docker build \
	--platform "$PLATFORM" \
	--build-arg "NODE_IMAGE=$NODE_IMAGE" \
	--build-arg "DSH_VERSION=$DSH_VERSION" \
	--build-arg "GO_VERSION=$GO_VERSION" \
	--build-arg "KUBECTL_VERSION=$KUBECTL_VERSION" \
	--build-arg "NPM_REGISTRY=$NPM_REGISTRY" \
	--build-arg HTTP_PROXY= --build-arg HTTPS_PROXY= \
	--build-arg http_proxy= --build-arg https_proxy= --build-arg NO_PROXY= \
	-t "$LAN_IMAGE:$TAG" -t "$CLUSTER_IMAGE:$TAG" \
	"$HERE/docker"

echo "==> smoke test"
docker rm -f dsh-smoke >/dev/null 2>&1 || true
# The host's docker config carries proxy env (localhost:7897) that Docker injects
# into the container; from inside there is no such proxy, and it would swallow
# every loopback curl. Clear it so the test measures the image.
docker run -d --name dsh-smoke \
	-e "DSH_AUTH_USER=$SMOKE_USER" -e "DSH_AUTH_PASS=$SMOKE_PASS" \
	-e "DSH_AUTH_TOKEN=$SMOKE_PASS" -e "DSH_AUTH_MODE=token" \
	-e "DSH_TRUSTED_HOST=$INGRESS_HOST" \
	-e http_proxy= -e https_proxy= -e HTTP_PROXY= -e HTTPS_PROXY= \
	-e ALL_PROXY= -e NO_PROXY='*' \
	-p "$SMOKE_PORT:8080" "$LAN_IMAGE:$TAG" >/dev/null

cleanup() { docker rm -f dsh-smoke >/dev/null 2>&1 || true; }
trap cleanup EXIT

BASE="http://127.0.0.1:$SMOKE_PORT"
for i in $(seq 1 90); do
	if curl --noproxy '*' -fsS -o /dev/null "$BASE/__healthz" 2>/dev/null; then break; fi
	if [[ "$(docker inspect -f '{{.State.Running}}' dsh-smoke 2>/dev/null)" != "true" ]]; then
		echo "!! container died during boot:" >&2
		docker logs dsh-smoke >&2 || true
		exit 1
	fi
	[[ "$i" == 90 ]] && { echo "!! not ready in time" >&2; docker logs dsh-smoke >&2; exit 1; }
	sleep 2
done

# Everything below speaks with the production Host header so dsh's browser-trust
# fence is exercised too (the container was started with --trusted-host).
HOST_HDR="Host: $INGRESS_HOST"
CURL=(curl --noproxy '*' -s -H "$HOST_HDR")
jar="$(mktemp)"
body="$(mktemp)"

# Browser navigation with no session: must redirect to the login page and must
# NOT emit a Basic challenge (that is what made browsers prompt repeatedly).
anon=$("${CURL[@]}" -H 'Accept: text/html' -o /dev/null -w '%{http_code} %{redirect_url}' "$BASE/")
challenges=$("${CURL[@]}" -H 'Accept: text/html' -D - -o /dev/null "$BASE/" | tr -d '\r' | grep -ci '^www-authenticate:' || true)
form=$("${CURL[@]}" -H 'Accept: text/html' -o "$body" -w '%{http_code}' "$BASE/__login")
if grep -q 'name="token"' "$body"; then formok=yes; else formok=no; fi
# Exchange the token for a signed session cookie, then use it.
login=$("${CURL[@]}" -c "$jar" -b "$jar" -o /dev/null -w '%{http_code}' \
	-X POST -d "token=$SMOKE_PASS" -d 'next=/' "$BASE/__login")
ui=$("${CURL[@]}" -c "$jar" -b "$jar" -L -o "$body" -w '%{http_code}' "$BASE/")
if grep -qiE '<div id="root"|<!doctype html' "$body"; then html=yes; else html=no; fi
session=$(grep -c 'dsh_auth' "$jar" 2>/dev/null || true)
api=$("${CURL[@]}" -b "$jar" -c "$jar" -o /dev/null -w '%{http_code}' "$BASE/api/state")
api_anon=$("${CURL[@]}" -o /dev/null -w '%{http_code}' "$BASE/api/state")
# Scripts hold only the login token: the proxy bootstraps dsh's own session
# server-side, so Bearer must work on the first request with no cookie jar.
bearer=$("${CURL[@]}" -H "Authorization: Bearer $SMOKE_PASS" -o "$body" -w '%{http_code}' "$BASE/")
if grep -qiE '<div id="root"|<!doctype html' "$body"; then bearer_html=yes; else bearer_html=no; fi
bearer_api=$("${CURL[@]}" -H "Authorization: Bearer $SMOKE_PASS" -o /dev/null -w '%{http_code}' "$BASE/api/state")
ws=$("${CURL[@]}" -b "$jar" -o /dev/null -w '%{http_code}' --http1.1 \
	-H 'Upgrade: websocket' -H 'Connection: Upgrade' \
	-H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' -H 'Sec-WebSocket-Version: 13' \
	"$BASE/api/remote.mux")
tools=$(docker exec dsh-smoke bash -lc 'go version; git --version; kubectl version --client 2>/dev/null | head -1; mysqldump --version; node -v; vim --version | head -1' 2>&1)

echo "--- smoke results ---"
echo "no session   /            -> $anon   (want 303 + /__login)"
echo "Basic challenge headers   -> $challenges   (want 0)"
echo "GET /__login form         -> $form, token field=$formok   (want 200/yes)"
echo "POST /__login token       -> $login   (want 303, cookie set=$session)"
echo "session cookie  /         -> $ui, html=$html   (want 200/yes)"
echo "session cookie  /api/state-> $api   (want != 401)"
echo "no session      /api/state-> $api_anon   (want 401)"
echo "Bearer only     /         -> $bearer, html=$bearer_html   (want 200/yes)"
echo "Bearer only     /api/state-> $bearer_api   (want != 401)"
echo "session cookie  WS upgrade-> $ws   (want 101)"
echo "$tools"
rm -f "$jar" "$body"

[[ "$anon" == 303* && "$anon" == *"/__login"* ]] || { echo "!! expected a login redirect, got: $anon" >&2; docker logs dsh-smoke >&2; exit 1; }
[[ "$challenges" == "0" ]] || { echo "!! a Basic challenge was sent — browsers would prompt" >&2; docker logs dsh-smoke >&2; exit 1; }
[[ "$form" == "200" && "$formok" == "yes" ]] || { echo "!! login form not served" >&2; docker logs dsh-smoke >&2; exit 1; }
[[ "$login" == "303" && "$session" != "0" ]] || { echo "!! token login did not set a session cookie" >&2; docker logs dsh-smoke >&2; exit 1; }
[[ "$ui" == "200" && "$html" == "yes" ]] || { echo "!! authenticated UI did not load" >&2; docker logs dsh-smoke >&2; exit 1; }
[[ "$api_anon" == "401" ]] || { echo "!! unauthenticated API should be 401" >&2; docker logs dsh-smoke >&2; exit 1; }
[[ "$bearer" == "200" && "$bearer_html" == "yes" ]] || { echo "!! Bearer-only request did not reach dsh" >&2; docker logs dsh-smoke >&2; exit 1; }
[[ "$ws" == "101" ]] || { echo "!! websocket with a session cookie should be 101" >&2; docker logs dsh-smoke >&2; exit 1; }

echo "==> smoke test passed"
cleanup
trap - EXIT

if [[ "$PUSH" != "1" ]]; then
	echo "==> PUSH=0, skipping push (image stays local: $LAN_IMAGE:$TAG)"
	exit 0
fi

echo "==> pushing $LAN_IMAGE:$TAG"
docker push "$LAN_IMAGE:$TAG"
# registry.code27.co is the same registry, just the in-cluster DNS name: from this
# Mac it resolves to 172.23.240.1 and serves an *expired* certificate, so the
# cluster name cannot be pushed from here (pods pull it fine from inside).
# One push under the LAN alias is enough; set PUSH_CLUSTER_NAME=1 to also try.
if [[ "${PUSH_CLUSTER_NAME:-0}" == "1" ]]; then
	echo "==> pushing $CLUSTER_IMAGE:$TAG"
	docker push "$CLUSTER_IMAGE:$TAG"
fi
echo "==> pushed $CLUSTER_IMAGE:$TAG (via $LAN_IMAGE)"
