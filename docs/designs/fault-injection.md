# Design: fault injection

_Phase 4. Status: implemented._

Hardware fails. A scheduler's behaviour when it does — how fast it notices, where it puts
the work instead, whether it still honours the constraints it was given — is the part
teams can least afford to discover in production, and the part they can least easily
test. Phase 4 makes it a scenario.

## What stage A established

**Kubernetes can already break a device.** `resourceslice.spec.devices[].taints` is beta and
enabled by default, with three effects: `NoExecute` evicts already-running pods that do not
tolerate the taint, `NoSchedule` blocks new ones, `None` is informational.

Verified on the cluster. Eight pods across four nodes; tainting one node's GPUs `NoExecute`:

```
before   node-1: 2   node-2: 2   node-3: 2   node-4: 2
after                node-2: 2   node-3: 3   node-4: 3      all eight still Running
```

Eviction and rescheduling, both done by Kubernetes. gpu-sim publishes the taint and measures
the result — the same shape as Phase 2, where the counters did the allocating.

**Killing a node is slow, and the cluster lies while it happens.** Deleting a node leaves its
pods reporting `Running` for about a minute before the pod garbage collector removes them:

```
t+5s    running=8    still on the deleted node=3
t+30s   running=8    still on the deleted node=3
t+60s   running=8    still on the deleted node=0
```

That delay is faithful — a real node takes ~40s for its heartbeat to lapse before anything
reacts — but it sets a trap, described in decision 3.

## Decision 1 — three faults, two mechanisms

**Decision: `degrade`, which taints devices, and `killNode`, which deletes one. NVLink
domain degradation and MIG partition failure are the same mechanism aimed at different
devices, and are not given separate names.**

The plan lists NVLink domain degradation, MIG partition failure and node loss as three
faults. Two of them are one mechanism: a device taint, applied to a different set of
devices. Giving them separate names in the schema would present as three concepts what is
really two, and every future device-level fault would need another name.

What differs between them is only **which devices break**, and the topology already has a
vocabulary for that — the same level names a workload uses to ask for placement:

```yaml
faults:
  - name: rack 2 loses its NVLink fabric
    at: 30s
    degrade:
      level: rack
      value: rack-2

  - name: one GPU's small partitions go unhealthy
    at: 30s
    degrade:
      devices: { profile: 1g.10gb }

  - name: a compute tray dies outright
    at: 45s
    killNode: gpu-node-1
```

`level`/`value` reads like the hardware and reuses `fault-domain`, `rack`, `nvlink-domain`
and `host` — words the scenario author already knows from `placement.required` and
`confinedTo`. The `devices` form matches on published attributes and is the escape hatch for
anything finer than a topology level: a MIG profile, a single UUID, one NUMA node.

**`devices` takes attribute matchers rather than the CEL a `deviceSelector` takes**, and the
reason is not the dependency it would add. A `deviceSelector`'s CEL is evaluated by the API
server; a fault's would have to be evaluated *here*, to know which devices to taint.
Reimplementing DRA's CEL semantics — attribute domain resolution, type coercion, the
handling of a missing attribute — would mean a fault could taint a different set of devices
than the identical expression selects, and the simulation would be wrong in a way nothing
would catch. Matching on attribute equality is less expressive and exactly as truthful, and
it reuses the vocabulary the `allocatedDevices` assertion already uses.

`effect` defaults to `NoExecute`, because a fault that only blocks new work is a maintenance
window rather than a failure, and the interesting question is what happens to work already
running. `NoSchedule` remains available for modelling a cordon.

**Faults are not repaired.** Every scenario reapplies its topology before it runs, which
republishes the slices without taints and recreates any deleted node, so a fault cannot leak
into the next scenario. Explicit repair (`repairAt`) is deliberately left out: for taints it
is trivial and for a killed node it is not, and no acceptance test needs it yet. Adding an
asymmetric feature ahead of demand is how schemas rot.

## Decision 2 — faults share the workload timeline

**Decision: `faults:` is a top-level list with `at:`, merged into the same ordered timeline
as `submitAt` and `retireAt`.**

A scenario already describes a sequence of events over time. A fault is another event. Two
independent timelines would raise the question of what happens when a fault and a submission
fall at the same offset, and the answer would have to be arbitrary; one timeline sorted by
offset, stable within an offset, answers it by declaration order — which the scenario author
controls and can read.

## Decision 3 — recovery is measured by identity, never by count

This is the decision the phase turns on.

The obvious assertion is "after the fault, are `replicas` pods running again?". **It is
wrong, and it fails open.** Stage A measured a node kill and saw `running=8` at every
sample, including thirty seconds when three of those eight were pods on a node that no
longer existed. A count-based check reports full health throughout, and a fault-injection
test that always passes is worse than no test.

So:

```
at the moment a fault fires, record every pod that is Running and affected by it
    -> the disrupted set, by pod UID

recovered when   |{pods Running now} \ disrupted|  >=  replicas
recovery time    = that moment, minus the fault's
```

Excluding the disrupted set by identity is what makes zombies harmless: a pod still
reporting `Running` on a deleted node *is* in the disrupted set, so it cannot be counted
towards recovery. Nothing else about the check has to know that zombies exist.

"Affected" is derived per fault: for `killNode`, the pods on that node; for `degrade`, the
pods holding a device the fault tainted. Both are readable from the snapshot the runner
takes when the fault fires.

**A fault that disrupts nothing fails the assertion.** If the disrupted set is empty there
was nothing to recover from, and reporting success would be the vacuous pass that
`confinedTo` already refuses on zero placed replicas. The error says so:

```
FAIL  the job comes back within two minutes
      the fault disrupted no replica, so there was nothing to recover from
```

This is the same discipline as every other assertion in the harness: the failure mode worth
guarding is the test that passes without testing.

## Decision 4 — two new assertions, and one reused

```yaml
assertions:
  - name: losing the rack takes out half the job
    workload: training
    disrupted: 16

  - name: and it comes back on the surviving rack
    workload: training
    rescheduledWithin: 120s

  - name: still inside one rack afterwards
    workload: training
    confinedTo: rack
```

`disrupted: <n>` states the blast radius. It is what a fault-domain test is actually about —
"a rack failure takes out sixteen GPUs, not eight" — and it is the assertion that catches a
fault which fired but hit nothing.

`rescheduledWithin: <duration>` is the recovery time, defined above.

`confinedTo` needs no change and is the third. Whether a scheduler still honours its
placement constraint *after* a failure is exactly the question a resilience test exists to
ask, and a scheduler that recovers by abandoning the constraint has not recovered — it has
quietly downgraded the job. Reusing the existing assertion means that question is already
expressible.

## Decision 5 — a node kill takes about a minute, and that is correct

A `killNode` scenario will not recover in ten seconds. The pod garbage collector needs
roughly a minute, which mirrors a real node's heartbeat lapsing before the node controller
acts. Speeding it up would make the simulation faster and less true, and the number a user
comes here for is precisely how long recovery takes.

`degrade` is the faster and more surgical fault — eviction is immediate — so a suite that
wants to exercise recovery logic quickly should prefer it, and reach for `killNode` when the
node's disappearance is itself the thing under test. The documentation says so rather than
leaving it to be discovered by an impatient timeout.

## Risks

**The snapshot is a point in time.** A pod that starts between the fault firing and the
snapshot being read is not in the disrupted set, and would count towards recovery
immediately. The window is milliseconds and the effect is to under-report recovery time
slightly; accepted rather than engineered around.

**`disrupted` counts are placement-dependent.** How many replicas a rack failure takes out
depends on where the scheduler put them. Scenarios asserting an exact number must control
placement — with `placement.required`, or a topology small enough to be deterministic, which
is the lesson Phase 2 learned the hard way.

## Stage C consequences

1. Parse `faults:`, validating that exactly one of `level`/`value`, `devices` or `killNode`
   is set.
2. Merge faults into the timeline; snapshot pods and their allocated devices when one fires.
3. Apply a taint by republishing the affected slices; delete the node for `killNode`.
4. Implement `disrupted` and `rescheduledWithin` against the snapshot.
5. Two scenarios: a device degradation with fast recovery, and a node kill measuring the
   real MTTR — each with the negative case that the fault hit something.
