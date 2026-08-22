package cluster

import (
	"context"
	"fmt"

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
