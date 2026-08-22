#!/usr/bin/env bash
#
# Bring up the gpu-sim development cluster: kind + KWOK + fake-gpu-operator, with a
# handful of simulated GPU nodes.
#
# This is Phase 0 scaffolding. It stands in for the `gpu-sim` CLI until Phase 3 replaces
# it, and it doubles as executable documentation of which upstream versions the project
# is known to work against.
#
# Usage: hack/setup-cluster.sh [topology-file]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

CLUSTER_NAME="${CLUSTER_NAME:-gpu-sim}"
TOPOLOGY="${1:-${TOPOLOGY:-${REPO_ROOT}/topologies/two-racks-h100.yaml}}"
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

# --- the simulated cluster ------------------------------------------------------------

# gpu-sim creates the nodes, publishes their ResourceSlices with per-GPU NVLink, PCIe
# and NUMA attributes, and writes the matching scheduler topology — all from the one file,
# so the three cannot describe different clusters.
log "Generating the simulated cluster from ${TOPOLOGY}"
go run "${REPO_ROOT}/cmd/gpu-sim" topology apply -f "${TOPOLOGY}" --namespace "${NAMESPACE}"

log "Cluster is ready"
kubectl get nodes -l type=kwok -o custom-columns=\
'NAME:.metadata.name,RACK:.metadata.labels.gpu-sim\.io/rack,FAULT-DOMAIN:.metadata.labels.gpu-sim\.io/fault-domain,NVLINK-DOMAIN:.metadata.labels.gpu-sim\.io/nvlink-domain'
echo
kubectl get resourceslices
