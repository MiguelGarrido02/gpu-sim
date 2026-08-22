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

func TestUnknownGPUProfileIsRefused(t *testing.T) {
	_, err := GeometryFor("b200")
	if err == nil {
		t.Fatal("GeometryFor accepted a GPU whose MIG table has not been verified")
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
