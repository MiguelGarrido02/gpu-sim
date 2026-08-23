#!/usr/bin/env bash
#
# Install Volcano on the gpu-sim development cluster.
#
# Volcano is the second scheduler gpu-sim targets, and the reason it exists here is to prove
# the scenario vocabulary describes intent rather than KAI's annotations: a scenario aimed at
# Volcano differs by one line.
set -euo pipefail

VOLCANO_VERSION="${VOLCANO_VERSION:-1.15.1}"
VOLCANO_NAMESPACE="${VOLCANO_NAMESPACE:-volcano-system}"

log() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

log "Installing Volcano ${VOLCANO_VERSION}"
helm repo add volcano-sh https://volcano-sh.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update volcano-sh >/dev/null
helm upgrade --install volcano volcano-sh/volcano \
  --version "${VOLCANO_VERSION}" \
  --namespace "${VOLCANO_NAMESPACE}" --create-namespace \
  --wait --timeout 10m

# Neither of the two settings below is on by default, and both fail silently when missing:
# without the first, Volcano schedules DRA workloads as though the claims did not exist;
# without the second, every network topology constraint is ignored.
log "Enabling DRA and network topology awareness"
kubectl patch configmap volcano-scheduler-configmap -n "${VOLCANO_NAMESPACE}" --type=merge -p "$(cat <<'PATCH'
{"data":{"volcano-scheduler.conf":"actions: \"enqueue, allocate, backfill\"\ntiers:\n- plugins:\n  - name: priority\n  - name: gang\n    enablePreemptable: false\n  - name: conformance\n- plugins:\n  - name: overcommit\n  - name: drf\n    enablePreemptable: false\n  - name: predicates\n    arguments:\n      predicate.DynamicResourceAllocationEnable: true\n  - name: proportion\n  - name: nodeorder\n  - name: binpack\n  - name: network-topology-aware\n    arguments:\n      weight: 10\n"}}
PATCH
)"

kubectl rollout restart deploy/volcano-scheduler -n "${VOLCANO_NAMESPACE}"
kubectl rollout status deploy/volcano-scheduler -n "${VOLCANO_NAMESPACE}" --timeout=180s

log "Volcano is ready"
kubectl get pods -n "${VOLCANO_NAMESPACE}"
echo
kubectl get queues.scheduling.volcano.sh
