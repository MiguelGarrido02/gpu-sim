// Package scenario defines the declarative format for a scheduling test: a cluster, some
// workloads, and what should happen to them.
package scenario

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	APIVersion = "gpu-sim.io/v1alpha1"
	Kind       = "Scenario"
)

// Scheduler names a scheduler the harness knows how to target.
type Scheduler string

const (
	SchedulerKAI     Scheduler = "kai"
	SchedulerVolcano Scheduler = "volcano"
	SchedulerDefault Scheduler = "default"
)

// Schedulers lists every target, for error messages.
func Schedulers() []string {
	return []string{string(SchedulerKAI), string(SchedulerVolcano), string(SchedulerDefault)}
}

type Scenario struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

type Metadata struct {
	Name string `json:"name"`
	// Description is printed in reports. A scenario whose name says what it does and
	// whose description says why is far more useful to whoever reads a failure.
	Description string `json:"description,omitempty"`
}

type Spec struct {
	Cluster    Cluster     `json:"cluster"`
	Workloads  []Workload  `json:"workloads"`
	Faults     []Fault     `json:"faults,omitempty"`
	Assertions []Assertion `json:"assertions"`
}

// Fault breaks something at a point on the scenario's timeline.
//
// Faults share the timeline with workload submission and retirement rather than having one
// of their own: a fault is another event, and two timelines would leave the ordering of a
// fault and a submission at the same offset undefined.
type Fault struct {
	// Name is printed in reports and should say what broke in the real world.
	Name string `json:"name"`

	At metav1.Duration `json:"at"`

	// Degrade taints devices so the scheduler treats them as unusable. Exactly one of
	// Degrade and KillNode is set.
	Degrade *Degrade `json:"degrade,omitempty"`

	// KillNode deletes a simulated node outright.
	//
	// Slower and blunter than Degrade: Kubernetes takes about a minute to garbage-collect
	// the orphaned pods, mirroring a real node's heartbeat lapsing before anything reacts.
	// Reach for it when the node's disappearance is itself under test; prefer Degrade when
	// the recovery logic is.
	KillNode string `json:"killNode,omitempty"`
}

// Degrade names the devices that break. Exactly one of (Level and Value) or Devices is set.
type Degrade struct {
	// Level and Value name a topology level and one of its values — "rack", "rack-2" —
	// using the same vocabulary a workload uses to ask for placement.
	Level string `json:"level,omitempty"`
	Value string `json:"value,omitempty"`

	// Devices matches published device attributes, e.g. {profile: 1g.10gb}. The escape
	// hatch for anything finer than a topology level.
	//
	// Attribute equality rather than the CEL a deviceSelector takes: a selector's CEL is
	// evaluated by the API server, but a fault's would have to be evaluated here, and
	// reimplementing DRA's CEL semantics would let a fault taint a different set of devices
	// than the identical expression selects.
	Devices map[string]string `json:"devices,omitempty"`

	// Effect defaults to NoExecute. A fault that only blocks new work is a maintenance
	// window rather than a failure, and what happens to running work is the question.
	Effect string `json:"effect,omitempty"`
}

// Taint effects, as the DRA API spells them.
const (
	EffectNoExecute  = "NoExecute"
	EffectNoSchedule = "NoSchedule"
)

// TaintEffect returns the effect to apply, defaulted.
func (d Degrade) TaintEffect() string {
	if d.Effect == "" {
		return EffectNoExecute
	}
	return d.Effect
}

type Cluster struct {
	// Topology is a path to a ClusterTopology document, relative to the scenario file.
	// Each scenario applies its own, so a run never measures whatever the previous one
	// happened to leave behind.
	Topology string `json:"topology"`

	Scheduler Scheduler `json:"scheduler"`
}

// Workload describes what to submit, in terms of intent rather than of any particular
// scheduler's annotations. Translation happens in package workload, which refuses by name
// when the target scheduler cannot express an intent.
type Workload struct {
	Name string `json:"name"`

	Replicas int `json:"replicas"`

	// GPUs requested per replica.
	GPUs int `json:"gpus"`

	// Gang makes the workload all-or-nothing: either every replica is placed or none is.
	Gang bool `json:"gang,omitempty"`

	Placement *Placement `json:"placement,omitempty"`

	// MIGProfile asks for a MIG partition of that profile rather than a whole GPU, e.g.
	// "1g.10gb". Partitions are selected through their own device class, so a workload
	// asking for a GPU never receives a partition of one.
	MIGProfile string `json:"migProfile,omitempty"`

	// DeviceSelector is a CEL expression filtering which GPUs qualify, e.g.
	//   device.attributes['gpu.nvidia.com'].numaNode == 0
	//
	// Deliberately raw rather than wrapped in a neutral abstraction: CEL device selection
	// is core DRA and already portable across schedulers, so a wrapper would only hide
	// the expression a user has to write against real hardware anyway.
	DeviceSelector string `json:"deviceSelector,omitempty"`

	// SubmitAt delays submission relative to the start of the run.
	SubmitAt metav1.Duration `json:"submitAt,omitempty"`

	// RetireAt deletes the workload at that offset, releasing whatever it held.
	//
	// Retirement is what produces fragmentation. The upstream DRA allocator packs, filling
	// each GPU from the lowest free placement upward, so a run that only ever submits
	// leaves GPUs either full or untouched. Holes appear when partitions are released out
	// of the order they were taken — which is the state a real cluster is in most of the
	// time, and the one worth measuring.
	RetireAt metav1.Duration `json:"retireAt,omitempty"`
}

type Placement struct {
	// Required names a level of the cluster topology — "rack", "nvlink-domain", "host" —
	// that every replica must share.
	Required string `json:"required,omitempty"`
}

// Assertion is one check. Exactly one condition field must be set; Validate enforces it,
// because an assertion with two conditions is ambiguous and one with none silently passes.
type Assertion struct {
	// Name is what a report prints. Phrase it as the thing that should be true.
	Name string `json:"name"`

	// Workload names the workload under test.
	Workload string `json:"workload"`

	// Scheduled counts replicas that were given a node: "all", "none", or a number.
	Scheduled string `json:"scheduled,omitempty"`

	// ConfinedTo requires every placed replica to share one value of a topology level.
	ConfinedTo string `json:"confinedTo,omitempty"`

	// Running requires exactly this many replicas to reach the Running phase. Distinct
	// from Scheduled: a replica can be placed and still not be running.
	Running *int `json:"running,omitempty"`

	// AllocatedDevices requires every GPU allocated to the workload to carry these
	// attribute values. Attribute names are as published, e.g. "numaNode".
	AllocatedDevices map[string]string `json:"allocatedDevices,omitempty"`

	// Disrupted requires a fault to have taken out exactly this many of the workload's
	// running replicas. It states the blast radius, which is what a fault-domain test is
	// actually about, and it catches a fault that fired but hit nothing.
	Disrupted *int `json:"disrupted,omitempty"`

	// RescheduledWithin requires the workload to be back to full strength within this
	// long of the fault, counting only replicas the fault did not disrupt.
	//
	// Counted by pod identity rather than by number: a pod on a deleted node keeps
	// reporting Running for about a minute, so a count-based check reports full health
	// throughout and the assertion would pass without testing anything.
	RescheduledWithin metav1.Duration `json:"rescheduledWithin,omitempty"`

	// Fragmentation asserts on MIG capacity that is free but unreachable. Unlike the
	// others it is a property of the cluster rather than of one workload, so it needs no
	// workload name.
	Fragmentation *FragmentationAssertion `json:"fragmentation,omitempty"`

	// UnschedulableReason requires the scheduler's own explanation to contain this text.
	// Asserting *why* something was refused is stronger than asserting that it was: a
	// workload can stay pending for the intended reason or for an unrelated one, and only
	// the first is a passing test.
	UnschedulableReason string `json:"unschedulableReason,omitempty"`

	// Within polls until the condition holds, failing if it never does. For conditions
	// that should become true.
	Within metav1.Duration `json:"within,omitempty"`

	// Settle waits the full period and then checks once. For conditions that should stay
	// false — the evidence is that nothing happened, so it has to be given time to
	// happen. Using Within for those would pass instantly, before the scheduler had tried.
	Settle metav1.Duration `json:"settle,omitempty"`
}

// FragmentationAssertion bounds the partitions lost to fragmentation across the cluster.
//
// Bounded rather than exact because the figure depends on which placements the scheduler
// chose, and pinning an exact number would make the test assert the allocator's current
// packing strategy rather than the property under test.
type FragmentationAssertion struct {
	AtLeast *int `json:"atLeast,omitempty"`
	AtMost  *int `json:"atMost,omitempty"`
}
