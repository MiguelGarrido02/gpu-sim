// Package gpuid reproduces the identity fake-gpu-operator assigns to each simulated GPU.
//
// gpu-sim publishes its own ResourceSlices rather than extending fake-gpu-operator (see
// docs/designs/topology-model.md), which makes these identifiers a contract between the
// two components rather than an implementation detail. fake-gpu-operator's status-updater
// tracks which pod holds which GPU, and its simulated nvidia-smi and Prometheus metrics
// resolve devices, by exactly these IDs. A slice published under different names would
// leave both sides describing the same hardware in mutually unintelligible terms, and
// nothing would report an error.
//
// The derivation is pinned by TestDeviceUUIDMatchesUpstream against values captured from a
// running cluster, so an upstream change breaks the build instead of silently
// desynchronising the two components.
package gpuid

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// uuidPrefix mirrors the "GPU-" prefix NVIDIA uses for real GPU UUIDs, which
// fake-gpu-operator reproduces.
const uuidPrefix = "GPU-"

// DeviceUUID returns the UUID of the GPU at the given zero-based index on the named node,
// in the form fake-gpu-operator publishes it.
//
// Upstream builds this as a UUIDv5 over "<nodeName>-<index>" using the nil namespace:
//
//	uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprintf("%s-%d", nodeName, idx)))
func DeviceUUID(nodeName string, index int) string {
	name := fmt.Sprintf("%s-%d", nodeName, index)
	return uuidPrefix + uuid.NewSHA1(uuid.Nil, []byte(name)).String()
}

// DeviceName returns the ResourceSlice device name for the GPU at the given index.
//
// Device names are DNS labels, so the UUID is lowercased; upstream does the same when
// building its slices.
func DeviceName(nodeName string, index int) string {
	return strings.ToLower(DeviceUUID(nodeName, index))
}
