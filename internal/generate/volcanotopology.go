package generate

import (
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/MiguelGarrido02/gpu-sim/internal/topology"
)

// Volcano's topology CRD. Declared here rather than imported so gpu-sim does not take a
// dependency on the whole Volcano module to write a handful of small objects.
const (
	hyperNodeAPIVersion = "topology.volcano.sh/v1alpha1"
	hyperNodeKind       = "HyperNode"
)

// HyperNode is Volcano's unit of network topology: a group of nodes, or a group of groups,
// at a numbered tier. Lower tiers communicate faster.
//
// A different shape from KAI's, which is one object listing node-label levels. Volcano wants
// the tree drawn explicitly, one object per domain — so where KAI's topology is a schema,
// Volcano's is data. Both are generated from the same ClusterTopology, which is the point:
// a scenario says "keep this job in one rack" once and each scheduler is told in its own
// terms.
type HyperNode struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Spec       HyperNodeSpec     `json:"spec"`
}

type HyperNodeSpec struct {
	Tier int `json:"tier"`

	// TierName lets a workload ask for "rack" rather than for a number, which is what
	// makes gpu-sim's level vocabulary survive the translation intact.
	TierName string            `json:"tierName,omitempty"`
	Members  []HyperNodeMember `json:"members"`
}

type HyperNodeMember struct {
	// Type is Node at the lowest tier and HyperNode above it.
	Type     string            `json:"type"`
	Selector HyperNodeSelector `json:"selector"`
}

type HyperNodeSelector struct {
	ExactMatch *HyperNodeExactMatch `json:"exactMatch,omitempty"`
}

type HyperNodeExactMatch struct {
	Name string `json:"name"`
}

const (
	memberTypeNode      = "Node"
	memberTypeHyperNode = "HyperNode"
)

// VolcanoHyperNodes builds the topology tree Volcano reads.
//
// Tiers run from the finest grouping upward, and are numbered by position rather than fixed,
// so a topology whose nodes have no NVLink domain simply has one tier fewer instead of a
// gap. Every level gpu-sim knows is emitted, which is what lets the same scenario name
// `rack` on either scheduler.
func VolcanoHyperNodes(resolved *topology.Resolved) []*HyperNode {
	levels := volcanoLevels(resolved)
	if len(levels) == 0 {
		return nil
	}

	var out []*HyperNode

	// The lowest tier groups nodes; every tier above groups the tier below it.
	for i, level := range levels {
		tier := i + 1
		groups := map[string][]string{}

		for _, node := range resolved.Nodes {
			parent := level.value(node)
			if parent == "" {
				continue
			}
			if i == 0 {
				groups[parent] = append(groups[parent], node.Name)
				continue
			}
			// Above the lowest tier, a group's members are the groups beneath it.
			child := levels[i-1].value(node)
			if child == "" {
				continue
			}
			groups[parent] = appendUnique(groups[parent], hyperNodeName(levels[i-1].name, child))
		}

		memberType := memberTypeHyperNode
		if i == 0 {
			memberType = memberTypeNode
		}

		for _, name := range sortedKeys(groups) {
			out = append(out, hyperNode(levels[i].name, name, tier, memberType, groups[name]))
		}
	}
	return out
}

// volcanoLevel pairs a level name with how to read it off a node.
type volcanoLevel struct {
	name  string
	value func(topology.ResolvedNode) string
}

// volcanoLevels lists the levels worth emitting, finest first.
//
// The NVLink level is skipped unless every node has a domain, for the same reason KAI's
// topology skips it: a tier that some nodes cannot be placed in would quietly exclude them.
func volcanoLevels(resolved *topology.Resolved) []volcanoLevel {
	levels := []volcanoLevel{
		{name: "host", value: func(n topology.ResolvedNode) string { return n.Name }},
	}
	if allNodesHaveNVLink(resolved) {
		levels = append(levels, volcanoLevel{
			name:  "nvlink-domain",
			value: func(n topology.ResolvedNode) string { return n.NVLinkDomain },
		})
	}
	levels = append(levels,
		volcanoLevel{name: "rack", value: func(n topology.ResolvedNode) string { return n.Rack }},
		volcanoLevel{name: "fault-domain", value: func(n topology.ResolvedNode) string { return n.FaultDomain }},
	)
	return levels
}

// VolcanoTierNames lists the level names a workload may ask for, in the order they are
// emitted. Used to reject a placement Volcano's tree cannot express.
func VolcanoTierNames(resolved *topology.Resolved) []string {
	levels := volcanoLevels(resolved)
	names := make([]string, 0, len(levels))
	for _, level := range levels {
		names = append(names, level.name)
	}
	return names
}

func hyperNodeName(level, value string) string {
	return fmt.Sprintf("gpu-sim-%s-%s", level, value)
}

func hyperNode(level, value string, tier int, memberType string, members []string) *HyperNode {
	sort.Strings(members)
	out := &HyperNode{
		APIVersion: hyperNodeAPIVersion,
		Kind:       hyperNodeKind,
		Metadata: metav1.ObjectMeta{
			Name: hyperNodeName(level, value),
			Labels: map[string]string{
				LabelManagedBy: ManagedByValue,
			},
		},
		Spec: HyperNodeSpec{Tier: tier, TierName: level},
	}
	for _, member := range members {
		out.Spec.Members = append(out.Spec.Members, HyperNodeMember{
			Type:     memberType,
			Selector: HyperNodeSelector{ExactMatch: &HyperNodeExactMatch{Name: member}},
		})
	}
	return out
}

func appendUnique(list []string, value string) []string {
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
