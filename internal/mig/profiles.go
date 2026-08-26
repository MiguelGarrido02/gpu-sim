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

// datacenterShape is the partition layout every MIG-capable datacenter GPU has presented
// from Ampere through Blackwell: 8 memory slices, 7 SM slices, and these six profiles in
// this order.
//
// Reading NVIDIA's tables for A100, H100, H200 and B200 side by side, the slice columns are
// identical and only the memory labels move. Writing the shape once says so in code, and
// makes a mistyped slice count impossible to introduce for one model alone.
var datacenterShape = []Profile{
	{MemorySlices: 1, SMSlices: 1},
	{MemorySlices: 2, SMSlices: 1},
	{MemorySlices: 2, SMSlices: 2},
	{MemorySlices: 4, SMSlices: 3},
	{MemorySlices: 4, SMSlices: 4},
	{MemorySlices: 8, SMSlices: 7},
}

// datacenterGeometry names the shared shape for one GPU model.
//
// The names are transcribed from NVIDIA's table rather than derived from memory, because
// they are not derivable: a B200 memory slice is 22.5 GB and NVIDIA prints it `23gb`, while
// an H200 slice is 17.6 GB and prints `18gb`. Rounding that by hand from a capacity would be
// guessing at a published string.
func datacenterGeometry(names ...string) Geometry {
	if len(names) != len(datacenterShape) {
		panic(fmt.Sprintf("datacenterGeometry: got %d names, want %d", len(names), len(datacenterShape)))
	}
	g := Geometry{MemorySlices: 8, SMSlices: 7}
	for i, name := range names {
		p := datacenterShape[i]
		p.Name = name
		g.Profiles = append(g.Profiles, p)
	}
	return g
}

// geometries covers the GPU models whose profile tables are published in NVIDIA's MIG user
// guide and could therefore be verified against it.
//
// Still deliberately absent, and refused by name rather than guessed at: GB300 and B300,
// whose tables NVIDIA does not publish — the figures circulating in secondary sources
// disagree with each other on both memory per instance and the total. GB200 is absent for a
// softer reason: its GPUs are B200 dies, so the B200 table is very likely right, but
// "very likely" is how a simulation ends up confidently wrong.
//
// The models here are all 8/7 datacenter parts. A30 and the RTX PRO Blackwell cards have
// genuinely different geometries (4/4 and 2/2) and would need their own shapes; none of them
// is a profile fake-gpu-operator publishes. L40S and T4 do not support MIG at all.
//
// Each model is pinned to NVIDIA's max-instances column by TestMaxInstancesMatchesNVIDIA.
var geometries = map[string]Geometry{
	// A100-SXM4-40GB, which is the A100 variant fake-gpu-operator's profile declares.
	"a100": datacenterGeometry("1g.5gb", "1g.10gb", "2g.10gb", "3g.20gb", "4g.20gb", "7g.40gb"),

	// H100 80GB, PCIe and SXM5 alike. The 94GB and 96GB variants use different labels.
	"h100": datacenterGeometry("1g.10gb", "1g.20gb", "2g.20gb", "3g.40gb", "4g.40gb", "7g.80gb"),

	// H200 141GB. Reachable through the operator's customProfiles rather than a builtin.
	"h200": datacenterGeometry("1g.18gb", "1g.35gb", "2g.35gb", "3g.71gb", "4g.71gb", "7g.141gb"),

	// B200 180GB, the production HGX/DGX specification. See docs/designs/mig-model.md for
	// why this disagrees with the 192 GiB the upstream b200 profile declares.
	"b200": datacenterGeometry("1g.23gb", "1g.45gb", "2g.45gb", "3g.90gb", "4g.90gb", "7g.180gb"),
}

// GeometryFor returns the MIG layout of a GPU profile, by the name a topology uses.
func GeometryFor(gpuProfile string) (Geometry, error) {
	g, ok := geometries[strings.ToLower(gpuProfile)]
	if !ok {
		return Geometry{}, fmt.Errorf(
			"MIG is not modelled for GPU profile %q; gpu-sim knows %s, whose profile tables NVIDIA publishes",
			gpuProfile, strings.Join(SupportedProfiles(), ", "))
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
