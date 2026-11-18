#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# kind-based end-to-end tests for the Helm chart (deploy/e2e/README.md):
# install protodex with the bundled SeaweedFS into a throwaway kind cluster,
# then run the buf CLI conformance suite and the restart, Litestream restore
# and upgrade tests against it. CI (.github/workflows/kind-e2e.yml) and
# `make e2e-kind` call this script; keep the workflow thin.
#
# usage: deploy/e2e/e2e.sh <command>
#
#   all            down, up, upgrade, conformance, restart, restore, down
#                  (E2E_KEEP=1 keeps the cluster; diagnostics on failure)
#   up             create the cluster, build and load images, install the
#                  release (at E2E_UPGRADE_FROM if set), bootstrap it
#   upgrade        upgrade test: seed data on the previous version, upgrade to
#                  this checkout, verify (skipped if nothing older is installed)
#   conformance    buf CLI conformance suite against the release
#   restart        restart test: seed, kill the pod, verify
#   restore        restore test: seed, delete the PVC, restore from S3, verify
#   down           delete the cluster
#   diagnostics    dump pods, events and logs
#
# Disruptions (run by the tests between seeding and verifying):
#   kill-pod, wipe-volume, upgrade-release, admin <protodex args...>
#
# Settings (environment):
#   E2E_CLUSTER       kind cluster name (protodex-e2e)
#   E2E_NAMESPACE     namespace (protodex-e2e)
#   E2E_RELEASE       Helm release (protodex)
#   E2E_PORT          host port of the NodePort service (30080)
#   E2E_UPGRADE_FROM  git ref to install first and upgrade from; "auto" (the
#                     latest v* tag, else the merge base with the base branch,
#                     else HEAD^) or "none" (default: auto)
#   E2E_BUF_VERSIONS  buf CLI versions for the conformance suite (pr); the
#                     lifecycle tests use the newest of them
#   E2E_KIND_IMAGE    kind node image (kind's default)
#   E2E_STATE_DIR     kubeconfig, token, reports (.e2e-kind)
#   E2E_TIMEOUT       timeout in seconds for each wait (300)
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SCRIPT="$ROOT/deploy/e2e/e2e.sh"
CLUSTER=${E2E_CLUSTER:-protodex-e2e}
NS=${E2E_NAMESPACE:-protodex-e2e}
RELEASE=${E2E_RELEASE:-protodex}
PORT=${E2E_PORT:-30080}
NODE_PORT=30080
STATE=${E2E_STATE_DIR:-$ROOT/.e2e-kind}
TIMEOUT=${E2E_TIMEOUT:-300}
BUF_VERSIONS=${E2E_BUF_VERSIONS:-pr}
IMAGE_REPO=protodex-e2e
SERVER_SELECTOR="app.kubernetes.io/instance=$RELEASE,app.kubernetes.io/name=protodex,app.kubernetes.io/component=server"

export KUBECONFIG="$STATE/kubeconfig"
export GOFLAGS=${GOFLAGS:--mod=readonly}

log() { printf '\n==> %s\n' "$*" >&2; }
die() { printf 'e2e: %s\n' "$*" >&2; exit 1; }

k() { kubectl -n "$NS" "$@"; }
h() { helm -n "$NS" "$@"; }

need() {
	local c
	for c in "$@"; do
		command -v "$c" >/dev/null 2>&1 || die "$c not found on PATH (see deploy/e2e/README.md)"
	done
}

# wait_until SECONDS DESCRIPTION COMMAND...: poll COMMAND every 2s until it
# succeeds; fail after SECONDS.
wait_until() {
	local timeout=$1 what=$2 deadline
	shift 2
	deadline=$((SECONDS + timeout))
	until "$@"; do
		if ((SECONDS >= deadline)); then
			echo "e2e: timed out after ${timeout}s waiting for $what" >&2
			return 1
		fi
		sleep 2
	done
}

token() { cat "$STATE/token"; }

# --- cluster and images -------------------------------------------------------

create_cluster() {
	need kind docker kubectl helm go git
	mkdir -p "$STATE"
	if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
		log "kind cluster $CLUSTER exists"
		kind export kubeconfig --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
		return
	fi
	log "creating kind cluster $CLUSTER (NodePort $NODE_PORT -> 127.0.0.1:$PORT)"
	cat >"$STATE/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      - containerPort: $NODE_PORT
        hostPort: $PORT
        listenAddress: 127.0.0.1
        protocol: TCP
EOF
	kind create cluster --name "$CLUSTER" --config "$STATE/kind.yaml" \
		--kubeconfig "$KUBECONFIG" --wait "${TIMEOUT}s" ${E2E_KIND_IMAGE:+--image "$E2E_KIND_IMAGE"}
}

# build_image DIR TAG VERSION: build the image from a source tree and load it
# into the cluster.
build_image() {
	log "building $IMAGE_REPO:$2 from $1"
	docker build -q --build-arg VERSION="$3" -t "$IMAGE_REPO:$2" "$1" >/dev/null
	kind load docker-image --name "$CLUSTER" "$IMAGE_REPO:$2"
}

resolve_upgrade_from() {
	local from=${E2E_UPGRADE_FROM:-auto} base
	if [ "$from" = none ]; then
		return
	fi
	if [ "$from" = auto ]; then
		base="origin/${GITHUB_BASE_REF:-main}"
		if from=$(git -C "$ROOT" describe --tags --abbrev=0 --match 'v[0-9]*' HEAD 2>/dev/null); then
			:
		elif from=$(git -C "$ROOT" merge-base HEAD "$base" 2>/dev/null) &&
			[ "$from" != "$(git -C "$ROOT" rev-parse HEAD)" ]; then
			:
		elif from=$(git -C "$ROOT" rev-parse -q --verify 'HEAD^{commit}^' 2>/dev/null); then
			:
		else
			echo "e2e: no earlier version to upgrade from; skipping the upgrade test" >&2
			return
		fi
	fi
	git -C "$ROOT" rev-parse -q --verify "$from^{commit}" ||
		die "E2E_UPGRADE_FROM=$from is not a commit (fetch tags/history: actions/checkout fetch-depth: 0)"
}

# --- release ------------------------------------------------------------------

# install CHART_DIR TAG: helm upgrade --install with the e2e values.
install() {
	log "installing $RELEASE from $1 (image $IMAGE_REPO:$2)"
	h upgrade --install "$RELEASE" "$1" --create-namespace \
		-f "$ROOT/deploy/e2e/values.yaml" \
		--set "image.repository=$IMAGE_REPO" --set "image.tag=$2" \
		--wait --timeout "${TIMEOUT}s"
}

install_current() {
	install "$ROOT/deploy/helm/protodex" current
	echo current >"$STATE/installed"
}

deployment() {
	k get deploy -l "$SERVER_SELECTOR" -o name | head -n1
}

# no_server_pods: true once every server pod is gone.
no_server_pods() { [ -z "$(k get pod -l "$SERVER_SELECTOR" -o name)" ]; }

scale() {
	local d
	d=$(deployment)
	k scale "$d" --replicas="$1"
	if [ "$1" = 0 ]; then
		wait_until "$TIMEOUT" "server pods to stop" no_server_pods
	else
		k rollout status "$d" --timeout="${TIMEOUT}s"
	fi
}

pod_phase() { k get pod "$1" -o jsonpath='{.status.phase}'; }
pod_done() { case "$(pod_phase "$1")" in Succeeded | Failed) return 0 ;; esac; return 1; }

# admin ARGS...: run `protodex ARGS...` against the release's database. SQLite
# allows one process, so the server is scaled to 0 first (the documented way
# to run bootstrap/migrate/owner commands in SQLite mode).
admin() {
	local d image claim db pod
	d=$(deployment)
	[ -n "$d" ] || die "no protodex deployment in $NS"
	image=$(k get "$d" -o jsonpath='{.spec.template.spec.containers[?(@.name=="protodex")].image}')
	claim=$(k get "$d" -o jsonpath='{.spec.template.spec.volumes[?(@.name=="data")].persistentVolumeClaim.claimName}')
	db=$(k get configmap "${d#*/}" -o jsonpath='{.data.PROTODEX_DB_PATH}')
	pod="protodex-admin-$(date +%s)"
	log "admin: protodex $* (image $image)"
	scale 0
	k apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pod
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    fsGroup: 65532
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: protodex
      image: $image
      imagePullPolicy: IfNotPresent
      args: [$(printf '"%s",' "$@" | sed 's/,$//')]
      env:
        - {name: PROTODEX_DB_PATH, value: "$db"}
        - name: PROTODEX_BOOTSTRAP_TOKEN
          valueFrom: {secretKeyRef: {name: protodex-e2e-admin, key: token}}
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: {drop: [ALL]}
      volumeMounts:
        - {name: data, mountPath: "$(dirname "$db")"}
  volumes:
    - name: data
      persistentVolumeClaim: {claimName: $claim}
EOF
	wait_until "$TIMEOUT" "admin pod $pod" pod_done "$pod"
	k logs "$pod"
	[ "$(pod_phase "$pod")" = Succeeded ] || die "admin pod $pod failed"
	k delete pod "$pod" --wait=true
	scale 1
}

# expose: a NodePort Service owned by the e2e (not the chart, so it works for
# any chart version), mapped to 127.0.0.1:$PORT by the kind config.
expose() {
	k apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: protodex-e2e-nodeport
spec:
  type: NodePort
  selector:
    app.kubernetes.io/instance: $RELEASE
    app.kubernetes.io/name: protodex
    app.kubernetes.io/component: server
  ports:
    - {name: http, port: 8080, targetPort: http, nodePort: $NODE_PORT, protocol: TCP}
EOF
	wait_until "$TIMEOUT" "http://127.0.0.1:$PORT/readyz" curl -fsS -o /dev/null "http://127.0.0.1:$PORT/readyz"
}

up() {
	local from version
	create_cluster
	build_image "$ROOT" current "$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -
	# The chart claims compatibility with the restricted Pod Security Standard.
	kubectl label namespace "$NS" --overwrite pod-security.kubernetes.io/enforce=restricted
	if [ ! -f "$STATE/token" ]; then
		printf 'pdx_%s' "$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')" >"$STATE/token"
	fi
	k create secret generic protodex-e2e-admin --from-file=token="$STATE/token" --dry-run=client -o yaml | k apply -f -

	if h status "$RELEASE" >/dev/null 2>&1; then
		log "release $RELEASE exists; reusing it (run '$0 down' for a clean run)"
	else
		from=$(resolve_upgrade_from)
		if [ -n "$from" ]; then
			local src="$STATE/src-previous"
			version=$(git -C "$ROOT" describe --tags --always "$from")
			log "installing the previous version $version"
			rm -rf "$src" && mkdir -p "$src"
			git -C "$ROOT" archive "$from" | tar -x -C "$src"
			[ -f "$src/deploy/helm/protodex/Chart.yaml" ] || die "$version has no Helm chart; set E2E_UPGRADE_FROM=none or a newer ref"
			build_image "$src" previous "$version"
			install "$src/deploy/helm/protodex" previous
			echo "previous $version" >"$STATE/installed"
		else
			install_current
		fi
		admin bootstrap -org acme
	fi
	expose
}

# --- tests --------------------------------------------------------------------

gotest() {
	(cd "$ROOT" && PROTODEX_CONFORMANCE_REMOTE="http://127.0.0.1:$PORT" \
		PROTODEX_CONFORMANCE_REMOTE_TOKEN="$(token)" \
		PROTODEX_CONFORMANCE_BACKENDS=remote \
		PROTODEX_CONFORMANCE_BUF_VERSIONS="$BUF_VERSIONS" \
		go test -count=1 -timeout 40m "$@" ./test/conformance/)
}

# disruption NAME: TestDisruption seeds data, runs `e2e.sh NAME`, then
# verifies. Variables assigned before the call reach the test.
disruption() {
	log "lifecycle test: $1"
	PROTODEX_E2E_DISRUPT="'$SCRIPT' $1" gotest -v -run '^TestDisruption$'
}

conformance() {
	log "conformance suite (buf $BUF_VERSIONS) against the release"
	PROTODEX_CONFORMANCE_REPORT="$STATE/conformance-report.md" gotest -run '^TestConformance$'
	cat "$STATE/conformance-report.md"
}

# kill-pod: delete the server pod with a 1s grace period and wait for the
# replacement to become ready.
server_pod_uid() { k get pod -l "$SERVER_SELECTOR" -o jsonpath='{.items[0].metadata.uid}'; }
replacement_ready() {
	k get pod -l "$SERVER_SELECTOR" \
		-o jsonpath='{range .items[*]}{.metadata.uid} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' |
		awk -v old="$1" '$1 != old && $2 == "True" { found = 1 } END { exit !found }'
}
kill_pod() {
	local uid
	uid=$(server_pod_uid)
	log "killing server pod $uid"
	k delete pod -l "$SERVER_SELECTOR" --grace-period=1 --wait=true
	wait_until "$TIMEOUT" "a replacement server pod to become ready" replacement_ready "$uid"
}

# wipe-volume: the restore drill from deploy/helm/protodex/README.md. Stop the
# writer (Litestream flushes on shutdown), delete the PVC, let Helm recreate
# it and the litestream-restore init container restore the replica.
pvc_gone() { ! k get pvc "$1" >/dev/null 2>&1; }
wipe_volume() {
	local d claim old new
	d=$(deployment)
	claim=$(k get "$d" -o jsonpath='{.spec.template.spec.volumes[?(@.name=="data")].persistentVolumeClaim.claimName}')
	old=$(k get pvc "$claim" -o jsonpath='{.metadata.uid}')
	log "deleting PVC $claim ($old)"
	scale 0
	k delete pvc "$claim" --wait=false
	wait_until "$TIMEOUT" "PVC $claim to be deleted" pvc_gone "$claim"
	install_current # recreates the PVC
	scale 1
	new=$(k get pvc "$claim" -o jsonpath='{.metadata.uid}')
	[ -n "$new" ] && [ "$new" != "$old" ] || die "PVC $claim was not recreated (uid $old -> $new)"
	log "litestream-restore log:"
	k logs "$(k get pod -l "$SERVER_SELECTOR" -o name | head -n1)" -c litestream-restore
}

upgrade_release() {
	install_current
	admin migrate up
}

upgrade() {
	local installed
	installed=$(cat "$STATE/installed" 2>/dev/null || echo current)
	if [ "${installed%% *}" != previous ]; then
		log "upgrade test skipped: the release already runs this checkout (E2E_UPGRADE_FROM=${E2E_UPGRADE_FROM:-auto})"
		return
	fi
	log "upgrade test: ${installed#previous } -> this checkout"
	disruption upgrade-release
}

restart() { disruption kill-pod; }
# Blobs live on the volume until the S3 blob store (#8) lands, so only the
# metadata (modules, commits, labels) survives a volume loss.
restore() { PROTODEX_E2E_CHECK_CONTENT=false disruption wipe-volume; }

diagnostics() {
	set +e
	log "diagnostics ($NS)"
	k get all,pvc,configmap,secret,networkpolicy -o wide
	kubectl get pv -o wide
	k get events --sort-by=.lastTimestamp
	h history "$RELEASE"
	local p
	for p in $(k get pod -o name); do
		log "$p"
		k describe "$p"
		k logs "$p" --all-containers --prefix --tail=300
		k logs "$p" --all-containers --prefix --previous --tail=100 2>/dev/null
	done
	return 0
}

down() {
	need kind
	if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
		log "deleting kind cluster $CLUSTER"
		kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG"
	fi
	rm -f "$STATE/installed" "$STATE/token"
}

finish() {
	local status=$?
	if [ "$status" -ne 0 ]; then
		diagnostics
	fi
	if [ "${E2E_KEEP:-}" != 1 ]; then
		down
	fi
	exit "$status"
}

all() {
	down
	trap finish EXIT
	up
	upgrade
	conformance
	restart
	restore
	log "kind e2e passed"
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case "$cmd" in
all | up | upgrade | conformance | restart | restore | down | diagnostics | admin) "$cmd" "$@" ;;
kill-pod) kill_pod ;;
wipe-volume) wipe_volume ;;
upgrade-release) upgrade_release ;;
*)
	sed -n '/^# usage:/,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//' >&2
	exit 2
	;;
esac
