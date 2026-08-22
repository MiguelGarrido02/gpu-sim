package cluster

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/MiguelGarrido02/gpu-sim/internal/generate"
	"github.com/MiguelGarrido02/gpu-sim/internal/mig"
)

// NodeFragmentation is one node's MIG picture.
type NodeFragmentation struct {
	Node string       `json:"node"`
	GPUs []mig.Report `json:"gpus"`
}

// TotalLost sums fragmentation across the node's GPUs.
func (n NodeFragmentation) TotalLost() int {
	total := 0
	for _, gpu := range n.GPUs {
		total += gpu.TotalLost()
	}
	return total
}

// Fragmentation reports, for every MIG-enabled node, how much of each GPU is free but
// unreachable.
//
// Everything it needs is read from the cluster rather than from a topology file: the
// geometry comes from the published counter sets, the profiles from the published devices,
// and the current state from which of those devices are allocated. So the report describes
// the cluster as it is, which is the only thing worth reporting — a file says what was
// intended.
func (c *Client) Fragmentation(ctx context.Context) ([]NodeFragmentation, error) {
	slices, err := c.kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{
		LabelSelector: generate.ManagedSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("listing managed ResourceSlices: %w", err)
	}

	allocated, err := c.allocatedDeviceNames(ctx)
	if err != nil {
		return nil, err
	}

	byNode := map[string]*nodeState{}
	for _, slice := range slices.Items {
		if slice.Spec.NodeName == nil {
			continue
		}
		state, ok := byNode[*slice.Spec.NodeName]
		if !ok {
			state = newNodeState()
			byNode[*slice.Spec.NodeName] = state
		}
		state.absorb(slice)
	}

	var out []NodeFragmentation
	for node, state := range byNode {
		if len(state.counterSets) == 0 {
			continue // not a MIG node
		}
		out = append(out, NodeFragmentation{Node: node, GPUs: state.analyse(allocated)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out, nil
}

func (c *Client) allocatedDeviceNames(ctx context.Context) (map[string]bool, error) {
	claims, err := c.kube.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing resource claims: %w", err)
	}
	allocated := map[string]bool{}
	for _, claim := range claims.Items {
		if claim.Status.Allocation == nil {
			continue
		}
		for _, result := range claim.Status.Allocation.Devices.Results {
			allocated[result.Device] = true
		}
	}
	return allocated, nil
}

// nodeState accumulates what one node published.
type nodeState struct {
	// counterSets maps a GPU index to its capacity, read from the published counters.
	counterSets map[int]mig.Geometry
	// devices maps a GPU index to the partitions published for it.
	devices map[int][]publishedPartition
}

type publishedPartition struct {
	name         string
	profile      mig.Profile
	memorySlices []int
}

func newNodeState() *nodeState {
	return &nodeState{counterSets: map[int]mig.Geometry{}, devices: map[int][]publishedPartition{}}
}

func (s *nodeState) absorb(slice resourceapi.ResourceSlice) {
	for _, set := range slice.Spec.SharedCounters {
		index, ok := gpuIndexFromCounterSet(set.Name)
		if !ok {
			continue
		}
		geometry := mig.Geometry{}
		for name, counter := range set.Counters {
			if name == mig.SMSliceCounter {
				geometry.SMSlices = int(counter.Value.Value())
				continue
			}
			if _, isMemory := memorySliceIndex(name); isMemory {
				geometry.MemorySlices++
			}
		}
		s.counterSets[index] = geometry
	}

	for _, device := range slice.Spec.Devices {
		if len(device.ConsumesCounters) == 0 {
			continue
		}
		consumption := device.ConsumesCounters[0]
		index, ok := gpuIndexFromCounterSet(consumption.CounterSet)
		if !ok {
			continue
		}

		partition := publishedPartition{name: device.Name}
		for name, counter := range consumption.Counters {
			if name == mig.SMSliceCounter {
				partition.profile.SMSlices = int(counter.Value.Value())
				continue
			}
			if slot, isMemory := memorySliceIndex(name); isMemory {
				partition.memorySlices = append(partition.memorySlices, slot)
			}
		}
		sort.Ints(partition.memorySlices)
		partition.profile.MemorySlices = len(partition.memorySlices)

		if attr, found := device.Attributes[generate.AttrProfile]; found && attr.StringValue != nil {
			partition.profile.Name = *attr.StringValue
		}

		s.devices[index] = append(s.devices[index], partition)
	}
}

func (s *nodeState) analyse(allocated map[string]bool) []mig.Report {
	indexes := make([]int, 0, len(s.counterSets))
	for index := range s.counterSets {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	reports := make([]mig.Report, 0, len(indexes))
	for _, index := range indexes {
		geometry := s.counterSets[index]
		geometry.Profiles = distinctProfiles(s.devices[index])

		state := mig.State{UsedMemorySlices: map[int]bool{}}
		for _, partition := range s.devices[index] {
			if !allocated[partition.name] {
				continue
			}
			for _, slot := range partition.memorySlices {
				state.UsedMemorySlices[slot] = true
			}
			state.UsedSMSlices += partition.profile.SMSlices
		}

		reports = append(reports, mig.Analyse(geometry, geometry.Profiles, index, state))
	}
	return reports
}

// distinctProfiles recovers the profile list from the published devices, smallest first so
// that the largest allocatable one falls out of a single pass.
func distinctProfiles(partitions []publishedPartition) []mig.Profile {
	seen := map[string]mig.Profile{}
	for _, partition := range partitions {
		if partition.profile.Name != "" {
			seen[partition.profile.Name] = partition.profile
		}
	}
	out := make([]mig.Profile, 0, len(seen))
	for _, profile := range seen {
		out = append(out, profile)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MemorySlices != out[j].MemorySlices {
			return out[i].MemorySlices < out[j].MemorySlices
		}
		return out[i].SMSlices < out[j].SMSlices
	})
	return out
}

func gpuIndexFromCounterSet(name string) (int, bool) {
	rest, found := strings.CutPrefix(name, "gpu-")
	if !found {
		return 0, false
	}
	index, err := strconv.Atoi(rest)
	return index, err == nil
}

func memorySliceIndex(counter string) (int, bool) {
	rest, found := strings.CutPrefix(counter, "memory-slice-")
	if !found {
		return 0, false
	}
	index, err := strconv.Atoi(rest)
	return index, err == nil
}
