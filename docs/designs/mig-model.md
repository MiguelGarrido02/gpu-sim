# Design: MIG partitioning and fragmentation

_Phase 2, stage B. Status: proposed._

MIG (Multi-Instance GPU) splits one physical GPU into up to seven isolated logical GPUs of
fixed sizes. Fragmentation is what happens when memory is free but no *aligned, contiguous*
region of the requested size remains — free capacity that cannot be reached.

Phase 2 models both, and reports the fragmentation, which is a number nobody can measure
today without owning the hardware.

## What stage A established

**Kubernetes already allocates partitioned devices.** `DRAPartitionableDevices` is beta and
on by default in v1.34+. A `ResourceSlice` declares `sharedCounters`, each device declares
what it `consumesCounters`, and the scheduler guarantees the sum never exceeds the set.
Verified on the cluster with a hand-written four-slice GPU:

```
large partition allocated (consumes all four)  ->  four small partitions: Pending
large partition released                       ->  four small partitions: Running
```

That reframes the phase. `PLAN.md` called for "a fragmentation engine that, given a
request, computes whether it fits and where". The scheduler does that. **gpu-sim's job is
to publish the partitions correctly and to measure what emerges.** Building our own
allocator would mean testing our arithmetic instead of Kubernetes' behaviour.

Three constraints came out of the same experiment, none of them guessable from the docs:

1. Counter names must be **RFC 1123 labels** — `memorySlice0` is rejected,
   `memory-slice-0` is accepted.
2. **A `ResourceSlice` may contain `sharedCounters` or `devices`, never both.** The counter
   sets must live in a separate slice from the devices consuming them.
3. **A slice holds at most 64 devices when any of them consumes counters** (128 otherwise).

Unlike Phase 1, the GPU profiles offer nothing to build on here: they carry
`max_gpu_instances: 7` and `mode_current: disabled`, and no geometry at all. Phase 1 was
plumbing NVIDIA's data through. This is modelling.

## The geometry

Both A100 and H100 present **8 memory slices and 7 compute (SM) slices**. From NVIDIA's
[supported MIG profiles](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/supported-mig-profiles.html),
for H100 80GB:

| Profile | Memory slices | SM slices | Max instances |
|---|---|---|---|
| `1g.10gb` | 1 | 1 | 7 |
| `1g.20gb` | 2 | 1 | 4 |
| `2g.20gb` | 2 | 2 | 3 |
| `3g.40gb` | 4 | 3 | 2 |
| `4g.40gb` | 4 | 4 | 1 |
| `7g.80gb` | 8 | 7 | 1 |

The asymmetry in `3g.40gb` — four memory slices but three SMs — is not a typo, and it is
the reason fragmentation exists at all. It also means **two counter dimensions are
required**: memory slices, which are positional, and SM slices, which are a plain count.

That both dimensions are needed is visible in the max-instances column, and this gives a
free correctness check. Model memory slices as eight positional counters and SM slices as a
pool of seven, and every documented maximum falls out:

| Profile | Memory-bound | SM-bound | Actual max | Binding constraint |
|---|---|---|---|---|
| `1g.10gb` | 8 | 7 | **7** | SMs |
| `1g.20gb` | 4 | 7 | **4** | memory |
| `2g.20gb` | 4 | 3 | **3** | SMs |
| `3g.40gb` | 2 | 2 | **2** | either |
| `4g.40gb` | 2 | 1 | **1** | SMs |
| `7g.80gb` | 1 | 1 | **1** | either |

Every row matches NVIDIA's table. **That becomes a unit test**: if the model ever produces
a different maximum for any profile, the geometry is wrong and the build fails.

### Placements

A partition occupies a contiguous, aligned run of memory slices. gpu-sim derives the valid
placement starts as multiples of the profile's memory size:

```
1g.10gb (size 1) -> starts 0,1,2,3,4,5,6,7   8 placements
1g.20gb (size 2) -> starts 0,2,4,6           4
2g.20gb (size 2) -> starts 0,2,4,6           4
3g.40gb (size 4) -> starts 0,4               2
4g.40gb (size 4) -> starts 0,4               2
7g.80gb (size 8) -> start  0                 1
                                            ---
                                             21 placements per GPU
```

**A stated assumption:** NVIDIA does not document placement indices, and `nvidia-smi mig
-lgip` reports seven placements for the smallest profile rather than eight. gpu-sim offers
eight and lets the SM counter cap concurrent instances at seven. The observable behaviour —
how many instances fit, and which combinations conflict — is identical either way; only the
identity of a specific placement index differs. Allocation behaviour is the higher-fidelity
concern and it is reproduced exactly; placement indices are the lower-fidelity one and this
is where the model is approximate. Said plainly here rather than discovered later.

## Decision 1 — publish every valid placement

**Decision: publish all 21 (profile, placement) combinations per GPU as devices, with
counters making the conflicting ones mutually exclusive. MIG-enabled GPUs are not published
as whole devices.**

The alternative is publishing one fixed partition layout per GPU — *static MIG*, which is
how many shops actually run in production, and which yields far fewer devices.

Publishing every placement wins because **fragmentation has to emerge, not be declared**.
If gpu-sim published a fixed layout, the answer to "does a 3g.40gb still fit?" would be
whatever we wrote in the file. Publishing the full set makes the scheduler choose, and the
fragmentation that results is a property of the workload sequence rather than of our
configuration. That is the entire subject of the phase.

Static MIG remains expressible as a special case by restricting the offered profiles;
dynamic is strictly more general.

A MIG-enabled GPU is not published as a whole device, because on real hardware it is not
directly allocatable — the whole GPU is the `7g.80gb` partition, which *is* published.

**The cost is object size.** 21 devices per GPU × 8 GPUs = **168 devices per node**, against
a hard limit of 64 per slice. A node therefore needs one counter slice plus **three device
slices**, with `resourceSliceCount: 4`.

Measured in stage C rather than assumed: the largest published slice is **37 KB**, and the
counter slice 2 KB. Against etcd's ~1.5 MB object limit that is comfortable, and the risk
this section was written to flag is closed.

## Decision 2 — what "fragmentation" means

A metric that cannot be checked by hand is not worth reporting, and the phase's acceptance
test is precisely "the reported fragmentation matches a manual calculation". So the
definition has to be arithmetic, not a vibe.

**Fragmentation is always relative to a profile.** There is no single number for "how
fragmented is this GPU" that means anything on its own — a GPU can be perfectly usable for
small partitions and useless for large ones at the same instant.

For a GPU in some state, and a profile `P`:

```
idealFit(P)     = floor(freeMemorySlices / memorySlices(P))   capped by free SM slices
actualFit(P)    = number of valid placements of P still allocatable
lostToFragmentation(P) = idealFit(P) - actualFit(P)
```

`idealFit` is what the free capacity *would* hold if it could be rearranged; `actualFit` is
what it holds where it actually sits. The gap is fragmentation, in units of whole
partitions that a perfect defragmentation would recover.

### Worked example

An H100 running four `1g.10gb` partitions at placements 0, 2, 4 and 6 — a plausible outcome
of four small inference jobs arriving one at a time. Four memory slices free (1, 3, 5, 7),
three SM slices free. **Half the GPU's memory is unused.**

| Profile | idealFit | actualFit | lost | why |
|---|---|---|---|---|
| `1g.10gb` | 3 | 3 | 0 | SM-bound; the free slices are individually usable |
| `1g.20gb` | 2 | 0 | **2** | every 2-slice start (0, 2, 4, 6) is occupied |
| `2g.20gb` | 1 | 0 | **1** | same, and only 3 SMs remain |
| `3g.40gb` | 1 | 0 | **1** | both 4-slice starts (0, 4) are blocked |
| `4g.40gb` | 0 | 0 | 0 | not enough free SMs regardless |
| `7g.80gb` | 0 | 0 | 0 | not enough free memory regardless |

40 GB of an 80 GB GPU is free and **nothing larger than a 1g.10gb can be placed on it**.
The free memory is not in short supply; it is in the wrong places. `largestAllocatableProfile`
is `1g.10gb`.

**That is the number this project exists to surface**, and every row above is checkable
with a pencil — which is the point of defining it arithmetically. The first draft of this
table had the `2g.20gb` row wrong, and recomputing it is what caught the error.

Alongside it, one intuitive per-GPU headline: **`largestAllocatableProfile`** — the biggest
partition the GPU can still hand out. It compresses the table into the thing an operator
actually asks.

## Decision 3 — how a topology declares MIG

```yaml
nodePools:
  dgx-h100-mig:
    profile: h100
    gpuCount: 8
    nvlink: full-mesh
    mig:
      enabled: true
      # Optional. Omitted means every profile the GPU supports. Restricting the list is
      # how a static or semi-static MIG policy is expressed.
      profiles: [1g.10gb, 2g.20gb, 3g.40gb]
```

Absent `mig`, nothing changes: GPUs are published whole, exactly as today. That keeps every
Phase 1 topology and test valid, and makes MIG opt-in per pool.

Per-pool rather than per-GPU. Real clusters do mix MIG-enabled and whole GPUs within a node,
but a pool already expresses "machines of this type", and a topology can declare two pools.
Per-GPU control can come later if a scenario needs it; adding it now would complicate the
schema ahead of any demand.

## Risks

**Object size is the real one.** 168 devices per node across four slices, each device
carrying attributes and counter consumption. Stage C measures the serialised size on a
four-node cluster before this design is accepted as final. If it is uncomfortable, the
mitigation is narrowing the default profile list rather than abandoning dynamic placement.

**Placement indices are approximate**, as stated above. Allocation behaviour is exact.

**Counters are beta.** `DRAPartitionableDevices` is on by default in v1.34+ but a cluster
can disable it, in which case every partition would appear independently allocatable and
the simulation would silently overcommit each GPU. `gpu-sim` must detect this and refuse
rather than publish a cluster that lies.

**KAI Scheduler v0.17.0 cannot allocate partitionable devices at all**, discovered in stage
C. On one cluster at one moment, an identical claim for a `7g.80gb` partition reached
Running under the stock kube-scheduler and stayed Pending under KAI, which reported
`cannot allocate all DRA claims on node ...`. It reproduces with a single pod and a single
partition, so it is not a scale or counter-exhaustion effect; KAI's own code has no
reference to counters, and its GPU accounting counts every partition as a whole GPU (336
where the cluster has 16).

The consequence is that MIG scenarios target the default scheduler for now, and the
project's primary scheduler under test cannot exercise MIG. This is worth reporting
upstream, and gpu-sim is the reproducer — which is the tool doing exactly what it exists
for: finding a real scheduler gap on a laptop, with no hardware.

## What stage C found: fragmentation needs churn

The upstream DRA allocator **packs**. Given a homogeneous sequence of partition requests it
fills each GPU from the lowest free placement upward, so a run of small partitions produces
tightly packed GPUs and zero fragmentation. Measured on the simulated cluster: 24 × 1g.10gb
followed by 12 × 2g.20gb across sixteen GPUs left every GPU either full or untouched, and
nothing lost.

Fragmentation therefore does not arise from arrival order alone, as the design assumed. It
arises from **churn** — partitions being released out of the order they were taken, leaving
holes a later request cannot use. The design's worked example (four small partitions at
offsets 0, 2, 4 and 6) is a state reachable by releasing, not by allocating.

That is a real correction to this document's premise, and it reshapes stage D. The
acceptance test cannot be "submit twenty workloads and watch fragmentation grow"; it has to
submit *and retire* them. The scenario harness has no way to retire a workload mid-run
today, so stage D needs that first — which is also what Phase 4's fault injection will need,
so the two share the mechanism.

## Stage C consequences

1. A profile geometry table, with the max-instances invariant as a unit test.
2. Placement enumeration, and the mapping from placement to consumed counters.
3. Publishing: one counter slice plus N device slices per node, honouring the 64-device
   limit, with `resourceSliceCount` set correctly.
4. Fail loudly when `DRAPartitionableDevices` is unavailable.
5. A fragmentation report reading live `ResourceClaim` allocations, computing the table
   above per GPU.
6. Measure the resulting object sizes and record them here.
