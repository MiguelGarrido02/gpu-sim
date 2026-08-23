package scenario

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func valid() *Scenario {
	return &Scenario{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{Name: "test"},
		Spec: Spec{
			Cluster: Cluster{Topology: "../topologies/two-racks-h100.yaml", Scheduler: SchedulerKAI},
			Workloads: []Workload{
				{Name: "training", Replicas: 12, GPUs: 1, Gang: true},
			},
			Assertions: []Assertion{
				{Name: "placed", Workload: "training", Scheduled: ScheduledAll},
			},
		},
	}
}

func TestValidateAccepts(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Errorf("Validate rejected a valid scenario: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Scenario)
		wantErr string
	}{
		{
			name:    "unknown scheduler",
			mutate:  func(s *Scenario) { s.Spec.Cluster.Scheduler = "volcano" },
			wantErr: `scheduler is "volcano"`,
		},
		{
			name:    "no topology",
			mutate:  func(s *Scenario) { s.Spec.Cluster.Topology = "" },
			wantErr: "topology is required",
		},
		{
			name:    "no assertions",
			mutate:  func(s *Scenario) { s.Spec.Assertions = nil },
			wantErr: "always passes",
		},
		{
			name:    "assertion references an unknown workload",
			mutate:  func(s *Scenario) { s.Spec.Assertions[0].Workload = "nope" },
			wantErr: "unknown workload",
		},
		{
			name: "duplicate workload names",
			mutate: func(s *Scenario) {
				s.Spec.Workloads = append(s.Spec.Workloads, s.Spec.Workloads[0])
			},
			wantErr: "declared more than once",
		},
		{
			name:    "assertion with no condition",
			mutate:  func(s *Scenario) { s.Spec.Assertions[0].Scheduled = "" },
			wantErr: "sets no condition",
		},
		{
			name: "assertion with two conditions",
			mutate: func(s *Scenario) {
				s.Spec.Assertions[0].ConfinedTo = "rack"
			},
			wantErr: "sets 2 conditions",
		},
		{
			name: "both within and settle",
			mutate: func(s *Scenario) {
				s.Spec.Assertions[0].Within = metav1.Duration{Duration: time.Minute}
				s.Spec.Assertions[0].Settle = metav1.Duration{Duration: time.Minute}
			},
			wantErr: "sets both within and settle",
		},
		{
			name:    "zero replicas",
			mutate:  func(s *Scenario) { s.Spec.Workloads[0].Replicas = 0 },
			wantErr: "want at least 1",
		},
		{
			name:    "unparseable scheduled count",
			mutate:  func(s *Scenario) { s.Spec.Assertions[0].Scheduled = "most" },
			wantErr: `scheduled "most"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := valid()
			tt.mutate(s)
			err := s.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid scenario")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestValidateRejectsPollingANegative guards the distinction the schema exists to teach.
// `within` succeeds the moment its condition holds, so using it to assert that nothing was
// scheduled would pass instantly — before the scheduler had been given a chance to act, and
// therefore while proving nothing at all.
func TestValidateRejectsPollingANegative(t *testing.T) {
	s := valid()
	s.Spec.Assertions[0].Scheduled = ScheduledNone
	s.Spec.Assertions[0].Within = metav1.Duration{Duration: 30 * time.Second}

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate accepted `scheduled: none` with `within`")
	}
	if !strings.Contains(err.Error(), "want settle") {
		t.Errorf("error %q does not point at settle", err)
	}
}

func TestValidateAcceptsSettlingANegative(t *testing.T) {
	s := valid()
	s.Spec.Assertions[0].Scheduled = ScheduledNone
	s.Spec.Assertions[0].Settle = metav1.Duration{Duration: 30 * time.Second}

	if err := s.Validate(); err != nil {
		t.Errorf("Validate rejected `scheduled: none` with `settle`: %v", err)
	}
}

func TestExpectedScheduled(t *testing.T) {
	tests := []struct {
		scheduled string
		replicas  int
		want      int
	}{
		{ScheduledAll, 12, 12},
		{ScheduledNone, 12, 0},
		{"7", 12, 7},
	}
	for _, tt := range tests {
		got, err := Assertion{Scheduled: tt.scheduled}.ExpectedScheduled(tt.replicas)
		if err != nil {
			t.Errorf("ExpectedScheduled(%q): %v", tt.scheduled, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ExpectedScheduled(%q, %d) = %d, want %d", tt.scheduled, tt.replicas, got, tt.want)
		}
	}
}

// TestValidateReportsEveryProblem checks validation does not stop at the first error, so a
// hand-written scenario can be fixed in one pass.
func TestValidateReportsEveryProblem(t *testing.T) {
	s := valid()
	s.Spec.Cluster.Topology = ""
	s.Spec.Workloads[0].Replicas = 0

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate accepted an invalid scenario")
	}
	for _, want := range []string{"topology is required", "want at least 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestValidateRejectsRetirementBeforeSubmission guards a timeline that cannot happen. A
// workload retired before it is submitted would silently never run, and the scenario would
// then assert against an empty cluster.
func TestValidateRejectsRetirementBeforeSubmission(t *testing.T) {
	s := valid()
	s.Spec.Workloads[0].SubmitAt = metav1.Duration{Duration: 60 * time.Second}
	s.Spec.Workloads[0].RetireAt = metav1.Duration{Duration: 30 * time.Second}

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate accepted a workload retired before it is submitted")
	}
	if !strings.Contains(err.Error(), "not after it is submitted") {
		t.Errorf("error %q does not explain the problem", err)
	}
}

func TestValidateAcceptsRetirement(t *testing.T) {
	s := valid()
	s.Spec.Workloads[0].SubmitAt = metav1.Duration{Duration: 20 * time.Second}
	s.Spec.Workloads[0].RetireAt = metav1.Duration{Duration: 60 * time.Second}

	if err := s.Validate(); err != nil {
		t.Errorf("Validate rejected a valid retirement: %v", err)
	}
}

// TestFragmentationAssertionNeedsNoWorkload covers the one assertion that is about the
// cluster rather than about a workload.
func TestFragmentationAssertionNeedsNoWorkload(t *testing.T) {
	atLeast := 3
	s := valid()
	s.Spec.Assertions = []Assertion{{
		Name:          "capacity is stranded",
		Fragmentation: &FragmentationAssertion{AtLeast: &atLeast},
	}}

	if err := s.Validate(); err != nil {
		t.Errorf("Validate rejected a workload-free fragmentation assertion: %v", err)
	}
}

func TestFragmentationAssertionNeedsABound(t *testing.T) {
	s := valid()
	s.Spec.Assertions = []Assertion{{
		Name:          "capacity is stranded",
		Fragmentation: &FragmentationAssertion{},
	}}

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate accepted a fragmentation assertion with no bound")
	}
	if !strings.Contains(err.Error(), "neither atLeast nor atMost") {
		t.Errorf("error %q does not explain the problem", err)
	}
}
