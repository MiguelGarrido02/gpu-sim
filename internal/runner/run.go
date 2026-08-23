// Package runner executes a scenario against a cluster and reports what happened.
package runner

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MiguelGarrido02/gpu-sim/internal/cluster"
	"github.com/MiguelGarrido02/gpu-sim/internal/generate"
	"github.com/MiguelGarrido02/gpu-sim/internal/scenario"
	"github.com/MiguelGarrido02/gpu-sim/internal/workload"
)

// Namespace is where scenario workloads are created. Kept separate from anything a user
// runs by hand so that clearing it between scenarios is safe.
const Namespace = "gpu-sim-scenarios"

// defaultWait applies to an assertion that sets neither `within` nor `settle`. Generous
// enough that a slow scheduler is not mistaken for a failing one.
const defaultWait = 60 * time.Second

type Runner struct {
	client *cluster.Client
	// Progress receives one line per step. Nil discards them, which is what tests want.
	Progress func(string)

	// faults records what each fault broke, in the order they fired. Assertions about
	// recovery are judged against these rather than against the cluster's current state,
	// because a pod on a deleted node keeps reporting Running for about a minute and a
	// check made only at the end cannot tell a survivor from a corpse.
	faults []faultRecord
}

// faultRecord is one fault and its casualties.
type faultRecord struct {
	name    string
	firedAt time.Time
	// disrupted maps a workload to the UIDs of its replicas that were Running when the
	// fault fired and were hit by it.
	disrupted map[string]map[string]bool
}

func New(client *cluster.Client) *Runner {
	return &Runner{client: client}
}

func (r *Runner) log(format string, args ...any) {
	if r.Progress != nil {
		r.Progress(fmt.Sprintf(format, args...))
	}
}

// Result is the outcome of one scenario.
type Result struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Passed      bool              `json:"passed"`
	Duration    time.Duration     `json:"-"`
	Cluster     ClusterSummary    `json:"cluster"`
	Assertions  []AssertionResult `json:"assertions"`
	Error       string            `json:"error,omitempty"`
}

type ClusterSummary struct {
	Topology  string `json:"topology"`
	Nodes     int    `json:"nodes"`
	Devices   int    `json:"devices"`
	Scheduler string `json:"scheduler"`
}

type AssertionResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	// Detail states what was expected against what was observed.
	Detail string `json:"detail"`
	// SchedulerSaid carries the scheduler's own words. On a failure this is usually the
	// only part worth reading.
	SchedulerSaid []string `json:"schedulerSaid,omitempty"`
	// Placement counts placed replicas per node.
	Placement map[string]int `json:"placement,omitempty"`
}

// Run executes one scenario end to end.
func (r *Runner) Run(ctx context.Context, s *scenario.Scenario) *Result {
	start := time.Now()
	result := &Result{
		Name:        s.Metadata.Name,
		Description: s.Metadata.Description,
		Cluster:     ClusterSummary{Scheduler: string(s.Spec.Cluster.Scheduler)},
	}

	r.faults = nil

	// Translation happens before anything is created, so a scenario asking a scheduler for
	// something it cannot express fails without leaving objects behind.
	objects := make(map[string]*workload.Objects, len(s.Spec.Workloads))
	for _, w := range s.Spec.Workloads {
		objs, err := workload.Translate(w, s.Spec.Cluster.Scheduler, Namespace, "")
		if err != nil {
			result.Error = err.Error()
			result.Duration = time.Since(start)
			return result
		}
		objects[w.Name] = objs
	}

	r.log("applying topology %s", s.Spec.Cluster.Topology)
	topo, err := r.client.ApplyTopology(ctx, s.Spec.Cluster.Topology)
	if err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start)
		return result
	}
	result.Cluster.Topology = topo.Name
	result.Cluster.Nodes = topo.Nodes
	result.Cluster.Devices = topo.Devices

	// The topology name is only known after applying it, and placement annotations
	// reference it, so workloads are re-translated now that it is available.
	for i, w := range s.Spec.Workloads {
		objs, err := workload.Translate(w, s.Spec.Cluster.Scheduler, Namespace, topo.Name)
		if err != nil {
			result.Error = err.Error()
			result.Duration = time.Since(start)
			return result
		}
		objects[s.Spec.Workloads[i].Name] = objs
	}

	if err := r.client.EnsureNamespace(ctx, Namespace); err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start)
		return result
	}
	if err := r.client.ClearNamespace(ctx, Namespace); err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start)
		return result
	}
	// Deletion is asynchronous, and submitting into a namespace still draining would
	// count the previous scenario's pods.
	if err := r.waitNamespaceEmpty(ctx); err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start)
		return result
	}

	if err := r.runTimeline(ctx, s, objects); err != nil {
		result.Error = err.Error()
		result.Duration = time.Since(start)
		return result
	}

	replicas := map[string]int{}
	for _, w := range s.Spec.Workloads {
		replicas[w.Name] = w.Replicas
	}

	result.Passed = true
	for _, a := range s.Spec.Assertions {
		ar := r.evaluate(ctx, a, replicas[a.Workload])
		if !ar.Passed {
			result.Passed = false
		}
		result.Assertions = append(result.Assertions, ar)
	}

	result.Duration = time.Since(start)
	return result
}

// event is one thing happening on a scenario's timeline.
type event struct {
	at       time.Duration
	retire   bool
	workload scenario.Workload
	fault    *scenario.Fault
}

// runTimeline submits and retires workloads at their declared offsets.
//
// Retirement is not a tidy-up step but part of the experiment. The upstream DRA allocator
// packs, filling each GPU from the lowest free placement upward, so a run that only ever
// submits leaves GPUs either full or untouched. Fragmentation appears when partitions are
// released out of the order they were taken — which is the state a real cluster spends most
// of its life in.
func (r *Runner) runTimeline(ctx context.Context, s *scenario.Scenario, objects map[string]*workload.Objects) error {
	var events []event
	for _, w := range s.Spec.Workloads {
		events = append(events, event{at: w.SubmitAt.Duration, workload: w})
		if w.RetireAt.Duration > 0 {
			events = append(events, event{at: w.RetireAt.Duration, retire: true, workload: w})
		}
	}
	for i := range s.Spec.Faults {
		events = append(events, event{at: s.Spec.Faults[i].At.Duration, fault: &s.Spec.Faults[i]})
	}
	// Stable sort keeps declaration order among events at the same offset, so a scenario
	// can rely on the order it wrote them in.
	sort.SliceStable(events, func(i, j int) bool { return events[i].at < events[j].at })

	start := time.Now()
	for _, e := range events {
		if wait := e.at - time.Since(start); wait > 0 {
			r.log("waiting %s", wait.Round(time.Second))
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		if e.fault != nil {
			r.log("injecting fault: %s", e.fault.Name)
			if err := r.fire(ctx, *e.fault); err != nil {
				return err
			}
			continue
		}

		if e.retire {
			r.log("retiring %s", e.workload.Name)
			if err := r.client.Retire(ctx, Namespace, e.workload.Name); err != nil {
				return err
			}
			continue
		}

		r.log("submitting %s (%d replicas, %d GPU each)", e.workload.Name, e.workload.Replicas, e.workload.GPUs)
		if err := r.client.Submit(ctx, Namespace, objects[e.workload.Name]); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) waitNamespaceEmpty(ctx context.Context) error {
	deadline := time.Now().Add(90 * time.Second)
	for {
		pods, err := r.client.AllPods(ctx, Namespace)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("namespace %s still had %d pods after clearing", Namespace, len(pods))
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// fire injects one fault and records what it broke.
//
// The snapshot is taken before the fault lands, so it captures who was running at the
// moment of failure; the casualties are then whichever of those the fault actually hit.
// Kubernetes does the evicting and rescheduling — what is recorded here is only enough to
// judge its reaction afterwards.
func (r *Runner) fire(ctx context.Context, f scenario.Fault) error {
	before, err := r.client.SnapshotPods(ctx, Namespace)
	if err != nil {
		return err
	}

	record := faultRecord{name: f.Name, firedAt: time.Now(), disrupted: map[string]map[string]bool{}}

	switch {
	case f.KillNode != "":
		if err := r.client.DeleteNode(ctx, f.KillNode); err != nil {
			return err
		}
		for _, pod := range before {
			if pod.Running && pod.Node == f.KillNode {
				record.add(pod)
			}
		}
		r.log("node %s deleted, %d replicas lost", f.KillNode, record.total())

	default:
		match, err := r.deviceMatch(ctx, *f.Degrade)
		if err != nil {
			return err
		}
		tainted, err := r.client.TaintDevices(ctx, match, f.Degrade.TaintEffect())
		if err != nil {
			return err
		}
		for _, pod := range before {
			if !pod.Running {
				continue
			}
			for _, device := range pod.Devices {
				if tainted[device] {
					record.add(pod)
					break
				}
			}
		}
		r.log("%d devices degraded, %d replicas lost", len(tainted), record.total())
	}

	r.faults = append(r.faults, record)
	return nil
}

// deviceMatch resolves a fault's target to a set of devices.
func (r *Runner) deviceMatch(ctx context.Context, d scenario.Degrade) (cluster.DeviceMatch, error) {
	if len(d.Devices) > 0 {
		return cluster.DeviceMatch{Attributes: d.Devices}, nil
	}

	label, ok := generate.LabelForLevel(d.Level)
	if !ok {
		return cluster.DeviceMatch{}, fmt.Errorf("unknown topology level %q, want one of %s",
			d.Level, strings.Join(generate.KnownLevels(), ", "))
	}
	nodes, err := r.client.NodesWithLabel(ctx, label, d.Value)
	if err != nil {
		return cluster.DeviceMatch{}, err
	}
	if len(nodes) == 0 {
		return cluster.DeviceMatch{}, fmt.Errorf("no node has %s %q, so the fault would break nothing", d.Level, d.Value)
	}
	return cluster.DeviceMatch{Nodes: nodes}, nil
}

func (f *faultRecord) add(pod cluster.PodState) {
	if f.disrupted[pod.Workload] == nil {
		f.disrupted[pod.Workload] = map[string]bool{}
	}
	f.disrupted[pod.Workload][pod.UID] = true
}

func (f faultRecord) total() int {
	n := 0
	for _, uids := range f.disrupted {
		n += len(uids)
	}
	return n
}

// disruptedFor gathers the UIDs every fault took from one workload, and when the last of
// them fired.
func (r *Runner) disruptedFor(name string) (map[string]bool, time.Time, bool) {
	uids := map[string]bool{}
	var last time.Time
	for _, record := range r.faults {
		for uid := range record.disrupted[name] {
			uids[uid] = true
		}
		if record.firedAt.After(last) {
			last = record.firedAt
		}
	}
	return uids, last, len(r.faults) > 0
}
