# gpu-sim

**Test Kubernetes GPU scheduling without GPUs.**

`gpu-sim` simulates GPU clusters — NVLink/NVSwitch fabric topology, MIG partitioning,
Dynamic Resource Allocation (DRA) `ResourceSlice`s and multi-node ComputeDomains — so
that platform and MLOps teams can validate scheduling policies before they touch
hardware that costs €30,000–€300,000 per node.

> **Status: early development.** Phase 0 is complete: a gang-scheduled GPU workload runs
> on a fully simulated cluster on a laptop, with no NVIDIA hardware anywhere. The
> value-add layers are not built yet. See [`docs/PLAN.md`](docs/PLAN.md) for the roadmap
> and [`docs/AUDIT.md`](docs/AUDIT.md) for what the underlying stack does and does not
> provide today.

## Running the simulated cluster

Requires Docker (or OrbStack/Colima), `kind`, `kubectl`, `helm`, `jq` and `yq`.

```bash
hack/setup-cluster.sh 4   # kind + KWOK + fake-gpu-operator + 4 simulated 8-GPU H100 nodes
hack/install-kai.sh       # KAI Scheduler
make smoke                # gang scheduling, positive and negative cases
```

Tear it down with `make cluster-down`.

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
