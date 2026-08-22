# gpu-sim

**Test Kubernetes GPU scheduling without GPUs.**

`gpu-sim` simulates GPU clusters — NVLink/NVSwitch fabric topology, MIG partitioning,
Dynamic Resource Allocation (DRA) `ResourceSlice`s and multi-node ComputeDomains — so
that platform and MLOps teams can validate scheduling policies before they touch
hardware that costs €30,000–€300,000 per node.

> **Status: early development.** Topology modelling and the scenario harness work end to
> end; MIG partitioning and fault injection do not exist yet. See
> [`docs/PLAN.md`](docs/PLAN.md) for the roadmap and [`docs/AUDIT.md`](docs/AUDIT.md) for
> what the underlying stack does and does not provide.

## Running the simulated cluster

Requires Docker (or OrbStack/Colima), `kind`, `kubectl`, `helm`, `jq` and `yq`.

```bash
hack/setup-cluster.sh   # kind + KWOK + fake-gpu-operator + the topology below
hack/install-kai.sh     # KAI Scheduler
make scenarios          # run the scenario suite
```

The cluster is described by one file. This one is two racks of two DGX H100 nodes — 32
GPUs across four NVLink domains and two fault domains:

```yaml
apiVersion: gpu-sim.io/v1alpha1
kind: ClusterTopology
metadata:
  name: two-racks-h100
spec:
  nodePools:
    dgx-h100:
      profile: h100       # product, memory, PCIe and NUMA come from NVIDIA's own profile
      gpuCount: 8
      nvlink: full-mesh
  racks:
    - name: rack-1
      faultDomain: fd-1
      nodes:
        - { name: gpu-node-1, pool: dgx-h100 }
        - { name: gpu-node-2, pool: dgx-h100 }
    - name: rack-2
      faultDomain: fd-2
      nodes:
        - { name: gpu-node-3, pool: dgx-h100 }
        - { name: gpu-node-4, pool: dgx-h100 }
```

`gpu-sim` turns that into simulated nodes with topology labels, DRA `ResourceSlice`s
carrying per-GPU NVLink domain, PCIe root and NUMA node, and the scheduler's own topology
object — all from the one file, so they cannot describe different clusters.
[`docs/topologies.md`](docs/topologies.md) documents every field, what ends up published,
and how to write policies against it.

```bash
gpu-sim topology render -f topologies/gb200-nvl72.yaml   # preview, no cluster changes
gpu-sim topology apply  -f topologies/gb200-nvl72.yaml
```

Switching topologies removes the nodes and slices the previous one created, so the cluster
always matches the file rather than accumulating leftovers.

Tear it down with `make cluster-down`.

## What it is for

A 32-GPU training job that requires all its GPUs in one NVLink domain, submitted unchanged
against two topologies:

| Topology | NVLink domain | Result |
|---|---|---|
| `two-racks-h100` | 8 GPUs (per node) | refused — not one pod placed |
| `gb200-nvl72` | 72 GPUs (per rack) | placed across 8 trays, one domain |

Both are scenarios, so the comparison is one command:

```
$ gpu-sim run scenarios/nvlink-gang-dgx.yaml scenarios/nvlink-gang-nvl72.yaml

==> nvlink-gang-dgx
    cluster two-racks-h100 · 4 nodes · 32 GPUs · scheduler kai
  PASS the job is refused outright
  PASS because a DGX NVLink domain holds only 8 GPUs
       the scheduler said: node-group fd-1.rack-1.gpu-node-1 can allocate only 8 of 32 required pods

==> nvlink-gang-nvl72
    cluster gb200-nvl72 · 18 nodes · 72 GPUs · scheduler kai
  PASS every replica is placed
  PASS and the whole job stays inside one NVLink domain
       all 32 placed replicas are in nvlink-domain "nvl72-1"
```

Same workload, same scheduler, different hardware — answered on a laptop, without owning
either machine. On real hardware the difference between those two rows is roughly €2M of
rack.

## Scenarios

A scenario declares a cluster, some workloads and what should happen. Workloads state
intent — a gang, a required topology level, a device selector — rather than one scheduler's
annotations, so the same file can be aimed at more than one scheduler, and a scheduler that
cannot express an intent fails the scenario by name instead of quietly running something
weaker.

```yaml
workloads:
  - name: training
    replicas: 12
    gpus: 1
    gang: true
    placement: { required: rack }

assertions:
  - name: every replica is placed
    workload: training
    scheduled: all
    within: 90s
  - name: and the job stays inside one rack
    workload: training
    confinedTo: rack
```

```bash
gpu-sim run scenarios/                     # the suite; non-zero exit if anything failed
gpu-sim run scenarios/ --json results.json # plus machine-readable output for CI
```

[`docs/scenarios.md`](docs/scenarios.md) covers every field, the assertion vocabulary, and
why `within` and `settle` are different words.

## Why

The Kubernetes GPU stack changed shape in 2026: DRA reached GA in v1.34/v1.35, NVIDIA
donated its DRA driver to the CNCF, and the modern stack (DRA + KAI Scheduler + Grove)
replaced the legacy device-plugin model that had been treating GPUs as opaque integers
(`nvidia.com/gpu: 1`) since 2017.

The new stack understands NVLink topology, MIG profiles, fault domains and multi-node
ComputeDomains. Exercising any of it currently requires the hardware. That leaves teams
unable to:

- validate scheduling policies before rolling them into production,
- compare gang-scheduling strategies without paying for the cluster,
- test failure scenarios (an NVLink domain going down, a GPU node degrading) without
  breaking a real one,
- run CI on anything that touches GPU scheduling — today those are blind deploys.

## What it adds over what already exists

`gpu-sim` does not reimplement the ecosystem. It builds on
[KWOK](https://github.com/kubernetes-sigs/kwok) for node simulation and
[fake-gpu-operator](https://github.com/run-ai/fake-gpu-operator) for GPU profiles, the
DRA plugin and NUMA topology, and contributes the layers nobody covers yet:

| Capability | KWOK | fake-gpu-operator | gpu-sim |
|---|---|---|---|
| Simulated nodes at scale | ✅ | ✅ | ✅ inherited |
| DRA `ResourceSlice`s with GPU profiles | ❌ | ✅ | ✅ inherited |
| Per-node NUMA topology (NRT) | ❌ | ✅ | ✅ inherited |
| Simulated ComputeDomains | ❌ | ✅ partial | ✅ inherited + extended |
| Inter-GPU NVLink/NVSwitch fabric topology | ❌ | ❌ | 🎯 **new** |
| MIG fragmentation with mixed profiles | ❌ | ❌ | 🎯 **new** |
| Declarative scenario test harness | ❌ | ❌ | 🎯 **new** |
| Scheduled fault injection | ❌ | ❌ | 🎯 **new** |
| Fragmentation / utilization metrics | ❌ | ❌ | 🎯 **new** |
| Topology & capacity recommender | ❌ | ❌ | 🔮 v2 |

## License

Apache License 2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
