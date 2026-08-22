package generate

import (
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/MiguelGarrido02/gpu-sim/internal/topology"
)

// ResourceSlices builds one DRA ResourceSlice per node, publishing each GPU as a device
// with the topology attributes a scheduler can select on.
//
// This is the projection that distinguishes gpu-sim from what already exists: upstream
// publishes five attributes per GPU, none of them positional, so every GPU on a node is
// indistinguishable from every other and no selector can express "the GPUs behind the same
// NVSwitch".
func ResourceSlices(resolved *topology.Resolved) []*resourceapi.ResourceSlice {
	slices := make([]*resourceapi.ResourceSlice, 0, len(resolved.Nodes))
	for _, rn := range resolved.Nodes {
		// A MIG-enabled GPU is not published as a whole device: on real hardware it is
		// not directly allocatable, and the whole GPU is simply the largest partition.
		if rn.MIG != nil {
			slices = append(slices, migSlices(rn, resolved.Name)...)
			continue
		}
		slices = append(slices, resourceSlice(rn, resolved.Name))
	}
	return slices
}

// SliceName is the ResourceSlice name for a node. It matches the name fake-gpu-operator's
// plugin uses, so that gpu-sim adopts an existing slice rather than publishing a second,
// competing one for the same node.
func SliceName(nodeName string) string {
	return fmt.Sprintf("kwok-%s-gpu", nodeName)
}

func resourceSlice(rn topology.ResolvedNode, topologyName string) *resourceapi.ResourceSlice {
	devices := make([]resourceapi.Device, 0, len(rn.GPUs))
	for _, gpu := range rn.GPUs {
		devices = append(devices, device(rn, gpu))
	}

	return &resourceapi.ResourceSlice{
		TypeMeta: metav1.TypeMeta{APIVersion: resourceapi.SchemeGroupVersion.String(), Kind: "ResourceSlice"},
		ObjectMeta: metav1.ObjectMeta{
			Name: SliceName(rn.Name),
			Labels: map[string]string{
				LabelRack:      rn.Rack,
				LabelManagedBy: ManagedByValue,
				LabelTopology:  topologyName,
			},
		},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   DriverName,
			NodeName: ptr.To(rn.Name),
			Pool: resourceapi.ResourcePool{
				Name:               rn.Name,
				ResourceSliceCount: 1,
			},
			Devices: devices,
		},
	}
}

func device(rn topology.ResolvedNode, gpu topology.ResolvedGPU) resourceapi.Device {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		// Names mirroring NVIDIA's real DRA driver.
		AttrType:        {StringValue: ptr.To(DeviceTypeGPU)},
		AttrUUID:        {StringValue: ptr.To(gpu.UUID)},
		AttrProductName: {StringValue: ptr.To(rn.ProductName)},

		// Reproduced from fake-gpu-operator so its DeviceClass keeps selecting these
		// devices; see the constants' documentation.
		AttrLegacyType:        {StringValue: ptr.To(DeviceTypeGPU)},
		AttrLegacyUUID:        {StringValue: ptr.To(gpu.UUID)},
		AttrLegacyProductName: {StringValue: ptr.To(rn.ProductName)},
		AttrLegacyModel:       {StringValue: ptr.To(rn.ProductName)},
	}

	if rn.Architecture != "" {
		attrs[AttrArchitecture] = resourceapi.DeviceAttribute{StringValue: ptr.To(rn.Architecture)}
	}
	if gpu.PCIBusID != "" {
		attrs[AttrPCIBusID] = resourceapi.DeviceAttribute{StringValue: ptr.To(gpu.PCIBusID)}
	}

	// Published only when the profile actually declared a PCIe root complex map.
	// Defaulting to NUMA node 0 would tell a topology-aware scheduler that every GPU
	// shares one socket, which is both false and unfalsifiable from the outside.
	if gpu.HasNUMA {
		attrs[AttrPCIeRoot] = resourceapi.DeviceAttribute{StringValue: ptr.To(gpu.PCIeRoot)}
		attrs[AttrNUMANode] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(gpu.NUMANode))}
	}

	// Fabric attributes, absent from the real driver. Omitted entirely for a GPU with no
	// NVLink, so that a selector requiring a domain excludes it rather than matching a
	// domain of one.
	if gpu.NVLinkDomain != "" {
		attrs[AttrNVLinkDomain] = resourceapi.DeviceAttribute{StringValue: ptr.To(gpu.NVLinkDomain)}
		attrs[AttrNVLinkPeerCount] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(gpu.NVLinkPeers))}
	}
	if gpu.FaultDomain != "" {
		attrs[AttrFaultDomain] = resourceapi.DeviceAttribute{StringValue: ptr.To(gpu.FaultDomain)}
	}

	return resourceapi.Device{
		Name:       gpu.DeviceName,
		Attributes: attrs,
	}
}
