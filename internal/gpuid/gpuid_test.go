package gpuid

import "testing"

// TestDeviceUUIDMatchesUpstream pins the identifiers against values read from a live
// cluster running fake-gpu-operator 0.2.0, on 2026-08-22:
//
//	kubectl get resourceslice kwok-gpu-node-1-gpu -o json | \
//	  jq -r '.spec.devices[].attributes["gpu.nvidia.com/uuid"].string'
//
// These are not example values to be regenerated when the test fails. A failure means
// upstream changed how it names devices, and gpu-sim's slices no longer refer to the same
// GPUs as its pod tracking and metrics. Investigate upstream before touching them.
func TestDeviceUUIDMatchesUpstream(t *testing.T) {
	tests := []struct {
		nodeName string
		index    int
		want     string
	}{
		{"gpu-node-1", 0, "GPU-977c4dd2-a349-5388-a0a9-448c62948bd8"},
		{"gpu-node-1", 1, "GPU-bea8c519-c518-5a51-a8e0-9205b8f99767"},
		{"gpu-node-1", 2, "GPU-6bce07b9-7c0b-5f36-b90f-065e46b2ac19"},
	}

	for _, tt := range tests {
		if got := DeviceUUID(tt.nodeName, tt.index); got != tt.want {
			t.Errorf("DeviceUUID(%q, %d) = %q, want %q (upstream compatibility broken)",
				tt.nodeName, tt.index, got, tt.want)
		}
	}
}

// TestDeviceUUIDIsNodeScoped guards against a derivation that ignores the node name, which
// would hand every node's GPUs the same identifiers and collapse a multi-node cluster into
// one set of devices.
func TestDeviceUUIDIsNodeScoped(t *testing.T) {
	if DeviceUUID("gpu-node-1", 0) == DeviceUUID("gpu-node-2", 0) {
		t.Error("GPUs at the same index on different nodes share a UUID")
	}
}

// TestDeviceNameIsLowercased checks the ResourceSlice device name is a valid DNS label,
// which the API server enforces on device names.
func TestDeviceNameIsLowercased(t *testing.T) {
	const want = "gpu-977c4dd2-a349-5388-a0a9-448c62948bd8"
	if got := DeviceName("gpu-node-1", 0); got != want {
		t.Errorf("DeviceName(...) = %q, want %q", got, want)
	}
}
