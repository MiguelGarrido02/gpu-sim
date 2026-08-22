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
	GPUs    int
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

	// Slices come second: a slice names its node, and publishing one for a node that does
	// not exist yet leaves the scheduler briefly seeing GPUs nowhere.
	slices := generate.ResourceSlices(resolved)
	gpus := 0
	for _, slice := range slices {
		if err := c.ApplyResourceSlice(ctx, slice); err != nil {
			return nil, err
		}
		gpus += len(slice.Spec.Devices)
	}

	kaiTopology := generate.KAITopologyFor(resolved)
	if err := c.ApplyKAITopology(ctx, kaiTopology); err != nil {
		return nil, fmt.Errorf("applying scheduler topology: %w", err)
	}

	// Pruning last, so a failure above leaves the previous cluster intact rather than a
	// half-deleted one.
	keep := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		keep[node.Name] = true
	}
	removed, err := c.Prune(ctx, keep)
	if err != nil {
		return nil, err
	}

	return &TopologyResult{
		Name:    resolved.Name,
		Nodes:   len(nodes),
		Slices:  len(slices),
		GPUs:    gpus,
		Removed: removed,
	}, nil
}
