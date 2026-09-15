#!/usr/bin/env bash
#
# Create the Secrets from this machine's real files and apply the manifests.
#
#   ./deploy/apply.sh                 # secrets + deployment + svc + ingress
#   NS=management ./deploy/apply.sh
#
# Secrets are created straight from the source files (never written into the
# repo) except the staged kubeconfig, which lands in deploy/.secrets/ (gitignored)
# because its server address has to be rewritten for in-cluster use.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STAGE="$HERE/.secrets"

NS="${NS:-management}"
APP="${APP:-dsh-bi-interface}"
IMAGE="${IMAGE:-registry.code27.co/app/dsh-bi-interface:0.1.0}"
# Cluster we deploy into (also the source of the RBAC/admin kubeconfig).
# Every kubectl call below must hit THIS cluster: the machine's default context
# is the local orbstack cluster, which is not where this app goes.
DEPLOY_KUBECONFIG="${DEPLOY_KUBECONFIG:-$HOME/.kube/config_bi}"
export KUBECONFIG="$DEPLOY_KUBECONFIG"
# The "bi" host from ~/bin/ssh_bi; its /root/.kube/config is the authoritative
# admin kubeconfig for this cluster.
SSH_HOST="${SSH_HOST:-root@47.90.211.213}"
SSH_PORT="${SSH_PORT:-2222}"
# Inside a pod the apiserver is reached through its Service.
IN_CLUSTER_SERVER="${IN_CLUSTER_SERVER:-https://kubernetes.default.svc:443}"
AUTH_USER="${AUTH_USER:-friddle}"
AUTH_PASS="${AUTH_PASS:-}"
# Login token for the proxy's own session cookie (see docker/auth-proxy.mjs).
# Empty = reuse whatever is already in the cluster, else generate. Set it
# explicitly to force a rotation.
AUTH_TOKEN="${AUTH_TOKEN:-}"
INGRESS_HOST="${INGRESS_HOST:-dsh-bi-interface.management.code27.co}"
# DRY_RUN=1 rehearses the whole flow with client-side dry runs only.
DRY_RUN="${DRY_RUN:-0}"

SSH_SRC="${SSH_SRC:-$HOME/.ssh}"
DSH_SRC="${DSH_SRC:-$HOME/.dsh}"

# Pipe a generated Secret manifest into the cluster (or just validate it).
apply_from_stdin() {
	if [[ "$DRY_RUN" == "1" ]]; then
		kubectl apply --dry-run=client -f -
	else
		kubectl apply -f -
	fi
}

mkdir -p "$STAGE"
chmod 0700 "$STAGE"

echo "==> staging kubeconfig (server -> $IN_CLUSTER_SERVER)"
if ssh -F /dev/null -p "$SSH_PORT" -o BatchMode=yes -o ConnectTimeout=15 "$SSH_HOST" \
	'cat /root/.kube/config' >"$STAGE/config" 2>/dev/null && [[ -s "$STAGE/config" ]]; then
	echo "    source: $SSH_HOST:/root/.kube/config"
else
	echo "    !! could not read $SSH_HOST:/root/.kube/config, falling back to $DEPLOY_KUBECONFIG" >&2
	cp "$DEPLOY_KUBECONFIG" "$STAGE/config"
fi
chmod 0600 "$STAGE/config"

CTX="$(kubectl --kubeconfig="$STAGE/config" config current-context)"
CLUSTER="$(kubectl --kubeconfig="$STAGE/config" config view -o jsonpath="{.contexts[?(@.name==\"$CTX\")].context.cluster}")"
[[ -n "$CLUSTER" ]] || { echo "!! no cluster in staged kubeconfig" >&2; exit 1; }
kubectl --kubeconfig="$STAGE/config" config set-cluster "$CLUSTER" --server="$IN_CLUSTER_SERVER" >/dev/null
echo "    context=$CTX cluster=$CLUSTER server=$IN_CLUSTER_SERVER"

echo "==> target: $KUBECONFIG context=$(kubectl config current-context)"
kubectl get ns "$NS" >/dev/null

# The Deployment persists through a node-local hostPath (this cluster has no
# StorageClass), so the directory must exist on the node it is pinned to —
# keep PROJECT_DIR and deployment.yaml's nodeSelector/hostPath in sync.
PROJECT_DIR="${PROJECT_DIR:-/root/data/project}"
DSH_HOME_DIR="${DSH_HOME_DIR:-/root/data/dsh-home}"
for dir in "$PROJECT_DIR" "$DSH_HOME_DIR"; do
	echo "==> hostPath target $dir on $SSH_HOST"
	if [[ "$DRY_RUN" == "1" ]]; then
		ssh -F /dev/null -p "$SSH_PORT" -o BatchMode=yes -o ConnectTimeout=15 "$SSH_HOST" \
			"ls -ld '$dir'" \
			|| echo "    (dry run: $dir does not exist yet on $SSH_HOST)"
	elif ssh -F /dev/null -p "$SSH_PORT" -o BatchMode=yes -o ConnectTimeout=15 "$SSH_HOST" \
		"mkdir -p '$dir' && chmod 0755 '$dir' && ls -ld '$dir'" >/dev/null; then
		echo "    ok"
	else
		echo "    !! could not create $dir on $SSH_HOST — the pod will not start" >&2
		exit 1
	fi
done

# The Deployment is pinned to one node (deployment.yaml nodeSelector) because the
# hostPath above is node-local. Catch a missing/renamed/cordoned node here rather
# than leaving the pod Pending with an unhelpful event.
PINNED_NODE="${PINNED_NODE:-$(kubectl -n "$NS" get deploy "$APP" \
	-o jsonpath='{.spec.template.spec.nodeSelector.kubernetes\.io/hostname}' 2>/dev/null \
	|| true)}"
PINNED_NODE="${PINNED_NODE:-iz0xi7yxr2llsjyhd7mvksz}"
echo "==> pinned node: $PINNED_NODE"
node_info="$(kubectl get node "$PINNED_NODE" \
	-o jsonpath='{.metadata.labels.kubernetes\.io/hostname} unschedulable={.spec.unschedulable}' 2>&1)" || {
	echo "    !! node $PINNED_NODE not found — update the nodeSelector in k8s/deployment.yaml" >&2
	exit 1
}
echo "    hostname-label=$node_info"
if [[ "$node_info" == *"unschedulable=true"* ]]; then
	echo "    !! node $PINNED_NODE is cordoned; the pod cannot be rescheduled there" >&2
	exit 1
fi

echo "==> secret/$APP-kubeconfig"
kubectl -n "$NS" create secret generic "$APP-kubeconfig" \
	--from-file=config="$STAGE/config" \
	--dry-run=client -o yaml | apply_from_stdin

echo "==> secret/$APP-ssh"
ssh_files=()
for f in id_ed25519 id_ed25519.pub config known_hosts; do
	[[ -f "$SSH_SRC/$f" ]] && ssh_files+=("--from-file=$f=$SSH_SRC/$f")
done
[[ ${#ssh_files[@]} -gt 0 ]] || { echo "!! no ssh files found in $SSH_SRC" >&2; exit 1; }
kubectl -n "$NS" create secret generic "$APP-ssh" "${ssh_files[@]}" \
	--dry-run=client -o yaml | apply_from_stdin

echo "==> secret/$APP-dsh-credentials"
cred_files=()
for f in .credentials.yaml settings.yaml; do
	[[ -f "$DSH_SRC/$f" ]] && cred_files+=("--from-file=$f=$DSH_SRC/$f")
done
[[ ${#cred_files[@]} -gt 0 ]] || { echo "!! no dsh credentials found in $DSH_SRC" >&2; exit 1; }
kubectl -n "$NS" create secret generic "$APP-dsh-credentials" "${cred_files[@]}" \
	--dry-run=client -o yaml | apply_from_stdin

if [[ -z "$AUTH_PASS" ]]; then
	echo "!! AUTH_PASS is empty; pass it explicitly, e.g. AUTH_PASS='...' $0" >&2
	exit 1
fi

# Keep the existing login token across re-applies: rotating it would log
# everybody out. AUTH_TOKEN='...' forces a new one.
if [[ -z "$AUTH_TOKEN" ]]; then
	AUTH_TOKEN="$(kubectl -n "$NS" get secret "$APP-auth" -o jsonpath='{.data.token}' 2>/dev/null \
		| base64 --decode 2>/dev/null || true)"
fi
TOKEN_SOURCE="reused from secret/$APP-auth"
if [[ -z "$AUTH_TOKEN" ]]; then
	AUTH_TOKEN="$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n')"
	TOKEN_SOURCE="generated"
fi
echo "==> secret/$APP-auth (login token: $TOKEN_SOURCE)"
kubectl -n "$NS" create secret generic "$APP-auth" \
	--from-literal=username="$AUTH_USER" --from-literal=password="$AUTH_PASS" \
	--from-literal=token="$AUTH_TOKEN" \
	--dry-run=client -o yaml | apply_from_stdin

echo "==> applying manifests"
if [[ "$DRY_RUN" == "1" ]]; then
	kubectl -n "$NS" apply --dry-run=client \
		-f "$HERE/k8s/service.yaml" -f "$HERE/k8s/deployment.yaml" -f "$HERE/k8s/ingress.yaml"
	echo "==> DRY_RUN=1: nothing was changed"
	exit 0
fi
kubectl -n "$NS" apply -f "$HERE/k8s/service.yaml" -f "$HERE/k8s/deployment.yaml" -f "$HERE/k8s/ingress.yaml"

echo "==> waiting for rollout"
kubectl -n "$NS" rollout status deployment/"$APP" --timeout=600s || {
	echo "!! rollout did not finish; recent events:" >&2
	kubectl -n "$NS" describe pod -l app.kubernetes.io/name="$APP" | tail -40 >&2
	exit 1
}

echo "==> done"
kubectl -n "$NS" get pod,svc,ingress -l app.kubernetes.io/name="$APP" -o wide
echo
echo "打开下面这个链接登录一次（之后 30 天免登录，滑动续期）:"
echo "  https://$INGRESS_HOST/?dsh_token=$AUTH_TOKEN"
echo
echo "Token 也可以随时再取："
echo "  kubectl -n $NS get secret $APP-auth -o jsonpath='{.data.token}' | base64 -d; echo"
echo "Basic Auth（仅 DSH_AUTH_MODE=both|basic 时可用）: user=$AUTH_USER"
