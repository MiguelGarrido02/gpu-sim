package topology

import (
	"errors"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Load reads and validates a topology document.
func Load(path string) (*ClusterTopology, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading topology: %w", err)
	}

	var ct ClusterTopology
	if err := yaml.UnmarshalStrict(data, &ct); err != nil {
		return nil, fmt.Errorf("parsing topology %s: %w", path, err)
	}

	if err := ct.Validate(); err != nil {
		return nil, fmt.Errorf("invalid topology %s: %w", path, err)
	}
	return &ct, nil
}

// Validate reports every problem it finds rather than stopping at the first, because a
// topology is usually edited by hand and fixing one mistake per run is tedious.
func (ct *ClusterTopology) Validate() error {
	var errs []error

	if ct.APIVersion != APIVersion {
		errs = append(errs, fmt.Errorf("apiVersion is %q, want %q", ct.APIVersion, APIVersion))
	}
	if ct.Kind != Kind {
		errs = append(errs, fmt.Errorf("kind is %q, want %q", ct.Kind, Kind))
	}
	if ct.Metadata.Name == "" {
		errs = append(errs, errors.New("metadata.name is required: it names the generated scheduler topology object"))
	}
	if len(ct.Spec.NodePools) == 0 {
		errs = append(errs, errors.New("spec.nodePools is empty"))
	}
	if len(ct.Spec.Racks) == 0 {
		errs = append(errs, errors.New("spec.racks is empty"))
	}

	for name, pool := range ct.Spec.NodePools {
		errs = append(errs, pool.validate(name)...)
	}

	// Node names become Kubernetes object names, so a duplicate would silently mean two
	// rack entries describing one node rather than two nodes.
	seenNodes := map[string]string{}
	seenRacks := map[string]bool{}

	for _, rack := range ct.Spec.Racks {
		if rack.Name == "" {
			errs = append(errs, errors.New("a rack has no name"))
			continue
		}
		if seenRacks[rack.Name] {
			errs = append(errs, fmt.Errorf("rack %q is declared more than once", rack.Name))
		}
		seenRacks[rack.Name] = true

		if rack.FaultDomain == "" {
			errs = append(errs, fmt.Errorf("rack %q has no faultDomain", rack.Name))
		}
		if len(rack.Nodes) == 0 {
			errs = append(errs, fmt.Errorf("rack %q has no nodes", rack.Name))
		}

		for _, node := range rack.Nodes {
			if node.Name == "" {
				errs = append(errs, fmt.Errorf("rack %q has a node with no name", rack.Name))
				continue
			}
			if other, dup := seenNodes[node.Name]; dup {
				errs = append(errs, fmt.Errorf("node %q is declared in both rack %q and rack %q",
					node.Name, other, rack.Name))
			}
			seenNodes[node.Name] = rack.Name

			if _, ok := ct.Spec.NodePools[node.Pool]; !ok {
				errs = append(errs, fmt.Errorf("node %q references undefined pool %q", node.Name, node.Pool))
			}
		}
	}

	errs = append(errs, ct.validateNVLinkConsistency()...)

	return errors.Join(errs...)
}

func (p NodePool) validate(name string) []error {
	var errs []error

	if p.Profile == "" {
		errs = append(errs, fmt.Errorf("pool %q has no profile", name))
	}
	if p.GPUCount <= 0 {
		errs = append(errs, fmt.Errorf("pool %q has gpuCount %d, want at least 1", name, p.GPUCount))
	}
	switch p.NVLink {
	case NVLinkFullMesh, NVLinkNone:
	case "":
		errs = append(errs, fmt.Errorf("pool %q has no nvlink setting, want %q or %q",
			name, NVLinkFullMesh, NVLinkNone))
	default:
		errs = append(errs, fmt.Errorf("pool %q has nvlink %q, want %q or %q",
			name, p.NVLink, NVLinkFullMesh, NVLinkNone))
	}

	return errs
}
