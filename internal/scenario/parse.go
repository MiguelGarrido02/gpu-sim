package scenario

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"sigs.k8s.io/yaml"
)

// ScheduledAll and ScheduledNone are the two symbolic values of Assertion.Scheduled; any
// other value must parse as a count.
const (
	ScheduledAll  = "all"
	ScheduledNone = "none"
)

// Load reads and validates a scenario. The returned scenario's topology path is resolved
// relative to the scenario file, so a suite of scenarios can share one topology by relative
// path and still be runnable from any working directory.
func Load(path string) (*Scenario, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading scenario: %w", err)
	}

	var s Scenario
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return nil, fmt.Errorf("parsing scenario %s: %w", path, err)
	}

	if !filepath.IsAbs(s.Spec.Cluster.Topology) && s.Spec.Cluster.Topology != "" {
		s.Spec.Cluster.Topology = filepath.Join(filepath.Dir(path), s.Spec.Cluster.Topology)
	}

	if err := s.Validate(); err != nil {
		return nil, fmt.Errorf("invalid scenario %s: %w", path, err)
	}
	return &s, nil
}

// Validate reports every problem it finds rather than stopping at the first, because
// scenarios are hand-written and fixing one mistake per run is tedious.
func (s *Scenario) Validate() error {
	var errs []error

	if s.APIVersion != APIVersion {
		errs = append(errs, fmt.Errorf("apiVersion is %q, want %q", s.APIVersion, APIVersion))
	}
	if s.Kind != Kind {
		errs = append(errs, fmt.Errorf("kind is %q, want %q", s.Kind, Kind))
	}
	if s.Metadata.Name == "" {
		errs = append(errs, errors.New("metadata.name is required"))
	}
	if s.Spec.Cluster.Topology == "" {
		errs = append(errs, errors.New("spec.cluster.topology is required"))
	}
	switch s.Spec.Cluster.Scheduler {
	case SchedulerKAI, SchedulerDefault:
	case "":
		errs = append(errs, fmt.Errorf("spec.cluster.scheduler is required, want %q or %q",
			SchedulerKAI, SchedulerDefault))
	default:
		errs = append(errs, fmt.Errorf("spec.cluster.scheduler is %q, want %q or %q",
			s.Spec.Cluster.Scheduler, SchedulerKAI, SchedulerDefault))
	}
	if len(s.Spec.Workloads) == 0 {
		errs = append(errs, errors.New("spec.workloads is empty"))
	}
	if len(s.Spec.Assertions) == 0 {
		errs = append(errs, errors.New("spec.assertions is empty: a scenario that checks nothing always passes"))
	}

	names := map[string]bool{}
	for i, w := range s.Spec.Workloads {
		errs = append(errs, w.validate(i)...)
		if names[w.Name] {
			errs = append(errs, fmt.Errorf("workload %q is declared more than once", w.Name))
		}
		names[w.Name] = true
	}

	for i, a := range s.Spec.Assertions {
		errs = append(errs, a.validate(i, names)...)
	}

	return errors.Join(errs...)
}

func (w Workload) validate(i int) []error {
	var errs []error
	where := fmt.Sprintf("workload[%d]", i)
	if w.Name != "" {
		where = fmt.Sprintf("workload %q", w.Name)
	} else {
		errs = append(errs, fmt.Errorf("%s has no name", where))
	}

	if w.Replicas <= 0 {
		errs = append(errs, fmt.Errorf("%s has replicas %d, want at least 1", where, w.Replicas))
	}
	if w.GPUs < 0 {
		errs = append(errs, fmt.Errorf("%s has gpus %d, want zero or more", where, w.GPUs))
	}
	if w.Placement != nil && w.Placement.Required == "" {
		errs = append(errs, fmt.Errorf("%s declares placement with no required level", where))
	}
	if w.SubmitAt.Duration < 0 {
		errs = append(errs, fmt.Errorf("%s has a negative submitAt", where))
	}
	if w.RetireAt.Duration != 0 && w.RetireAt.Duration <= w.SubmitAt.Duration {
		errs = append(errs, fmt.Errorf("%s retires at %s, which is not after it is submitted at %s",
			where, w.RetireAt.Duration, w.SubmitAt.Duration))
	}

	return errs
}

func (a Assertion) validate(i int, workloads map[string]bool) []error {
	var errs []error
	where := fmt.Sprintf("assertion[%d]", i)
	if a.Name != "" {
		where = fmt.Sprintf("assertion %q", a.Name)
	} else {
		errs = append(errs, fmt.Errorf("%s has no name: reports print it, so it has to say what should be true", where))
	}

	// A fragmentation assertion is about the cluster, not about one workload.
	if a.Workload == "" && a.Fragmentation == nil {
		errs = append(errs, fmt.Errorf("%s names no workload", where))
	} else if a.Workload != "" && len(workloads) > 0 && !workloads[a.Workload] {
		errs = append(errs, fmt.Errorf("%s references unknown workload %q", where, a.Workload))
	}

	// Exactly one condition. Two are ambiguous about which one failed; none passes
	// silently, which is the worst outcome a test framework can produce.
	set := 0
	if a.Scheduled != "" {
		set++
		if a.Scheduled != ScheduledAll && a.Scheduled != ScheduledNone {
			if _, err := strconv.Atoi(a.Scheduled); err != nil {
				errs = append(errs, fmt.Errorf("%s has scheduled %q, want %q, %q or a number",
					where, a.Scheduled, ScheduledAll, ScheduledNone))
			}
		}
	}
	if a.ConfinedTo != "" {
		set++
	}
	if a.Running != nil {
		set++
	}
	if len(a.AllocatedDevices) > 0 {
		set++
	}
	if a.UnschedulableReason != "" {
		set++
	}
	if a.Fragmentation != nil {
		set++
		if a.Fragmentation.AtLeast == nil && a.Fragmentation.AtMost == nil {
			errs = append(errs, fmt.Errorf("%s sets fragmentation with neither atLeast nor atMost", where))
		}
	}

	switch set {
	case 1:
	case 0:
		errs = append(errs, fmt.Errorf("%s sets no condition, so it would always pass", where))
	default:
		errs = append(errs, fmt.Errorf("%s sets %d conditions, want exactly one", where, set))
	}

	if a.Within.Duration > 0 && a.Settle.Duration > 0 {
		errs = append(errs, fmt.Errorf("%s sets both within and settle, want one: "+
			"within polls until the condition holds, settle waits the full period and then checks", where))
	}

	// A condition asserting that nothing happened cannot use `within`, which would be
	// satisfied immediately — before the scheduler had a chance to act.
	if a.Scheduled == ScheduledNone && a.Within.Duration > 0 {
		errs = append(errs, fmt.Errorf("%s asserts nothing is scheduled but uses within, want settle: "+
			"within would pass instantly, before the scheduler had tried", where))
	}

	return errs
}

// ExpectedScheduled resolves the Scheduled field against a workload's replica count.
func (a Assertion) ExpectedScheduled(replicas int) (int, error) {
	switch a.Scheduled {
	case ScheduledAll:
		return replicas, nil
	case ScheduledNone:
		return 0, nil
	default:
		return strconv.Atoi(a.Scheduled)
	}
}
