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
	SchedulerDefault Scheduler = "default"
)

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
	Assertions []Assertion `json:"assertions"`
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
