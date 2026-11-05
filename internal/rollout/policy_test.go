package rollout

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

// validSpec is a complete policy for a fleet of at least 100 devices.
func validSpec() PolicySpec {
	return PolicySpec{
		Stages: []StageSpec{{Percent: 10}, {Percent: 50}, {Percent: 100}},
		Safety: SafetySpec{MaxFailureBPS: ptr(200)},
	}
}

func violationFields(t *testing.T, err error) []string {
	t.Helper()
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want a *PolicyError", err)
	}
	fields := make([]string, len(pe.Violations))
	for i, v := range pe.Violations {
		fields[i] = v.Field
	}
	return fields
}

func TestNormalizeFillsDefaultsExplicitly(t *testing.T) {
	p, err := validSpec().Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if got := p.Safety.RolloutMaxFailureBPS; got != 200 {
		t.Errorf("RolloutMaxFailureBPS = %d, want the stage limit of 200", got)
	}
	if got := p.Safety.RequiredCoverageBPS; got != 9000 {
		t.Errorf("RequiredCoverageBPS = %d, want 9000", got)
	}
	if got := p.Safety.MaxUnknownBPS; got != 500 {
		t.Errorf("MaxUnknownBPS = %d, want 500", got)
	}
	if got := p.Safety.StaleAfter.Std(); got != 15*time.Minute {
		t.Errorf("StaleAfter = %v, want 15m", got)
	}
	if got := p.Safety.OnBreach; got != OnBreachHalt {
		t.Errorf("OnBreach = %q, want halt", got)
	}
	for i, s := range p.Stages {
		if s.Soak.Std() != 10*time.Minute || s.Deadline.Std() != time.Hour || s.MinObservations != 1 {
			t.Errorf("stage %d defaults = %+v", i, s)
		}
	}
}

func TestNormalizeKeepsExplicitZeroValues(t *testing.T) {
	spec := validSpec()
	spec.Safety.MaxFailureBPS = ptr(0)
	spec.Safety.MaxUnknownBPS = ptr(0)
	spec.Stages[0].Soak = ptr(Duration(0))
	p, err := spec.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Safety.MaxFailureBPS != 0 || p.Safety.MaxUnknownBPS != 0 {
		t.Errorf("explicit zero limits were replaced: %+v", p.Safety)
	}
	if p.Stages[0].Soak != 0 {
		t.Errorf("explicit zero soak was replaced: %v", p.Stages[0].Soak)
	}
}

func TestNormalizeRequiresAnExplicitFailureThreshold(t *testing.T) {
	spec := validSpec()
	spec.Safety.MaxFailureBPS = nil
	_, err := spec.Normalize()
	if fields := violationFields(t, err); !strings.Contains(strings.Join(fields, ","), "safety.max_failure_bps") {
		t.Fatalf("violations = %v", fields)
	}
}

func TestNormalizeRejectsInvalidPolicies(t *testing.T) {
	cases := []struct {
		name  string
		field string
		edit  func(*PolicySpec)
	}{
		{"no stages", "stages", func(s *PolicySpec) { s.Stages = nil }},
		{"too many stages", "stages", func(s *PolicySpec) {
			s.Stages = make([]StageSpec, 21)
			for i := range s.Stages {
				s.Stages[i].Percent = i + 1
			}
			s.Stages[20].Percent = 100
		}},
		{"percent below range", "stages[0].percent", func(s *PolicySpec) { s.Stages[0].Percent = 0 }},
		{"percent above range", "stages[2].percent", func(s *PolicySpec) { s.Stages[2].Percent = 101 }},
		{"repeated percent", "stages[1].percent", func(s *PolicySpec) { s.Stages[1].Percent = 10 }},
		{"decreasing percent", "stages[1].percent", func(s *PolicySpec) { s.Stages[1].Percent = 5 }},
		{"final stage below 100", "stages[2].percent", func(s *PolicySpec) { s.Stages[2].Percent = 90 }},
		{"negative soak", "stages[0].soak", func(s *PolicySpec) { s.Stages[0].Soak = ptr(Duration(-time.Second)) }},
		{"excessive soak", "stages[0].soak", func(s *PolicySpec) { s.Stages[0].Soak = ptr(Duration(31 * 24 * time.Hour)) }},
		{"zero deadline", "stages[0].deadline", func(s *PolicySpec) { s.Stages[0].Deadline = ptr(Duration(0)) }},
		{"deadline equal to stale window", "stages[0].deadline", func(s *PolicySpec) {
			s.Stages[0].Deadline = ptr(Duration(15 * time.Minute))
		}},
		{"zero min observations", "stages[0].min_observations", func(s *PolicySpec) { s.Stages[0].MinObservations = ptr(0) }},
		{"failure limit above 100%", "safety.max_failure_bps", func(s *PolicySpec) { s.Safety.MaxFailureBPS = ptr(10001) }},
		{"negative failure limit", "safety.max_failure_bps", func(s *PolicySpec) { s.Safety.MaxFailureBPS = ptr(-1) }},
		{"rollout limit above 100%", "safety.rollout_max_failure_bps", func(s *PolicySpec) { s.Safety.RolloutMaxFailureBPS = ptr(10001) }},
		{"zero coverage", "safety.required_coverage_bps", func(s *PolicySpec) { s.Safety.RequiredCoverageBPS = ptr(0) }},
		{"coverage above 100%", "safety.required_coverage_bps", func(s *PolicySpec) { s.Safety.RequiredCoverageBPS = ptr(10001) }},
		{"unknown limit above 100%", "safety.max_unknown_bps", func(s *PolicySpec) { s.Safety.MaxUnknownBPS = ptr(10001) }},
		{"stale window too short", "safety.stale_after", func(s *PolicySpec) { s.Safety.StaleAfter = ptr(Duration(time.Millisecond)) }},
		{"unknown breach action", "safety.on_breach", func(s *PolicySpec) { s.Safety.OnBreach = "panic" }},
		{"malformed selector key", "selector", func(s *PolicySpec) { s.Selector = map[string]string{"bad key": "x"} }},
		{"empty selector value", "selector", func(s *PolicySpec) { s.Selector = map[string]string{"site": ""} }},
		{"malformed excluded device", "exclude_devices", func(s *PolicySpec) { s.ExcludeDevices = []string{"bad id"} }},
		{"duplicate excluded device", "exclude_devices", func(s *PolicySpec) { s.ExcludeDevices = []string{"a", "a"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec()
			tc.edit(&spec)
			_, err := spec.Normalize()
			if fields := violationFields(t, err); !strings.Contains(strings.Join(fields, ","), tc.field) {
				t.Fatalf("violations = %v, want %q among them", fields, tc.field)
			}
		})
	}
}

func TestNormalizeReportsEveryViolation(t *testing.T) {
	spec := validSpec()
	spec.Stages[0].Percent = 0
	spec.Safety.MaxFailureBPS = ptr(-1)
	_, err := spec.Normalize()
	if fields := violationFields(t, err); len(fields) < 2 {
		t.Fatalf("violations = %v, want all of them reported at once", fields)
	}
}

func TestPolicyDigestSurvivesPostgresStyleRoundTrip(t *testing.T) {
	spec := validSpec()
	spec.Selector = map[string]string{"site": "lab", "ring": "a", "model": "x1"}
	p, err := spec.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	canonical, digest, err := p.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	// jsonb reorders keys and normalizes whitespace, so simulate that by decoding
	// into a generic value and encoding it again.
	var generic any
	if err := json.Unmarshal(canonical, &generic); err != nil {
		t.Fatalf("decode: %v", err)
	}
	reordered, err := json.Marshal(generic)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var restored Policy
	if err := json.Unmarshal(reordered, &restored); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	_, restoredDigest, err := restored.Canonical()
	if err != nil {
		t.Fatalf("Canonical after round trip: %v", err)
	}
	if restoredDigest != digest {
		t.Fatal("policy digest changed after a storage round trip")
	}
}

func TestPlanComputesCohortsAndSkipsEmptyStages(t *testing.T) {
	spec := PolicySpec{
		Stages: []StageSpec{{Percent: 1}, {Percent: 10}, {Percent: 50}, {Percent: 100}},
		Safety: SafetySpec{MaxFailureBPS: ptr(200)},
	}
	p, err := spec.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	plan, err := p.Plan(3)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	wantCohorts := []int{1, 0, 1, 1}
	wantTargets := []int{1, 1, 2, 3}
	for i, stage := range plan {
		if stage.Ordinal != i+1 || stage.CohortSize != wantCohorts[i] || stage.Target != wantTargets[i] {
			t.Errorf("stage %d = %+v, want cohort %d target %d", i, stage, wantCohorts[i], wantTargets[i])
		}
	}
}

func TestPlanRejectsImpossibleSampleSizes(t *testing.T) {
	spec := PolicySpec{
		Stages: []StageSpec{{Percent: 1, MinObservations: ptr(50)}, {Percent: 100}},
		Safety: SafetySpec{MaxFailureBPS: ptr(200)},
	}
	p, err := spec.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	_, err = p.Plan(1000) // The first stage exposes 10 devices, so 50 is unreachable.
	fields := violationFields(t, err)
	if len(fields) != 1 || fields[0] != "stages[0].min_observations" {
		t.Fatalf("violations = %v", fields)
	}
	if msg := err.Error(); !strings.Contains(msg, "10") || !strings.Contains(msg, "50") {
		t.Fatalf("error %q should state both the cohort size and the minimum", msg)
	}

	// The boundary: a cohort of exactly 50 can satisfy a minimum of 50.
	if _, err := p.Plan(5000); err != nil {
		t.Fatalf("Plan(5000): %v", err)
	}
}

func TestPlanIgnoresSampleSizeOfEmptyStage(t *testing.T) {
	spec := PolicySpec{
		Stages: []StageSpec{{Percent: 50}, {Percent: 60, MinObservations: ptr(5)}, {Percent: 100}},
		Safety: SafetySpec{MaxFailureBPS: ptr(200)},
	}
	p, err := spec.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	// With one device, the 60% stage adds nobody, so its minimum can't matter.
	if _, err := p.Plan(1); err != nil {
		t.Fatalf("Plan(1): %v", err)
	}
}

func TestPlanRejectsEmptyMembership(t *testing.T) {
	p, err := validSpec().Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	_, err = p.Plan(0)
	if fields := violationFields(t, err); len(fields) != 1 || fields[0] != "membership" {
		t.Fatalf("violations = %v", fields)
	}
}

func TestDurationJSON(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"90s"`), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Std() != 90*time.Second {
		t.Fatalf("decoded %v", d.Std())
	}
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(out) != `"1m30s"` {
		t.Fatalf("encoded %s", out)
	}
	for _, bad := range []string{`90`, `"soon"`, `null`} {
		var got Duration
		if err := json.Unmarshal([]byte(bad), &got); err == nil && bad != "null" {
			t.Errorf("decoding %s succeeded", bad)
		}
	}
}
