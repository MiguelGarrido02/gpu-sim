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
			mutate:  func(s *Scenario) { s.Spec.Cluster.Scheduler = "yunikorn" },
			wantErr: `scheduler is "yunikorn"`,
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

func faultScenario(mutate func(*Fault)) *Scenario {
	s := valid()
	f := Fault{
		Name:    "a node dies",
		At:      metav1.Duration{Duration: 30 * time.Second},
		Degrade: &Degrade{Level: "rack", Value: "rack-1"},
	}
	mutate(&f)
	s.Spec.Faults = []Fault{f}
	return s
}

func TestValidateFaults(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Fault)
		wantErr string
	}{
		{
			name:    "neither degrade nor killNode",
			mutate:  func(f *Fault) { f.Degrade = nil },
			wantErr: "sets neither degrade nor killNode",
		},
		{
			name:    "both degrade and killNode",
			mutate:  func(f *Fault) { f.KillNode = "gpu-node-1" },
			wantErr: "sets both degrade and killNode",
		},
		{
			name:    "degrade by level and devices at once",
			mutate:  func(f *Fault) { f.Degrade.Devices = map[string]string{"profile": "1g.10gb"} },
			wantErr: "by both level and devices",
		},
		{
			name:    "a level with no value",
			mutate:  func(f *Fault) { f.Degrade.Value = "" },
			wantErr: "needs both level and value",
		},
		{
			name:    "an effect the API does not have",
			mutate:  func(f *Fault) { f.Degrade.Effect = "PreferNoSchedule" },
			wantErr: `effect "PreferNoSchedule"`,
		},
		{
			// A fault at zero would break the cluster before anything ran on it, so it
			// could only ever disrupt nothing.
			name:    "a fault at offset zero",
			mutate:  func(f *Fault) { f.At = metav1.Duration{} },
			wantErr: "want a positive offset",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := faultScenario(tt.mutate).Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid fault")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateAcceptsFaults(t *testing.T) {
	for _, mutate := range []func(*Fault){
		func(*Fault) {},
		func(f *Fault) { f.Degrade = nil; f.KillNode = "gpu-node-1" },
		func(f *Fault) { f.Degrade = &Degrade{Devices: map[string]string{"profile": "1g.10gb"}} },
		func(f *Fault) { f.Degrade.Effect = EffectNoSchedule },
	} {
		if err := faultScenario(mutate).Validate(); err != nil {
			t.Errorf("Validate rejected a valid fault: %v", err)
		}
	}
}

// TestFaultAssertionsNeedAFault guards a scenario that asks about recovery without breaking
// anything: both assertions read a snapshot taken when a fault fires, so with no fault they
// would report a confusing zero rather than the mistake in the scenario.
func TestFaultAssertionsNeedAFault(t *testing.T) {
	disrupted := 2
	for _, a := range []Assertion{
		{Name: "blast radius", Workload: "training", Disrupted: &disrupted},
		{Name: "recovery", Workload: "training", RescheduledWithin: metav1.Duration{Duration: time.Minute}},
	} {
		s := valid()
		s.Spec.Assertions = []Assertion{a}

		err := s.Validate()
		if err == nil {
			t.Fatalf("Validate accepted %q with no fault declared", a.Name)
		}
		if !strings.Contains(err.Error(), "the scenario declares none") {
			t.Errorf("error %q does not explain the problem", err)
		}
	}
}

func TestDefaultTaintEffectIsNoExecute(t *testing.T) {
	// A fault that only blocks new work is a maintenance window rather than a failure.
	if got := (Degrade{}).TaintEffect(); got != EffectNoExecute {
		t.Errorf("default effect = %q, want %q", got, EffectNoExecute)
	}
	if got := (Degrade{Effect: EffectNoSchedule}).TaintEffect(); got != EffectNoSchedule {
		t.Errorf("explicit effect = %q, want it respected", got)
	}
}

// TestSchedulersAreAccepted keeps the list of targets and the validation in step: adding a
// scheduler to Schedulers() without teaching Validate about it would reject every scenario
// that named it.
func TestSchedulersAreAccepted(t *testing.T) {
	for _, name := range Schedulers() {
		s := valid()
		s.Spec.Cluster.Scheduler = Scheduler(name)
		// The default scheduler cannot express a gang, and the fixture declares one.
		if Scheduler(name) == SchedulerDefault {
			s.Spec.Workloads[0].Gang = false
		}
		if err := s.Validate(); err != nil {
			t.Errorf("Validate rejected scheduler %q: %v", name, err)
		}
	}
}
