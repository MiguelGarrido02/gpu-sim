// Package workload turns a scenario's scheduler-neutral intent into Kubernetes objects for
// a particular scheduler.
//
// The neutral layer is deliberately small — only intents that have a working test behind
// them — because an abstraction derived from a single implementation is usually wrong, and
// only KAI is implemented so far. What the layer buys is the ability to run one scenario
// against several schedulers, which is the project's stated purpose and is impossible in
// any one scheduler's dialect.
package workload

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/MiguelGarrido02/gpu-sim/internal/scenario"
)

const (
	// gpuDeviceClass is published by fake-gpu-operator and selects whole simulated GPUs.
	gpuDeviceClass = "gpu.nvidia.com"

	// migDeviceClass selects MIG partitions. They live in a separate class so that a
	// workload asking for a GPU never receives a slice of one.
	migDeviceClass = "mig.nvidia.com"

	// Simulated nodes carry a NoSchedule taint so real workloads never land on them by
	// accident; anything meant for the simulation opts in explicitly.
	kwokNodeLabel = "type"
	kwokNodeValue = "kwok"
	kwokTaintKey  = "kwok.x-k8s.io/node"

	// Nothing is executed on a simulated node, so the image only has to exist.
	workloadImage = "ubuntu:24.04"
)

// KAI's contract, from docs/topologies.md.
const (
	kaiSchedulerName   = "kai-scheduler"
	kaiQueueLabel      = "kai.scheduler/queue"
	kaiDefaultQueue    = "default-queue"
	kaiMinMemberAnn    = "kai.scheduler/batch-min-member"
	kaiTopologyAnn     = "kai.scheduler/topology"
	kaiRequiredPlaceAn = "kai.scheduler/topology-required-placement"
)

// UnsupportedError reports an intent the target scheduler has no way to express.
//
// Refusing is the point. Quietly running something weaker — scheduling a gang's replicas
// independently, say — would report a pass for a guarantee the scheduler never made, and a
// team evaluating schedulers wants precisely the opposite answer.
type UnsupportedError struct {
	Scheduler scenario.Scheduler
	Workload  string
	Intent    string
	Why       string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("scheduler %q cannot express %s (workload %q): %s",
		e.Scheduler, e.Intent, e.Workload, e.Why)
}

// Objects are what a workload becomes. Exactly one of Job or Deployment is set.
type Objects struct {
	ClaimTemplate *resourceapi.ResourceClaimTemplate
	Job           *batchv1.Job
	Deployment    *appsv1.Deployment
}

// Translate builds the objects for a workload. topologyName is the name of the applied
// ClusterTopology, which schedulers reference when placing by topology level.
func Translate(w scenario.Workload, sched scenario.Scheduler, namespace, topologyName string) (*Objects, error) {
	if err := checkSupported(w, sched); err != nil {
		return nil, err
	}

	objs := &Objects{}
	if w.GPUs > 0 {
		objs.ClaimTemplate = claimTemplate(w, namespace)
	}

	spec := podSpec(w, sched, namespace)

	// A gang becomes a Job and everything else a Deployment. Not a preference: KWOK drives
	// restartPolicy: Never pods straight to Completed, and a terminal pod releases its
	// ResourceClaim, so a workload meant to hold GPUs has to be a Deployment. Gangs are
	// asserted on placement rather than on holding, so a Job is fine there and is what
	// carries the gang annotation.
	if w.Gang {
		objs.Job = job(w, sched, namespace, topologyName, spec)
	} else {
		objs.Deployment = deployment(w, sched, namespace, spec)
	}

	return objs, nil
}

func checkSupported(w scenario.Workload, sched scenario.Scheduler) error {
	if sched != scenario.SchedulerDefault {
		return nil
	}
	if w.Gang {
		return &UnsupportedError{
			Scheduler: sched, Workload: w.Name, Intent: "gang scheduling",
			Why: "the default scheduler places pods one at a time and has no all-or-nothing concept",
		}
	}
	if w.Placement != nil && w.Placement.Required != "" {
		return &UnsupportedError{
			Scheduler: sched, Workload: w.Name, Intent: "required topology placement",
			Why: "the default scheduler can spread across a topology key but cannot confine a workload to one domain",
		}
	}
	return nil
}

func claimTemplateName(workload string) string { return workload + "-gpu" }

func claimTemplate(w scenario.Workload, namespace string) *resourceapi.ResourceClaimTemplate {
	deviceClass := gpuDeviceClass
	var selectors []resourceapi.DeviceSelector

	if w.MIGProfile != "" {
		deviceClass = migDeviceClass
		selectors = append(selectors, resourceapi.DeviceSelector{
			CEL: &resourceapi.CELDeviceSelector{
				Expression: fmt.Sprintf("device.attributes[%q].profile == %q", gpuDeviceClass, w.MIGProfile),
			},
		})
	}
	if w.DeviceSelector != "" {
		selectors = append(selectors, resourceapi.DeviceSelector{
			CEL: &resourceapi.CELDeviceSelector{Expression: w.DeviceSelector},
		})
	}

	request := resourceapi.DeviceRequest{
		Name: "gpu",
		Exactly: &resourceapi.ExactDeviceRequest{
			DeviceClassName: deviceClass,
			Count:           int64(w.GPUs),
			AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
			Selectors:       selectors,
		},
	}

	return &resourceapi.ResourceClaimTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: claimTemplateName(w.Name), Namespace: namespace},
		Spec: resourceapi.ResourceClaimTemplateSpec{
			Spec: resourceapi.ResourceClaimSpec{
				Devices: resourceapi.DeviceClaim{Requests: []resourceapi.DeviceRequest{request}},
			},
		},
	}
}

func podSpec(w scenario.Workload, sched scenario.Scheduler, namespace string) corev1.PodSpec {
	spec := corev1.PodSpec{
		NodeSelector: map[string]string{kwokNodeLabel: kwokNodeValue},
		Tolerations: []corev1.Toleration{{
			Key:      kwokTaintKey,
			Operator: corev1.TolerationOpEqual,
			Value:    "fake",
			Effect:   corev1.TaintEffectNoSchedule,
		}},
		Containers: []corev1.Container{{
			Name:    "main",
			Image:   workloadImage,
			Command: []string{"sleep", "infinity"},
		}},
	}

	if sched == scenario.SchedulerKAI {
		spec.SchedulerName = kaiSchedulerName
	}

	if w.GPUs > 0 {
		spec.Containers[0].Resources.Claims = []corev1.ResourceClaim{{Name: "gpu"}}
		spec.ResourceClaims = []corev1.PodResourceClaim{{
			Name:                      "gpu",
			ResourceClaimTemplateName: ptr.To(claimTemplateName(w.Name)),
		}}
	}

	return spec
}

func podLabels(w scenario.Workload, sched scenario.Scheduler) map[string]string {
	labels := map[string]string{LabelWorkload: w.Name}
	if sched == scenario.SchedulerKAI {
		// KAI refuses to schedule a workload that does not belong to a queue.
		labels[kaiQueueLabel] = kaiDefaultQueue
	}
	return labels
}

// LabelWorkload marks every pod with the workload it came from, which is how assertions
// find their subjects.
const LabelWorkload = "gpu-sim.io/workload"

func job(w scenario.Workload, sched scenario.Scheduler, namespace, topologyName string, spec corev1.PodSpec) *batchv1.Job {
	spec.RestartPolicy = corev1.RestartPolicyNever

	annotations := map[string]string{}
	if sched == scenario.SchedulerKAI {
		// Without this KAI's pod-grouper builds a PodGroup with minMember=1 and the
		// replicas are scheduled independently — the workload would look like a gang and
		// behave like a crowd.
		annotations[kaiMinMemberAnn] = fmt.Sprint(w.Replicas)
		if w.Placement != nil && w.Placement.Required != "" {
			annotations[kaiTopologyAnn] = topologyName
			annotations[kaiRequiredPlaceAn] = w.Placement.Required
		}
	}

	replicas := int32(w.Replicas)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: w.Name, Namespace: namespace, Annotations: annotations},
		Spec: batchv1.JobSpec{
			Parallelism: &replicas,
			Completions: &replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels(w, sched)},
				Spec:       spec,
			},
		},
	}
}

func deployment(w scenario.Workload, sched scenario.Scheduler, namespace string, spec corev1.PodSpec) *appsv1.Deployment {
	spec.RestartPolicy = corev1.RestartPolicyAlways

	replicas := int32(w.Replicas)

	// The selector matches on the workload label alone; the pod template also carries
	// whatever the scheduler needs. A Deployment's selector is immutable, so keeping
	// scheduler-specific labels out of it means switching a scenario's scheduler does not
	// require deleting and recreating the Deployment.
	selector := map[string]string{LabelWorkload: w.Name}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: w.Name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels(w, sched)},
				Spec:       spec,
			},
		},
	}
}
