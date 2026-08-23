package cluster

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/MiguelGarrido02/gpu-sim/internal/generate"
	"github.com/MiguelGarrido02/gpu-sim/internal/topology"
)

// TopologyResult reports what applying a topology changed.
type TopologyResult struct {
	Name    string
	Nodes   int
	Slices  int
	Devices int
	Removed []string
}

// ApplyTopology loads a topology file and makes the cluster match it.
//
// Shared by the `topology apply` command and by the scenario runner, which applies a
// scenario's own topology before measuring anything — otherwise a run would be counting
// whatever the previous one left behind.
func (c *Client) ApplyTopology(ctx context.Context, path string) (*TopologyResult, error) {
	ct, err := topology.Load(path)
	if err != nil {
		return nil, err
	}

	resolved, err := ct.Resolve(c.LoadProfile(ctx))
	if err != nil {
		return nil, err
	}

	nodes := generate.Nodes(resolved)
	for _, node := range nodes {
		if err := c.ApplyNode(ctx, node); err != nil {
			return nil, err
		}
	}

	// Checked after the nodes exist, since the probe names one, and before any slice is
	// published, so a cluster that cannot store counters is refused rather than filled
	// with partitions it would treat as independently allocatable.
	if resolvedHasMIG(resolved) {
		if err := c.CheckPartitionableDevices(ctx, nodes[0].Name); err != nil {
			return nil, err
		}
	}

	// Slices come second: a slice names its node, and publishing one for a node that does
	// not exist yet leaves the scheduler briefly seeing GPUs nowhere.
	slices := generate.ResourceSlices(resolved)
	devices := 0
	for _, slice := range slices {
		if err := c.ApplyResourceSlice(ctx, slice); err != nil {
			return nil, err
		}
		devices += len(slice.Spec.Devices)
	}

	// MIG partitions are selected through their own DeviceClass, so a claim asking for a
	// GPU never receives a partition of one.
	if resolvedHasMIG(resolved) {
		if err := c.ApplyMIGDeviceClass(ctx); err != nil {
			return nil, err
		}
	}

	kaiTopology := generate.KAITopologyFor(resolved)
	if err := c.ApplyKAITopology(ctx, kaiTopology); err != nil {
		return nil, fmt.Errorf("applying scheduler topology: %w", err)
	}
	if err := c.applyHyperNodes(ctx, resolved); err != nil {
		return nil, fmt.Errorf("applying Volcano topology: %w", err)
	}

	// Pruning last, so a failure above leaves the previous cluster intact rather than a
	// half-deleted one.
	keepNodes := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		keepNodes[node.Name] = true
	}
	keepSlices := make(map[string]bool, len(slices))
	for _, slice := range slices {
		keepSlices[slice.Name] = true
	}
	removed, err := c.Prune(ctx, keepNodes, keepSlices)
	if err != nil {
		return nil, err
	}

	return &TopologyResult{
		Name:    resolved.Name,
		Nodes:   len(nodes),
		Slices:  len(slices),
		Devices: devices,
		Removed: removed,
	}, nil
}

func resolvedHasMIG(resolved *topology.Resolved) bool {
	for _, node := range resolved.Nodes {
		if node.MIG != nil {
			return true
		}
	}
	return false
}

var hyperNodeGVR = schema.GroupVersionResource{
	Group:    "topology.volcano.sh",
	Version:  "v1alpha1",
	Resource: "hypernodes",
}

// applyHyperNodes writes Volcano's topology tree, and removes the ones a previous topology
// left behind.
//
// Skipped silently when the CRD is absent, since Volcano is optional: a cluster running only
// KAI should not fail to build because a scheduler it does not have is not installed.
func (c *Client) applyHyperNodes(ctx context.Context, resolved *topology.Resolved) error {
	nodes := generate.VolcanoHyperNodes(resolved)
	hyperNodes := c.dynamic.Resource(hyperNodeGVR)

	keep := make(map[string]bool, len(nodes))
	for _, hn := range nodes {
		keep[hn.Metadata.Name] = true

		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(hn)
		if err != nil {
			return fmt.Errorf("converting HyperNode %s: %w", hn.Metadata.Name, err)
		}
		obj := &unstructured.Unstructured{Object: raw}

		existing, err := hyperNodes.Get(ctx, hn.Metadata.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if _, err := hyperNodes.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
				return fmt.Errorf("creating HyperNode %s: %w", hn.Metadata.Name, err)
			}
			continue
		}
		if err != nil {
			if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				return nil // Volcano is not installed
			}
			return fmt.Errorf("reading HyperNode %s: %w", hn.Metadata.Name, err)
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		if _, err := hyperNodes.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating HyperNode %s: %w", hn.Metadata.Name, err)
		}
	}

	existing, err := hyperNodes.List(ctx, metav1.ListOptions{LabelSelector: generate.ManagedSelector})
	if err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("listing managed HyperNodes: %w", err)
	}
	for _, hn := range existing.Items {
		if keep[hn.GetName()] {
			continue
		}
		if err := hyperNodes.Delete(ctx, hn.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting HyperNode %s: %w", hn.GetName(), err)
		}
	}
	return nil
}
