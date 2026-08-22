#!/usr/bin/env bash
#
# Bring up the gpu-sim development cluster: kind + KWOK + fake-gpu-operator, with a
# handful of simulated GPU nodes.
#
# This is Phase 0 scaffolding. It stands in for the `gpu-sim` CLI until Phase 3 replaces
# it, and it doubles as executable documentation of which upstream versions the project
# is known to work against.
#
# Usage: hack/setup-cluster.sh [node-count]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-gpu-sim}"
NODE_COUNT="${1:-${NODE_COUNT:-4}}"
NAMESPACE="${NAMESPACE:-gpu-operator}"

# Upstream versions are pinned so a broken upstream release cannot silently break a
# local run. KWOK v0.7.0 is what fake-gpu-operator's own e2e suite tests against; v0.8.0
# exists but has not been validated with the operator yet.
KWOK_VERSION="${KWOK_VERSION:-v0.7.0}"
FGO_VERSION="${FGO_VERSION:-0.2.0}"
FGO_CHART="oci://ghcr.io/run-ai/fake-gpu-operator/fake-gpu-operator"

log() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

# --- kind cluster -------------------------------------------------------------------

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  log "kind cluster '${CLUSTER_NAME}' already exists, reusing it"
else
  log "Creating kind cluster '${CLUSTER_NAME}'"
  kind create cluster --config "${REPO_ROOT}/hack/kind-cluster.yaml"
fi

kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null

# --- KWOK ---------------------------------------------------------------------------

log "Installing KWOK ${KWOK_VERSION}"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/kwok.yaml"
kubectl wait --for=condition=Ready pod -l app=kwok-controller -n kube-system --timeout=180s

# stage-fast collapses KWOK's node and pod lifecycle transitions into a single step, so
# simulated pods reach Running immediately instead of walking through timed stages.
log "Installing KWOK fast stages"
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_VERSION}/stage-fast.yaml"

# --- fake-gpu-operator --------------------------------------------------------------

log "Installing fake-gpu-operator ${FGO_VERSION}"
# The chart's components need to run privileged; kind enforces the restricted Pod
# Security profile by default on labelled namespaces, so relax it up front.
kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl label namespace "${NAMESPACE}" pod-security.kubernetes.io/enforce=privileged --overwrite

helm upgrade --install gpu-operator "${FGO_CHART}" \
  --version "${FGO_VERSION}" \
  --namespace "${NAMESPACE}" \
  --values "${REPO_ROOT}/hack/values-fake-gpu-operator.yaml" \
  --wait --timeout 5m

# --- simulated GPU nodes ------------------------------------------------------------

log "Creating ${NODE_COUNT} simulated GPU nodes"
for i in $(seq 1 "${NODE_COUNT}"); do
  sed -e "s/NODE_NAME/gpu-node-${i}/g" -e "s/NODE_POOL/default/g" \
    "${REPO_ROOT}/hack/kwok-gpu-node.yaml" | kubectl apply -f -
done

# --- wait for the simulation to converge ---------------------------------------------

log "Waiting for the operator to publish topology and ResourceSlices"
for i in $(seq 1 "${NODE_COUNT}"); do
  node="gpu-node-${i}"
  for _ in $(seq 1 60); do
    if kubectl get resourceslice "kwok-${node}-gpu" >/dev/null 2>&1; then
      break
    fi
    sleep 2
  done
  if ! kubectl get resourceslice "kwok-${node}-gpu" >/dev/null 2>&1; then
    echo "ERROR: no ResourceSlice was published for ${node}" >&2
    exit 1
  fi
done

log "Cluster is ready"
kubectl get nodes -l type=kwok
echo
kubectl get resourceslices
