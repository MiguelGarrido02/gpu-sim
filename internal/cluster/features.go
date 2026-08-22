package cluster

import (
	"context"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// CheckPartitionableDevices verifies the cluster will actually store the counters MIG
// depends on.
//
// `DRAPartitionableDevices` is beta and on by default, but a cluster can disable it — and
// when it is off the API server silently drops sharedCounters and consumesCounters on
// write rather than rejecting them. Every partition would then appear independently
// allocatable, and the simulation would cheerfully place seven 7g.80gb instances on one
// GPU. A simulator that lies is worse than one that refuses, so this refuses.
//
// Detected with a server-side dry run rather than by reading feature-gate metrics, which
// need privileges a user of this tool should not have to have.
func (c *Client) CheckPartitionableDevices(ctx context.Context, nodeName string) error {
	probe := &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-sim-partitionable-probe"},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   "gpu-sim.io-probe",
			NodeName: ptr.To(nodeName),
			Pool:     resourceapi.ResourcePool{Name: "gpu-sim-probe", ResourceSliceCount: 1},
			SharedCounters: []resourceapi.CounterSet{{
				Name: "probe",
				Counters: map[string]resourceapi.Counter{
					"probe": {Value: *resource.NewQuantity(1, resource.DecimalSI)},
				},
			}},
		},
	}

	created, err := c.kube.ResourceV1().ResourceSlices().Create(
		ctx, probe, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
	if err != nil {
		return fmt.Errorf("checking for partitionable device support: %w", err)
	}

	if len(created.Spec.SharedCounters) == 0 {
		return fmt.Errorf(
			"this cluster drops ResourceSlice shared counters, so the DRAPartitionableDevices " +
				"feature gate is disabled; MIG cannot be simulated without it, because every " +
				"partition would look independently allocatable and the cluster would appear to " +
				"hold several whole GPUs where it has one")
	}
	return nil
}
