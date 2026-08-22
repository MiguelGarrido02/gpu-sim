#!/usr/bin/env bash
#
# Install KAI Scheduler on the gpu-sim development cluster.
#
# KAI is the primary scheduler under test: it is the one that reads GPU topology and
# does gang scheduling, so it is what the simulated ResourceSlices have to satisfy.
set -euo pipefail

KAI_VERSION="${KAI_VERSION:-v0.17.0}"
KAI_CHART="oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler"
KAI_NAMESPACE="${KAI_NAMESPACE:-kai-scheduler}"

log() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }

log "Installing KAI Scheduler ${KAI_VERSION}"
helm upgrade --install kai-scheduler "${KAI_CHART}" \
  --version "${KAI_VERSION}" \
  --namespace "${KAI_NAMESPACE}" --create-namespace \
  --wait --timeout 10m

log "Waiting for the default queue hierarchy"
# The operator creates default-parent-queue/default-queue on a fresh install; workloads
# must reference a queue to be schedulable at all.
for _ in $(seq 1 60); do
  if kubectl get queue default-queue >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

log "KAI Scheduler is ready"
kubectl get pods -n "${KAI_NAMESPACE}"
echo
kubectl get queues
