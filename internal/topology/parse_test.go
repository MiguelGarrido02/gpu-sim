package topology

import (
	"strings"
	"testing"
)

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ClusterTopology)
		wantErr string
	}{
		{
			name:    "unknown pool reference",
			mutate:  func(ct *ClusterTopology) { ct.Spec.Racks[0].Nodes[0].Pool = "nope" },
			wantErr: "undefined pool",
		},
		{
			name: "duplicate node across racks",
			mutate: func(ct *ClusterTopology) {
				ct.Spec.Racks = append(ct.Spec.Racks, Rack{
					Name: "rack-2", FaultDomain: "fd-2",
					Nodes: []Node{{Name: "node-1", Pool: "dgx"}},
				})
			},
			wantErr: "declared in both",
		},
		{
			name:    "missing fault domain",
			mutate:  func(ct *ClusterTopology) { ct.Spec.Racks[0].FaultDomain = "" },
			wantErr: "no faultDomain",
		},
		{
			name: "unusable nvlink setting",
			mutate: func(ct *ClusterTopology) {
				ct.Spec.NodePools["dgx"] = NodePool{Profile: "h100", GPUCount: 8, NVLink: "mesh"}
			},
			wantErr: `nvlink "mesh"`,
		},
		{
			name: "rack NVLink domain over nodes without NVLink",
			mutate: func(ct *ClusterTopology) {
				ct.Spec.Racks[0].NVLinkDomain = "nvl72-1"
				ct.Spec.NodePools["dgx"] = NodePool{Profile: "h100", GPUCount: 8, NVLink: NVLinkNone}
			},
			wantErr: "nvlink: none",
		},
		{
			name:    "wrong apiVersion",
			mutate:  func(ct *ClusterTopology) { ct.APIVersion = "v1" },
			wantErr: "apiVersion",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := dgxTopology(NVLinkFullMesh, "")
			tt.mutate(ct)

			err := ct.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid topology")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateAcceptsValidTopology(t *testing.T) {
	if err := dgxTopology(NVLinkFullMesh, "").Validate(); err != nil {
		t.Errorf("Validate rejected a valid topology: %v", err)
	}
}

// TestValidateReportsEveryProblem checks validation does not stop at the first error, so a
// hand-edited file can be fixed in one pass rather than one mistake per run.
func TestValidateReportsEveryProblem(t *testing.T) {
	ct := dgxTopology(NVLinkFullMesh, "")
	ct.Spec.Racks[0].FaultDomain = ""
	ct.Spec.Racks[0].Nodes[0].Pool = "nope"

	err := ct.Validate()
	if err == nil {
		t.Fatal("Validate accepted an invalid topology")
	}
	for _, want := range []string{"no faultDomain", "undefined pool"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
