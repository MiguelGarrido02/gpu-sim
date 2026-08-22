package topology

import (
	"fmt"

	"github.com/MiguelGarrido02/gpu-sim/internal/gpuid"
	"github.com/MiguelGarrido02/gpu-sim/internal/profile"
)

// Resolved is a ClusterTopology expanded into concrete per-node and per-GPU facts, with
// the hardware details filled in from the GPU profiles.
//
// Generators consume this rather than ClusterTopology directly, so that the rules for
// deriving a GPU's NVLink domain or peer count are applied once, here, instead of being
// re-implemented — and diverging — in each of the three projections.
type Resolved struct {
	// Name comes from the document's metadata and names the generated scheduler
	// topology object.
	Name  string
	Nodes []ResolvedNode
}

type ResolvedNode struct {
	Name string
	Pool string

	// ProductName and Architecture come from the GPU profile.
	ProductName  string
	Architecture string

	Rack        string
	FaultDomain string

	// NVLinkDomain is the domain every GPU on this node belongs to: the rack's domain
	// on GB200-class hardware, the node's own name on DGX-class hardware, and empty when
	// the pool has no NVLink at all.
	NVLinkDomain string

	GPUs []ResolvedGPU
}

type ResolvedGPU struct {
	Index      int
	UUID       string
	DeviceName string

	// PCIBusID, PCIeRoot and NUMANode carry NVIDIA's own profile data. HasNUMA
	// distinguishes "NUMA node 0" from "the profile said nothing", which matters because
	// publishing an invented zero would claim every GPU shares a socket.
	PCIBusID string
	PCIeRoot string
	NUMANode int
	HasNUMA  bool

	NVLinkDomain string
	NVLinkPeers  int
	FaultDomain  string
}

// ProfileLoader resolves a profile name to its parsed document.
type ProfileLoader func(name string) (*profile.Profile, error)

// Resolve expands the topology, loading each referenced GPU profile once.
func (ct *ClusterTopology) Resolve(load ProfileLoader) (*Resolved, error) {
	profiles := map[string]*profile.Profile{}
	for _, pool := range ct.Spec.NodePools {
		if _, done := profiles[pool.Profile]; done {
			continue
		}
		p, err := load(pool.Profile)
		if err != nil {
			return nil, fmt.Errorf("loading profile %q: %w", pool.Profile, err)
		}
		profiles[pool.Profile] = p
	}

	resolved := &Resolved{Name: ct.Metadata.Name}

	for _, rack := range ct.Spec.Racks {
		rackGPUs := ct.gpusInRack(rack)

		for _, node := range rack.Nodes {
			pool := ct.Spec.NodePools[node.Pool]
			prof := profiles[pool.Profile]

			domain, peers := nvlinkFor(pool, rack, node.Name, rackGPUs)

			rn := ResolvedNode{
				Name:         node.Name,
				Pool:         node.Pool,
				ProductName:  prof.DeviceDefaults.Name,
				Architecture: prof.DeviceDefaults.Architecture,
				Rack:         rack.Name,
				FaultDomain:  rack.FaultDomain,
				NVLinkDomain: domain,
			}

			for i := 0; i < pool.GPUCount; i++ {
				facts, err := prof.GPU(i)
				if err != nil {
					return nil, fmt.Errorf("node %s GPU %d: %w", node.Name, i, err)
				}
				rn.GPUs = append(rn.GPUs, ResolvedGPU{
					Index:        i,
					UUID:         gpuid.DeviceUUID(node.Name, i),
					DeviceName:   gpuid.DeviceName(node.Name, i),
					PCIBusID:     facts.PCIBusID,
					PCIeRoot:     facts.PCIeRoot,
					NUMANode:     facts.NUMANode,
					HasNUMA:      prof.HasPCIeTopology() && facts.PCIeRoot != "",
					NVLinkDomain: domain,
					NVLinkPeers:  peers,
					FaultDomain:  rack.FaultDomain,
				})
			}

			resolved.Nodes = append(resolved.Nodes, rn)
		}
	}

	return resolved, nil
}

// nvlinkFor decides which NVLink domain a node's GPUs belong to, and how many peers each
// one can reach over NVLink.
//
// Peer count is what lets a selector tell a full mesh from an isolated GPU without
// enumerating every peer, which would put an O(n²) blob in each ResourceSlice for no gain.
func nvlinkFor(pool NodePool, rack Rack, nodeName string, rackGPUs int) (domain string, peers int) {
	if pool.NVLink == NVLinkNone {
		return "", 0
	}
	if rack.NVLinkDomain != "" {
		// Multi-node NVLink: the domain spans the rack, so a GPU reaches every other
		// GPU in the rack.
		return rack.NVLinkDomain, rackGPUs - 1
	}
	// NVLink stops at the node boundary, so each node forms its own domain, named after
	// itself. Naming it after the node keeps domain identifiers unique cluster-wide
	// without a separate numbering scheme to keep in sync.
	return nodeName, pool.GPUCount - 1
}

func (ct *ClusterTopology) gpusInRack(rack Rack) int {
	total := 0
	for _, node := range rack.Nodes {
		total += ct.Spec.NodePools[node.Pool].GPUCount
	}
	return total
}

// validateNVLinkConsistency rejects a rack that claims a multi-node NVLink domain while
// containing nodes whose pool has no NVLink at all. Silently picking one over the other
// would produce a topology that looks fine and describes hardware that cannot exist.
func (ct *ClusterTopology) validateNVLinkConsistency() []error {
	var errs []error
	for _, rack := range ct.Spec.Racks {
		if rack.NVLinkDomain == "" {
			continue
		}
		for _, node := range rack.Nodes {
			pool, ok := ct.Spec.NodePools[node.Pool]
			if !ok {
				continue // already reported by the pool reference check
			}
			if pool.NVLink == NVLinkNone {
				errs = append(errs, fmt.Errorf(
					"rack %q declares nvlinkDomain %q but node %q uses pool %q, which has nvlink: none",
					rack.Name, rack.NVLinkDomain, node.Name, node.Pool))
			}
		}
	}
	return errs
}
