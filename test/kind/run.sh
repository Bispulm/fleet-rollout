#!/usr/bin/env bash
# Runs rollouts end to end on a local kind cluster. The Helm chart installs the control plane and
# the operator, Fleet and Rollout resources drive them, and simulated devices carry the rollouts
# out through a port-forward. A good release has to complete, a bad one has to halt, and deleting
# the halted resource has to abort its rollout.
#
# Needs docker, kind, kubectl, helm, and openssl. The cluster gets its own kubeconfig, so the
# default kube context is never touched. Set KEEP_CLUSTER=1 to leave the cluster up afterward.
set -euo pipefail

CLUSTER="${CLUSTER:-fleet-rollout-dev}"
NAMESPACE=fleet-rollout
LOCAL_PORT="${LOCAL_PORT:-18101}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/fleet-rollout-kind.XXXXXX")"
export KUBECONFIG="$WORK/kubeconfig"
ROLLOUT_ADMIN_TOKEN="$(openssl rand -hex 24)"
export ROLLOUT_ADMIN_TOKEN
export ROLLOUT_API_URL="http://127.0.0.1:$LOCAL_PORT"
BIN="$ROOT/bin"
pids=()

step() { printf '\n==> %s\n' "$*"; }

cleanup() {
  local status=$?
  # The odd expansion is for bash 3.2, which calls an empty array unbound.
  for pid in ${pids[@]+"${pids[@]}"}; do
    kill "$pid" 2>/dev/null || true
  done
  if [ "$status" -ne 0 ] && [ -f "$KUBECONFIG" ]; then
    step "Failed. State of the cluster:"
    kubectl -n "$NAMESPACE" get pods,rollouts,fleets 2>&1 || true
    kubectl -n "$NAMESPACE" logs deploy/fleet-rollout-operator --tail=40 2>&1 || true
    kubectl -n "$NAMESPACE" logs deploy/fleet-rollout-rolloutd --tail=40 2>&1 || true
  fi
  if [ "${KEEP_CLUSTER:-0}" = 1 ]; then
    echo "Kept cluster $CLUSTER. Use it with KUBECONFIG=$KUBECONFIG"
  else
    kind delete cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" >/dev/null 2>&1 || true
    rm -rf "$WORK"
  fi
  exit "$status"
}
# A cluster left from an earlier run holds its fleet and artifacts, which this run would collide
# with, so start from nothing. Checked before the cleanup trap is set, so it can't delete it.
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  echo "cluster $CLUSTER already exists; delete it with: kind delete cluster --name $CLUSTER" >&2
  rm -rf "$WORK"
  exit 1
fi
trap cleanup EXIT

# wait_for runs a command until it succeeds, for up to a number of seconds.
wait_for() {
  local seconds=$1
  shift
  for _ in $(seq "$seconds"); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "gave up waiting for: $*" >&2
  return 1
}

rollout_resource() {
  local name=$1 version=$2 on_breach=${3:-Halt}
  cat <<EOF
apiVersion: rollouts.bispulm.github.io/v1alpha1
kind: Rollout
metadata:
  name: $name
spec:
  fleetRef: lab-k8s
  artifact: {name: demo-app, version: "$version", platform: linux/arm64}
  stages:
    - {percent: 25, soak: 5s}
    - {percent: 50, soak: 5s}
    - {percent: 100, soak: 5s}
  safety: {maxFailureRate: "20%", onBreach: $on_breach}
EOF
}

# stop_fleet stops a simulated fleet and shows what it reported, so that two never run the
# same devices at once.
stop_fleet() {
  kill -INT "$1"
  wait "$1" || true
  cat "$2"
}

step "Building the binaries and the image"
make -C "$ROOT" build >/dev/null
docker build -q -t fleet-rollout:dev "$ROOT" >/dev/null

step "Creating cluster $CLUSTER"
kind create cluster --name "$CLUSTER" --kubeconfig "$KUBECONFIG" --wait 120s
# Only the image built here is loaded. The cluster pulls Postgres itself, because loading a
# multi-platform image fails when the local image store holds just one platform's layers.
kind load docker-image fleet-rollout:dev --name "$CLUSTER"

step "Installing the chart"
"$BIN/rolloutctl" key generate --private-key "$WORK/release.key" --public-key "$WORK/release.pub"
helm upgrade --install fleet-rollout "$ROOT/deploy/helm/fleet-rollout" --namespace "$NAMESPACE" --create-namespace \
  --set adminToken.value="$ROLLOUT_ADMIN_TOKEN" --set-file trustKeys="$WORK/release.pub" \
  --set postgres.password="$(openssl rand -hex 16)" --set rolloutd.pollInterval=2s \
  --wait --timeout 5m

step "Reaching the API through a port-forward"
kubectl -n "$NAMESPACE" port-forward svc/fleet-rollout-rolloutd "$LOCAL_PORT:18100" >"$WORK/port-forward.log" 2>&1 &
pids+=($!)
disown # So that stopping it at the end doesn't print a job notice.
wait_for 30 curl -fsS "$ROLLOUT_API_URL/readyz"

step "Creating a fleet from a Fleet resource"
kubectl apply -f - <<EOF
apiVersion: rollouts.bispulm.github.io/v1alpha1
kind: Fleet
metadata:
  name: lab-k8s
spec:
  labels: {site: kind}
EOF
kubectl wait fleet/lab-k8s --for=condition=Ready --timeout=60s
"$BIN/rolloutctl" device register-synthetic --fleet lab-k8s --count 40 --platform linux/arm64 \
  --tokens-file "$WORK/tokens.txt"
for release in 1 2 3; do
  head -c 65536 /dev/urandom >"$WORK/demo-app-$release.bin"
  "$BIN/rolloutctl" artifact push --name demo-app --version "$release.0.0" --platform linux/arm64 \
    --release "$release" --file "$WORK/demo-app-$release.bin" --key "$WORK/release.key" >/dev/null
done

step "Rolling out a good release from a Rollout resource, over a flaky network"
"$BIN/rollout-sim" fleet --tokens-file "$WORK/tokens.txt" --trust-keys "$WORK/release.pub" \
  --api "$ROLLOUT_API_URL" --preset flaky --soak 5s --seed 1 >"$WORK/fleet-good.log" 2>&1 &
good_fleet=$!
pids+=("$good_fleet")
rollout_resource demo-app-1.0.0 1.0.0 | kubectl apply -f -
kubectl wait rollout/demo-app-1.0.0 --for=jsonpath='{.status.phase}'=completed --timeout=300s
kubectl get rollout demo-app-1.0.0
stop_fleet "$good_fleet" "$WORK/fleet-good.log"

step "Rolling out a release that fails on every device, which has to halt"
"$BIN/rollout-sim" fleet --tokens-file "$WORK/tokens.txt" --trust-keys "$WORK/release.pub" \
  --api "$ROLLOUT_API_URL" --preset clean --bad-release-rate 1 --initial-release 1 >"$WORK/fleet-bad.log" 2>&1 &
bad_fleet=$!
pids+=("$bad_fleet")
rollout_resource demo-app-2.0.0 2.0.0 | kubectl apply -f -
kubectl wait rollout/demo-app-2.0.0 --for=jsonpath='{.status.phase}'=halted --timeout=180s
kubectl get rollout demo-app-2.0.0 -o jsonpath='{.status.haltReason}: {.status.haltDetail}{"\n"}'

step "Deleting the halted resource aborts its rollout"
bad_id="$(kubectl get rollout demo-app-2.0.0 -o jsonpath='{.status.rolloutID}')"
kubectl delete rollout demo-app-2.0.0 --timeout=60s
if ! curl -fsS -H "Authorization: Bearer $ROLLOUT_ADMIN_TOKEN" "$ROLLOUT_API_URL/v1/rollouts/$bad_id" |
  grep -q '"state":"aborted"'; then
  echo "the control plane hasn't aborted rollout $bad_id" >&2
  exit 1
fi

stop_fleet "$bad_fleet" "$WORK/fleet-bad.log"

step "Rolling out a release that fails on half the devices, with rollback on breach"
"$BIN/rollout-sim" fleet --tokens-file "$WORK/tokens.txt" --trust-keys "$WORK/release.pub" \
  --api "$ROLLOUT_API_URL" --preset clean --bad-release-rate 0.5 --initial-release 1 --seed 3 \
  >"$WORK/fleet-rollback.log" 2>&1 &
rollback_fleet=$!
pids+=("$rollback_fleet")
rollout_resource demo-app-3.0.0 3.0.0 Rollback | kubectl apply -f -
kubectl wait rollout/demo-app-3.0.0 --for=jsonpath='{.status.phase}'=rolled_back --timeout=300s
kubectl get rollout demo-app-3.0.0 \
  -o jsonpath='{.status.haltReason}: {.status.haltDetail}{"\n"}rolled back: {.status.rollback.restored} of {.status.rollback.needed} updated devices restored{"\n"}'
stop_fleet "$rollback_fleet" "$WORK/fleet-rollback.log"

step "Passed"
