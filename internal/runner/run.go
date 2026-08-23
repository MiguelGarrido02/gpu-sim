// Package runner executes a scenario against a cluster and reports what happened.
package runner

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/MiguelGarrido02/gpu-sim/internal/cluster"
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
