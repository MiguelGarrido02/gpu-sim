package mig

import (
	"fmt"
	"strings"
)

// Counter names. They must be RFC 1123 labels, so no camelCase — an API constraint that is
// only discoverable by being rejected.
const (
	// MemorySliceCounter is per position: consuming memory-slice-3 is what makes two
	// partitions overlapping that slice mutually exclusive. A single "memory" total
	// would let the scheduler place two partitions in the same physical space.
	MemorySliceCounter = "memory-slice-%d"

	// SMSliceCounter is a plain pool. Compute slices are interchangeable, and it is this
	// dimension that caps 1g.10gb at seven instances despite eight memory slices.
	SMSliceCounter = "sm-slices"
)

// CounterSetName is the shared counter set representing one physical GPU.
func CounterSetName(gpuIndex int) string {
	return fmt.Sprintf("gpu-%d", gpuIndex)
}

// Partition is one allocatable MIG instance: a profile at a particular offset.
type Partition struct {
	Profile Profile

	// GPUIndex is the physical GPU this partition lives on.
	GPUIndex int

	// Start is the first memory slice it occupies.
	Start int

	// DeviceName is the ResourceSlice device name.
	DeviceName string
}

// MemorySlices lists the positions the partition occupies.
func (p Partition) MemorySlices() []int {
	slices := make([]int, 0, p.Profile.MemorySlices)
	for i := 0; i < p.Profile.MemorySlices; i++ {
		slices = append(slices, p.Start+i)
	}
	return slices
}

// Overlaps reports whether two partitions on the same GPU compete for memory.
func (p Partition) Overlaps(other Partition) bool {
	if p.GPUIndex != other.GPUIndex {
		return false
	}
	return p.Start < other.Start+other.Profile.MemorySlices &&
		other.Start < p.Start+p.Profile.MemorySlices
}

// PartitionsFor enumerates every partition a GPU can offer.
//
// All of them are published, not one chosen layout, because fragmentation has to emerge
// from the order workloads arrive rather than be declared in a file. Publishing a fixed
// layout would make "does a 3g.40gb still fit?" answerable by reading our own configuration.
func PartitionsFor(g Geometry, profiles []Profile, gpuIndex int) []Partition {
	var out []Partition
	for _, profile := range profiles {
		for _, start := range g.Placements(profile) {
			out = append(out, Partition{
				Profile:    profile,
				GPUIndex:   gpuIndex,
				Start:      start,
				DeviceName: deviceName(gpuIndex, profile.Name, start),
			})
		}
	}
	return out
}

// deviceName mirrors the shape NVIDIA's DRA driver uses, `gpu-<n>-mig-<profile>-<start>`,
// minus the NVML profile ID, which has no meaning in a simulation and no source to take it
// from. Dots are stripped because a device name must be a DNS label.
func deviceName(gpuIndex int, profile string, start int) string {
	return fmt.Sprintf("gpu-%d-mig-%s-%d", gpuIndex, strings.ReplaceAll(profile, ".", ""), start)
}
