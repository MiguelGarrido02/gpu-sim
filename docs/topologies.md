# Writing a cluster topology

A topology file describes a GPU cluster: what machines it has, how their GPUs are wired to
each other, and which failures take out which parts. `gpu-sim` turns that one file
into everything a scheduler needs to reason about the cluster.

```bash
gpu-sim topology render -f topologies/two-racks-h100.yaml   # see what it would create
gpu-sim topology apply  -f topologies/two-racks-h100.yaml   # create it
```

## What a topology produces

Three things, from the one file, so they cannot disagree with each other:

| Output | Answers | Read by |
|---|---|---|
| Node labels | *which machine* | The scheduler's topology levels |
| `ResourceSlice` device attributes | *which GPU inside that machine* | `DeviceClass` and `ResourceClaim` CEL selectors |
| A `Topology` object | which labels are levels, and their order | KAI Scheduler |

Node labels handle inter-node placement — "keep this job in one rack". Device attributes
handle intra-node selection — "give me a GPU on NUMA node 0". Most real policies use both.

## The schema

```yaml
apiVersion: gpu-sim.io/v1alpha1
kind: ClusterTopology
metadata:
  name: two-racks-h100          # names the generated Topology object
spec:
  nodePools:
    dgx-h100:                   # a machine type, referenced by nodes below
      profile: h100
      gpuCount: 8
      nvlink: full-mesh
  racks:
    - name: rack-1
      faultDomain: fd-1
      nvlinkDomain: nvl72-1     # optional — see below
      nodes:
        - { name: gpu-node-1, pool: dgx-h100 }
```

### Node pools

| Field | Meaning |
|---|---|
| `profile` | One of the GPU profiles fake-gpu-operator publishes: `a100`, `b200`, `gb200`, `gb300`, `h100`, `l40s`, `t4`. |
| `gpuCount` | GPUs per node in this pool. |
| `nvlink` | `full-mesh` (an NVSwitch backplane connects every GPU to every other) or `none` (PCIe only). |

A pool is deliberately thin. Product name, memory, architecture, per-GPU PCI bus IDs, PCIe
root complexes and NUMA assignments all come from the profile, which is synced from
NVIDIA's own test infrastructure. Restating any of it here would mean inventing hardware
details that already exist in a more authoritative form, and inviting them to drift.

`gpuCount` may be smaller than the profile's device list, which is how a smaller machine is
modelled from a stock profile. It may also be larger, in which case PCIe and NUMA
assignments wrap around — the distribution stays balanced, it just repeats. That is not a
machine anyone builds, but it is a legitimate thing to ask a simulator for.

### Racks and NVLink domains

`nvlinkDomain` on a rack is the one field that separates the two shapes of GPU hardware,
and it is the distinction a scheduling policy actually has to reason about.

**Omitted** — NVLink stops at the node boundary. Each node forms its own domain, named
after itself. This is DGX/HGX-class hardware: an H100 node's 8 GPUs are a full mesh, but
nothing crosses to the next node.

**Set** — one NVLink domain spans the whole rack. This is GB200 NVL72: 72 GPUs across 18
compute trays behind one NVSwitch fabric, all reachable from each other at NVLink speed.

The consequence is concrete. A 32-GPU job requiring all its GPUs in one NVLink domain is
unschedulable on the first and schedules on the second — the same workload, the same
scheduler, a different answer. Reproducing that is what `scenarios/nvlink-gang-dgx.yaml` and
`scenarios/nvlink-gang-nvl72.yaml` do, and the reason the project exists.

`faultDomain` is the blast radius: what goes down together. Several racks may share one
when they sit behind the same power or cooling. Nothing consumes it yet beyond selection;
Phase 4's fault injection is what it is there for.

## What ends up on the objects

### Node labels

```
gpu-sim.io/rack=rack-1
gpu-sim.io/fault-domain=fd-1
gpu-sim.io/nvlink-domain=gpu-node-1     # omitted when the pool has nvlink: none
run.ai/simulated-gpu-node-pool=dgx-h100
kubernetes.io/hostname=gpu-node-1
kubernetes.io/os=linux
kubernetes.io/arch=amd64
type=kwok
app.kubernetes.io/managed-by=gpu-sim
gpu-sim.io/topology=two-racks-h100
```

The `kubernetes.io/*` labels are not decoration. A real kubelet registers them; a simulated
node has no kubelet, so `gpu-sim` adds them. Schedulers assume they are always
present — KAI drops any node missing a label for *any* level of its topology, and
`kubernetes.io/hostname` is the conventional leaf, so omitting it empties the topology tree
and leaves every constrained workload pending with an error about capacity on an idle
cluster. See [`AUDIT.md`](AUDIT.md) §7.

### Device attributes

Every GPU carries these. The unqualified names are exactly the ones
[NVIDIA's real DRA driver](https://github.com/NVIDIA/k8s-dra-driver-gpu) publishes, so a
selector written against a simulated cluster still matches on real hardware:

| Attribute | Source |
|---|---|
| `type`, `uuid`, `productName`, `architecture` | GPU profile |
| `pciBusID`, `pcieRoot`, `numaNode` | GPU profile's PCIe root complex map |
| `gpu-sim.io/nvlinkDomain` | topology file |
| `gpu-sim.io/nvlinkPeerCount` | topology file: GPUs reachable over NVLink |
| `gpu-sim.io/faultDomain` | topology file |
| `gpu.nvidia.com/*`, `model` | reproduced from fake-gpu-operator for compatibility |

`pcieRoot` and `numaNode` appear only when the profile declares a root complex map.
Defaulting them to zero would tell a scheduler every GPU shares one socket, which is both
false and unfalsifiable from outside.

The `gpu-sim.io/*` attributes exist because NVIDIA's driver has no equivalent: fabric
information reaches workloads through the separate ComputeDomain/IMEX machinery rather than
as device attributes. That gap is what this project fills, and the prefix keeps it
unambiguous that these are simulation extensions rather than something NVIDIA ships.

## Writing policies against it

Placement across nodes, by topology level — `fault-domain`, `rack`, `nvlink-domain` or
`host`:

```yaml
metadata:
  annotations:
    kai.scheduler/topology: "two-racks-h100"
    kai.scheduler/topology-required-placement: "rack"
    kai.scheduler/batch-min-member: "12"        # without this, a Job is not a gang
```

Selection within a node, by device attribute:

```yaml
selectors:
  - cel:
      expression: device.attributes['gpu.nvidia.com'].numaNode == 0
  - cel:
      expression: device.attributes['gpu-sim.io'].nvlinkPeerCount >= 7
```

Unqualified attributes take the publishing driver's name as their domain, which is why
`numaNode` is read as `device.attributes['gpu.nvidia.com'].numaNode`. The `gpu-sim.io`
ones are explicitly qualified.

Worked examples of all of these, each paired with a case that must *not* schedule, are in
[`scenarios/`](../scenarios).

## Bundled topologies

| File | Shape |
|---|---|
| [`two-racks-h100.yaml`](../topologies/two-racks-h100.yaml) | 2 racks × 2 DGX H100 nodes. 32 GPUs, four 8-GPU NVLink domains, two fault domains. |
| [`gb200-nvl72.yaml`](../topologies/gb200-nvl72.yaml) | One NVL72 rack: 18 trays × 4 GPUs. 72 GPUs in a single NVLink domain. |

## Things worth knowing

**Switching topologies prunes.** `gpu-sim` labels what it creates and deletes the
nodes and slices a previous topology left behind, so the cluster matches the file instead
of accumulating leftovers. Only objects carrying `app.kubernetes.io/managed-by=gpu-sim` are
ever deleted, so a real node in a mixed cluster is never at risk.

**The `Topology` object is not pruned.** Switching leaves the previous one behind, pointing
at labels no node now carries. Harmless, but a workload referencing it gets an unhelpful
"no resources" rather than a clear error. Remove it by hand if it gets in the way:

```bash
kubectl delete topologies.kai.scheduler <name>
```

**KAI's default queues have a GPU quota of zero.** Preemptible workloads (a Job, by
default) may exceed their quota and schedule anyway; non-preemptible ones (a Deployment)
are refused with an error about quota on a cluster where quota was never configured. Quota
is hierarchical, so the parent queue needs raising too. `hack/install-kai.sh` clears both.

**A `Job`'s pods do not stay Running.** KWOK drives `restartPolicy: Never` pods straight to
`Completed`, and a terminal pod releases its `ResourceClaim`. Anything that needs to hold
GPUs long enough to be counted should be a `Deployment`.
