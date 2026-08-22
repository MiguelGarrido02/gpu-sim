// Package profile reads the GPU profile documents fake-gpu-operator publishes, which it
// syncs from NVIDIA/k8s-test-infra.
//
// The profiles carry per-device PCIe bus IDs and a PCIe root complex map annotated with
// NUMA nodes — the same facts NVIDIA's real DRA driver publishes as the `pciBusID`,
// `pcieRoot` and `numaNode` device attributes. fake-gpu-operator loads them and then
// discards them, because its own topology model has nowhere to put per-GPU data.
//
// Reading them here means gpu-sim's PCIe and NUMA attributes are NVIDIA's own data rather
// than something plausible we made up.
package profile

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

// Profile is the subset of a GPU profile document gpu-sim consumes. Profiles carry a great
// deal more (clocks, power, ECC, temperatures) that only the simulated nvidia-smi needs.
type Profile struct {
	System         System         `json:"system"`
	DeviceDefaults DeviceDefaults `json:"device_defaults"`
	Devices        []Device       `json:"devices"`
	PCIeTopology   PCIeTopology   `json:"pcie_topology"`
}

type System struct {
	DriverVersion string `json:"driver_version"`
	CUDAVersion   string `json:"cuda_version"`
}

type DeviceDefaults struct {
	Name         string `json:"name"`
	Brand        string `json:"brand"`
	Architecture string `json:"architecture"`
}

type Device struct {
	Index int    `json:"index"`
	UUID  string `json:"uuid"`
	PCI   PCI    `json:"pci"`
}

type PCI struct {
	BusID string `json:"bus_id"`
}

type PCIeTopology struct {
	RootComplexes []RootComplex `json:"root_complexes"`
}

type RootComplex struct {
	ID       string   `json:"id"`
	NUMANode int      `json:"numa_node"`
	Devices  []string `json:"devices"`
}

// GPUFacts are the per-GPU hardware details gpu-sim publishes as DRA device attributes,
// under the names NVIDIA's real driver uses.
type GPUFacts struct {
	PCIBusID string
	PCIeRoot string
	NUMANode int
}

// Parse reads a profile document. Parsing is lenient: profiles carry many fields gpu-sim
// does not model, and new ones appear as upstream syncs from NVIDIA.
func Parse(data []byte) (*Profile, error) {
	var p Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing GPU profile: %w", err)
	}
	if len(p.Devices) == 0 {
		return nil, fmt.Errorf("GPU profile declares no devices")
	}
	return &p, nil
}

// GPU returns the hardware facts for the GPU at the given index.
//
// A pool may declare more GPUs per node than the profile has device entries — an eight-GPU
// H100 profile used for a sixteen-GPU node, say. That is not a machine anyone builds, but
// it is a legitimate thing to ask a simulator for, so indices past the end of the profile
// wrap around. The PCIe and NUMA distribution stays balanced; it just repeats.
func (p *Profile) GPU(index int) (GPUFacts, error) {
	if index < 0 {
		return GPUFacts{}, fmt.Errorf("GPU index %d is negative", index)
	}

	device := p.Devices[index%len(p.Devices)]
	facts := GPUFacts{PCIBusID: device.PCI.BusID}

	root, found := p.rootComplexFor(device.PCI.BusID)
	if !found {
		// Not fatal: a profile may omit pcie_topology entirely, in which case the
		// attributes are simply not published rather than published as zero, which
		// would claim every GPU sits on NUMA node 0.
		return facts, nil
	}
	facts.PCIeRoot = root.ID
	facts.NUMANode = root.NUMANode

	return facts, nil
}

// HasPCIeTopology reports whether the profile carries a PCIe root complex map. Callers use
// it to distinguish "NUMA node 0" from "no NUMA information".
func (p *Profile) HasPCIeTopology() bool {
	return len(p.PCIeTopology.RootComplexes) > 0
}

func (p *Profile) rootComplexFor(busID string) (RootComplex, bool) {
	for _, root := range p.PCIeTopology.RootComplexes {
		for _, device := range root.Devices {
			if device == busID {
				return root, true
			}
		}
	}
	return RootComplex{}, false
}
