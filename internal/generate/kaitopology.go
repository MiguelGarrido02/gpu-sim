package generate

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/MiguelGarrido02/gpu-sim/internal/topology"
)

// KAI's Topology CRD. Declared here rather than imported so that gpu-sim does not take a
// dependency on the whole KAI module to write one small object, and so that supporting a
// second scheduler later means adding a file, not untangling one.
const (
	kaiTopologyAPIVersion = "kai.scheduler/v1alpha1"
	kaiTopologyKind       = "Topology"
)

// KAITopology is the scheduler-side view of the same topology: an ordered list of node
// labels, broadest first, that the scheduler groups nodes by.
type KAITopology struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Spec       KAITopologySpec   `json:"spec"`
}

type KAITopologySpec struct {
	Levels []KAITopologyLevel `json:"levels"`
}

type KAITopologyLevel struct {
	NodeLabel string `json:"nodeLabel"`
	// Alias lets a workload say requiredTopologyLevel: rack instead of repeating the
	// raw label key.
	Alias string `json:"alias,omitempty"`
}

// KAITopologyFor builds the scheduler topology matching the labels Nodes() emits.
//
// Level order is coarsest to finest: fault domain, rack, NVLink domain, host.
//
// The NVLink level is included only when *every* node has an NVLink domain. KAI drops any
// node missing a label for any level of the topology, so a single non-NVLink node in the
// cluster would silently remove every node from the tree — the exact failure mode
// diagnosed in Phase 1 stage A, where one absent label made an idle cluster report itself
// out of capacity. When the level is dropped, rack- and host-level constraints still work;
// only the hardware-independent way of saying "same NVLink domain" is unavailable, which
// is preferable to a topology that silently matches nothing.
func KAITopologyFor(resolved *topology.Resolved) *KAITopology {
	levels := []KAITopologyLevel{
		{NodeLabel: LabelFaultDomain, Alias: "fault-domain"},
		{NodeLabel: LabelRack, Alias: "rack"},
	}

	if allNodesHaveNVLink(resolved) {
		levels = append(levels, KAITopologyLevel{NodeLabel: LabelNVLinkDomain, Alias: "nvlink-domain"})
	}

	// The host level is what makes "keep this job on one machine" expressible, and it is
	// the conventional leaf every scheduler expects to find.
	levels = append(levels, KAITopologyLevel{NodeLabel: LabelHostname, Alias: "host"})

	return &KAITopology{
		APIVersion: kaiTopologyAPIVersion,
		Kind:       kaiTopologyKind,
		Metadata:   metav1.ObjectMeta{Name: resolved.Name},
		Spec:       KAITopologySpec{Levels: levels},
	}
}

func allNodesHaveNVLink(resolved *topology.Resolved) bool {
	for _, node := range resolved.Nodes {
		if node.NVLinkDomain == "" {
			return false
		}
	}
	return len(resolved.Nodes) > 0
}
