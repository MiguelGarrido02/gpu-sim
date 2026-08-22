# Writing a scenario

A scenario is a file describing a cluster, some workloads, and what should happen to them.
`gpu-sim` runs it and answers pass or fail.

```bash
gpu-sim run scenarios/rack-local-training.yaml   # one
gpu-sim run scenarios/                            # the whole suite
gpu-sim run scenarios/ --json results.json        # plus machine-readable output for CI
```

The exit status is non-zero if anything failed, so a scenario suite drops into a CI job
without further plumbing.

## A complete example

```yaml
apiVersion: gpu-sim.io/v1alpha1
kind: Scenario
metadata:
  name: rack-local-training
  description: A 12-GPU gang must land entirely inside one NVLink rack.

spec:
  cluster:
    topology: ../topologies/two-racks-h100.yaml
    scheduler: kai

  workloads:
    - name: training
      replicas: 12
      gpus: 1
      gang: true
      placement:
        required: rack

  assertions:
    - name: every replica is placed
      workload: training
      scheduled: all
      within: 90s

    - name: and the job stays inside one rack
      workload: training
      confinedTo: rack
```

Each scenario applies its own topology and clears its namespace before submitting anything,
so a run never measures what the previous one left behind. The topology path is resolved
relative to the scenario file.

## Workloads

Workloads declare *intent*, not a particular scheduler's annotations, so one scenario can be
aimed at more than one scheduler.

| Field | Meaning |
|---|---|
| `replicas` | how many pods |
| `gpus` | GPUs per replica; `0` for a CPU-only workload |
| `gang` | all-or-nothing: either every replica is placed or none is |
| `placement.required` | a topology level every replica must share — `fault-domain`, `rack`, `nvlink-domain`, `host` |
| `deviceSelector` | a CEL expression filtering which GPUs qualify |
| `submitAt` | delay before submitting, relative to the start of the run |

`deviceSelector` is deliberately raw CEL rather than a neutral wrapper. Device selection is
core DRA and already portable across schedulers, so wrapping it would only hide the
expression you will eventually write against real hardware:

```yaml
deviceSelector: device.attributes['gpu.nvidia.com'].numaNode == 0
deviceSelector: device.attributes['gpu-sim.io'].nvlinkPeerCount >= 7
```

`submitAt` is not decoration. Arrival order decides fragmentation: the same partitions
arriving in a different order leave a GPU usable or useless.

### What a workload becomes is not configurable

A gang becomes a `Job`; anything else becomes a `Deployment`. That is not a style
preference. KWOK drives `restartPolicy: Never` pods straight to `Completed`, and a terminal
pod releases its `ResourceClaim` — so a workload that must *hold* GPUs long enough to be
counted has to be a Deployment. Exposing this as a knob would require understanding a KWOK
implementation detail in order to write a correct test.

## Schedulers, and what they cannot do

```yaml
cluster:
  scheduler: kai      # or: default
```

| Intent | `kai` | `default` |
|---|---|---|
| `deviceSelector` | ✅ | ✅ — identical, it is core DRA |
| `gang` | ✅ | ❌ |
| `placement.required` | ✅ | ❌ |

A scheduler that cannot express a declared intent **fails the scenario by name**, rather
than quietly running something weaker:

```
==> gang-on-default-scheduler
  ERROR scheduler "default" cannot express gang scheduling (workload "g"):
        the default scheduler places pods one at a time and has no
        all-or-nothing concept
```

This is a feature. Scheduling the replicas independently and reporting a pass would claim a
guarantee the scheduler never made, and a team comparing schedulers wants exactly the
opposite answer.

The refusal happens before anything is created, so a scenario that asks for the impossible
leaves nothing behind.

## Assertions

Exactly one condition per assertion. Two are ambiguous about which one failed; none passes
silently, which is the worst thing a test framework can do.

| Condition | Checks |
|---|---|
| `scheduled: all \| none \| <n>` | how many replicas were given a node |
| `running: <n>` | how many reached the Running phase |
| `confinedTo: <level>` | every placed replica shares one value of that topology level |
| `allocatedDevices: {attr: value}` | every GPU allocated to the workload has these attributes |
| `unschedulableReason: <substring>` | the scheduler's own explanation contains this text |

`scheduled` and `running` differ: a replica can be placed on a node and not yet running, and
a device-selector test cares about the second.

`unschedulableReason` is the strongest assertion available for a negative case. Asserting
*why* something was refused beats asserting *that* it was — a workload can stay pending for
the intended reason or for a completely unrelated one, and only the first is a passing test:

```yaml
- name: because a DGX NVLink domain holds only 8 GPUs
  workload: single-domain-training
  unschedulableReason: "can allocate only 8 of 32"
```

### `within` and `settle`

Two different words, on purpose.

```yaml
within: 90s     # poll until the condition holds; fail if it never does
settle: 45s     # wait the whole period, then check once
```

Use `within` for something that should *become* true. Use `settle` for something that should
*stay* false — its evidence is that nothing happened, so the scheduler has to be given a
fair chance to act first. Polling a negative assertion would pass instantly, before the
scheduler had even tried, and `gpu-sim` rejects that combination rather than letting it
produce a green tick:

```
assertion "no replica is placed" asserts nothing is scheduled but uses within,
want settle: within would pass instantly, before the scheduler had tried
```

An assertion with neither waits up to 60 seconds, polling.

## Reading a failure

A failing assertion prints the scheduler's own words, which is usually the only part worth
reading:

```
==> deliberately-failing
  FAIL all 40 replicas are placed
       expected 40 of 40 replicas scheduled, got 0

       the scheduler said:
         Resources were found for 32 pods while 40 are required for gang
         scheduling. Additional pods cannot be scheduled due to: no nodes with
         enough resources were found.

       placement: (unscheduled)=40
```

For KAI that text comes from the PodGroup's scheduling conditions, broken down per topology
domain; for the default scheduler, from the pod's scheduling condition. Both are collected
automatically.

## The bundled suite

| Scenario | What it establishes |
|---|---|
| `rack-local-training` | a 12-GPU gang lands entirely in one rack |
| `rack-local-impossible` | a 20-GPU rack-local gang is refused although 32 GPUs are free |
| `gang-oversized` | a gang larger than the cluster places *no* replica |
| `numa-selector` | a CEL selector on `numaNode` limits a workload to that socket's GPUs |
| `nvlink-domain-selector` | the same for `gpu-sim.io/nvlinkDomain` |
| `nvlink-gang-dgx` | a 32-GPU single-domain job does not fit DGX H100 |
| `nvlink-gang-nvl72` | the identical job fits GB200 NVL72 |
| `default-scheduler` | the simulated GPUs work with no KAI at all |

Every positive case is paired with a negative one. A suite that only asserts success passes
just as happily against a scheduler that ignores the constraint entirely — which is exactly
how the first version of the gang test proved nothing.

## Things worth knowing

**Scenarios are not parallel.** Each one owns the cluster while it runs, because it applies
its own topology. Running a suite is therefore sequential, and a suite with several `settle`
assertions spends most of its wall time deliberately waiting.

**The namespace is `gpu-sim-scenarios`** and is cleared before each scenario. Do not put
anything there you want to keep.

**KAI's default queues carry a GPU quota of zero**, which silently refuses non-gang
workloads. `hack/install-kai.sh` clears it; see [`topologies.md`](topologies.md) for the
details.
