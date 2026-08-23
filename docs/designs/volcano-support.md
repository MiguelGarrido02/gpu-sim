# Volcano support, and what it revealed

_Status: implemented, with two limitations recorded below._

Volcano was added before packaging for one reason: **the scheduler-neutral workload
vocabulary was derived from a single scheduler.** An abstraction with one implementation is
usually wrong, and the Phase 3 design said so explicitly. A second scheduler is what proves
or breaks it.

## The verdict on the abstraction: it holds

A scenario aimed at Volcano differs from the same scenario aimed at KAI by one line.
Everything else — replicas, GPUs, `gang`, `placement.required`, `deviceSelector` — is
unchanged, and gpu-sim writes each scheduler's own dialect underneath.

The two dialects are genuinely different, which is the useful part:

| Intent | KAI | Volcano |
|---|---|---|
| topology description | one `Topology` object listing node-label levels | a tree of `HyperNode` objects, one per domain, at numbered tiers |
| gang | `kai.scheduler/batch-min-member` annotation on the Job | `PodGroup` with `minMember`, pods annotated `scheduling.k8s.io/group-name` |
| placement | two annotations on the Job | `networkTopology` on the PodGroup |
| queue | a pod label | a `PodGroup` field |

KAI's topology is a *schema* — "these labels are levels". Volcano's is *data* — "here is the
tree". Nothing about the neutral layer needed changing to serve both, which is the strongest
evidence available that it describes intent rather than one scheduler's shape.

One detail made the translation cleaner than expected: Volcano's `HyperNode.spec.tierName`
and `PodGroup.spec.networkTopology.highestTierName` mean gpu-sim's level names survive
intact. A scenario says `rack` and Volcano is told `rack`. The scheduler's own logs confirm
it reads the tree gpu-sim publishes:

```
Finish adjusting jobs' network topology spec according to hyperNodeTierNameMap
    map[fault-domain:4 host:1 nvlink-domain:2 rack:3]
HyperNodesReadyToSchedule: true
```

## Limitation 1 — Volcano's topology constraint does not apply to DRA workloads

A 12-replica gang with `placement.required: rack`, on a cluster of two racks of 16 GPUs:

| GPUs requested through | Result |
|---|---|
| DRA `ResourceClaim` | spread across both racks, three runs out of three |
| plain CPU requests | all twelve in one rack |

Same PodGroup shape, same `networkTopology: {mode: hard, highestTierName: rack}`, same
HyperNode tree, same scheduler configuration with `network-topology-aware` enabled. The only
difference is how the pods ask for their GPUs.

`hard` mode is documented to leave a job pending rather than spread it, so this is not a
soft-preference falling back. The constraint appears simply not to be consulted on the path
that allocates DRA claims.

Not root-caused further, and reported here rather than worked around. gpu-sim's job is to
surface what a scheduler does, and the finding — that **Volcano's network topology awareness
and DRA do not compose today** — is worth more to a team choosing a scheduler than a
scenario quietly rewritten until it passed.

## Limitation 2 — an oversized gang is not observably refused

Under KAI, a 40-replica gang on a 32-GPU cluster places nothing: the all-or-nothing
guarantee is visible. Under Volcano the same workload ends with all 40 replicas having been
placed.

This one is likely an artefact of the simulation rather than of Volcano. KWOK drives a Job's
pods to `Completed` the moment they are bound, so a Job churns: a pod finishes, releases its
claim, and the next is admitted. Nothing is ever concurrently over capacity, so a gang
guarantee expressed as "do not exceed capacity at any instant" is never violated. KAI refuses
up front and so is unaffected; Volcano appears to admit incrementally.

Distinguishing "Volcano's gang guarantee is weaker" from "the simulation cannot express this
question to Volcano" needs a workload whose pods hold their GPUs *and* form a gang — which
means neither a Job (its pods complete) nor a Deployment (not a gang). That is a gap in
gpu-sim, not in Volcano, and it is the honest reason `scenarios/volcano-gang.yaml` asserts
only the positive half.

## Configuration Volcano needs

Neither of these is on by default, and both are silent when missing — the first schedules
DRA workloads as if the claims did not exist, the second ignores every topology constraint:

```yaml
- name: predicates
  arguments:
    predicate.DynamicResourceAllocationEnable: true
- name: network-topology-aware
  arguments:
    weight: 10
```

`hack/install-volcano.sh` sets both.

## What this cost, and what it bought

Two intents translated, one topology generator, one PodGroup type. The abstraction survived
unchanged, and two upstream behaviours were discovered that nobody could have found without
a cluster — on a laptop, with no GPUs. That is the tool doing what it exists for, twice, on
a scheduler it had never targeted before.
