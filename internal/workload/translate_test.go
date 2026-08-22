package workload

import (
	"errors"
	"strings"
	"testing"

	"github.com/MiguelGarrido02/gpu-sim/internal/scenario"
)

const (
	ns   = "gpu-sim-scenarios"
	topo = "two-racks-h100"
)

func gangWorkload() scenario.Workload {
	return scenario.Workload{
		Name:      "training",
		Replicas:  12,
		GPUs:      1,
		Gang:      true,
		Placement: &scenario.Placement{Required: "rack"},
	}
}

// TestGangBecomesJobWithMinMember covers the trap that made the first Phase 0 gang test a
// false positive: without the annotation KAI builds a PodGroup with minMember=1 and the
// replicas are scheduled independently, so the workload looks like a gang and behaves like
// a crowd.
func TestGangBecomesJobWithMinMember(t *testing.T) {
	objs, err := Translate(gangWorkload(), scenario.SchedulerKAI, ns, topo)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}

	if objs.Job == nil {
		t.Fatal("a gang did not become a Job")
	}
	if objs.Deployment != nil {
		t.Error("a gang also produced a Deployment")
	}

	if got := objs.Job.Annotations[kaiMinMemberAnn]; got != "12" {
		t.Errorf("%s = %q, want \"12\"", kaiMinMemberAnn, got)
	}
	if got := objs.Job.Annotations[kaiTopologyAnn]; got != topo {
		t.Errorf("%s = %q, want %q", kaiTopologyAnn, got, topo)
	}
	if got := objs.Job.Annotations[kaiRequiredPlaceAn]; got != "rack" {
		t.Errorf("%s = %q, want \"rack\"", kaiRequiredPlaceAn, got)
	}
	if got := objs.Job.Spec.Template.Spec.SchedulerName; got != kaiSchedulerName {
		t.Errorf("schedulerName = %q, want %q", got, kaiSchedulerName)
	}
	if got := objs.Job.Spec.Template.Labels[kaiQueueLabel]; got != kaiDefaultQueue {
		t.Errorf("queue label = %q, want %q", got, kaiDefaultQueue)
	}
}

// TestNonGangBecomesDeployment covers the other trap: KWOK drives restartPolicy: Never pods
// straight to Completed and a terminal pod releases its ResourceClaim, so a workload meant
// to hold GPUs long enough to be counted has to be a Deployment.
func TestNonGangBecomesDeployment(t *testing.T) {
	w := scenario.Workload{Name: "inference", Replicas: 20, GPUs: 1}

	objs, err := Translate(w, scenario.SchedulerKAI, ns, topo)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if objs.Deployment == nil {
		t.Fatal("a non-gang workload did not become a Deployment")
	}
	if objs.Job != nil {
		t.Error("a non-gang workload also produced a Job")
	}
	if got := objs.Deployment.Spec.Template.Spec.RestartPolicy; got != "Always" {
		t.Errorf("restartPolicy = %q, want Always: a terminal pod releases its claim", got)
	}
	if got := objs.Deployment.Spec.Template.Labels[kaiQueueLabel]; got != kaiDefaultQueue {
		t.Errorf("queue label = %q, want %q: KAI refuses a workload with no queue", got, kaiDefaultQueue)
	}
}

// TestSelectorIsNotInTheDeploymentQueueLabel guards a subtle breakage: a Deployment's
// selector is immutable, so a scheduler-specific label in it would make switching a
// scenario's scheduler require deleting the Deployment.
func TestDeploymentSelectorIsSchedulerIndependent(t *testing.T) {
	w := scenario.Workload{Name: "inference", Replicas: 2, GPUs: 1}

	objs, err := Translate(w, scenario.SchedulerKAI, ns, topo)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if _, found := objs.Deployment.Spec.Selector.MatchLabels[kaiQueueLabel]; found {
		t.Error("the Deployment selector contains a scheduler-specific label")
	}
	if got := objs.Deployment.Spec.Selector.MatchLabels[LabelWorkload]; got != "inference" {
		t.Errorf("selector workload label = %q, want \"inference\"", got)
	}
}

func TestDefaultSchedulerRefusesGang(t *testing.T) {
	_, err := Translate(gangWorkload(), scenario.SchedulerDefault, ns, topo)

	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Translate returned %v, want an UnsupportedError", err)
	}
	if unsupported.Intent != "gang scheduling" {
		t.Errorf("intent = %q, want \"gang scheduling\"", unsupported.Intent)
	}
	if !strings.Contains(err.Error(), "all-or-nothing") {
		t.Errorf("error %q does not explain why", err)
	}
}

func TestDefaultSchedulerRefusesRequiredPlacement(t *testing.T) {
	w := scenario.Workload{
		Name: "spread", Replicas: 4, GPUs: 1,
		Placement: &scenario.Placement{Required: "rack"},
	}

	_, err := Translate(w, scenario.SchedulerDefault, ns, topo)

	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Translate returned %v, want an UnsupportedError", err)
	}
	if unsupported.Intent != "required topology placement" {
		t.Errorf("intent = %q, want \"required topology placement\"", unsupported.Intent)
	}
}

// TestDefaultSchedulerAcceptsDeviceSelection is the other half of the refusals above:
// device selection is core DRA and needs no translation, so it must work unchanged under
// the stock scheduler. If this ever fails, the neutral layer has grown a KAI dependency.
func TestDefaultSchedulerAcceptsDeviceSelection(t *testing.T) {
	const expr = "device.attributes['gpu.nvidia.com'].numaNode == 0"
	w := scenario.Workload{Name: "stock", Replicas: 4, GPUs: 1, DeviceSelector: expr}

	objs, err := Translate(w, scenario.SchedulerDefault, ns, topo)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if objs.ClaimTemplate == nil {
		t.Fatal("no claim template was produced")
	}

	selectors := objs.ClaimTemplate.Spec.Spec.Devices.Requests[0].Exactly.Selectors
	if len(selectors) != 1 || selectors[0].CEL == nil {
		t.Fatalf("got %d selectors, want one CEL selector", len(selectors))
	}
	if got := selectors[0].CEL.Expression; got != expr {
		t.Errorf("expression = %q, want it passed through verbatim", got)
	}

	if got := objs.Deployment.Spec.Template.Spec.SchedulerName; got != "" {
		t.Errorf("schedulerName = %q, want empty so the stock scheduler takes it", got)
	}
	if _, found := objs.Deployment.Spec.Template.Labels[kaiQueueLabel]; found {
		t.Error("a default-scheduler workload carries a KAI queue label")
	}
}

func TestNoGPUsMeansNoClaim(t *testing.T) {
	w := scenario.Workload{Name: "cpu-only", Replicas: 2, GPUs: 0}

	objs, err := Translate(w, scenario.SchedulerKAI, ns, topo)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if objs.ClaimTemplate != nil {
		t.Error("a workload requesting no GPUs produced a claim template")
	}
	if len(objs.Deployment.Spec.Template.Spec.ResourceClaims) != 0 {
		t.Error("a workload requesting no GPUs references a claim")
	}
}

func TestGPUCountReachesTheClaim(t *testing.T) {
	w := scenario.Workload{Name: "multi", Replicas: 2, GPUs: 4}

	objs, err := Translate(w, scenario.SchedulerKAI, ns, topo)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got := objs.ClaimTemplate.Spec.Spec.Devices.Requests[0].Exactly.Count; got != 4 {
		t.Errorf("device count = %d, want 4", got)
	}
}

// TestSimulatedNodesAreOptedInto checks every workload tolerates the KWOK taint and selects
// simulated nodes. Without both, pods either land nowhere or land on a real node.
func TestSimulatedNodesAreOptedInto(t *testing.T) {
	objs, err := Translate(gangWorkload(), scenario.SchedulerKAI, ns, topo)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	spec := objs.Job.Spec.Template.Spec

	if spec.NodeSelector[kwokNodeLabel] != kwokNodeValue {
		t.Error("workload does not select simulated nodes")
	}
	tolerated := false
	for _, tol := range spec.Tolerations {
		if tol.Key == kwokTaintKey {
			tolerated = true
		}
	}
	if !tolerated {
		t.Error("workload does not tolerate the KWOK taint, so it can never be placed")
	}
}
