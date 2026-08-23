package workload

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/MiguelGarrido02/gpu-sim/internal/scenario"
)

// Volcano's contract.
const (
	volcanoSchedulerName = "volcano"
	volcanoDefaultQueue  = "default"

	// volcanoGroupAnnotation is what associates a plain pod with a PodGroup. Volcano's
	// own Job type carries gang settings inline, but gpu-sim submits ordinary Deployments
	// and Jobs so that a scenario reads the same whichever scheduler it targets.
	volcanoGroupAnnotation = "scheduling.k8s.io/group-name"

	volcanoPodGroupAPIVersion = "scheduling.volcano.sh/v1beta1"
	volcanoPodGroupKind       = "PodGroup"

	// volcanoTopologyHard refuses to place a workload that will not fit inside one domain,
	// which is what `placement.required` means. The soft mode is best-effort and would
	// turn a required constraint into a preference.
	volcanoTopologyHard = "hard"
)

// VolcanoPodGroup carries both of Volcano's group-level settings: how many members must be
// placed together, and how tightly they must be placed.
//
// Declared here rather than imported so gpu-sim does not depend on Volcano's module to write
// one object. Where KAI splits these across a pod-grouper annotation and a topology
// annotation, Volcano puts both on the group — which is why the neutral layer describes
// intent and lets each translation put it wherever that scheduler keeps it.
type VolcanoPodGroup struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   metav1.ObjectMeta   `json:"metadata"`
	Spec       VolcanoPodGroupSpec `json:"spec"`
}

type VolcanoPodGroupSpec struct {
	MinMember int32  `json:"minMember"`
	Queue     string `json:"queue"`

	NetworkTopology *VolcanoNetworkTopology `json:"networkTopology,omitempty"`
}

type VolcanoNetworkTopology struct {
	Mode string `json:"mode"`
	// HighestTierName names the coarsest grouping the workload may span. gpu-sim's level
	// names are emitted as HyperNode tier names, so "rack" means the same word here as it
	// does in the scenario.
	HighestTierName string `json:"highestTierName,omitempty"`
}

func volcanoPodGroup(w scenario.Workload) *VolcanoPodGroup {
	// A non-gang workload still gets a group, because Volcano keeps topology there too.
	// minMember of one leaves each replica independently schedulable, which is what
	// "not a gang" means.
	minMember := int32(1)
	if w.Gang {
		minMember = int32(w.Replicas)
	}

	pg := &VolcanoPodGroup{
		APIVersion: volcanoPodGroupAPIVersion,
		Kind:       volcanoPodGroupKind,
		Metadata:   metav1.ObjectMeta{Name: w.Name},
		Spec: VolcanoPodGroupSpec{
			MinMember: minMember,
			Queue:     volcanoDefaultQueue,
		},
	}

	if w.Placement != nil && w.Placement.Required != "" {
		pg.Spec.NetworkTopology = &VolcanoNetworkTopology{
			Mode:            volcanoTopologyHard,
			HighestTierName: w.Placement.Required,
		}
	}

	return pg
}
