# Design: the scenario harness

_Phase 3, stage B. Status: proposed._

A scenario is a file describing a cluster, some workloads, and what should happen. The
harness runs it and answers pass or fail. It is what turns gpu-sim from something poked at
with `kubectl` into a test framework that goes in CI.

## What stage A established

**Nothing existing does this.** [kube-scheduler-simulator](https://github.com/kubernetes-sigs/kube-scheduler-simulator)
(1,095 stars, actively maintained) is UI-driven, targets the default scheduler's plugins,
and knows nothing about GPUs, DRA or topology. It is a *debugger* for one scheduling
decision; this is a *test framework* for scheduling policies. The one project that did build
a scenario system, `sanposhiho/kube-scheduler-simulator-cli`, has three stars and has not
been touched since October 2024.

One idea is worth borrowing: that simulator annotates each pod with how every plugin scored
it. **When an assertion fails, the "why" is the whole value.** A generic error cost a full
day in Phase 1 stage A, and the report design below is shaped by that.

**The prototype already exists.** `hack/run-smoke.sh` is the harness written by hand. Its
assertion vocabulary is not something to invent — it is something to read off tests that
already work, and it comes to five kinds. Two implementation lessons come with it, and both
are promoted into the schema below rather than left as folklore:

- The cluster must be built from a known topology before measuring, or the run counts
  whatever the previous one left behind.
- **A negative assertion needs a settling period, not a timeout.** A positive assertion can
  stop the moment it is satisfied; a negative one's evidence is that nothing happened, so it
  must wait out the full budget.

## Decision 1 — neutral intent, with an escape hatch

**Decision: workloads declare intent in scheduler-neutral terms for the few things we have
actually needed and can translate. Anything else passes through a `raw` block. A scheduler
that cannot express a declared intent fails the scenario loudly rather than quietly running
something weaker.**

Today, "keep this job in one rack" is written like this:

```yaml
annotations:
  kai.scheduler/topology: "two-racks-h100"
  kai.scheduler/topology-required-placement: "rack"
  kai.scheduler/batch-min-member: "12"
labels:
  kai.scheduler/queue: default-queue
schedulerName: kai-scheduler
```

That is KAI's dialect. A scenario written in it can only ever test KAI — and the project's
stated goal is to be *"the reference the community reaches for when evaluating KAI, Kueue,
Grove or any other scheduler"*. **Comparison is the point, and you cannot compare
schedulers using one scheduler's vocabulary.**

The counter-argument is real and worth stating: abstractions derived from a single example
are usually wrong, and we have implemented exactly one scheduler. So the neutral layer stays
deliberately small — only intents we have needed in a working test, and only where a
translation exists for more than one target:

| Intent | KAI | Default scheduler |
|---|---|---|
| Device selection by attribute | CEL in the claim | CEL in the claim — **identical** |
| Gang (all-or-nothing) | `batch-min-member` annotation | **cannot express** |
| Required placement at a level | topology annotations | **cannot express** |
| Queue | `kai.scheduler/queue` label | not applicable |

Device selection needs no translation at all: it is core DRA, and Phase 1 stage D proved it
works unchanged under the stock scheduler.

**"Cannot express" is a feature, not a gap.** When a scenario asks for gang scheduling
against the default scheduler, the harness refuses with

```
scheduler "default" cannot express: gang scheduling
  gang requires all-or-nothing placement, which the default scheduler has no concept of
```

rather than silently scheduling the pods independently and reporting a pass. A team
evaluating schedulers wants exactly this answer, and producing it is a genuine feature of
the tool rather than an apology for a missing one.

## Decision 2 — the scenario format

```yaml
apiVersion: gpu-sim.io/v1alpha1
kind: Scenario
metadata:
  name: gang-stays-in-one-rack
  description: A 12-GPU job that must not be split across racks.

spec:
  cluster:
    topology: topologies/two-racks-h100.yaml
    scheduler: kai                    # kai | default

  workloads:
    - name: training
      replicas: 12
      gpus: 1                         # per replica
      gang: true
      placement:
        required: rack                # a level of the topology
      submitAt: 0s

  assertions:
    - name: the job is placed
      workload: training
      scheduled: all
      within: 60s

    - name: and it stays inside one rack
      workload: training
      confinedTo: rack
```

Notes on what is *not* in there. The plan's draft had `gpu_profile: "h100"` on each workload;
it is gone, because the profile is a property of a node, not of a job. A workload asks for a
GPU with certain properties and the cluster decides which one that is. Restating the profile
on the workload would let a scenario describe a job asking for hardware its cluster does not
have.

`submitAt` stays, and Phase 2 is why. Fragmentation depends entirely on arrival order: four
small partitions arriving one way leave a GPU usable, and arriving another way leave it
useless. Ordered submission is not decoration.

### Negative assertions

```yaml
    - name: nothing is placed at all
      workload: oversized
      scheduled: none
      settle: 45s
```

`within` and `settle` are deliberately different words. `within` succeeds as soon as the
condition holds; `settle` waits the whole period and then checks. Using `within` for a
negative assertion would pass instantly, before the scheduler had even tried.

### Device selection

```yaml
    - name: numa0-only
      replicas: 20
      gpus: 1
      deviceSelector: device.attributes['gpu.nvidia.com'].numaNode == 0
```

A raw CEL expression rather than a neutral abstraction, because it already is portable and
inventing a wrapper would only hide the expression a user must eventually write against real
hardware.

### Workload shape is inferred, not configured

A gang workload becomes a `Job`; a non-gang one becomes a `Deployment`. This is not a
preference: KWOK drives `restartPolicy: Never` pods straight to `Completed`, and a terminal
pod releases its `ResourceClaim`. A workload meant to *hold* GPUs — which every fragmentation
scenario needs — must therefore be a Deployment. Exposing this as a knob would ask users to
understand a KWOK implementation detail in order to write a correct test.

## Decision 3 — the assertion vocabulary

Five kinds, each one already earning its place in a passing test:

| Assertion | Means | Comes from |
|---|---|---|
| `scheduled: all \| none \| <n>` | how many replicas got a node | gang tests, both halves |
| `confinedTo: <level>` | every placed pod shares one value of that topology level | rack placement |
| `running: <n>` | exactly n replicas reached Running | device selector tests |
| `allocatedDevices: {attribute: value}` | every allocated device has this attribute | the NUMA test |
| `unschedulableReason: <substring>` | the scheduler's own explanation contains this | new |

The last is new, and is the lesson from Phase 1 stage A made executable. Asserting *why*
something was refused is stronger than asserting that it was: a job can stay pending for the
right reason or for an entirely unrelated one, and only the first is a passing test.

Two more arrive with later phases and the vocabulary is shaped to take them without
rework: `fragmentation` (Phase 2) and `rescheduledWithin` (Phase 4).

## Decision 4 — the report

Terminal output for a human, JSON for CI, from the same run.

On failure the report carries **the scheduler's own words**, not just a mismatch. For KAI
that is the PodGroup's scheduling conditions; for the default scheduler, the pod events:

```
FAIL  the job is placed
      expected 12 replicas scheduled, got 0

      scheduler said:
        node-group fd-1.rack-1 can allocate only 16 of 20 required pods
        node-group fd-2.rack-2 can allocate only 16 of 20 required pods

      placement: (none)
```

That block is the difference between a report that tells you a test failed and one that
tells you what to do about it.

## Decision 5 — one binary

**Decision: fold `topology-gen` into a single `gpu-sim` command.**

```
gpu-sim topology apply -f topologies/two-racks-h100.yaml
gpu-sim topology render -f ...
gpu-sim run scenarios/gang-in-one-rack.yaml
gpu-sim run scenarios/            # the whole suite
```

The plan names `gpu-sim` as the deliverable, and users should install one thing. Doing it
now, while there is one command to move, costs almost nothing; doing it in Phase 5 would
mean changing published documentation.

## Scope

The harness does **not** create the kind cluster — `hack/setup-cluster.sh` remains the
bootstrap, and installing Kubernetes is not a scheduling concern. `gpu-sim run` applies the
scenario's topology to a cluster that already exists, which also keeps a run fast enough to
sit in a CI loop.

Each scenario is hermetic: it applies its own topology and clears its namespace before
submitting anything, for the reason stage A recorded.

## Risks

**The neutral vocabulary is derived from one scheduler.** Mitigated by keeping it to four
intents, all of which have a working test behind them, and by the `raw` escape hatch. The
second scheduler implementation is what will prove or break it, and that is expected.

**`settle` makes negative assertions slow by construction.** A suite with several negatives
spends most of its time waiting. Acceptable for now; if it bites, negatives can share one
settling window rather than each taking their own.

## Stage C consequences

1. Parse and validate the scenario schema.
2. Translate workloads per target scheduler, refusing unsupported intents by name.
3. Submit on the `submitAt` timeline.
4. Evaluate the five assertions, with `within` and `settle` semantics kept distinct.
5. Collect the scheduler's explanation for anything unplaced.
6. Report to terminal and JSON.
7. Fold `topology-gen` into `gpu-sim`, and port `hack/run-smoke.sh` to scenarios so the
   existing coverage survives the move.
