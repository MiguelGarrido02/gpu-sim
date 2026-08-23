package runner

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/MiguelGarrido02/gpu-sim/internal/generate"
	"github.com/MiguelGarrido02/gpu-sim/internal/scenario"
)

// evaluate runs one assertion.
//
// The two waiting modes are not interchangeable. `within` polls and succeeds the moment the
// condition holds; `settle` waits out the whole period and then checks once, because a
// condition asserting that nothing happened needs the scheduler to have been given a fair
// chance to act. Polling a negative assertion would pass instantly and prove nothing.
func (r *Runner) evaluate(ctx context.Context, a scenario.Assertion, replicas int) AssertionResult {
	result := AssertionResult{Name: a.Name}

	if a.Settle.Duration > 0 {
		r.log("settling %s before checking %q", a.Settle.Duration, a.Name)
		select {
		case <-time.After(a.Settle.Duration):
		case <-ctx.Done():
			result.Detail = ctx.Err().Error()
			return result
		}
		ok, detail := r.check(ctx, a, replicas)
		result.Passed, result.Detail = ok, detail
	} else {
		budget := a.Within.Duration
		if budget <= 0 {
			budget = defaultWait
		}
		deadline := time.Now().Add(budget)

		// A recovery budget is measured from the fault rather than from now, since that
		// is the number a user came for: how long the cluster took to heal, not how long
		// the harness waited after it.
		if a.RescheduledWithin.Duration > 0 {
			if _, firedAt, fired := r.disruptedFor(a.Workload); fired {
				deadline = firedAt.Add(a.RescheduledWithin.Duration)
			}
		}
		for {
			ok, detail := r.check(ctx, a, replicas)
			result.Passed, result.Detail = ok, detail
			if ok || time.Now().After(deadline) {
				break
			}
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				result.Detail = ctx.Err().Error()
				return result
			}
		}
	}

	if a.Workload == "" {
		return result
	}

	result.Placement = r.placement(ctx, a.Workload)
	if !result.Passed {
		// Only on failure: the scheduler's explanation is the useful half of a bad
		// result, and noise on a good one.
		result.SchedulerSaid = r.client.SchedulerReasons(ctx, Namespace, a.Workload)
	}
	return result
}

func (r *Runner) check(ctx context.Context, a scenario.Assertion, replicas int) (bool, string) {
	switch {
	case a.Scheduled != "":
		return r.checkScheduled(ctx, a, replicas)
	case a.ConfinedTo != "":
		return r.checkConfinedTo(ctx, a)
	case a.Running != nil:
		return r.checkRunning(ctx, a)
	case len(a.AllocatedDevices) > 0:
		return r.checkAllocatedDevices(ctx, a)
	case a.UnschedulableReason != "":
		return r.checkUnschedulableReason(ctx, a)
	case a.Disrupted != nil:
		return r.checkDisrupted(a)
	case a.RescheduledWithin.Duration > 0:
		return r.checkRescheduled(ctx, a, replicas)
	case a.Fragmentation != nil:
		return r.checkFragmentation(ctx, a)
	}
	return false, "assertion sets no condition"
}

func (r *Runner) checkScheduled(ctx context.Context, a scenario.Assertion, replicas int) (bool, string) {
	want, err := a.ExpectedScheduled(replicas)
	if err != nil {
		return false, err.Error()
	}
	pods, err := r.client.WorkloadPods(ctx, Namespace, a.Workload)
	if err != nil {
		return false, err.Error()
	}
	got := 0
	for _, pod := range pods {
		if pod.Spec.NodeName != "" {
			got++
		}
	}
	return got == want, fmt.Sprintf("expected %d of %d replicas scheduled, got %d", want, replicas, got)
}

func (r *Runner) checkRunning(ctx context.Context, a scenario.Assertion) (bool, string) {
	pods, err := r.client.WorkloadPods(ctx, Namespace, a.Workload)
	if err != nil {
		return false, err.Error()
	}
	got := 0
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodRunning {
			got++
		}
	}
	return got == *a.Running, fmt.Sprintf("expected %d replicas running, got %d", *a.Running, got)
}

// checkConfinedTo requires every placed replica to share one value of a topology level.
//
// An empty workload passes vacuously, which would be a silent false positive, so it fails
// explicitly instead: "all of nothing is in one rack" is not a useful thing to assert.
func (r *Runner) checkConfinedTo(ctx context.Context, a scenario.Assertion) (bool, string) {
	label, ok := generate.LabelForLevel(a.ConfinedTo)
	if !ok {
		return false, fmt.Sprintf("unknown topology level %q, want one of %s",
			a.ConfinedTo, strings.Join(generate.KnownLevels(), ", "))
	}

	pods, err := r.client.WorkloadPods(ctx, Namespace, a.Workload)
	if err != nil {
		return false, err.Error()
	}

	values := map[string]int{}
	placed := 0
	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}
		placed++
		value, err := r.client.NodeLabel(ctx, pod.Spec.NodeName, label)
		if err != nil {
			return false, err.Error()
		}
		values[value]++
	}

	if placed == 0 {
		return false, fmt.Sprintf("no replica was placed, so %q cannot be confined to one %s",
			a.Workload, a.ConfinedTo)
	}

	if len(values) == 1 {
		for value, n := range values {
			return true, fmt.Sprintf("all %d placed replicas are in %s %q", n, a.ConfinedTo, value)
		}
	}
	return false, fmt.Sprintf("placed replicas span %d %ss: %s",
		len(values), a.ConfinedTo, describeCounts(values))
}

func (r *Runner) checkAllocatedDevices(ctx context.Context, a scenario.Assertion) (bool, string) {
	byPod, err := r.client.AllocatedDevices(ctx, Namespace, a.Workload)
	if err != nil {
		return false, err.Error()
	}
	attrs, err := r.client.DeviceAttributes(ctx)
	if err != nil {
		return false, err.Error()
	}

	if len(byPod) == 0 {
		return false, "no device was allocated to the workload"
	}

	checked := 0
	for pod, devices := range byPod {
		for _, device := range devices {
			got, known := attrs[device]
			if !known {
				return false, fmt.Sprintf("device %s is allocated but not published", device)
			}
			for name, want := range a.AllocatedDevices {
				if got[name] != want {
					return false, fmt.Sprintf("pod %s holds device %s with %s=%q, want %q",
						pod, device, name, got[name], want)
				}
			}
			checked++
		}
	}
	return true, fmt.Sprintf("all %d allocated devices match %s", checked, describeAttrs(a.AllocatedDevices))
}

func (r *Runner) checkUnschedulableReason(ctx context.Context, a scenario.Assertion) (bool, string) {
	reasons := r.client.SchedulerReasons(ctx, Namespace, a.Workload)
	for _, reason := range reasons {
		if strings.Contains(reason, a.UnschedulableReason) {
			return true, fmt.Sprintf("the scheduler said: %s", reason)
		}
	}
	if len(reasons) == 0 {
		return false, fmt.Sprintf("the scheduler gave no reason; expected one containing %q", a.UnschedulableReason)
	}
	return false, fmt.Sprintf("no reason contained %q; the scheduler said: %s",
		a.UnschedulableReason, strings.Join(reasons, " | "))
}

// checkDisrupted states the blast radius: how many of a workload's running replicas the
// faults took out.
func (r *Runner) checkDisrupted(a scenario.Assertion) (bool, string) {
	uids, _, fired := r.disruptedFor(a.Workload)
	if !fired {
		return false, "no fault fired, so nothing was disrupted"
	}
	got := len(uids)
	return got == *a.Disrupted,
		fmt.Sprintf("the fault took out %d of %q's running replicas, expected %d", got, a.Workload, *a.Disrupted)
}

// checkRescheduled measures how long the workload took to get back to full strength.
//
// Recovery is counted by pod identity, never by number. A replica on a deleted node keeps
// reporting Running for about a minute, so counting running pods would report full health
// throughout the outage and this assertion would pass without testing anything. Excluding
// the disrupted UIDs makes those pods harmless: a corpse is in the set, so it cannot be
// mistaken for its own replacement.
func (r *Runner) checkRescheduled(ctx context.Context, a scenario.Assertion, replicas int) (bool, string) {
	disrupted, firedAt, fired := r.disruptedFor(a.Workload)
	if !fired {
		return false, "no fault fired, so there was nothing to recover from"
	}
	if len(disrupted) == 0 {
		return false, fmt.Sprintf("the fault disrupted no replica of %q, so there was nothing to recover from", a.Workload)
	}

	pods, err := r.client.WorkloadPods(ctx, Namespace, a.Workload)
	if err != nil {
		return false, err.Error()
	}

	healthy := 0
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodRunning && !disrupted[string(pod.UID)] {
			healthy++
		}
	}

	elapsed := time.Since(firedAt).Round(time.Second)
	if healthy >= replicas {
		return true, fmt.Sprintf("%d replicas lost, back to %d running %s after the fault",
			len(disrupted), healthy, elapsed)
	}
	return false, fmt.Sprintf("%d replicas lost, only %d of %d running again %s after the fault",
		len(disrupted), healthy, replicas, elapsed)
}

// checkFragmentation bounds the partitions lost to fragmentation across the cluster.
func (r *Runner) checkFragmentation(ctx context.Context, a scenario.Assertion) (bool, string) {
	nodes, err := r.client.Fragmentation(ctx)
	if err != nil {
		return false, err.Error()
	}
	if len(nodes) == 0 {
		return false, "no MIG-enabled nodes were found, so there is no fragmentation to measure"
	}

	total := 0
	var worst []string
	for _, node := range nodes {
		total += node.TotalLost()
		for _, gpu := range node.GPUs {
			if gpu.TotalLost() > 0 {
				largest := gpu.LargestAllocatable
				if largest == "" {
					largest = "nothing"
				}
				worst = append(worst, fmt.Sprintf("%s GPU %d lost %d (%d memory slices free, largest allocatable %s)",
					node.Node, gpu.GPUIndex, gpu.TotalLost(), gpu.FreeMemorySlices, largest))
			}
		}
	}

	detail := fmt.Sprintf("%d partitions lost to fragmentation", total)
	if len(worst) > 0 {
		detail += ": " + strings.Join(worst, "; ")
	}

	if a.Fragmentation.AtLeast != nil && total < *a.Fragmentation.AtLeast {
		return false, fmt.Sprintf("%s, want at least %d", detail, *a.Fragmentation.AtLeast)
	}
	if a.Fragmentation.AtMost != nil && total > *a.Fragmentation.AtMost {
		return false, fmt.Sprintf("%s, want at most %d", detail, *a.Fragmentation.AtMost)
	}
	return true, detail
}

func (r *Runner) placement(ctx context.Context, name string) map[string]int {
	pods, err := r.client.WorkloadPods(ctx, Namespace, name)
	if err != nil {
		return nil
	}
	out := map[string]int{}
	for _, pod := range pods {
		node := pod.Spec.NodeName
		if node == "" {
			node = "(unscheduled)"
		}
		out[node]++
	}
	return out
}

func describeCounts(values map[string]int) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, values[k]))
	}
	return strings.Join(parts, ", ")
}

func describeAttrs(attrs map[string]string) string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, attrs[k]))
	}
	return strings.Join(parts, ", ")
}
