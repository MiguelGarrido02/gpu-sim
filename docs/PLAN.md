# gpu-sim — Project Plan

_Last updated: August 2026._

## 1. Problem statement

GPU orchestration on Kubernetes changed shape in 2026. Dynamic Resource Allocation
(DRA) reached GA in Kubernetes v1.34/v1.35, NVIDIA donated its DRA driver to the CNCF
at KubeCon Europe 2026, and the modern stack (DRA + KAI Scheduler + Grove) replaced the
legacy device-plugin model that had been treating GPUs as opaque integers
(`nvidia.com/gpu: 1`) since 2017.

The new stack understands NVLink topology, MIG profiles, fault domains and multi-node
ComputeDomains. Testing it requires hardware costing €30,000–€300,000 per node
(H100/H200/B200/GB200). That leaves a large gap:

- MLOps and platform teams cannot validate scheduling policies before deploying to
  production.
- They cannot compare gang-scheduling strategies without paying for the hardware.
- They cannot test failure scenarios (an NVLink domain going down, a GPU node
  degrading) without breaking a real cluster.
- CI/CD pipelines that touch GPU scheduling are blind deploys: tested in production, or
  not tested at all.

### Why now

The ecosystem is at an inflection point. DRA has just gone GA, KAI Scheduler was
open-sourced under Apache 2.0, and ComputeDomains (multi-node NVLink for GB200/GB300)
are the next frontier. Teams adopting this stack need a way to test it without
hardware. No complete, packaged, vendor-neutral tool covers the whole spectrum today —
from individual GPU profiles up to multi-node fabric topology with fault injection.

## 2. State of the art

### 2.1 KWOK (Kubernetes WithOut Kubelet)

- Repository: `github.com/kubernetes-sigs/kwok` — Apache 2.0
- **Does:** simulates thousands of Kubernetes nodes and pods without a kubelet. Holds
  1,000 nodes and 100,000 pods comfortably; creates ~20 nodes/second.
- **Does not:** know anything about GPUs, DRA, topology or AI-specific scheduling. It is
  the base node-simulation layer.
- **Relevance:** the foundation. We use it, we do not reimplement it.

### 2.2 fake-gpu-operator

- Repository: `github.com/run-ai/fake-gpu-operator` — Apache 2.0 (cloning, modifying and
  renaming are all permitted)
- **Does:**
  - uses KWOK to create simulated GPU nodes;
  - ships predefined profiles: A100, H100, B200, GB200, L40S, T4, GB300 (synced from
    NVIDIA/k8s-test-infra);
  - provides a DRA plugin (`kwok-dra-plugin`) that publishes `ResourceSlice`s with
    configured GPUs;
  - publishes per-node NUMA topology (`NodeResourceTopology`/NRT) for topology-aware
    schedulers such as KAI;
  - injects a simulated `nvidia-smi` into GPU pods;
  - supports simulated ComputeDomains (flagged as new in the README);
  - offers a "mock" backend with `nvml-mock` for applications that call NVML directly;
  - allows real and simulated GPU nodes to coexist in the same cluster;
  - recent releases add a `pcie_topology` block to the profiles, with PCI root complexes
    and a `numa_node` per device.
- **Does not (the gaps):**
  - **No inter-GPU NVLink/NVSwitch fabric topology.** It knows a node has 8 GPUs and a
    NUMA layout, but not which GPUs are NVLink-connected versus PCIe-attached, nor
    rack-level NVSwitch domains.
  - **No realistic MIG fragmentation.** You can declare N GPUs of profile X, but there is
    no fragmentation engine modelling what happens when mixed profiles coexist
    (1g.10gb + 3g.40gb + 7g.80gb) and how availability degrades.
  - **No reproducible scenario test harness.** It gives you the fake backend, not a
    declarative "define scenario → run policy → compare results" framework.
  - **No scheduled fault injection.** You cannot say "at t=30s, kill the NVLink domain of
    rack 2 and measure how the scheduler reacts".
  - **No recommendation / capacity-planning layer.**

### 2.3 NVIDIA/k8s-test-infra — ComputeDomain simulation (issue #304)

- Repository: `github.com/NVIDIA/k8s-test-infra` — issue closed, PRs #337 and #342 merged.
- **What they did:** extended `nvml-mock` to test ComputeDomains without GB200/GB300
  hardware. Mocked the NVML fabric APIs (`GetGpuFabricInfo`), added fake IMEX binaries
  (`nvidia-imex`, `nvidia-imex-ctl`) coordinating through marker files on a shared
  volume, and a topology ConfigMap declaring domains and cliques.
- **Scope:** internal test infrastructure for validating NVIDIA's own ComputeDomain
  controller. It is not a packaged tool for end users, and it targets KIND rather than
  being a product.
- **Relevance:** proof that the gap exists (NVIDIA needed it themselves), and Apache 2.0
  code we can study, reference or integrate.

### 2.4 dra-example-driver

- Repository: `github.com/kubernetes-sigs/dra-example-driver` — Apache 2.0
- **Does:** example DRA driver for developers to fork. Publishes `ResourceSlice`s with
  generic GPUs (`LATEST-GPU-MODEL`).
- **Does not:** topology, real profiles, anything NVLink- or MIG-related.
- **Relevance:** a reference for understanding the DRA API, not a base to build on.

### 2.5 KAI Scheduler

- Repository: `github.com/kai-scheduler/KAI-Scheduler` — Apache 2.0
- **Does:** Kubernetes-native scheduler for AI: fractional GPU sharing, gang scheduling,
  topology-aware scheduling (TAS), hierarchical PodGroups, DRA support for
  ComputeResources (GB200/GB300), hierarchical queues with fair-share.
- **Relevance:** the primary scheduler we want to test. We consume it, we do not
  reimplement it.

### 2.6 Kueue and Grove

- **Kueue** (`kubernetes-sigs/kueue`): queue management for batch workloads — fair-share,
  priority, quotas.
- **Grove** (NVIDIA): operator for PodCliqueSets with startup ordering, scaling policies
  and gang-scheduling constraints.
- **Relevance:** further schedulers/operators we want to test. Consume, do not
  reimplement.

### 2.7 Capacity-planning tooling

Sizing calculators exist for LLM inference (VRAM estimation, per-token throughput,
cost). No topology-aware recommender exists for training/gang scheduling at scale that
analyses a simulated cluster and reports "you need X nodes with Y topology".

## 3. Gap analysis

| Capability | KWOK | fake-gpu-operator | k8s-test-infra #304 | **gpu-sim** |
|---|---|---|---|---|
| Simulated nodes at scale | ✅ | ✅ (via KWOK) | N/A (KIND) | ✅ inherited |
| DRA `ResourceSlice`s with GPU profiles | ❌ | ✅ | ✅ (GB200 only) | ✅ inherited |
| Per-node NUMA topology | ❌ | ✅ (NRT) | ❌ | ✅ inherited |
| Simulated ComputeDomains | ❌ | ✅ partial | ✅ complete but internal | ✅ inherited + extended |
| Inter-GPU NVLink/NVSwitch fabric topology | ❌ | ❌ | partial (fabric mock) | 🎯 **new** |
| MIG fragmentation with mixed profiles | ❌ | ❌ | ❌ | 🎯 **new** |
| Declarative scenario test harness | ❌ | ❌ | ❌ | 🎯 **new** |
| Scheduled fault injection | ❌ | ❌ | ❌ | 🎯 **new** |
| Fragmentation / utilization metrics | ❌ | ❌ | ❌ | 🎯 **new** |
| Topology & capacity recommender | ❌ | ❌ | ❌ | 🔮 v2 |

**Conclusion:** do not reinvent the wheel. Build the four or five missing pieces on top
of what already exists.

## 4. Legal and code strategy

Apache 2.0 explicitly permits cloning, forking, modifying and redistributing under a
different name, for commercial and non-commercial use alike. The obligations are:
retain the original copyright and license notices, state the changes made, and include a
copy of the license.

Three options were considered:

- **A — Fork on GitHub.** Fork `run-ai/fake-gpu-operator`, rename, keep Run:ai copyright
  headers in untouched files, add ours to new files. Standard practice, legally cleanest.
- **B — New project consuming it as a dependency.** New repository, using
  fake-gpu-operator as a Go dependency or Helm subchart. Conceptually cleaner, more
  integration work.
- **C — Independent from scratch.** Not recommended: months of reimplementation with no
  added value.

**Decision: start with option B.** A project with its own identity that uses
fake-gpu-operator as a base (Helm chart dependency or equivalent) and adds the missing
layers on top. If development shows we need to modify the fake-gpu-operator core, we
fork that specific component (option A). This gives us a clear identity, correct
upstream credit through an explicit dependency rather than unattributed copy-paste, and
freedom to evolve without dragging the whole fake-gpu-operator codebase along.

## 5. Feature list

### Inherited (already exists, not reimplemented)

1. GPU node simulation with KWOK.
2. DRA `ResourceSlice` publication with GPU profiles (A100, H100, B200, GB200, L40S, T4,
   GB300).
3. Per-node NUMA topology (`NodeResourceTopology`).
4. Simulated `nvidia-smi` inside pods.
5. Mock backend with `nvml-mock` for NVML applications.
6. DRA plugin (`kwok-dra-plugin`) creating per-node `ResourceSlice`s.
7. Basic ComputeDomain support.

### Built here (the added value)

8. **🎯 Cluster-level NVLink/NVSwitch topology model.** Declarative YAML definition of how
   GPUs connect inside a node (intra-node NVLink) and between nodes (NVSwitch/NVLink
   domain), translated into DRA attributes and labels that topology-aware schedulers can
   consume.
9. **🎯 MIG fragmentation engine.** Simulation of MIG partitioning with mixed profiles.
   Given a node with N GPUs and a MIG policy, compute which slices are available, which
   are fragmented (free VRAM with no contiguous profile able to use it), and publish
   updated `ResourceSlice`s.
10. **🎯 Scenario test harness.** Declarative YAML framework defining cluster topology,
    workloads to submit (jobs, gang-scheduling groups, priorities), the scheduling policy
    under test (KAI, Kueue, default) and expected assertions (pod placement, wait times,
    resulting fragmentation). Output: a results report with metrics.
11. **🎯 Fault injection.** Given a running scenario: kill a simulated GPU node at t=X,
    degrade an NVLink domain (mark its GPUs unavailable), simulate an NVSwitch failure
    (disconnect nodes from a domain), and measure how the scheduler reacts
    (re-scheduling, recovery times).
12. **🎯 Fragmentation and utilization metrics.** Report/dashboard showing the share of
    GPUs idle vs allocated vs fragmented, the distribution of MIG profiles and their
    utilization, and simulated cost given a price/hour per GPU type.
13. **🔮 Topology recommender (v2).** Given a set of workloads (training/inference
    profiles), recommend the minimum cluster topology that satisfies them.

## 6. Development environment

Hardware: MacBook Air M5, 20 GB RAM, macOS, ARM64 (Apple Silicon).

| Component | Tool | Rationale |
|---|---|---|
| Local Kubernetes | **kind** | Runs on ARM64/macOS, lightweight, multi-node, YAML-configured. 20 GB RAM comfortably runs a control plane plus 3–4 workers. |
| Container runtime | **OrbStack** (or Docker Desktop / Colima) | Required by kind. OrbStack is the lightest option on Apple Silicon. |
| Node simulation | **KWOK** (inside kind) | Deployed as a pod in the kind cluster; KWOK nodes consume no real resources. |
| Simulated GPUs | **fake-gpu-operator** (Helm chart) | Installed on top of kind + KWOK. |
| Scheduler under test | **KAI Scheduler** / **Kueue** | Installed as additional Helm charts. |
| Main language | **Go** | The ecosystem (KWOK, fake-gpu-operator, KAI) is Go; new components touching the Kubernetes API must be Go for native integration. |
| CLI tooling | kubectl, helm, jq, yq | Standard Kubernetes tooling. |

### Constraints of the development machine

- No real NVIDIA GPU: CUDA cannot run — which is exactly the point of the project.
- 20 GB RAM: enough for kind (control plane ~500 MB) plus KWOK (simulated nodes cost no
  extra RAM) plus 2–3 operator pods. Simulating 512 GPUs means 512 KWOK nodes, roughly
  200 MB in total.
- ARM64: kind, KWOK and fake-gpu-operator publish multi-arch images. The arm64 manifest
  of fake-gpu-operator must be verified; build locally if it is missing.

## 7. Execution plan

### Phase 0 — Setup and audit (weeks 1–2)

**Goal:** working environment, and an understanding of the existing code.

1. **Install local tooling.** OrbStack (or Colima) for macOS ARM64;
   `brew install kind kubectl helm jq yq go`. Verify with
   `kind create cluster --name test && kubectl get nodes && kind delete cluster --name test`.
2. **Bring up a kind cluster with KWOK + fake-gpu-operator.** One control plane; install
   KWOK; install fake-gpu-operator with the H100 profile and 8 GPUs per node; create 2–4
   KWOK nodes with a GPU label. Verify that `kubectl get resourceslices` shows simulated
   GPUs and that
   `kubectl get nodes -o custom-columns="NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu"`
   reports GPUs.
3. **Audit the fake-gpu-operator code.** Clone the repository and read its structure:
   Helm chart, status-updater, kwok-dra-plugin, topology-server. Identify where
   `ResourceSlice`s are generated, how profiles are read, which DRA attributes are
   published. Document which topology attributes are missing relative to what KAI
   Scheduler expects to read.
4. **Run KAI Scheduler on the simulated cluster.** Install the Helm chart, create a
   simple PodGroup with 2 pods requesting 1 GPU each, verify placement and gang
   scheduling. If it fails, document why (missing DRA attributes? labels? NRT?).

**Acceptance test:** can a gang-scheduled GPU job be submitted on a fully simulated
cluster on the laptop and reach `Running`? If yes, the base works.

**Artifact:** [`docs/AUDIT.md`](AUDIT.md) — code structure, what works, which attributes
are missing, identified extension points.

### Phase 1 — NVLink topology model (weeks 3–5)

**Goal:** `ResourceSlice`s reflecting realistic NVLink topology that a topology-aware
scheduler can consume.

**Technical context:** a DGX H100 node has 8 GPUs connected by NVLink through NVSwitch. A
GB200 NVL72 rack has 72 GPUs in a multi-node NVLink domain. The scheduler needs to know
which GPUs can talk fast (NVLink) versus slow (PCIe/network). DRA exposes this through
`ResourceSlice` attributes and CEL selectors in `DeviceClass`.

1. **Define the cluster topology YAML schema:**

   ```yaml
   cluster:
     name: "test-cluster"
     racks:
       - name: "rack-1"
         nvlink_domain: "domain-1"
         nodes:
           - name: "node-1"
             gpu_profile: "h100"
             gpu_count: 8
             nvlink_topology: "full_mesh"  # all 8 GPUs NVLink-connected via NVSwitch
           - name: "node-2"
             gpu_profile: "h100"
             gpu_count: 8
             nvlink_topology: "full_mesh"
       - name: "rack-2"
         nvlink_domain: "domain-2"
         nodes:
           - name: "node-3"
             gpu_profile: "h200"
             gpu_count: 8
             nvlink_topology: "full_mesh"
     fault_domains:
       - name: "fd-1"
         racks: ["rack-1"]
       - name: "fd-2"
         racks: ["rack-2"]
   ```

2. **Implement the topology → Kubernetes object translator.** Read the topology YAML; for
   each node create a KWOK node with the appropriate labels
   (`topology.kubernetes.io/rack`, `nvidia.com/nvlink-domain`, …); for each GPU generate
   DRA attributes in the `ResourceSlice` (`nvlink_connected_to`, `nvswitch_domain`,
   `fault_domain`). Output: `ResourceSlice`s whose attributes a `DeviceClass` CEL selector
   can filter on.
3. **Validate against KAI Scheduler TAS.** Create a PodGroup requiring 4 GPUs in the same
   NVLink domain and verify KAI places all 4 pods in the same rack/domain. Create a
   16-GPU PodGroup that must cross domains and verify the behaviour.

**Acceptance test:** given a cluster with 2 racks in distinct NVLink domains, a 4-GPU job
lands entirely within one rack; a 16-GPU job is either rejected or placed cross-domain
according to the configured policy.

**Artifact:** a Go module/CLI (`topology-gen`) that reads a topology YAML and
creates/updates KWOK nodes plus `ResourceSlice`s, and a Helm wrapper chart or setup script
orchestrating fake-gpu-operator + topology-gen.

### Phase 2 — MIG fragmentation engine (weeks 6–8)

> **Reordered after Phase 1.** Phase 3 is being built first. Phase 2's own acceptance test —
> "submit 20 workloads with mixed MIG profiles and verify fragmentation grows" — is itself a
> scenario, and cannot be run properly without the harness Phase 3 provides; the steps below
> already concede this by calling for integration with it. Phase 3 also multiplies the value
> of what Phase 1 already built, without depending on anything new. The design work for
> Phase 2 is complete and waiting in [`designs/mig-model.md`](designs/mig-model.md).

**Goal:** simulate MIG partitioning with mixed profiles and report fragmentation.

**Technical context:** MIG (Multi-Instance GPU) partitions a GPU (A100, H100, H200) into
up to 7 isolated instances. Profiles have fixed sizes (1g.10gb, 2g.20gb, 3g.40gb,
4g.40gb, 7g.80gb for H100). Fragmentation happens when memory is free but no contiguous
slot of the requested size exists — an H100 with 3×1g.10gb allocated has 50 GB free but
cannot create a 4g.40gb profile.

1. **Define the MIG data model.** A GPU as an array of slices (compute instances +
   memory); profiles as templates consuming N contiguous slices; per-slice state:
   allocated, free, fragmented (free but unusable for the requested profile).
2. **Implement the fragmentation engine.** Input: current MIG state per GPU plus a new
   profile request. Output: whether the request fits and where it lands, or why it does
   not (insufficient total memory vs fragmentation). Publish the updated state as DRA
   `ResourceSlice`s, each MIG slice a device with profile attributes.
3. **Integrate with the scenario harness (prelude to Phase 3).** A script creating N
   workloads requesting varied MIG profiles in sequence, measuring resulting
   fragmentation after each, and emitting a CSV/JSON report with per-node fragmentation
   metrics.

**Acceptance test:** a cluster with 4 H100 nodes; submit 20 workloads with mixed MIG
profiles (70 % 1g.10gb, 20 % 3g.40gb, 10 % 7g.80gb). The engine reports increasing
fragmentation, the `ResourceSlice`s reflect the updated state, and new pods request MIG
slices correctly.

**Artifact:** Go module `mig-engine` modelling MIG fragmentation, integrated as a sidecar
or controller in the simulated cluster.

### Phase 3 — Scenario test harness (weeks 9–12)

**Goal:** a declarative framework to define, run and evaluate GPU scheduling scenarios.

1. **Define the scenario YAML format:**

   ```yaml
   scenario:
     name: "gang-scheduling-cross-rack"
     description: "Verifies that an 8-GPU job lands in a single rack"

     cluster:
       topology_file: "topologies/2-racks-h100.yaml"  # from Phase 1
       scheduler: "kai"  # kai | kueue | default

     workloads:
       - name: "training-job-1"
         type: gang
         replicas: 8
         gpu_per_replica: 1
         gpu_profile: "h100"
         constraints:
           same_nvlink_domain: true
         submit_at: "0s"

       - name: "inference-batch"
         type: batch
         replicas: 20
         gpu_per_replica: 1
         gpu_profile: "h100-mig-1g.10gb"
         submit_at: "10s"

     assertions:
       - workload: "training-job-1"
         condition: "all_pods_same_rack"
         timeout: "30s"
       - workload: "inference-batch"
         condition: "all_pods_scheduled"
         timeout: "60s"
       - metric: "cluster_gpu_utilization"
         operator: ">="
         value: 0.75

     faults:  # prelude to Phase 4
       - type: "kill_node"
         target: "node-3"
         at: "45s"
       - assert_after_fault:
           workload: "training-job-1"
           condition: "rescheduled_within"
           value: "120s"
   ```

2. **Implement the scenario runner.** `gpu-sim run scenario.yaml`: build the cluster
   (kind + KWOK + topology-gen + fake-gpu-operator + scheduler), submit workloads in
   order respecting `submit_at`, evaluate assertions by polling, and generate a report
   with results (pass/fail, metrics, timeline).
3. **Implement the assertion library.** `all_pods_same_rack` (placement via labels),
   `all_pods_scheduled` (all pods reach Running), `no_fragmentation_above(X%)` (queries
   the MIG engine), `rescheduled_within(Xs)` (post-failure recovery time),
   `cluster_gpu_utilization` (share of allocated GPUs).
4. **Ship a predefined scenario suite:** `basic-gang.yaml`,
   `topology-aware-placement.yaml`, `mig-fragmentation.yaml`, `fault-recovery.yaml`,
   `preemption-priority.yaml`.

**Acceptance test:** `gpu-sim run scenarios/` runs the whole suite; all basic scenarios
pass with KAI Scheduler; the report shows clear metrics.

**Artifact:** the `gpu-sim` CLI (Go) with `run`, `list-scenarios` and `report`
subcommands, the predefined YAML scenario suite, and documentation on writing custom
scenarios.

### Phase 4 — Fault injection (weeks 13–15)

**Goal:** simulate hardware failures and measure scheduler resilience.

1. **Implement the fault controller.** Read the `faults:` section of the scenario YAML and
   execute the action at t=X: `kill_node` (`kubectl delete node <name>` — harmless under
   KWOK), `degrade_nvlink_domain` (update `ResourceSlice`s to mark the domain's GPUs
   unavailable), `mig_slice_failure` (mark a specific MIG slice unhealthy),
   `network_partition` (apply a NetworkPolicy or taint isolating a group of nodes).
2. **Implement recovery detection.** Monitor affected pods, measure time to
   re-scheduling, and check whether the scheduler still honours the constraints after the
   failure.
3. **Integrate with the report.** Add a resilience section: simulated MTTR, affected pods,
   scheduler behaviour.

**Acceptance test:** a scenario with 32 GPUs across 4 nodes and an 8-GPU gang job; a node
is killed at t=30s. The report shows how long KAI takes to re-place the job and whether
it respects the same-NVLink-domain constraint.

**Artifact:** `fault-injector` module integrated into the `gpu-sim` CLI, plus predefined
resilience scenarios.

### Phase 5 — Packaging and publication (weeks 16–18)

**Goal:** a ready-to-use, documented, published tool.

1. **Packaging.** A Helm chart installing the whole stack (KWOK + fake-gpu-operator +
   topology-gen + mig-engine + gpu-sim); a distributable CLI binary (goreleaser,
   multi-arch: linux/amd64, linux/arm64, darwin/arm64); GitHub Actions CI for build, test
   and release.
2. **Documentation.** README with a 5-minute quickstart; a guide to the predefined
   topologies (DGX H100, DGX H200, GB200 NVL72); a guide to writing custom scenarios; a
   contribution guide.
3. **Publication.** Public GitHub repository under Apache 2.0; a technical blog post;
   submission to the CNCF Landscape (Testing/Simulation section); sharing on
   r/kubernetes, CNCF Slack and the KAI Scheduler community.

**Acceptance test:** somebody else can run `brew install gpu-sim && gpu-sim quickstart`
and have a simulated cluster running a scenario in under 5 minutes — verified by asking
someone to actually try it.

### Phase 6 (future) — Topology recommender

**Goal:** given a set of workloads, recommend the minimum viable cluster topology.

Kept as a separate phase because it needs data from Phases 1–4 to be useful, and because
it requires modelling performance rather than just fit (bandwidth, all-reduce latency,
per-workload roofline). It is the long-term differentiator that nothing in open source
currently offers.

- **Input:** workload profile (model size, batch size, parallelism strategy, latency SLA).
- **Engine:** simulate several topologies, run scheduling scenarios, compare metrics.
- **Output:** "for these workloads you need 4× H100 8-GPU nodes with intra-node NVLink, or
  2× GB200 NVL72. Estimated cost: €X/month. Expected utilization: Y%."

## 8. Testing summary

| Phase | Main test | How to run | Success criterion |
|---|---|---|---|
| 0 | Simulated cluster works | `kubectl get resourceslices` | Simulated GPUs visible; a GPU pod reaches Running |
| 1 | NVLink topology respected | 4-GPU gang job, 2 racks | All the job's GPUs in the same rack |
| 2 | MIG fragmentation detected | 20 mixed MIG workloads | Report shows fragmentation > 0, matching a manual calculation |
| 3 | Scenario suite passes | `gpu-sim run scenarios/` | All predefined scenarios pass |
| 4 | Post-failure recovery measurable | Scenario with `kill_node` | MTTR below the configured timeout, job correctly re-scheduled |
| 5 | Clean-room install works | Someone else runs the quickstart | Works without help |

## 9. Goal

**Build the Kubernetes ecosystem's standard tool for testing GPU scheduling without
hardware.** Concretely:

- A CLI (`gpu-sim`) and a Helm chart any MLOps/platform team can install in 5 minutes.
- A translation of real hardware topologies (DGX H100, DGX H200, GB200 NVL72) into
  simulated Kubernetes objects, faithful enough to test scheduling, gang scheduling, MIG
  and failures.
- A reproducible scenario framework usable as CI for GPU scheduling policies.
- The reference the community reaches for when evaluating KAI Scheduler, Kueue, Grove or
  any other scheduler against realistic GPU topologies.

It is the missing middle step between "we read the DRA docs" and "we deploy to production
on €200,000 hardware".

## Appendix A — Key references

| Resource | URL |
|---|---|
| KWOK | https://github.com/kubernetes-sigs/kwok |
| fake-gpu-operator | https://github.com/run-ai/fake-gpu-operator |
| KAI Scheduler | https://github.com/kai-scheduler/KAI-Scheduler |
| dra-example-driver | https://github.com/kubernetes-sigs/dra-example-driver |
| k8s-test-infra ComputeDomain issue | https://github.com/NVIDIA/k8s-test-infra/issues/304 |
| Kubernetes DRA docs | https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation |
| Kueue | https://github.com/kubernetes-sigs/kueue |
| Apache License 2.0 | https://www.apache.org/licenses/LICENSE-2.0 |
