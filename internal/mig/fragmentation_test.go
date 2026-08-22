package mig

import "testing"

func h100(t *testing.T) Geometry {
	t.Helper()
	g, err := GeometryFor("h100")
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// allocate places one instance of a profile at a memory offset.
func allocate(t *testing.T, g Geometry, profile string, start int) Partition {
	t.Helper()
	p, ok := g.Lookup(profile)
	if !ok {
		t.Fatalf("no profile %q", profile)
	}
	return Partition{Profile: p, Start: start}
}

// TestScatteredSmallPartitions is the worked example from docs/designs/mig-model.md, and
// the case the whole phase exists to surface.
//
// Four 1g.10gb partitions at offsets 0, 2, 4 and 6 — a plausible outcome of four small
// inference jobs arriving one at a time. Half the GPU's memory is unused and nothing larger
// than a 1g.10gb can be placed on it. The memory is not in short supply; it is in the wrong
// places.
//
// Every number here was checked by hand before being written down. The first draft of the
// table in the design document had a row wrong, which is precisely why the metric is defined
// arithmetically rather than by feel.
func TestScatteredSmallPartitions(t *testing.T) {
	g := h100(t)
	state := StateFromAllocated([]Partition{
		allocate(t, g, "1g.10gb", 0),
		allocate(t, g, "1g.10gb", 2),
		allocate(t, g, "1g.10gb", 4),
		allocate(t, g, "1g.10gb", 6),
	})

	report := Analyse(g, g.Profiles, 0, state)

	if report.FreeMemorySlices != 4 {
		t.Errorf("free memory slices = %d, want 4", report.FreeMemorySlices)
	}
	if report.FreeSMSlices != 3 {
		t.Errorf("free SM slices = %d, want 3", report.FreeSMSlices)
	}
	if report.LargestAllocatable != "1g.10gb" {
		t.Errorf("largest allocatable = %q, want \"1g.10gb\" — half the GPU is free and nothing bigger fits",
			report.LargestAllocatable)
	}

	want := map[string]ProfileFit{
		"1g.10gb": {Ideal: 3, Actual: 3, Lost: 0},
		"1g.20gb": {Ideal: 2, Actual: 0, Lost: 2},
		"2g.20gb": {Ideal: 1, Actual: 0, Lost: 1},
		"3g.40gb": {Ideal: 1, Actual: 0, Lost: 1},
		"4g.40gb": {Ideal: 0, Actual: 0, Lost: 0},
		"7g.80gb": {Ideal: 0, Actual: 0, Lost: 0},
	}

	for _, got := range report.Profiles {
		w := want[got.Profile]
		if got.Ideal != w.Ideal || got.Actual != w.Actual || got.Lost != w.Lost {
			t.Errorf("%s: ideal=%d actual=%d lost=%d, want ideal=%d actual=%d lost=%d",
				got.Profile, got.Ideal, got.Actual, got.Lost, w.Ideal, w.Actual, w.Lost)
		}
	}

	if got := report.TotalLost(); got != 4 {
		t.Errorf("total lost = %d, want 4", got)
	}
}

// TestPackedSmallPartitionsWasteNothing is the same four partitions, packed instead of
// scattered. Identical GPU usage, no fragmentation — which is the point: fragmentation is
// caused by *where* the allocations landed, not how much they took.
func TestPackedSmallPartitionsWasteNothing(t *testing.T) {
	g := h100(t)
	state := StateFromAllocated([]Partition{
		allocate(t, g, "1g.10gb", 0),
		allocate(t, g, "1g.10gb", 1),
		allocate(t, g, "1g.10gb", 2),
		allocate(t, g, "1g.10gb", 3),
	})

	report := Analyse(g, g.Profiles, 0, state)

	if report.TotalLost() != 0 {
		t.Errorf("total lost = %d, want 0: the same four partitions packed waste nothing",
			report.TotalLost())
	}
	if report.LargestAllocatable != "3g.40gb" {
		t.Errorf("largest allocatable = %q, want \"3g.40gb\"", report.LargestAllocatable)
	}
}

func TestIdleGPUHasNoFragmentation(t *testing.T) {
	g := h100(t)
	report := Analyse(g, g.Profiles, 0, StateFromAllocated(nil))

	if report.TotalLost() != 0 {
		t.Errorf("an idle GPU reports %d lost partitions, want 0", report.TotalLost())
	}
	if report.LargestAllocatable != "7g.80gb" {
		t.Errorf("largest allocatable = %q, want the whole GPU", report.LargestAllocatable)
	}
	if report.FreeMemorySlices != 8 || report.FreeSMSlices != 7 {
		t.Errorf("idle GPU has %d/%d free, want 8/7", report.FreeMemorySlices, report.FreeSMSlices)
	}
}

func TestFullGPUAllocatesNothing(t *testing.T) {
	g := h100(t)
	state := StateFromAllocated([]Partition{allocate(t, g, "7g.80gb", 0)})

	report := Analyse(g, g.Profiles, 0, state)

	if report.LargestAllocatable != "" {
		t.Errorf("largest allocatable = %q, want nothing", report.LargestAllocatable)
	}
	if report.FreeMemorySlices != 0 {
		t.Errorf("free memory slices = %d, want 0", report.FreeMemorySlices)
	}
}

// TestSMSlicesBindBeforeMemory covers the dimension that is easy to forget. A single
// 4g.40gb takes half the memory but four of seven SMs, so what remains is limited by
// compute rather than by space.
func TestSMSlicesBindBeforeMemory(t *testing.T) {
	g := h100(t)
	state := StateFromAllocated([]Partition{allocate(t, g, "4g.40gb", 0)})

	report := Analyse(g, g.Profiles, 0, state)

	if report.FreeMemorySlices != 4 {
		t.Errorf("free memory slices = %d, want 4", report.FreeMemorySlices)
	}
	if report.FreeSMSlices != 3 {
		t.Errorf("free SM slices = %d, want 3", report.FreeSMSlices)
	}
	// Four memory slices remain, contiguous and aligned, so a 3g.40gb (4 slices, 3 SMs)
	// still fits — but a 4g.40gb does not, for want of a fourth SM.
	for _, fit := range report.Profiles {
		switch fit.Profile {
		case "3g.40gb":
			if fit.Actual != 1 {
				t.Errorf("3g.40gb actual = %d, want 1", fit.Actual)
			}
		case "4g.40gb":
			if fit.Actual != 0 {
				t.Errorf("4g.40gb actual = %d, want 0: only 3 SM slices remain", fit.Actual)
			}
		}
	}
}

func TestOverlapsDetectsCompetingPartitions(t *testing.T) {
	g := h100(t)
	big := allocate(t, g, "3g.40gb", 0)    // slices 0-3
	inside := allocate(t, g, "1g.10gb", 2) // slice 2
	outside := allocate(t, g, "1g.10gb", 5)

	if !big.Overlaps(inside) || !inside.Overlaps(big) {
		t.Error("overlapping partitions were not detected")
	}
	if big.Overlaps(outside) {
		t.Error("disjoint partitions were reported as overlapping")
	}

	onAnotherGPU := outside
	onAnotherGPU.GPUIndex = 1
	if outside.Overlaps(onAnotherGPU) {
		t.Error("partitions on different GPUs were reported as overlapping")
	}
}
