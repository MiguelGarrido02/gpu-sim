// Package generate turns a resolved topology into the Kubernetes objects a scheduler
// reads: simulated nodes with topology labels, DRA ResourceSlices with per-GPU attributes,
// and the scheduler's own topology configuration.
//
// All three come from the same resolved topology so they cannot disagree. Phase 0 showed
// how expensive an inconsistency between them is to diagnose: a single missing node label
// made every topology-constrained workload unschedulable, with an error that blamed
// capacity on an entirely idle cluster.
package generate

// Node label keys carrying gpu-sim's topology. These are what a scheduler groups nodes by,
// so they name the levels of the generated scheduler topology too.
const (
	LabelRack         = "gpu-sim.io/rack"
	LabelFaultDomain  = "gpu-sim.io/fault-domain"
	LabelNVLinkDomain = "gpu-sim.io/nvlink-domain"

	// LabelNodePool is fake-gpu-operator's, and is what makes its status-updater adopt
	// a node and give it the pool's GPU configuration.
	LabelNodePool = "run.ai/simulated-gpu-node-pool"

	// LabelKWOKType marks a node as simulated. Workloads meant for the simulation select
	// on it, and it pairs with the KWOK taint that keeps everything else off.
	LabelKWOKType = "type"
	KWOKTypeValue = "kwok"
)

// Well-known labels every real kubelet registers when a node joins. A simulated node has
// no kubelet, so nothing else would add them.
//
// Not cosmetic: KAI's topology plugin drops any node missing a label for any level of its
// topology, and kubernetes.io/hostname is the conventional leaf level. Omitting it leaves
// the topology tree empty and every topology-constrained workload pending. Anything else
// consuming node topology makes the same assumption, so gpu-sim emits the full set.
const (
	LabelHostname = "kubernetes.io/hostname"
	LabelOS       = "kubernetes.io/os"
	LabelArch     = "kubernetes.io/arch"
)

// DRA device attribute names.
//
// The unqualified names mirror NVIDIA/k8s-dra-driver-gpu exactly, so that a DeviceClass
// CEL selector written against a simulated cluster still matches on real hardware. A
// selector reading an attribute gpu-sim invented would silently match nothing in
// production, which is worse than not simulating the attribute at all.
const (
	AttrType         = "type"
	AttrUUID         = "uuid"
	AttrProductName  = "productName"
	AttrArchitecture = "architecture"
	AttrPCIBusID     = "pciBusID"
	AttrPCIeRoot     = "pcieRoot"
	AttrNUMANode     = "numaNode"

	// DeviceTypeGPU is the value of AttrType for a whole physical GPU.
	DeviceTypeGPU = "gpu"
)

// Attributes gpu-sim adds because the real driver has no equivalent: NVIDIA exposes fabric
// information through the separate ComputeDomain/IMEX machinery rather than as GPU device
// attributes. The gpu-sim.io prefix keeps it unambiguous that these are simulation
// extensions and not something NVIDIA ships.
const (
	AttrNVLinkDomain    = "gpu-sim.io/nvlinkDomain"
	AttrNVLinkPeerCount = "gpu-sim.io/nvlinkPeerCount"
	AttrFaultDomain     = "gpu-sim.io/faultDomain"
)

// Attributes fake-gpu-operator publishes today. Reproduced so that gpu-sim's slices remain
// a drop-in replacement: the DeviceClass shipped with the operator selects on
// device.attributes['gpu.nvidia.com'].type, and dropping these would leave every GPU
// unselectable.
const (
	AttrLegacyType        = "gpu.nvidia.com/type"
	AttrLegacyUUID        = "gpu.nvidia.com/uuid"
	AttrLegacyProductName = "gpu.nvidia.com/productName"
	AttrLegacyModel       = "model"
)

// DriverName is the DRA driver gpu-sim publishes under. It matches fake-gpu-operator's so
// the operator's DeviceClass keeps selecting these devices.
const DriverName = "gpu.nvidia.com"
