// Package topology defines gpu-sim's cluster topology schema and expands it into the
// per-node, per-GPU facts the generators publish.
package topology

// APIVersion and Kind identify a topology document. They are checked on load so that a
// file aimed at some other tool fails with a clear message rather than parsing into an
// empty topology.
const (
	APIVersion = "gpu-sim.io/v1alpha1"
	Kind       = "ClusterTopology"
)

// ClusterTopology is the declarative description of a simulated GPU cluster: the single
// source of truth from which node labels, ResourceSlices and the scheduler's own topology
// configuration are all generated.
type ClusterTopology struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
}

type Metadata struct {
	Name string `json:"name"`
}

type Spec struct {
	// NodePools describe machine types, keyed by pool name.
	NodePools map[string]NodePool `json:"nodePools"`

	// Racks describe the physical layout. Rack is the coarsest level modelled; a
	// multi-rack NVLink domain is not a thing real hardware does.
	Racks []Rack `json:"racks"`
}

// NodePool is a machine type: what GPUs a node has and how they are wired to each other.
//
// Deliberately thin. Product name, memory, architecture, per-GPU PCIe bus IDs, PCIe root
// complexes and NUMA node assignments all come from the named GPU profile, which
// fake-gpu-operator syncs from NVIDIA's own test infrastructure. Restating any of that
// here would mean inventing hardware details that already exist in a more authoritative
// form, and inviting them to drift.
type NodePool struct {
	// Profile names a GPU profile ConfigMap published by fake-gpu-operator: one of
	// a100, h100, h200, b200, gb200, gb300, l40s, t4.
	Profile string `json:"profile"`

	// GPUCount is how many GPUs each node in the pool has.
	GPUCount int `json:"gpuCount"`

	// NVLink is how the node's own GPUs are connected to each other.
	NVLink NVLinkTopology `json:"nvlink"`
}

// NVLinkTopology describes intra-node GPU interconnect.
type NVLinkTopology string

const (
	// NVLinkFullMesh means every GPU on the node reaches every other over NVLink, which
	// is what an NVSwitch backplane provides on DGX/HGX-class hardware.
	NVLinkFullMesh NVLinkTopology = "full-mesh"

	// NVLinkNone means the GPUs talk over PCIe only, as on inference nodes built from
	// discrete cards.
	NVLinkNone NVLinkTopology = "none"
)

// Rack is a group of nodes sharing a physical location, and usually a failure mode.
type Rack struct {
	Name string `json:"name"`

	// FaultDomain is the blast radius the rack belongs to. Several racks may share one
	// when they sit behind the same power or cooling infrastructure.
	FaultDomain string `json:"faultDomain"`

	// NVLinkDomain, when set, means NVLink spans the whole rack rather than stopping at
	// the node boundary — the GB200 NVL72 arrangement, where 72 GPUs across many nodes
	// form one domain. Leave it empty for DGX-class hardware, where each node is its own
	// domain.
	//
	// This one field is what distinguishes the two hardware generations, and it is
	// exactly the distinction a scheduling policy has to reason about.
	NVLinkDomain string `json:"nvlinkDomain,omitempty"`

	Nodes []Node `json:"nodes"`
}

// Node is one simulated machine.
type Node struct {
	Name string `json:"name"`

	// Pool references a key of Spec.NodePools.
	Pool string `json:"pool"`
}
