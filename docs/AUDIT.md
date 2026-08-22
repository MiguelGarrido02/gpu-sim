# Phase 0 audit — what the existing stack gives us, and what it does not

_Carried out August 2026 on a MacBook Air M5 (macOS 26.4, arm64, 20 GB RAM)._

This is the Phase 0 artifact called for in [`PLAN.md`](PLAN.md): a map of the upstream
code, an account of what actually works when it is running, and the concrete extension
points Phase 1 will build on. Everything below was verified on a live cluster, not read
off a README.

## 1. Verified environment

| Component | Version | Notes |
|---|---|---|
| Kubernetes (kind node image) | v1.36.1 | `resource.k8s.io/v1` served by default — DRA needs no feature gate |
| kind | v0.32.0 | |
| Container runtime | OrbStack (Docker API 29.4.0) | 10 CPUs, 11.7 GiB to the VM; lighter than Docker Desktop |
| KWOK | v0.7.0 | Pinned to what fake-gpu-operator's own e2e suite tests; v0.8.0 exists but is unvalidated with the operator |
| fake-gpu-operator | 0.2.0 | Chart and images from `ghcr.io/run-ai/fake-gpu-operator` |
| KAI Scheduler | v0.17.0 | Chart from `ghcr.io/kai-scheduler/kai-scheduler` |
| Helm | 4.2.4 | |
| Go | 1.27.0 | |

**arm64 is not a problem.** Every image needed — `kwok-dra-plugin`, `status-updater`,
`status-exporter`, `kwok-gpu-device-plugin`, `topology-server` and the KAI scheduler —
publishes a `linux/arm64` manifest. No local builds were required. This was the main
platform risk going in, and it is closed.

**Resource cost is negligible.** Four simulated 8-GPU nodes plus the full operator and
KAI control planes fit comfortably. Simulating hundreds of nodes on this laptop is not
in question.

## 2. How the simulation is wired together

The chain from configuration to a scheduled pod runs like this:

```
Helm values (cluster.nodePools[].gpu.profile)
    │
    ▼
gpu-profile-<name> ConfigMap          builtin profiles synced from NVIDIA/k8s-test-infra
    │                                 (a100, h100, b200, gb200, gb300, l40s, t4)
    ▼
status-updater                        watches nodes labelled
    │                                 run.ai/simulated-gpu-node-pool=<pool>,
    │                                 resolves the profile, writes one topology
    ▼                                 ConfigMap per node
topology ConfigMap (per node)
    │
    ├──────────────► kwok-dra-plugin ──► ResourceSlice (one per node)
    │
    ├──────────────► status-exporter ──► NodeResourceTopology (NUMA), Prometheus metrics
    │
    └──────────────► topology-server ──► serves topology to the simulated nvidia-smi
```

The relevant source lives at:

| Concern | Path in `run-ai/fake-gpu-operator` |
|---|---|
| Topology data model | `internal/common/topology/types.go` |
| Profile resolution (Load → Merge → Extract) | `internal/common/topology/resolve.go` |
| **ResourceSlice generation** | `internal/kwok-dra-plugin/handlers/resourceslice/handler.go` |
| ConfigMap watch driving the above | `internal/kwok-dra-plugin/controllers/configmap/reconciler.go` |
| DeviceClass manifest | `deploy/fake-gpu-operator/templates/dra-device-plugin/templates/deviceclass.yaml` |
| Builtin GPU profiles | `deploy/fake-gpu-operator/templates/profiles/builtin.yaml` |

`handler.go` is the single most important file for this project. It is where Phase 1
either extends or replaces behaviour.

## 3. What the simulated cluster actually publishes

Four KWOK nodes on the `h100` profile with `device_count: 8` produce four
`ResourceSlice`s of 8 devices each. A device looks like this, in full:

```json
{
  "name": "gpu-977c4dd2-a349-5388-a0a9-448c62948bd8",
  "attributes": {
    "gpu.nvidia.com/type":        { "string": "gpu" },
    "gpu.nvidia.com/uuid":        { "string": "GPU-977c4dd2-a349-5388-a0a9-448c62948bd8" },
    "gpu.nvidia.com/productName": { "string": "NVIDIA H100 80GB HBM3" },
    "uuid":                       { "string": "GPU-977c4dd2-a349-5388-a0a9-448c62948bd8" },
    "model":                      { "string": "NVIDIA H100 80GB HBM3" }
  },
  "capacity": { "memory": { "value": "80Gi" } }
}
```

Five attributes, two of which are back-compat duplicates, plus one capacity field. That
is the entire topological vocabulary available to a scheduler today.

The `DeviceClass` is minimal but does one significant thing:

```yaml
name: gpu.nvidia.com
spec:
  selectors:
    - cel:
        expression: device.driver == 'gpu.nvidia.com' && device.attributes['gpu.nvidia.com'].type == 'gpu'
  extendedResourceName: nvidia.com/gpu
```

`extendedResourceName` is a Kubernetes v1.34+ feature: a pod may request a plain
`nvidia.com/gpu: 1` limit with no `ResourceClaim` at all, and the scheduler routes it
onto DRA devices. **This was verified working with the legacy device plugin disabled** —
a 4-pod job requesting `nvidia.com/gpu: 1` was placed and bound. It means gpu-sim does
not need `kwok-gpu-device-plugin`, and existing GPU manifests migrate to the simulated
cluster unchanged.

## 4. Gap 1 — the data model cannot express topology

The gap is not that the ResourceSlice is missing a few fields. It is that the type it is
generated from has nowhere to put them:

```go
// internal/common/topology/types.go
type GpuDetails struct {
    ID     string    `yaml:"id"`
    Status GpuStatus `yaml:"status"`
}
```

A GPU is a UUID and an allocation status. There is no device index, no NUMA node, no
PCIe root complex, no NVLink peer set, no NVSwitch or fabric domain, no MIG partition.
`NodeTopology` above it carries `GpuMemory`, `GpuProduct` and `MigStrategy` — all
per-node scalars, uniform across every GPU on the node.

This has a consequence worth stating plainly: **every GPU on a node is
indistinguishable from every other GPU on that node.** No selector can express "the four
GPUs behind the same NVSwitch" because nothing distinguishes them.

`devicesFromTopology()` in `handler.go` reflects this faithfully — it loops over
`nodeTopology.Gpus` and emits the same five attributes for each, varying only the UUID.

Note the asymmetry: the *profiles* are richer than the model that consumes them. Recent
builtin profiles carry a `pcie_topology` block with PCI root complexes and a per-device
`numa_node`, synced from NVIDIA's test infrastructure. That information is loaded,
merged and then discarded, because `GpuDetails` has no field to hold it and
`devicesFromTopology` never looks for it. **Some of the data we need is already in the
cluster; it simply is not plumbed through.** That makes Phase 1 cheaper than expected.

## 5. Gap 2 — MIG is a strategy string

`MigStrategy` is a single per-node string (`none`, `mixed`, `single`). There is no
representation of a GPU's partition table, no notion of which slices are carved, which
are free, or whether the free capacity is contiguous. `migFaker` operates on real nodes
through the device-plugin path and does not touch the KWOK/DRA path at all. Phase 2
builds this from nothing; there is no partial implementation to extend.

## 6. What works: gang scheduling on a fully simulated cluster

This is the Phase 0 acceptance test, and it passes.

**Positive case** ([`hack/smoke/kai-gang.yaml`](../hack/smoke/kai-gang.yaml)) — a
PodGroup with `minMember: 2`, two pods each claiming one GPU through a
`ResourceClaimTemplate`:

```
gang-member-1  Running  gpu-node-1
gang-member-2  Running  gpu-node-1
distinct devices allocated: 2
PodGroup status: allocated gpu.nvidia.com=2, requested gpu.nvidia.com=2
```

Two *distinct* devices, so the allocation is real rather than a scheduler that ignored
the GPU request.

**Negative case**
([`hack/smoke/kai-gang-overcapacity.yaml`](../hack/smoke/kai-gang-overcapacity.yaml)) —
a 40-pod gang against a 32-GPU cluster:

```
pending: 40 / scheduled: 0
```

All-or-nothing semantics hold: not one pod is placed. Together these two prove the whole
chain, from profile ConfigMap through ResourceSlice to KAI's binder and DRA allocation.

### Two traps that produce false passes

Both of these were hit during the audit and cost real time. They are worth writing down
because the Phase 3 harness must not fall into either.

1. **A plain `batch/v1` Job is not a gang.** KAI's pod-grouper gives it a PodGroup with
   `minMember: 1`, so the pods are scheduled independently and a "gang test" passes
   without ever testing gang behaviour. The `kai.scheduler/batch-min-member` annotation
   on the Job is required, and it works — annotating `"40"` produced `minMember=40`.

2. **A terminal pod releases its ResourceClaim.** KWOK's `stage-fast` drives pods with
   `restartPolicy: Never` straight to `Completed`, after which the claims are gone and
   there is nothing left to inspect. The first run of this audit showed two "successful"
   pods and zero ResourceClaims in the namespace — indistinguishable from a run where
   GPUs were never allocated. Long-lived assertions need `restartPolicy: Always`.

Also worth noting: KAI v0.17's binder enables its `dynamicresources` plugin by default.
The project's [`docs/dra`](https://github.com/kai-scheduler/KAI-Scheduler/tree/main/docs/dra)
still instructs users to pass `--feature-gates=DynamicResourceAllocation=true` to the
scheduler and binder; that is stale, and DRA worked here without it.

## 7. What does not work: topology-aware scheduling

**No topology-constrained workload can be scheduled on the simulated cluster at all.**

KAI's topology-aware scheduling is driven by a `Topology` CRD listing node-label levels,
with workloads opting in through annotations:

```yaml
apiVersion: kai.scheduler/v1alpha1
kind: Topology
spec:
  levels:
    - nodeLabel: "gpu-sim.io/rack"
      alias: "rack"
    - nodeLabel: "kubernetes.io/hostname"
```

Nodes were labelled into two racks of two nodes (16 GPUs per rack) and gangs were
submitted with `kai.scheduler/topology-required-placement`. Every variation failed
identically:

| Variation | Gang size | Result |
|---|---|---|
| `required` at `rack`, DRA `ResourceClaim` | 12 | all pending |
| `required` at `rack`, extended resource | 12 | all pending |
| `required` at `rack`, extended resource | 10 | all pending |
| `required` at `rack`, extended resource | 8 (fits one node) | all pending |
| `required` at `kubernetes.io/hostname` | 8 | all pending |
| `preferred` at `rack` | 8 | all pending |

Always the same message:

```
topology cluster-topology, requirement gpu-sim.io/rack couldn't be satisfied
for job <...>: not enough resources in the cluster to allocate the job
```

The identical 12-pod gang **with the topology annotations removed schedules
immediately** (8 pods on `gpu-node-1`, 4 on `gpu-node-2`), so this is not a capacity
problem and not a gang-size problem.

Hypotheses ruled out during the audit:

- *Node-level GPU capacity is missing.* Enabling `kwok-gpu-device-plugin` so nodes
  advertise `nvidia.com/gpu: 8` in `status.allocatable` changed nothing.
- *The resource is DRA-only and the topology plugin cannot see it.* Requesting the
  extended resource `nvidia.com/gpu: 1` instead of a `ResourceClaim` changed nothing.
- *The KWOK `NoSchedule` taint excludes nodes from domain accounting.* Removing the
  taint changed nothing.
- *The label alias is not resolving.* It resolves — the error message names the raw
  label `gpu-sim.io/rack`, and the PodGroup's `spec.topologyConstraint` is populated
  correctly.
- *A stale scheduler cache.* Restarting `kai-scheduler-default` and recreating the
  workloads changed nothing.

Since a gang that fits entirely on a single node still fails at
`kubernetes.io/hostname` level, KAI's topology plugin is computing **zero available
resources in every domain**, whatever the level.

### Resolution: the simulated nodes were missing well-known labels

Root-caused at the start of Phase 1, and it is **not** a KAI bug.

The decisive experiment was submitting a topology-constrained job that requested **no
GPU at all**, only CPU and memory. It failed identically. That eliminated GPUs, DRA and
resource accounting in a single step and pointed at the shape of the topology tree
rather than its contents.

`isNodePartOfTopology` in `pkg/scheduler/plugins/topology/common.go` drops any node that
is missing a label for *any* level of the `Topology` CR:

```go
func isNodePartOfTopology(nodeInfo *node_info.NodeInfo, levels []kaiv1alpha1.TopologyLevel) bool {
	for _, level := range levels {
		if _, found := nodeInfo.Node.Labels[level.NodeLabel]; !found {
			return false
		}
	}
	return true
}
```

The `Topology` CR used `kubernetes.io/hostname` as its leaf level, which is the
conventional choice and appears in KAI's own documentation. **The KWOK nodes did not
carry that label.** A real kubelet registers `kubernetes.io/hostname`, `kubernetes.io/os`
and `kubernetes.io/arch` on every node it joins; a node object created from a hand-written
manifest has no kubelet, so nothing adds them. Diffing a simulated node's labels against
the real control-plane node's makes the omission obvious in one line.

Consequence: every node was rejected from the topology tree, every domain ended up empty,
and every domain therefore reported zero capacity — regardless of level, constraint type,
gang size or resource requested. Every observation in the table above follows from that
one missing label.

With `kubernetes.io/hostname`, `kubernetes.io/os` and `kubernetes.io/arch` added to the
generated nodes — a requirement `topology-gen` now enforces unconditionally — a 12-GPU gang
requiring same-rack placement is scheduled entirely inside one rack:

```
gpu-node-1: 6
gpu-node-2: 6
racks used: 12 × rack-1
```

That is the Phase 1 acceptance test, passing on a fully simulated cluster.

**The lesson generalises well beyond this bug, and it is the project's thesis in
miniature.** The simulation failed not because it modelled GPUs badly, but because a
simulated node was subtly *less complete* than a real one in a way no error message
pointed at.

KAI's diagnostics made it worse, though narrowly. `checkJobDomainFit` builds a precise
per-resource error and `subSetNodesFn` discards it, substituting a generic message. That
only bites when the failing domain is the root — which is exactly the case here, because an
empty tree collapses every lookup to the root, and "not enough resources in the cluster"
was the least useful thing the scheduler could have said about a completely idle cluster.
Once nodes are in the tree, other paths do report usefully: the Phase 1 negative test gets
back "node-group fd-1.rack-1 can allocate only 16 of 20 required pods". Propagating the
detail in the root case too is a small, worthwhile contribution back to KAI.

For Phase 1 this sets a requirement: `topology-gen` must emit the **full well-known
label set** a real kubelet would register, not just the topology labels the project cares
about. Fidelity gaps in simulated objects show up as inexplicable scheduler behaviour,
which is exactly the failure mode gpu-sim exists to spare its users.

## 8. A trap in the configuration: double-counted GPUs

With both `kwok-gpu-device-plugin` (node-level `nvidia.com/gpu`) and `kwok-dra-plugin`
(DRA `ResourceSlice`s) enabled, KAI's scheduler logs report:

```
Total allocatable resources are <CPU: 519.6 (cores), memory: 8805.31 (GB), Gpus: 64>, number of nodes: <5>
```

64 GPUs on a cluster that has 32. The two publication paths are counted additively.
Anything reasoning about cluster utilization would be wrong by a factor of two.

`hack/values-fake-gpu-operator.yaml` therefore keeps `kwokGpuDevicePlugin` disabled and
relies on the DeviceClass's `extendedResourceName` for legacy `nvidia.com/gpu` requests,
which section 3 confirms works. Phase 3's utilization metrics must not assume otherwise.

## 9. Extension points for Phase 1

Ordered by how much they cost and how much they give back.

1. **`devicesFromTopology()` in `internal/kwok-dra-plugin/handlers/resourceslice/handler.go`.**
   The single function that decides what a simulated GPU looks like to a scheduler.
   Everything in Phase 1 either changes this function or replaces the controller that
   calls it.

2. **`GpuDetails` in `internal/common/topology/types.go`.** Needs per-GPU fields —
   index, NUMA node, PCIe root, NVLink peers, fabric/NVSwitch domain — before the
   handler has anything to publish.

3. **The profile `pcie_topology` block.** Already present in the builtin profiles,
   already loaded, currently discarded. Plumbing it through is the cheapest real
   topology gpu-sim can offer, and it comes from NVIDIA's own data.

4. **Node labels, not just device attributes.** KAI's topology levels are *node* labels.
   Whatever cluster topology gpu-sim describes has to be projected twice: into node
   labels for inter-node domain selection, and into DRA device attributes for
   intra-node GPU selection. The Phase 1 YAML schema in `PLAN.md` already anticipates
   both; this audit confirms both are load-bearing.

5. **`ResourceSlice` ownership.** `kwok-dra-plugin` reconciles the whole slice from the
   topology ConfigMap on every change, overwriting it. gpu-sim cannot simply patch
   attributes onto the slices from a sidecar — they would be reverted.

   The obvious alternative is to feed data through the topology ConfigMap that
   `kwok-dra-plugin` already reads, and `status-updater` does leave an existing ConfigMap
   alone when creating. **That does not work either**, as stage B of Phase 1 established:
   `status-updater`'s *pod* handler server-side-applies the whole `NodeTopology` struct
   back into the ConfigMap with `Force: true` on every GPU pod event, and since the
   payload is one opaque YAML string there is no field-level merge to protect extra data.
   Anything gpu-sim wrote there would survive until the first pod was scheduled.

   The resolution — gpu-sim publishing its own `ResourceSlice`s from its own topology
   resource, with `kwokDraPlugin` disabled — is argued in
   [`designs/topology-model.md`](designs/topology-model.md).

## 10. Reproducing this

```bash
hack/setup-cluster.sh 4        # kind + KWOK + fake-gpu-operator + 4 simulated 8-GPU nodes
hack/install-kai.sh            # KAI Scheduler v0.17.0

kubectl apply -f hack/smoke/kai-gang.yaml               # expect: 2 pods Running, 2 distinct devices
kubectl apply -f hack/smoke/kai-gang-overcapacity.yaml  # expect: 40 pods Pending, 0 scheduled
```

Teardown: `make cluster-down`.

## 11. Phase 0 verdict

The base works. A gang-scheduled GPU workload runs on a fully simulated cluster on a
laptop with no NVIDIA hardware, which is what Phase 0 set out to establish. The four
value-add gaps identified in `PLAN.md` are all confirmed to be genuinely absent rather
than merely undocumented.

Topology-aware scheduling also works, once the simulated nodes carry the labels a real
kubelet would have registered (§7). Phase 1 can therefore proceed as originally planned —
modelling NVLink topology and publishing it — rather than having to repair the consumer
first.
