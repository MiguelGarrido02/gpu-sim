package mig

// Fragmentation is memory that is free but unreachable: a GPU can hold plenty of unused
// capacity and still refuse a partition, because the free slices are in the wrong places.
//
// It is always relative to a profile. There is no single number for "how fragmented is this
// GPU" that means anything on its own — a GPU can be perfectly usable for small partitions
// and useless for large ones at the same instant.

// State is what a GPU currently has allocated.
type State struct {
	// UsedMemorySlices holds the occupied positions. Positions matter: two free slices at
	// 1 and 3 hold no 2-slice partition, while the same two at 2 and 3 hold one.
	UsedMemorySlices map[int]bool

	// UsedSMSlices is a count, since compute slices are interchangeable.
	UsedSMSlices int
}

// StateFromAllocated builds the state implied by a set of allocated partitions.
func StateFromAllocated(allocated []Partition) State {
	state := State{UsedMemorySlices: map[int]bool{}}
	for _, p := range allocated {
		for _, slice := range p.MemorySlices() {
			state.UsedMemorySlices[slice] = true
		}
		state.UsedSMSlices += p.Profile.SMSlices
	}
	return state
}

// ProfileFit is how a single profile fares against a GPU's current state.
type ProfileFit struct {
	Profile string `json:"profile"`

	// Ideal is how many instances the free capacity would hold if the free slices could
	// be rearranged into one contiguous run.
	Ideal int `json:"ideal"`

	// Actual is how many fit where the free slices actually sit.
	Actual int `json:"actual"`

	// Lost is Ideal minus Actual: whole partitions a perfect defragmentation would
	// recover. This is the fragmentation, in the only unit that means anything.
	Lost int `json:"lost"`
}

// Report is one GPU's fragmentation picture.
type Report struct {
	GPUIndex         int `json:"gpuIndex"`
	FreeMemorySlices int `json:"freeMemorySlices"`
	FreeSMSlices     int `json:"freeSMSlices"`

	// LargestAllocatable is the biggest profile the GPU can still hand out, or empty if
	// it can hand out nothing. It compresses the table into the thing an operator asks.
	LargestAllocatable string `json:"largestAllocatable"`

	Profiles []ProfileFit `json:"profiles"`
}

// TotalLost sums the fragmentation across profiles. Useful as a single trend line over a
// run, but the per-profile table is what explains it.
func (r Report) TotalLost() int {
	total := 0
	for _, p := range r.Profiles {
		total += p.Lost
	}
	return total
}

// Analyse computes the fragmentation of one GPU.
func Analyse(g Geometry, profiles []Profile, gpuIndex int, state State) Report {
	freeMemory := g.MemorySlices - len(state.UsedMemorySlices)
	freeSM := g.SMSlices - state.UsedSMSlices

	report := Report{
		GPUIndex:         gpuIndex,
		FreeMemorySlices: freeMemory,
		FreeSMSlices:     freeSM,
	}

	// Profiles are ordered smallest-first in the geometry, so tracking the last one that
	// fits leaves the largest.
	for _, profile := range profiles {
		fit := ProfileFit{Profile: profile.Name}

		fit.Ideal = min(freeMemory/profile.MemorySlices, freeSM/profile.SMSlices)

		free := 0
		for _, start := range g.Placements(profile) {
			if placementIsFree(state, start, profile.MemorySlices) {
				free++
			}
		}
		fit.Actual = min(free, freeSM/profile.SMSlices)

		fit.Lost = fit.Ideal - fit.Actual
		report.Profiles = append(report.Profiles, fit)

		if fit.Actual > 0 {
			report.LargestAllocatable = profile.Name
		}
	}

	return report
}

func placementIsFree(state State, start, size int) bool {
	for i := start; i < start+size; i++ {
		if state.UsedMemorySlices[i] {
			return false
		}
	}
	return true
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
