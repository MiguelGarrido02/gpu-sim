package topology

import (
	"testing"

	"github.com/MiguelGarrido02/gpu-sim/internal/profile"
)

// hgxProfile mirrors the shape of the builtin h100 profile: eight devices split across two
// PCIe root complexes, one per NUMA node.
const hgxProfile = `
system:
  driver_version: "550.163.01"
  cuda_version: "12.4"
device_defaults:
  name: "NVIDIA H100 80GB HBM3"
  brand: "nvidia"
  architecture: "hopper"
devices:
  - {index: 0, pci: {bus_id: "0000:1A:00.0"}}
  - {index: 1, pci: {bus_id: "0000:1B:00.0"}}
  - {index: 2, pci: {bus_id: "0000:4A:00.0"}}
  - {index: 3, pci: {bus_id: "0000:4B:00.0"}}
  - {index: 4, pci: {bus_id: "0000:8A:00.0"}}
  - {index: 5, pci: {bus_id: "0000:8B:00.0"}}
  - {index: 6, pci: {bus_id: "0000:CA:00.0"}}
  - {index: 7, pci: {bus_id: "0000:CB:00.0"}}
pcie_topology:
  root_complexes:
    - id: "pci0000:00"
      numa_node: 0
      devices: ["0000:1A:00.0", "0000:1B:00.0", "0000:4A:00.0", "0000:4B:00.0"]
    - id: "pci0000:80"
      numa_node: 1
      devices: ["0000:8A:00.0", "0000:8B:00.0", "0000:CA:00.0", "0000:CB:00.0"]
`

func loaderFor(t *testing.T, doc string) ProfileLoader {
	t.Helper()
	p, err := profile.Parse([]byte(doc))
	if err != nil {
		t.Fatalf("parsing test profile: %v", err)
	}
	return func(string) (*profile.Profile, error) { return p, nil }
}

func dgxTopology(nvlink NVLinkTopology, rackDomain string) *ClusterTopology {
	return &ClusterTopology{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "test"},
		Spec: Spec{
			NodePools: map[string]NodePool{
				"dgx": {Profile: "h100", GPUCount: 8, NVLink: nvlink},
			},
			Racks: []Rack{{
				Name:         "rack-1",
				FaultDomain:  "fd-1",
				NVLinkDomain: rackDomain,
				Nodes: []Node{
					{Name: "node-1", Pool: "dgx"},
					{Name: "node-2", Pool: "dgx"},
				},
			}},
		},
	}
}

// TestResolveNodeScopedNVLink covers DGX-class hardware, where NVLink stops at the node
// boundary: each node is its own domain and a GPU reaches the other seven on its node.
func TestResolveNodeScopedNVLink(t *testing.T) {
	got, err := dgxTopology(NVLinkFullMesh, "").Resolve(loaderFor(t, hgxProfile))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(got.Nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(got.Nodes))
	}
	for _, node := range got.Nodes {
		if node.NVLinkDomain != node.Name {
			t.Errorf("node %s: NVLink domain is %q, want the node's own name",
				node.Name, node.NVLinkDomain)
		}
		if len(node.GPUs) != 8 {
			t.Fatalf("node %s: got %d GPUs, want 8", node.Name, len(node.GPUs))
		}
		for _, gpu := range node.GPUs {
			if gpu.NVLinkPeers != 7 {
				t.Errorf("node %s GPU %d: %d NVLink peers, want 7",
					node.Name, gpu.Index, gpu.NVLinkPeers)
			}
		}
	}

	// Domains must not collide across nodes, or a scheduler asked to keep a job inside
	// one domain would happily spread it over both.
	if got.Nodes[0].NVLinkDomain == got.Nodes[1].NVLinkDomain {
		t.Error("two nodes share an NVLink domain on node-scoped hardware")
	}
}

// TestResolveRackScopedNVLink covers GB200 NVL72-class hardware, where one NVLink domain
// spans the rack, so a GPU's peers are every other GPU in the rack rather than in its node.
func TestResolveRackScopedNVLink(t *testing.T) {
	got, err := dgxTopology(NVLinkFullMesh, "nvl72-1").Resolve(loaderFor(t, hgxProfile))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for _, node := range got.Nodes {
		if node.NVLinkDomain != "nvl72-1" {
			t.Errorf("node %s: NVLink domain is %q, want the rack's domain",
				node.Name, node.NVLinkDomain)
		}
		for _, gpu := range node.GPUs {
			// 2 nodes × 8 GPUs, minus the GPU itself.
			if gpu.NVLinkPeers != 15 {
				t.Errorf("node %s GPU %d: %d NVLink peers, want 15",
					node.Name, gpu.Index, gpu.NVLinkPeers)
			}
		}
	}
}

// TestResolveWithoutNVLink checks that a pool with no NVLink publishes no domain at all,
// rather than a domain of one, which a selector could not distinguish from real NVLink.
func TestResolveWithoutNVLink(t *testing.T) {
	got, err := dgxTopology(NVLinkNone, "").Resolve(loaderFor(t, hgxProfile))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	for _, node := range got.Nodes {
		if node.NVLinkDomain != "" {
			t.Errorf("node %s: NVLink domain is %q, want empty", node.Name, node.NVLinkDomain)
		}
		for _, gpu := range node.GPUs {
			if gpu.NVLinkPeers != 0 {
				t.Errorf("node %s GPU %d: %d NVLink peers, want 0", node.Name, gpu.Index, gpu.NVLinkPeers)
			}
		}
	}
}

// TestResolveUsesProfileNUMAAndPCIe checks the NUMA and PCIe attributes come from the
// profile's own root complex map, split 4/4 across two sockets as on real HGX hardware.
func TestResolveUsesProfileNUMAAndPCIe(t *testing.T) {
	got, err := dgxTopology(NVLinkFullMesh, "").Resolve(loaderFor(t, hgxProfile))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	perNUMA := map[int]int{}
	for _, gpu := range got.Nodes[0].GPUs {
		if !gpu.HasNUMA {
			t.Fatalf("GPU %d has no NUMA information despite a profile that declares it", gpu.Index)
		}
		if gpu.PCIeRoot == "" {
			t.Errorf("GPU %d has no PCIe root", gpu.Index)
		}
		if gpu.PCIBusID == "" {
			t.Errorf("GPU %d has no PCI bus ID", gpu.Index)
		}
		perNUMA[gpu.NUMANode]++
	}

	if perNUMA[0] != 4 || perNUMA[1] != 4 {
		t.Errorf("GPUs per NUMA node = %v, want 4 on each of nodes 0 and 1", perNUMA)
	}
}

// TestResolveGPUCountBelowProfileSize checks a pool may use fewer GPUs than the profile
// describes, which is how a smaller machine is modelled from a stock profile.
func TestResolveGPUCountBelowProfileSize(t *testing.T) {
	ct := dgxTopology(NVLinkFullMesh, "")
	pool := ct.Spec.NodePools["dgx"]
	pool.GPUCount = 4
	ct.Spec.NodePools["dgx"] = pool

	got, err := ct.Resolve(loaderFor(t, hgxProfile))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if n := len(got.Nodes[0].GPUs); n != 4 {
		t.Fatalf("got %d GPUs, want 4", n)
	}
	for _, gpu := range got.Nodes[0].GPUs {
		if gpu.NVLinkPeers != 3 {
			t.Errorf("GPU %d: %d peers, want 3", gpu.Index, gpu.NVLinkPeers)
		}
	}
}
