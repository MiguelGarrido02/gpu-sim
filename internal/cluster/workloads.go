package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/MiguelGarrido02/gpu-sim/internal/generate"
	"github.com/MiguelGarrido02/gpu-sim/internal/workload"
)

// podGroupGVR is KAI's PodGroup. Reached dynamically so gpu-sim does not depend on KAI's
// module just to read a status message.
var podGroupGVR = schema.GroupVersionResource{
	Group:    "scheduling.run.ai",
	Version:  "v2alpha2",
	Resource: "podgroups",
}

// EnsureNamespace creates the namespace if it is missing.
func (c *Client) EnsureNamespace(ctx context.Context, name string) error {
	_, err := c.kube.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading namespace %s: %w", name, err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if _, err := c.kube.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating namespace %s: %w", name, err)
	}
	return nil
}

// ClearNamespace removes everything a previous run left behind. Scenarios are hermetic, and
// a run that measured leftovers would report a confident wrong answer.
func (c *Client) ClearNamespace(ctx context.Context, ns string) error {
	background := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &background}
	all := metav1.ListOptions{}

	if err := c.kube.AppsV1().Deployments(ns).DeleteCollection(ctx, opts, all); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("clearing deployments: %w", err)
	}
	if err := c.kube.BatchV1().Jobs(ns).DeleteCollection(ctx, opts, all); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("clearing jobs: %w", err)
	}
	if err := c.kube.CoreV1().Pods(ns).DeleteCollection(ctx, opts, all); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("clearing pods: %w", err)
	}
	if err := c.kube.ResourceV1().ResourceClaimTemplates(ns).DeleteCollection(ctx, opts, all); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("clearing claim templates: %w", err)
	}
	// PodGroups outlive their Job unless the owner reference catches up, and a stale one
	// makes the next run's scheduler reasons refer to the previous workload.
	_ = c.dynamic.Resource(podGroupGVR).Namespace(ns).DeleteCollection(ctx, opts, all)
	_ = c.dynamic.Resource(volcanoPodGroupGVR).Namespace(ns).DeleteCollection(ctx, opts, all)

	return nil
}

var volcanoPodGroupGVR = schema.GroupVersionResource{
	Group:    "scheduling.volcano.sh",
	Version:  "v1beta1",
	Resource: "podgroups",
}

// Submit creates a workload's objects.
//
// The PodGroup goes first when there is one: Volcano's webhook rejects a pod naming a group
// that does not exist yet.
func (c *Client) Submit(ctx context.Context, ns string, objs *workload.Objects) error {
	if objs.PodGroup != nil {
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(objs.PodGroup)
		if err != nil {
			return fmt.Errorf("converting PodGroup: %w", err)
		}
		_, err = c.dynamic.Resource(volcanoPodGroupGVR).Namespace(ns).Create(
			ctx, &unstructured.Unstructured{Object: raw}, metav1.CreateOptions{})
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating PodGroup %s: %w", objs.PodGroup.Metadata.Name, err)
		}
	}
	if objs.ClaimTemplate != nil {
		if _, err := c.kube.ResourceV1().ResourceClaimTemplates(ns).Create(
			ctx, objs.ClaimTemplate, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating claim template: %w", err)
		}
	}
	if objs.Job != nil {
		if _, err := c.kube.BatchV1().Jobs(ns).Create(ctx, objs.Job, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating job %s: %w", objs.Job.Name, err)
		}
	}
	if objs.Deployment != nil {
		if _, err := c.kube.AppsV1().Deployments(ns).Create(ctx, objs.Deployment, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating deployment %s: %w", objs.Deployment.Name, err)
		}
	}
	return nil
}

// WorkloadPods returns the pods belonging to a workload.
func (c *Client) WorkloadPods(ctx context.Context, ns, name string) ([]corev1.Pod, error) {
	list, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: workload.LabelWorkload + "=" + name,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods for workload %s: %w", name, err)
	}
	return list.Items, nil
}

// AllocatedDevices returns the devices allocated to a workload's pods, by pod name.
func (c *Client) AllocatedDevices(ctx context.Context, ns, name string) (map[string][]string, error) {
	pods, err := c.WorkloadPods(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	owned := map[string]bool{}
	for _, pod := range pods {
		owned[string(pod.UID)] = true
	}

	claims, err := c.kube.ResourceV1().ResourceClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing resource claims: %w", err)
	}

	byPod := map[string][]string{}
	for _, claim := range claims.Items {
		if claim.Status.Allocation == nil {
			continue
		}
		// A claim generated from a template is owned by the pod that triggered it, which
		// is how a claim is attributed to a workload without parsing generated names.
		for _, ref := range claim.OwnerReferences {
			if ref.Kind != "Pod" || !owned[string(ref.UID)] {
				continue
			}
			for _, result := range claim.Status.Allocation.Devices.Results {
				byPod[ref.Name] = append(byPod[ref.Name], result.Device)
			}
		}
	}
	return byPod, nil
}

// DeviceAttributes indexes every published device by name, with its attributes flattened to
// strings so an assertion can compare without caring whether a value was a string or an int.
func (c *Client) DeviceAttributes(ctx context.Context) (map[string]map[string]string, error) {
	slices, err := c.kube.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing resource slices: %w", err)
	}

	out := map[string]map[string]string{}
	for _, slice := range slices.Items {
		for _, device := range slice.Spec.Devices {
			attrs := map[string]string{}
			for name, value := range device.Attributes {
				attrs[stripDomain(string(name))] = attributeString(value)
			}
			out[device.Name] = attrs
		}
	}
	return out, nil
}

// stripDomain drops the domain prefix from a qualified attribute name so a scenario can
// write `numaNode` whether the driver published it bare or as `gpu.nvidia.com/numaNode`.
func stripDomain(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func attributeString(v resourceapi.DeviceAttribute) string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case v.IntValue != nil:
		return fmt.Sprint(*v.IntValue)
	case v.BoolValue != nil:
		return fmt.Sprint(*v.BoolValue)
	case v.VersionValue != nil:
		return *v.VersionValue
	}
	return ""
}

// NodeLabel returns one label value from a node.
func (c *Client) NodeLabel(ctx context.Context, node, label string) (string, error) {
	n, err := c.kube.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading node %s: %w", node, err)
	}
	return n.Labels[label], nil
}

// SchedulerReasons collects the scheduler's own explanation for a workload that was not
// placed.
//
// This is the whole value of a failure report. A generic "not enough resources" cost a day
// in Phase 1; the useful text is what the scheduler itself said, and where it lives depends
// on which scheduler ran.
func (c *Client) SchedulerReasons(ctx context.Context, ns, name string) []string {
	seen := map[string]bool{}
	var reasons []string
	add := func(msg string) {
		msg = strings.TrimSpace(msg)
		if msg == "" || seen[msg] {
			return
		}
		seen[msg] = true
		reasons = append(reasons, msg)
	}

	// KAI records the richest explanation on the PodGroup, broken down by topology domain.
	if groups, err := c.dynamic.Resource(podGroupGVR).Namespace(ns).List(ctx, metav1.ListOptions{}); err == nil {
		for _, g := range groups.Items {
			if !strings.Contains(g.GetName(), name) {
				continue
			}
			conditions, found, _ := unstructuredSlice(g.Object, "status", "schedulingConditions")
			if !found {
				continue
			}
			for _, cond := range conditions {
				if m, ok := cond.(map[string]any); ok {
					if msg, ok := m["message"].(string); ok {
						add(msg)
					}
				}
			}
		}
	}

	// The default scheduler explains itself through pod events instead.
	pods, err := c.WorkloadPods(ctx, ns, name)
	if err == nil {
		for _, pod := range pods {
			if pod.Spec.NodeName != "" {
				continue
			}
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse {
					add(cond.Message)
				}
			}
		}
	}

	sort.Strings(reasons)
	return reasons
}

func unstructuredSlice(obj map[string]any, path ...string) ([]any, bool, error) {
	cur := any(obj)
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur, ok = m[key]
		if !ok {
			return nil, false, nil
		}
	}
	out, ok := cur.([]any)
	return out, ok, nil
}

// AllPods lists every pod in a namespace, including ones no workload label claims.
func (c *Client) AllPods(ctx context.Context, ns string) ([]corev1.Pod, error) {
	list, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing pods in %s: %w", ns, err)
	}
	return list.Items, nil
}

var deviceClassGVR = schema.GroupVersionResource{
	Group:    "resource.k8s.io",
	Version:  "v1",
	Resource: "deviceclasses",
}

// ApplyMIGDeviceClass creates or updates the DeviceClass MIG partitions are selected
// through. Applied dynamically to keep the generator's own DeviceClass type as the single
// description of its shape.
func (c *Client) ApplyMIGDeviceClass(ctx context.Context) error {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(generate.MIGDeviceClass())
	if err != nil {
		return fmt.Errorf("converting MIG DeviceClass: %w", err)
	}
	obj := &unstructured.Unstructured{Object: raw}

	classes := c.dynamic.Resource(deviceClassGVR)
	existing, err := classes.Get(ctx, generate.MIGDeviceClassName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if _, err := classes.Create(ctx, obj, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating MIG DeviceClass: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading MIG DeviceClass: %w", err)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	if _, err := classes.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating MIG DeviceClass: %w", err)
	}
	return nil
}

// Retire deletes a workload's objects, releasing whatever they held.
//
// The claim template stays: deleting it would not free anything (claims are owned by their
// pods) and a scenario may submit the same workload shape again.
func (c *Client) Retire(ctx context.Context, ns, name string) error {
	background := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &background}

	err := c.kube.AppsV1().Deployments(ns).Delete(ctx, name, opts)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("retiring deployment %s: %w", name, err)
	}
	err = c.kube.BatchV1().Jobs(ns).Delete(ctx, name, opts)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("retiring job %s: %w", name, err)
	}
	_ = c.dynamic.Resource(volcanoPodGroupGVR).Namespace(ns).Delete(ctx, name, opts)

	// Wait for the pods to go, or the partitions they hold would still be allocated when
	// the next event fires and the release this call exists to cause would not have
	// happened yet.
	deadline := time.Now().Add(60 * time.Second)
	for {
		pods, err := c.WorkloadPods(ctx, ns, name)
		if err != nil {
			return err
		}
		if len(pods) == 0 || time.Now().After(deadline) {
			return nil
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
