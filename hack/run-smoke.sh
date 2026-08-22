#!/usr/bin/env bash
#
# Run the smoke suite against a cluster built by hack/setup-cluster.sh and
# hack/install-kai.sh, and assert the outcomes rather than printing them for a human to
# squint at.
#
# Every check pairs a workload that should be placed with one that should not. A test that
# only ever asserts success passes just as happily against a scheduler that ignores the
# constraint entirely, which is how the first version of the Phase 0 gang test quietly
# proved nothing.
#
# This is Phase 0-3 scaffolding: the declarative scenario harness of Phase 3 replaces it.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SMOKE="${REPO_ROOT}/hack/smoke"
NS=gpu-sim-test

# How long to wait for the scheduler to settle. Placement is near-instant on a simulated
# cluster; the budget exists for the negative cases, where the only evidence is that
# nothing happened after a fair chance to happen.
TIMEOUT="${TIMEOUT:-60}"

passed=0
failed=0

pass() { printf '  \033[32mPASS\033[0m %s\n' "$1"; passed=$((passed + 1)); }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; failed=$((failed + 1)); }
head() { printf '\n\033[1;34m==>\033[0m %s\n' "$1"; }

reset_namespace() {
  kubectl delete job,deployment,pod,resourceclaimtemplate --all -n "${NS}" --wait=true >/dev/null 2>&1
}

scheduled_count() {
  kubectl get pods -n "${NS}" -l "$1" -o json 2>/dev/null |
    jq -r '[.items[] | select(.spec.nodeName != null)] | length'
}

# wait_for_scheduled blocks until the wanted number of pods is placed, or the budget runs
# out. It returns the count either way: a caller asserting that nothing is placed wants the
# full wait, and a caller asserting placement wants to stop as soon as it happens.
wait_for_scheduled() {
  local selector=$1 want=$2 count=0
  for _ in $(seq 1 $((TIMEOUT / 3))); do
    count=$(scheduled_count "${selector}")
    [ "${count:-0}" -ge "${want}" ] && break
    sleep 3
  done
  echo "${count:-0}"
}

# distinct_label_values reports how many different values of a node label the placed pods
# span — 1 means the whole workload landed inside a single domain.
distinct_label_values() {
  local selector=$1 label=$2
  local nodes
  nodes=$(kubectl get pods -n "${NS}" -l "${selector}" -o jsonpath='{.items[*].spec.nodeName}')
  for node in ${nodes}; do
    kubectl get node "${node}" -o jsonpath="{.metadata.labels.${label}}"
    echo
  done | sort -u | grep -c . || true
}

kubectl create namespace "${NS}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# Every assertion below counts GPUs, so the cluster has to be a known shape: two racks of
# two DGX H100 nodes, 32 GPUs, 16 per NUMA node, 8 per NVLink domain. Applying the topology
# here rather than assuming it means the suite is not silently measuring whatever was left
# over from the last thing anyone ran.
head "Building the cluster from topologies/two-racks-h100.yaml"
go run "${REPO_ROOT}/cmd/topology-gen" apply -f "${REPO_ROOT}/topologies/two-racks-h100.yaml" >/dev/null

# --- gang scheduling ------------------------------------------------------------------

head "Gang scheduling"
reset_namespace
kubectl apply -f "${SMOKE}/kai-gang.yaml" >/dev/null
running=0
for _ in $(seq 1 $((TIMEOUT / 3))); do
  running=$(kubectl get pods -n "${NS}" gang-member-1 gang-member-2 -o json 2>/dev/null |
    jq -r '[.items[] | select(.status.phase=="Running")] | length')
  [ "${running:-0}" -eq 2 ] && break
  sleep 3
done
if [ "${running:-0}" -eq 2 ]; then
  pass "a 2-pod gang is placed"
else
  fail "a 2-pod gang is placed"
fi

reset_namespace
kubectl apply -f "${SMOKE}/kai-gang.yaml" >/dev/null
kubectl apply -f "${SMOKE}/kai-gang-overcapacity.yaml" >/dev/null
sleep "${TIMEOUT}"
if [ "$(scheduled_count 'job-name=gang-overcapacity')" -eq 0 ]; then
  pass "a gang larger than the cluster places no pod at all"
else
  fail "a gang larger than the cluster places no pod at all"
fi

# --- topology placement ---------------------------------------------------------------

head "Rack-level placement"
reset_namespace
kubectl apply -f "${SMOKE}/topology-placement.yaml" >/dev/null
if [ "$(wait_for_scheduled 'job-name=rack-local-training' 12)" -eq 12 ] &&
   [ "$(distinct_label_values 'job-name=rack-local-training' 'gpu-sim\.io/rack')" -eq 1 ]; then
  pass "a 12-GPU job requiring one rack lands entirely in one rack"
else
  fail "a 12-GPU job requiring one rack lands entirely in one rack"
fi

reset_namespace
kubectl apply -f "${SMOKE}/topology-placement.yaml" >/dev/null
kubectl delete job rack-local-training -n "${NS}" --wait=true >/dev/null 2>&1
kubectl apply -f "${SMOKE}/topology-placement-impossible.yaml" >/dev/null
sleep "${TIMEOUT}"
if [ "$(scheduled_count 'job-name=rack-local-impossible')" -eq 0 ]; then
  pass "a 20-GPU job requiring one rack is refused, though the cluster has 32 free GPUs"
else
  fail "a 20-GPU job requiring one rack is refused, though the cluster has 32 free GPUs"
fi

# --- per-GPU device attributes ---------------------------------------------------------

head "Device attribute selection"
reset_namespace
kubectl apply -f "${SMOKE}/device-selector-numa.yaml" >/dev/null
numa_running=$(wait_for_scheduled 'app=numa0-only' 16)
if [ "${numa_running}" -eq 16 ]; then
  pass "20 pods selecting NUMA node 0 get exactly the 16 GPUs on it"
else
  fail "20 pods selecting NUMA node 0 get exactly the 16 GPUs on it (got ${numa_running})"
fi

reset_namespace
kubectl apply -f "${SMOKE}/device-selector-nvlink.yaml" >/dev/null
nvlink_running=$(wait_for_scheduled 'app=one-nvlink-domain' 8)
if [ "${nvlink_running}" -eq 8 ] &&
   [ "$(distinct_label_values 'app=one-nvlink-domain' 'gpu-sim\.io/nvlink-domain')" -eq 1 ]; then
  pass "12 pods selecting one NVLink domain get exactly its 8 GPUs"
else
  fail "12 pods selecting one NVLink domain get exactly its 8 GPUs (got ${nvlink_running})"
fi

# --- portability ------------------------------------------------------------------------

head "Stock kube-scheduler"
reset_namespace
kubectl apply -f "${SMOKE}/default-scheduler.yaml" >/dev/null
phase=""
for _ in $(seq 1 $((TIMEOUT / 3))); do
  phase=$(kubectl get pod default-scheduler-gpu -n "${NS}" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "${phase}" = "Running" ] && break
  sleep 3
done
allocated=$(kubectl get resourceclaims -n "${NS}" -o json |
  jq -r '[.items[] | select(.status.allocation != null) | .status.allocation.devices.results[0].device] | first // ""')
numa=$(kubectl get resourceslices -o json |
  jq -r --arg d "${allocated}" '[.items[].spec.devices[] | select(.name==$d) | .attributes.numaNode.int] | first // ""')
if [ "${phase}" = "Running" ] && [ "${numa}" = "1" ]; then
  pass "the stock kube-scheduler allocates a GPU on the NUMA node the selector asked for"
else
  fail "the stock kube-scheduler allocates a GPU on the NUMA node the selector asked for"
fi

reset_namespace

# --- result ------------------------------------------------------------------------------

head "Result"
printf '  %d passed, %d failed\n\n' "${passed}" "${failed}"
[ "${failed}" -eq 0 ]
