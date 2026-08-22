package generate

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/MiguelGarrido02/gpu-sim/internal/topology"
)

const (
	// kwokNodeAnnotation hands the node's lifecycle to the KWOK controller, which
	// answers the heartbeats a kubelet would otherwise send.
	kwokNodeAnnotation = "kwok.x-k8s.io/node"
	kwokNodeValue      = "fake"

	// kwokTaint keeps real DaemonSets off simulated nodes. Without it they would be
	// scheduled onto a node with no kubelet and stay Pending forever.
	kwokTaintKey = "kwok.x-k8s.io/node"
)

// Node capacity is fixed rather than declared per pool. Simulated pods consume nothing
// real, so the only thing these numbers do is stop CPU or memory from becoming the binding
// constraint in a test about GPU placement. They are sized generously for that reason.
var nodeCapacity = corev1.ResourceList{
	corev1.ResourceCPU:    resource.MustParse("128"),
	corev1.ResourceMemory: resource.MustParse("2Ti"),
	corev1.ResourcePods:   resource.MustParse("256"),
}

// Nodes builds the simulated Node objects for a resolved topology.
func Nodes(resolved *topology.Resolved) []*corev1.Node {
	nodes := make([]*corev1.Node, 0, len(resolved.Nodes))
	for _, rn := range resolved.Nodes {
		nodes = append(nodes, node(rn))
	}
	return nodes
}

func node(rn topology.ResolvedNode) *corev1.Node {
	labels := map[string]string{
		LabelKWOKType: KWOKTypeValue,
		LabelNodePool: rn.Pool,

		LabelHostname: rn.Name,
		LabelOS:       "linux",
		// GPU nodes are overwhelmingly x86, and the simulated architecture is
		// independent of the machine running the simulation — nothing is executed.
		LabelArch: "amd64",

		LabelRack:        rn.Rack,
		LabelFaultDomain: rn.FaultDomain,
	}

	// A node whose pool has no NVLink gets no domain label at all, rather than an empty
	// one. An empty value is a value: it would place every non-NVLink node in the same
	// domain and let a scheduler treat them as adjacent.
	if rn.NVLinkDomain != "" {
		labels[LabelNVLinkDomain] = rn.NVLinkDomain
	}

	return &corev1.Node{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
		ObjectMeta: metav1.ObjectMeta{
			Name:        rn.Name,
			Labels:      labels,
			Annotations: map[string]string{kwokNodeAnnotation: kwokNodeValue},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{
				Key:    kwokTaintKey,
				Value:  kwokNodeValue,
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Status: corev1.NodeStatus{
			Capacity:    nodeCapacity,
			Allocatable: nodeCapacity,
			NodeInfo: corev1.NodeSystemInfo{
				Architecture:    "amd64",
				OperatingSystem: "linux",
				KubeletVersion:  "fake",
				OSImage:         "fake",
				KernelVersion:   "fake",
			},
		},
	}
}
