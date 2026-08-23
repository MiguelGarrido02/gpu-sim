package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/MiguelGarrido02/gpu-sim/internal/generate"
	"github.com/MiguelGarrido02/gpu-sim/internal/workload"
)

// TaintKey marks a device broken by gpu-sim. One key for every fault, since a scenario
// reapplies its topology before it runs and therefore never inherits another's taints.
const TaintKey = "gpu-sim.io/faulty"

// DeviceMatch selects the devices a fault breaks. Exactly one field is set.
type DeviceMatch struct {
	// Nodes breaks every device on these nodes.
	Nodes map[string]bool

	// Attributes breaks devices whose published attributes all match.
	Attributes map[string]string
}

// NodesWithLabel returns the simulated nodes carrying a label value, which is how a fault
// aimed at a topology level resolves to hardware.
func (c *Client) NodesWithLabel(ctx context.Context, label, value string) (map[string]bool, error) {
	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", label, value),
	})
	if err != nil {
		return nil, fmt.Errorf("listing nodes with %s=%s: %w", label, value, err)
	}
	out := map[string]bool{}
	for _, node := range nodes.Items {
		out[node.Name] = true
	}
	return out, nil
}

// TaintDevices marks matching devices as broken and returns their names.
//
// The taint goes into the ResourceSlice gpu-sim already publishes, so no extra machinery is
// needed to break a device — and Kubernetes does the rest: NoExecute evicts the pods holding
// a tainted device and the scheduler places them elsewhere. What gpu-sim measures is that
// reaction, not its own.
func (c *Client) TaintDevices(ctx context.Context, match DeviceMatch, effect string) (map[string]bool, error) {
	slices, err := c.kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{
		LabelSelector: generate.ManagedSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing managed ResourceSlices: %w", err)
	}

	tainted := map[string]bool{}

	for i := range slices.Items {
		slice := slices.Items[i]
		if len(slice.Spec.Devices) == 0 {
			continue // a counter-only slice has nothing to taint
		}

		touched := false
		for j := range slice.Spec.Devices {
			device := &slice.Spec.Devices[j]
			if !match.matches(slice, *device) {
				continue
			}
			device.Taints = append(device.Taints, resourceapi.DeviceTaint{
				Key:    TaintKey,
				Effect: resourceapi.DeviceTaintEffect(effect),
			})
			tainted[device.Name] = true
			touched = true
		}
		if !touched {
			continue
		}

		if err := c.updateSliceDevices(ctx, slice.Name, slice.Spec.Devices); err != nil {
			return tainted, err
		}
	}

	return tainted, nil
}

func (m DeviceMatch) matches(slice resourceapi.ResourceSlice, device resourceapi.Device) bool {
	if len(m.Nodes) > 0 {
		return slice.Spec.NodeName != nil && m.Nodes[*slice.Spec.NodeName]
	}
	for name, want := range m.Attributes {
		got, found := device.Attributes[resourceapi.QualifiedName(name)]
		if !found {
			// Try the qualified form, since a scenario writes `profile` for what may be
			// published as `gpu.nvidia.com/profile`.
			if got, found = lookupQualified(device, name); !found {
				return false
			}
		}
		if attributeString(got) != want {
			return false
		}
	}
	return len(m.Attributes) > 0
}

func lookupQualified(device resourceapi.Device, name string) (resourceapi.DeviceAttribute, bool) {
	for qualified, value := range device.Attributes {
		if stripDomain(string(qualified)) == name {
			return value, true
		}
	}
	return resourceapi.DeviceAttribute{}, false
}

func (c *Client) updateSliceDevices(ctx context.Context, name string, devices []resourceapi.Device) error {
	slices := c.kube.ResourceV1().ResourceSlices()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := slices.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		existing.Spec.Devices = devices
		_, err = slices.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("tainting devices in ResourceSlice %s: %w", name, err)
	}
	return nil
}

// DeleteNode removes a simulated node.
func (c *Client) DeleteNode(ctx context.Context, name string) error {
	err := c.kube.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting node %s: %w", name, err)
	}
	return nil
}

// PodState is one pod at one instant, with everything a fault assertion needs to decide
// whether that pod was a casualty.
type PodState struct {
	UID      string
	Name     string
	Workload string
	Node     string
	Running  bool
	Devices  []string
}

// SnapshotPods records the namespace as it stands.
//
// Taken at the moment a fault fires, because recovery has to be judged against who was
// running *then*. A pod on a deleted node keeps reporting Running for about a minute, so a
// check made only at the end cannot tell a survivor from a corpse.
func (c *Client) SnapshotPods(ctx context.Context, ns string) ([]PodState, error) {
	pods, err := c.AllPods(ctx, ns)
	if err != nil {
		return nil, err
	}

	claims, err := c.kube.ResourceV1().ResourceClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing resource claims: %w", err)
	}

	devicesByPod := map[string][]string{}
	for _, claim := range claims.Items {
		if claim.Status.Allocation == nil {
			continue
		}
		for _, ref := range claim.OwnerReferences {
			if ref.Kind != "Pod" {
				continue
			}
			for _, result := range claim.Status.Allocation.Devices.Results {
				devicesByPod[string(ref.UID)] = append(devicesByPod[string(ref.UID)], result.Device)
			}
		}
	}

	out := make([]PodState, 0, len(pods))
	for _, pod := range pods {
		out = append(out, PodState{
			UID:      string(pod.UID),
			Name:     pod.Name,
			Workload: pod.Labels[workload.LabelWorkload],
			Node:     pod.Spec.NodeName,
			Running:  pod.Status.Phase == corev1.PodRunning,
			Devices:  devicesByPod[string(pod.UID)],
		})
	}
	return out, nil
}
