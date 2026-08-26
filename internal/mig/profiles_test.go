package mig

import "testing"

// TestMaxInstancesMatchesNVIDIA pins the model against the max-instances column of NVIDIA's
// published profile table:
//
//	https://docs.nvidia.com/datacenter/tesla/mig-user-guide/supported-mig-profiles.html
//
// This is the model's correctness check, not an example to be regenerated when it fails.
// Modelling memory slices as positional counters and SMs as a pool of seven has to
// reproduce every row; if it does not, the geometry is wrong and any fragmentation number
// computed from it is meaningless.
//
// Both dimensions bind, in different rows, which is why both are needed: 1g is capped at 7
// by SMs despite 8 memory slices, and 1g.20gb at 4 by memory despite 7 SMs remaining.
//
// The max-instances column is also the only trustworthy one. NVIDIA's "Fraction of Memory"
// column prints 1/8 for A100 1g.10gb, which cannot be right when the same row allows four
// instances of it — the profile takes two of the eight slices. Every slice count below is
// therefore derived from the instance count, not from the fraction.
func TestMaxInstancesMatchesNVIDIA(t *testing.T) {
	tests := []struct {
		gpu     string
		profile string
		want    int
	}{
		{"h100", "1g.10gb", 7},
		{"h100", "1g.20gb", 4},
		{"h100", "2g.20gb", 3},
		{"h100", "3g.40gb", 2},
		{"h100", "4g.40gb", 1},
		{"h100", "7g.80gb", 1},

		{"a100", "1g.5gb", 7},
		{"a100", "1g.10gb", 4},
		{"a100", "2g.10gb", 3},
		{"a100", "3g.20gb", 2},
		{"a100", "4g.20gb", 1},
		{"a100", "7g.40gb", 1},

		{"h200", "1g.18gb", 7},
		{"h200", "1g.35gb", 4},
		{"h200", "2g.35gb", 3},
		{"h200", "3g.71gb", 2},
		{"h200", "4g.71gb", 1},
		{"h200", "7g.141gb", 1},

		{"b200", "1g.23gb", 7},
		{"b200", "1g.45gb", 4},
		{"b200", "2g.45gb", 3},
		{"b200", "3g.90gb", 2},
		{"b200", "4g.90gb", 1},
		{"b200", "7g.180gb", 1},
	}

	for _, tt := range tests {
		g, err := GeometryFor(tt.gpu)
		if err != nil {
			t.Fatalf("GeometryFor(%q): %v", tt.gpu, err)
		}
		p, ok := g.Lookup(tt.profile)
		if !ok {
			t.Errorf("%s has no profile %q", tt.gpu, tt.profile)
			continue
		}
		if got := g.MaxInstances(p); got != tt.want {
			t.Errorf("%s %s: max instances = %d, want %d (NVIDIA's published table)",
				tt.gpu, tt.profile, got, tt.want)
		}
	}
}

func TestPlacementsAreAligned(t *testing.T) {
	g, err := GeometryFor("h100")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		profile string
		want    []int
	}{
		{"1g.10gb", []int{0, 1, 2, 3, 4, 5, 6, 7}},
		{"2g.20gb", []int{0, 2, 4, 6}},
		{"3g.40gb", []int{0, 4}},
		{"7g.80gb", []int{0}},
	}

	for _, tt := range tests {
		p, _ := g.Lookup(tt.profile)
		got := g.Placements(p)
		if len(got) != len(tt.want) {
			t.Errorf("%s: got %v placements, want %v", tt.profile, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("%s: placements = %v, want %v", tt.profile, got, tt.want)
				break
			}
		}
	}
}

// TestTotalPartitionsDrivesTheSliceBudget records the number the publishing code has to
// budget for. A ResourceSlice holds at most 64 devices once any of them consumes counters,
// so an 8-GPU node needs one counter slice plus three device slices.
func TestTotalPartitionsDrivesTheSliceBudget(t *testing.T) {
	g, err := GeometryFor("h100")
	if err != nil {
		t.Fatal(err)
	}

	const wantPerGPU = 21
	if got := g.TotalPartitions(g.Profiles); got != wantPerGPU {
		t.Errorf("partitions per GPU = %d, want %d", got, wantPerGPU)
	}

	const devicesPerSlice = 64
	perNode := wantPerGPU * 8
	slices := (perNode + devicesPerSlice - 1) / devicesPerSlice
	if slices != 3 {
		t.Errorf("an 8-GPU node needs %d device slices, want 3", slices)
	}
}

// TestUnknownGPUProfileIsRefused guards the rule that a GPU whose MIG table NVIDIA does not
// publish is refused rather than approximated.
//
// gb300 and l40s are refused for different reasons and both matter: GB300's table is simply
// unpublished — the figures in secondary sources contradict each other — while L40S is an
// Ada part that has no MIG support to model at all. A future geometry added for either
// would have to be invented, and an invented table produces a simulation that is
// confidently wrong.
func TestUnknownGPUProfileIsRefused(t *testing.T) {
	for _, gpu := range []string{"gb300", "gb200", "l40s", "t4"} {
		if _, err := GeometryFor(gpu); err == nil {
			t.Errorf("GeometryFor(%q) accepted a GPU whose MIG table has not been verified", gpu)
		}
	}
}

// TestDatacenterModelsShareOneShape pins the finding that motivated adding H200 and B200:
// the datacenter MIG geometry has not changed from Ampere to Blackwell. Every model here
// presents the same 8 memory and 7 SM slices and cuts them the same six ways; only the
// memory labels differ.
//
// If a model is ever added whose slice shape genuinely differs — A30 and the RTX PRO
// Blackwell cards do — it belongs in its own geometry rather than in datacenterShape, and
// this test is what will say so.
func TestDatacenterModelsShareOneShape(t *testing.T) {
	reference, err := GeometryFor("h100")
	if err != nil {
		t.Fatal(err)
	}

	for _, gpu := range []string{"a100", "h200", "b200"} {
		g, err := GeometryFor(gpu)
		if err != nil {
			t.Fatalf("GeometryFor(%q): %v", gpu, err)
		}
		if g.MemorySlices != reference.MemorySlices || g.SMSlices != reference.SMSlices {
			t.Errorf("%s: %d/%d slices, want %d/%d",
				gpu, g.MemorySlices, g.SMSlices, reference.MemorySlices, reference.SMSlices)
		}
		if len(g.Profiles) != len(reference.Profiles) {
			t.Fatalf("%s: %d profiles, want %d", gpu, len(g.Profiles), len(reference.Profiles))
		}
		for i, p := range g.Profiles {
			want := reference.Profiles[i]
			if p.MemorySlices != want.MemorySlices || p.SMSlices != want.SMSlices {
				t.Errorf("%s %s: %d memory / %d SM slices, want %d / %d (same shape as %s)",
					gpu, p.Name, p.MemorySlices, p.SMSlices, want.MemorySlices, want.SMSlices, want.Name)
			}
			if p.Name == want.Name {
				t.Errorf("%s %s: shares a name with H100, which means a label was not transcribed", gpu, p.Name)
			}
		}
	}
}

func TestSelectProfiles(t *testing.T) {
	g, _ := GeometryFor("h100")

	all, err := g.SelectProfiles(nil)
	if err != nil || len(all) != len(g.Profiles) {
		t.Errorf("an empty selection should mean every profile, got %d", len(all))
	}

	some, err := g.SelectProfiles([]string{"1g.10gb", "3g.40gb"})
	if err != nil {
		t.Fatalf("SelectProfiles: %v", err)
	}
	if len(some) != 2 || some[0].Name != "1g.10gb" || some[1].Name != "3g.40gb" {
		t.Errorf("selection = %v, want the two named profiles in order", some)
	}

	if _, err := g.SelectProfiles([]string{"9g.99gb"}); err == nil {
		t.Error("SelectProfiles accepted an unknown profile")
	}
}
