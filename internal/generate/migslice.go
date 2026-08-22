package generate

import (
	"fmt"
	"regexp"
	"strconv"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/MiguelGarrido02/gpu-sim/internal/mig"
	"github.com/MiguelGarrido02/gpu-sim/internal/topology"
)

// maxDevicesPerSlice is the API's limit once any device consumes counters. Without counters
// it is 128; with them it halves, and there is no way to discover that other than being
// rejected. An 8-GPU node offering 21 partitions each needs 168 devices, so a node's
// partitions are spread across several slices.
const maxDevicesPerSlice = 64

// MIG device attribute names, mirroring NVIDIA's DRA driver so that a selector written
// against the simulator still matches on real hardware.
const (
	AttrParentUUID = "parentUUID"
	AttrProfile    = "profile"

	// DeviceTypeMIG is the value of AttrType for a MIG instance rather than a whole GPU.
	DeviceTypeMIG = "mig"
)

// MIGDeviceClassName is the class MIG instances are selected through. NVIDIA publishes GPUs
// and MIG instances under one driver but separate device classes, so a claim asking for a
// GPU never receives a partition of one.
const MIGDeviceClassName = "mig.nvidia.com"

// counterSliceName and partitionSliceName keep a node's slices in one pool.
func counterSliceName(node string) string { return fmt.Sprintf("kwok-%s-mig-counters", node) }
func partitionSliceName(node string, i int) string {
	return fmt.Sprintf("kwok-%s-mig-%d", node, i)
}

// migSlices publishes a MIG-enabled node: one slice carrying the shared counters, and as
// many device slices as the partitions need.
//
// The counters have to live in their own slice because the API accepts either sharedCounters
// or devices on a slice, never both — another constraint only discoverable by being
// rejected. Every slice declares the same pool and the same total count, which is what makes
// the scheduler treat them as one inventory.
func migSlices(rn topology.ResolvedNode, topologyName string) []*resourceapi.ResourceSlice {
	devices := make([]resourceapi.Device, 0, len(rn.GPUs)*len(rn.MIG.Profiles))
	counterSets := make([]resourceapi.CounterSet, 0, len(rn.GPUs))

	for _, gpu := range rn.GPUs {
		counterSets = append(counterSets, counterSetFor(rn.MIG.Geometry, gpu.Index))
		for _, partition := range gpu.Partitions {
			devices = append(devices, migDevice(rn, gpu, partition))
		}
	}

	chunks := chunkDevices(devices, maxDevicesPerSlice)
	total := int64(1 + len(chunks))

	slices := make([]*resourceapi.ResourceSlice, 0, len(chunks)+1)
	slices = append(slices, newSlice(rn, topologyName, counterSliceName(rn.Name), total, func(spec *resourceapi.ResourceSliceSpec) {
		spec.SharedCounters = counterSets
	}))
	for i, chunk := range chunks {
		slices = append(slices, newSlice(rn, topologyName, partitionSliceName(rn.Name, i), total, func(spec *resourceapi.ResourceSliceSpec) {
			spec.Devices = chunk
		}))
	}
	return slices
}

func newSlice(rn topology.ResolvedNode, topologyName, name string, total int64,
	fill func(*resourceapi.ResourceSliceSpec)) *resourceapi.ResourceSlice {

	spec := resourceapi.ResourceSliceSpec{
		Driver:   DriverName,
		NodeName: ptr.To(rn.Name),
		Pool: resourceapi.ResourcePool{
			Name:               rn.Name,
			ResourceSliceCount: total,
		},
	}
	fill(&spec)

	return &resourceapi.ResourceSlice{
		TypeMeta: metav1.TypeMeta{APIVersion: resourceapi.SchemeGroupVersion.String(), Kind: "ResourceSlice"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				LabelRack:      rn.Rack,
				LabelManagedBy: ManagedByValue,
				LabelTopology:  topologyName,
			},
		},
		Spec: spec,
	}
}

// counterSetFor describes one physical GPU's capacity.
//
// Memory slices are counted per position rather than as a total, because that is what makes
// two partitions overlapping the same physical space mutually exclusive. A single "8 memory
// slices" counter would happily let the scheduler place two partitions on top of each other.
// Compute slices are a plain pool, since they are interchangeable.
func counterSetFor(g mig.Geometry, gpuIndex int) resourceapi.CounterSet {
	counters := make(map[string]resourceapi.Counter, g.MemorySlices+1)
	for i := 0; i < g.MemorySlices; i++ {
		counters[fmt.Sprintf(mig.MemorySliceCounter, i)] = resourceapi.Counter{
			Value: *resource.NewQuantity(1, resource.DecimalSI),
		}
	}
	counters[mig.SMSliceCounter] = resourceapi.Counter{
		Value: *resource.NewQuantity(int64(g.SMSlices), resource.DecimalSI),
	}
	return resourceapi.CounterSet{Name: mig.CounterSetName(gpuIndex), Counters: counters}
}

func migDevice(rn topology.ResolvedNode, gpu topology.ResolvedGPU, p mig.Partition) resourceapi.Device {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		AttrType:        {StringValue: ptr.To(DeviceTypeMIG)},
		AttrProfile:     {StringValue: ptr.To(p.Profile.Name)},
		AttrParentUUID:  {StringValue: ptr.To(gpu.UUID)},
		AttrProductName: {StringValue: ptr.To(rn.ProductName)},
	}
	if rn.Architecture != "" {
		attrs[AttrArchitecture] = resourceapi.DeviceAttribute{StringValue: ptr.To(rn.Architecture)}
	}
	// A partition inherits its parent's position in the machine and in the fabric: it is
	// on the same socket, behind the same PCIe root, in the same NVLink and fault domains.
	if gpu.HasNUMA {
		attrs[AttrPCIeRoot] = resourceapi.DeviceAttribute{StringValue: ptr.To(gpu.PCIeRoot)}
		attrs[AttrNUMANode] = resourceapi.DeviceAttribute{IntValue: ptr.To(int64(gpu.NUMANode))}
	}
	if gpu.NVLinkDomain != "" {
		attrs[AttrNVLinkDomain] = resourceapi.DeviceAttribute{StringValue: ptr.To(gpu.NVLinkDomain)}
	}
	if gpu.FaultDomain != "" {
		attrs[AttrFaultDomain] = resourceapi.DeviceAttribute{StringValue: ptr.To(gpu.FaultDomain)}
	}

	counters := make(map[string]resourceapi.Counter, p.Profile.MemorySlices+1)
	for _, slice := range p.MemorySlices() {
		counters[fmt.Sprintf(mig.MemorySliceCounter, slice)] = resourceapi.Counter{
			Value: *resource.NewQuantity(1, resource.DecimalSI),
		}
	}
	counters[mig.SMSliceCounter] = resourceapi.Counter{
		Value: *resource.NewQuantity(int64(p.Profile.SMSlices), resource.DecimalSI),
	}

	device := resourceapi.Device{
		Name:       p.DeviceName,
		Attributes: attrs,
		ConsumesCounters: []resourceapi.DeviceCounterConsumption{{
			CounterSet: mig.CounterSetName(gpu.Index),
			Counters:   counters,
		}},
	}

	if memory, ok := profileMemory(p.Profile.Name); ok {
		device.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			"memory": {Value: memory},
		}
	}

	return device
}

// profileMemoryPattern reads the memory size out of a profile name: the "10gb" of
// "1g.10gb". The size is part of the name by NVIDIA's own convention, so it needs no
// separate table to fall out of sync with.
var profileMemoryPattern = regexp.MustCompile(`\.(\d+)gb$`)

func profileMemory(name string) (resource.Quantity, bool) {
	match := profileMemoryPattern.FindStringSubmatch(name)
	if match == nil {
		return resource.Quantity{}, false
	}
	gigabytes, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return resource.Quantity{}, false
	}
	return *resource.NewQuantity(gigabytes*1024*1024*1024, resource.BinarySI), true
}

func chunkDevices(devices []resourceapi.Device, size int) [][]resourceapi.Device {
	var chunks [][]resourceapi.Device
	for start := 0; start < len(devices); start += size {
		end := start + size
		if end > len(devices) {
			end = len(devices)
		}
		chunks = append(chunks, devices[start:end])
	}
	return chunks
}

// MIGDeviceClass is the DeviceClass MIG instances are selected through.
func MIGDeviceClass() *DeviceClass {
	return &DeviceClass{
		APIVersion: resourceapi.SchemeGroupVersion.String(),
		Kind:       "DeviceClass",
		Metadata:   metav1.ObjectMeta{Name: MIGDeviceClassName},
		Spec: DeviceClassSpec{
			Selectors: []DeviceClassSelector{{
				CEL: &DeviceClassCEL{
					Expression: fmt.Sprintf("device.driver == %q && device.attributes[%q].type == %q",
						DriverName, DriverName, DeviceTypeMIG),
				},
			}},
		},
	}
}

// DeviceClass is declared here rather than imported so the generator can emit it with the
// same shape as the rest of its output.
type DeviceClass struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Spec       DeviceClassSpec   `json:"spec"`
}

type DeviceClassSpec struct {
	Selectors []DeviceClassSelector `json:"selectors"`
}

type DeviceClassSelector struct {
	CEL *DeviceClassCEL `json:"cel,omitempty"`
}

type DeviceClassCEL struct {
	Expression string `json:"expression"`
}
