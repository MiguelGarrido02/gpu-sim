# Design: the cluster topology model

_Phase 1, stage B. Status: proposed._

Phase 1 gives gpu-sim a topology model: a declarative description of how GPUs are wired
together, projected into the Kubernetes objects a scheduler actually reads. This document
settles the three decisions that shape everything built in stage C.

## Problem

A simulated GPU today carries five attributes, and every GPU on a node is identical to
every other GPU on that node (see [`../AUDIT.md`](../AUDIT.md) §3–4). Nothing can express
"these four GPUs sit behind the same NVSwitch", so nothing can be tested against it.

What has to come out the other end:

1. **Per-GPU attributes** on DRA `ResourceSlice`s, so a `DeviceClass` CEL selector can
   pick GPUs by their position in the fabric.
2. **Node labels**, because inter-node topology decisions are made from node labels — KAI's
   `Topology` CR is a list of node-label levels, and the same is true of Kueue's topology
   support.
3. **A KAI `Topology` CR** consistent with those labels.

All three from one source of truth. A simulator whose two projections disagree is worse
than no simulator.

## Decision 1 — gpu-sim owns the ResourceSlices

**Decision: gpu-sim publishes `ResourceSlice`s itself, from its own topology resource, and
`kwokDraPlugin` is disabled. No fork of fake-gpu-operator.**

The audit suggested fake-gpu-operator's per-node topology ConfigMap as the integration
seam, on the grounds that `kwok-dra-plugin` reads it and `status-updater` only *creates* it
(it returns early if the ConfigMap already exists, so it will not clobber a richer one).
Checking the write path invalidates that:

```go
// internal/common/topology/kubernetes.go
func UpdateNodeTopologyCM(kubeclient kubernetes.Interface, nodeTopology *NodeTopology, nodeName string) error {
	_, cm, err := ToNodeTopologyCM(nodeTopology, nodeName)
	...
	_, err = kubeclient.CoreV1().ConfigMaps(...).Apply(context.TODO(), cm,
		metav1.ApplyOptions{FieldManager: "fake-gpu-operator", Force: true})
```

`status-updater`'s **pod** handler calls this on every GPU pod add, update and delete. It
marshals the whole `NodeTopology` struct into a single `topology.yml` string and
server-side-applies it with `Force: true`. Because the payload is one opaque string, there
is no field-level merge to protect anything inside it, and `NodeTopology`/`GpuDetails` have
no fields for topology data. **Any per-GPU attributes written into that ConfigMap would
survive exactly until the first pod is scheduled on the node.** The seam is not a seam.

That leaves two real options:

| Option | Cost |
|---|---|
| Fork fake-gpu-operator | `GpuDetails` lives in `internal/common/topology`, shared by `status-updater`. Forking `kwok-dra-plugin` alone is not enough — the pod handler would still wipe the fields — so this means forking the operator's core and tracking it forever. |
| Own publisher | ~400 lines to replace, and gpu-sim owns its topology end to end. |

The second is both cheaper and cleaner, and it draws a defensible line: **fake-gpu-operator
owns which GPUs exist and who is using them; gpu-sim owns how they are wired together.**

### What makes this safe: GPU IDs are deterministic

The concern with taking over publication is drift — fake-gpu-operator's `nvidia-smi`,
metrics and pod-to-GPU tracking key off the GPU IDs it generates, and if our
`ResourceSlice` used different IDs, that tracking would silently break.

It will not, because the IDs are a pure function of node name and device index:

```go
// internal/status-updater/handlers/node/topology_cm.go
ID: fmt.Sprintf("GPU-%s", uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprintf("%s-%d", nodeName, idx))))
```

Verified against the live cluster — a UUIDv5 over `<nodeName>-<index>` with the nil
namespace reproduces the published UUIDs exactly:

```
gpu-node-1-0  ->  GPU-977c4dd2-a349-5388-a0a9-448c62948bd8   ✓ matches ResourceSlice
gpu-node-1-1  ->  GPU-bea8c519-c518-5a51-a8e0-9205b8f99767   ✓
gpu-node-1-2  ->  GPU-6bce07b9-7c0b-5f36-b90f-065e46b2ac19   ✓
```

gpu-sim reproduces the same derivation, so its slices stay drop-in compatible. The
derivation is a compatibility contract with upstream and needs a unit test asserting these
exact vectors, so an upstream change to it fails loudly rather than silently desynchronising
the two components.

The index is also *recoverable* from the ID, which is what makes per-GPU topology
attachable at all: topology is declared per index, and the index maps to a stable identity
without either side having to carry the other's data.

## Decision 2 — mirror NVIDIA's attribute names; namespace only what is genuinely new

**Decision: use the real NVIDIA DRA driver's attribute names wherever they exist. Publish
gpu-sim's own attributes under a `gpu-sim.io` prefix only for properties the real driver
does not model.**

The point of gpu-sim is that a policy validated against it works on real hardware. If a
user writes a CEL selector against `gpu-sim.io/numaNode` and production publishes
`numaNode`, the selector silently matches nothing and the simulation has actively misled
them. Fidelity in the *names* is as load-bearing as fidelity in the values.

`NVIDIA/k8s-dra-driver-gpu` publishes, per physical GPU:

```
type, uuid, productName, brand, architecture,
cudaComputeCapability, driverVersion, cudaDriverVersion,
pciBusID, pcieRoot, numaNode, addressingMode
gpuModuleID, partition<N>     (behind the FabricManagerPartitioning feature gate)
```

Two things follow.

**`pcieRoot` and `numaNode` already exist upstream** — and fake-gpu-operator's builtin
profiles, synced from NVIDIA's own test infrastructure, already carry `pcie_topology` and a
per-device `numa_node` that the current code loads and discards. So the first slice of
Phase 1 is not invention at all: it is plumbing NVIDIA's own data through to NVIDIA's own
attribute names.

**There is no NVLink-domain or NVLink-peer attribute.** Fabric information reaches
workloads through the separate ComputeDomain/IMEX machinery, not through GPU device
attributes. That absence is precisely the gap this project exists to fill, and it is the
one place where inventing names is legitimate:

| Attribute | Origin | Meaning |
|---|---|---|
| `nvlinkDomain` | gpu-sim | Identifier of the NVLink/NVSwitch domain the GPU belongs to. Node-scoped on DGX-class hardware, rack-scoped on GB200 NVL72. |
| `nvlinkPeerCount` | gpu-sim | Number of GPUs reachable over NVLink. Lets a selector distinguish a full mesh from an isolated GPU without enumerating peers. |
| `faultDomain` | gpu-sim | The blast radius the GPU sits in. Consumed by Phase 4's fault injection. |

These are published as `gpu-sim.io/<name>` so it is unambiguous, both to users and in
issue reports, that they are a simulation extension and not something NVIDIA ships. If
upstream later standardises an equivalent, we adopt it and deprecate ours.

## Decision 3 — one source of truth, three projections

The topology YAML is authored once and fans out:

```
                        ClusterTopology (YAML)
                                 │
            ┌────────────────────┼────────────────────┐
            ▼                    ▼                    ▼
      KWOK Node objects     ResourceSlices      KAI Topology CR
      + node labels         + per-GPU attrs     (node-label levels)
      (inter-node)          (intra-node)        (scheduler config)
```

Node labels answer *which node*; device attributes answer *which GPU within it*; the
`Topology` CR tells KAI which labels are levels. Generating all three from one file is what
keeps them consistent — and stage A showed how expensive an inconsistency is to debug.

Stage A also produced a hard requirement: **generated nodes must carry the full well-known
label set a real kubelet registers** (`kubernetes.io/hostname`, `kubernetes.io/os`,
`kubernetes.io/arch`), not merely the topology labels. Omitting `kubernetes.io/hostname`
made every topology-constrained workload unschedulable with an error that pointed at
capacity on an idle cluster. `topology-gen` emits them unconditionally.

## The schema

```yaml
apiVersion: gpu-sim.io/v1alpha1
kind: ClusterTopology
metadata:
  name: two-racks-h100
spec:
  # Node pools describe a machine type. `profile` names one of fake-gpu-operator's
  # builtin GPU profiles, so product name, memory and CUDA versions come from NVIDIA's
  # data rather than being restated here.
  nodePools:
    dgx-h100:
      profile: h100
      gpuCount: 8
      intraNode:
        nvlink: full-mesh      # full-mesh | none
        numaZones: 2           # 4 GPUs per zone, as on a real DGX H100
        pcieRootsPerNumaZone: 1

  racks:
    - name: rack-1
      faultDomain: fd-1
      # nvlinkDomain is omitted: on DGX-class hardware NVLink stops at the node
      # boundary, so each node forms its own domain.
      nodes:
        - { name: node-1, pool: dgx-h100 }
        - { name: node-2, pool: dgx-h100 }

    - name: rack-2
      faultDomain: fd-2
      nodes:
        - { name: node-3, pool: dgx-h100 }
        - { name: node-4, pool: dgx-h100 }
```

A GB200 NVL72 rack — 72 GPUs in one multi-node NVLink domain — is the same schema with the
domain declared at rack level:

```yaml
  racks:
    - name: nvl72-rack-1
      faultDomain: fd-1
      nvlinkDomain: nvl72-1    # NVLink spans the rack, not just the node
      nodes: [ ... 18 nodes of 4 GPUs ... ]
```

Whether `nvlinkDomain` is set at rack level is the single knob distinguishing the two
hardware generations, which is the property worth capturing: it is exactly the distinction
a scheduling policy has to reason about.

### Generated output

For the two-rack example above:

**Node labels** (per node, in addition to the well-known kubelet set)

```
gpu-sim.io/rack=rack-1
gpu-sim.io/fault-domain=fd-1
gpu-sim.io/nvlink-domain=node-1        # node-scoped, since the rack declares none
run.ai/simulated-gpu-node-pool=dgx-h100
```

**Device attributes** (per GPU, on the node's `ResourceSlice`)

```
type=gpu  uuid=GPU-977c4dd2-…  productName="NVIDIA H100 80GB HBM3"
architecture=hopper  pcieRoot=pci0000:0d  numaNode=0
gpu-sim.io/nvlinkDomain=node-1  gpu-sim.io/nvlinkPeerCount=7  gpu-sim.io/faultDomain=fd-1
```

**KAI `Topology` CR**

```yaml
apiVersion: kai.scheduler/v1alpha1
kind: Topology
metadata:
  name: two-racks-h100
spec:
  levels:
    - nodeLabel: gpu-sim.io/rack
      alias: rack
    - nodeLabel: kubernetes.io/hostname
```

## Risks

**Taking over publication means tracking upstream.** If fake-gpu-operator changes its GPU
ID derivation, gpu-sim's slices desynchronise from its pod tracking. Mitigated by the unit
test pinning the derivation, which turns a silent break into a failing build.

**`gpu-sim.io` attributes are ours alone.** No real scheduler filters on them today. They
are useful immediately for gpu-sim's own assertions and for anyone writing a topology-aware
policy, but their value grows only if the ecosystem converges on something similar. The
`pcieRoot`/`numaNode`/node-label projections carry real value regardless, which is why they
come first in stage C.

## Stage C consequences

1. Reproduce and unit-test the GPU ID derivation.
2. Parse the schema; generate KWOK nodes with the full label set.
3. Publish `ResourceSlice`s: NVIDIA-compatible attributes first (`pcieRoot`, `numaNode`,
   `architecture`), from profile data where it exists.
4. Add the `gpu-sim.io` fabric attributes.
5. Generate the KAI `Topology` CR from the same file.
6. Disable `kwokDraPlugin` in the Helm values, with a comment pointing here.
