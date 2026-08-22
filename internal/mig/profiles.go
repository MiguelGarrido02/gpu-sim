// Package mig models MIG partitioning: how a physical GPU can be cut into isolated
// instances, and how much capacity that arrangement wastes.
//
// gpu-sim does not decide which partition a workload gets — Kubernetes does, through
// partitionable devices, where a ResourceSlice declares shared counters and each device
// declares what it consumes. Building an allocator here would mean testing our arithmetic
// instead of the scheduler's behaviour. What this package does is publish the partitions
// correctly and measure the fragmentation that emerges.
package mig

import (
	"fmt"
	"sort"
	"strings"
)

// Geometry is a GPU family's partitioning layout.
//
// Both A100 and H100 present 8 memory slices and 7 SM (compute) slices. The asymmetry
// between the two is what makes fragmentation possible at all: a 3g profile occupies four
// memory slices but only three SMs, so neither dimension alone predicts what fits.
type Geometry struct {
	MemorySlices int
	SMSlices     int
	Profiles     []Profile
}

// Profile is one MIG instance size.
type Profile struct {
	// Name is NVIDIA's, e.g. "3g.40gb": compute slices, then memory.
	Name string

	// MemorySlices is how many contiguous memory slices the profile occupies. It does
	// not always equal the compute count in the name — 3g.40gb takes four.
	MemorySlices int

	// SMSlices is how many of the GPU's seven compute slices it takes.
	SMSlices int
}

// geometries covers the two GPU families whose profile tables are published in NVIDIA's
// MIG user guide and could therefore be verified.
//
// Deliberately not extended by guesswork to B200 or GB200: an invented profile table would
// produce a simulation that is confidently wrong, which is worse than one that refuses.
// PartitionsFor returns an error for anything not listed here.
var geometries = map[string]Geometry{
	"h100": {
		MemorySlices: 8,
		SMSlices:     7,
		Profiles: []Profile{
			{Name: "1g.10gb", MemorySlices: 1, SMSlices: 1},
			{Name: "1g.20gb", MemorySlices: 2, SMSlices: 1},
			{Name: "2g.20gb", MemorySlices: 2, SMSlices: 2},
			{Name: "3g.40gb", MemorySlices: 4, SMSlices: 3},
			{Name: "4g.40gb", MemorySlices: 4, SMSlices: 4},
			{Name: "7g.80gb", MemorySlices: 8, SMSlices: 7},
		},
	},
	"a100": {
		MemorySlices: 8,
		SMSlices:     7,
		Profiles: []Profile{
			{Name: "1g.5gb", MemorySlices: 1, SMSlices: 1},
			{Name: "1g.10gb", MemorySlices: 2, SMSlices: 1},
			{Name: "2g.10gb", MemorySlices: 2, SMSlices: 2},
			{Name: "3g.20gb", MemorySlices: 4, SMSlices: 3},
			{Name: "4g.20gb", MemorySlices: 4, SMSlices: 4},
			{Name: "7g.40gb", MemorySlices: 8, SMSlices: 7},
		},
	},
}

// GeometryFor returns the MIG layout of a GPU profile, by the name a topology uses.
func GeometryFor(gpuProfile string) (Geometry, error) {
	g, ok := geometries[strings.ToLower(gpuProfile)]
	if !ok {
		return Geometry{}, fmt.Errorf(
			"MIG is not modelled for GPU profile %q; gpu-sim knows %s, whose profile tables NVIDIA publishes",
			gpuProfile, strings.Join(SupportedProfiles(), " and "))
	}
	return g, nil
}

// SupportedProfiles lists the GPU profiles MIG can be simulated for.
func SupportedProfiles() []string {
	names := make([]string, 0, len(geometries))
	for name := range geometries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Lookup finds a profile by name.
func (g Geometry) Lookup(name string) (Profile, bool) {
	for _, p := range g.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// Placements lists the memory-slice offsets a profile may start at.
//
// A partition occupies a contiguous, aligned run, so the valid starts are the multiples of
// its size. NVIDIA does not publish placement indices, and `nvidia-smi mig -lgip` reports
// seven placements for the smallest profile rather than eight; gpu-sim offers eight and lets
// the SM counter cap concurrent instances at seven. The observable behaviour — how many fit
// and which combinations conflict — is identical either way, and MaxInstances below is
// checked against NVIDIA's published table. Only the identity of a placement index differs.
func (g Geometry) Placements(p Profile) []int {
	var starts []int
	for start := 0; start+p.MemorySlices <= g.MemorySlices; start += p.MemorySlices {
		starts = append(starts, start)
	}
	return starts
}

// MaxInstances is how many instances of a profile fit on an idle GPU.
//
// Both dimensions bind, in different rows of NVIDIA's table: 1g.10gb is limited to 7 by the
// SM count even though 8 memory slices exist, while 1g.20gb is limited to 4 by memory even
// though 7 SMs remain. Reproducing every row is what validates the model, and the unit test
// asserts exactly that.
func (g Geometry) MaxInstances(p Profile) int {
	byMemory := g.MemorySlices / p.MemorySlices
	bySM := g.SMSlices / p.SMSlices
	if byMemory < bySM {
		return byMemory
	}
	return bySM
}

// TotalPartitions is how many (profile, placement) devices a GPU publishes. It drives the
// object-size budget: a ResourceSlice holds at most 64 devices once any of them consumes
// counters, so a node needs one counter slice plus enough device slices to hold
// gpuCount × TotalPartitions.
func (g Geometry) TotalPartitions(profiles []Profile) int {
	total := 0
	for _, p := range profiles {
		total += len(g.Placements(p))
	}
	return total
}

// SelectProfiles narrows a geometry to the named profiles, preserving declaration order.
// An empty list means every profile the GPU supports.
func (g Geometry) SelectProfiles(names []string) ([]Profile, error) {
	if len(names) == 0 {
		return g.Profiles, nil
	}
	out := make([]Profile, 0, len(names))
	for _, name := range names {
		p, ok := g.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("unknown MIG profile %q; this GPU supports %s",
				name, strings.Join(g.ProfileNames(), ", "))
		}
		out = append(out, p)
	}
	return out, nil
}

// ProfileNames lists the profile names, for error messages.
func (g Geometry) ProfileNames() []string {
	names := make([]string, 0, len(g.Profiles))
	for _, p := range g.Profiles {
		names = append(names, p.Name)
	}
	return names
}
