package rollout

import (
	"strings"
	"testing"
	"time"
)

var evalNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// evalPolicy has a 2% limit, 90% coverage, 5% unknown limit, and a 10m soak.
func evalPolicy(t *testing.T, edit func(*PolicySpec)) Policy {
	t.Helper()
	spec := PolicySpec{
		Stages: []StageSpec{
			{Percent: 10, MinObservations: ptr(5)},
			{Percent: 100, MinObservations: ptr(50)},
		},
		Safety: SafetySpec{MaxFailureBPS: ptr(200)},
	}
	if edit != nil {
		edit(&spec)
	}
	p, err := spec.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return p
}

// running is a first-stage evaluation with plenty of time left on the deadline.
func running(o Outcomes) Evaluation {
	return Evaluation{
		State:        StateRunning,
		CurrentStage: 1,
		Stages:       []StageOutcomes{{Ordinal: 1, Outcomes: o}},
		DeadlineAt:   evalNow.Add(time.Hour),
		Now:          evalNow,
	}
}

func soaking(o Outcomes, elapsed time.Duration) Evaluation {
	e := running(o)
	e.State = StateSoaking
	e.SoakElapsed = elapsed
	return e
}

func TestFailureRateBoundaries(t *testing.T) {
	policy := evalPolicy(t, func(s *PolicySpec) {
		s.Stages[0].MinObservations = ptr(50)
	})
	cases := []struct {
		name           string
		cohort         int
		succeeded      int
		failed         int
		wantHalt       bool
		wantHaltReason HaltReason
	}{
		{name: "exactly at the limit passes", cohort: 1000, succeeded: 49, failed: 1},
		{name: "just over the limit breaches", cohort: 1000, succeeded: 97, failed: 2, wantHalt: true, wantHaltReason: HaltStageBreach},
		{name: "exactly at the limit at a larger scale passes", cohort: 1000, succeeded: 98, failed: 2},
		{name: "below the sample size waits", cohort: 1000, succeeded: 8, failed: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := policy.Evaluate(running(Outcomes{Cohort: tc.cohort, Succeeded: tc.succeeded, Failed: tc.failed}))
			if got := v.Action == ActionHalt; got != tc.wantHalt {
				t.Fatalf("verdict = %+v, want halt = %v", v, tc.wantHalt)
			}
			if tc.wantHalt && v.Reason != tc.wantHaltReason {
				t.Fatalf("reason = %q, want %q", v.Reason, tc.wantHaltReason)
			}
		})
	}
}

func TestZeroToleranceHaltsOnAnyFailureOnceObserved(t *testing.T) {
	policy := evalPolicy(t, func(s *PolicySpec) { s.Safety.MaxFailureBPS = ptr(0) })
	v := policy.Evaluate(running(Outcomes{Cohort: 100, Succeeded: 4, Failed: 1}))
	if v.Action != ActionHalt {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestCertainBreachHaltsBeforeTheSampleSizeIsReached(t *testing.T) {
	policy := evalPolicy(t, func(s *PolicySpec) { s.Stages[0].MinObservations = ptr(50) })

	// 2% of 100 is 2, so 3 failures breach even if the other 97 all succeed.
	v := policy.Evaluate(running(Outcomes{Cohort: 100, Failed: 3}))
	if v.Action != ActionHalt || v.Reason != HaltCertainBreach {
		t.Fatalf("verdict = %+v, want a certain breach", v)
	}

	// 2 failures could still end up exactly at the limit, so keep going.
	v = policy.Evaluate(running(Outcomes{Cohort: 100, Failed: 2}))
	if v.Action == ActionHalt {
		t.Fatalf("verdict = %+v, want no halt yet", v)
	}
}

func TestEarlierSuccessesDoNotDiluteANewCohort(t *testing.T) {
	policy := evalPolicy(t, nil)
	e := running(Outcomes{Cohort: 100, Succeeded: 90, Failed: 10})
	e.CurrentStage = 2
	e.Stages = []StageOutcomes{
		{Ordinal: 1, Outcomes: Outcomes{Cohort: 1000, Succeeded: 1000}},
		{Ordinal: 2, Outcomes: Outcomes{Cohort: 100, Succeeded: 90, Failed: 10}},
	}
	// Across everything the rate is under 1%, but the new cohort is failing at 10%.
	v := policy.Evaluate(e)
	if v.Action != ActionHalt || v.Reason != HaltStageBreach {
		t.Fatalf("verdict = %+v, want a stage breach", v)
	}
}

func TestRolloutWideBreach(t *testing.T) {
	policy := evalPolicy(t, func(s *PolicySpec) {
		s.Safety.MaxFailureBPS = ptr(500)
		s.Safety.RolloutMaxFailureBPS = ptr(100)
		s.Stages[0].MinObservations = ptr(10)
	})

	v := policy.Evaluate(running(Outcomes{Cohort: 1000, Succeeded: 97, Failed: 3}))
	if v.Action != ActionHalt || v.Reason != HaltRolloutBreach {
		t.Fatalf("verdict = %+v, want a rollout-wide breach", v)
	}

	// Exactly at the rollout-wide limit is allowed.
	v = policy.Evaluate(running(Outcomes{Cohort: 1000, Succeeded: 99, Failed: 1}))
	if v.Action == ActionHalt {
		t.Fatalf("verdict = %+v, want no halt", v)
	}
}

func TestRolloutWideCheckRespectsTheMinimumSampleSize(t *testing.T) {
	policy := evalPolicy(t, func(s *PolicySpec) {
		s.Safety.MaxFailureBPS = ptr(5000)
		s.Safety.RolloutMaxFailureBPS = ptr(100)
		s.Stages[0].MinObservations = ptr(10)
	})
	// One failure of one observed is 100%, but that's below the sample size.
	v := policy.Evaluate(running(Outcomes{Cohort: 1000, Failed: 1}))
	if v.Action == ActionHalt {
		t.Fatalf("verdict = %+v, want no halt below the sample size", v)
	}
}

func TestLateFailureInAPassedStageHaltsAndNamesTheStage(t *testing.T) {
	policy := evalPolicy(t, nil)
	e := running(Outcomes{Cohort: 900})
	e.CurrentStage = 2
	e.Stages = []StageOutcomes{
		{Ordinal: 1, Outcomes: Outcomes{Cohort: 100, Succeeded: 90, Failed: 10}},
		{Ordinal: 2, Outcomes: Outcomes{Cohort: 900}},
	}
	policy.Safety.RolloutMaxFailureBPS = FullBPS // Isolate the check from the rollout-wide one.
	v := policy.Evaluate(e)
	if v.Action != ActionHalt || v.Reason != HaltLateRegression {
		t.Fatalf("verdict = %+v, want a late regression", v)
	}
	if !strings.Contains(v.Detail, "stage 1") {
		t.Fatalf("detail %q should name stage 1", v.Detail)
	}
}

func TestCoverageGateBoundaries(t *testing.T) {
	policy := evalPolicy(t, func(s *PolicySpec) {
		s.Stages[0].MinObservations = ptr(5)
		s.Safety.RequiredCoverageBPS = ptr(9000)
	})
	cases := []struct {
		name      string
		outcomes  Outcomes
		want      Action
		wantWaits string
	}{
		{"exactly at coverage", Outcomes{Cohort: 100, Succeeded: 90}, ActionEnterSoak, ""},
		{"just under coverage", Outcomes{Cohort: 100, Succeeded: 89}, ActionNone, "coverage"},
		{"coverage rounds up", Outcomes{Cohort: 11, Succeeded: 9}, ActionNone, "coverage"},
		{"coverage rounds up, met", Outcomes{Cohort: 11, Succeeded: 10}, ActionEnterSoak, ""},
		{"coverage met but not the minimum", Outcomes{Cohort: 4, Succeeded: 4}, ActionNone, "coverage"},
		{"failures count as observed", Outcomes{Cohort: 100, Succeeded: 89, Failed: 1}, ActionEnterSoak, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := policy.Evaluate(running(tc.outcomes))
			if v.Action != tc.want {
				t.Fatalf("verdict = %+v, want %q", v, tc.want)
			}
			if tc.wantWaits != "" && !strings.Contains(v.Waiting, tc.wantWaits) {
				t.Fatalf("waiting = %q, want it to mention %q", v.Waiting, tc.wantWaits)
			}
		})
	}
}

func TestMissingReportsAreNeverSuccesses(t *testing.T) {
	policy := evalPolicy(t, nil)
	// Nobody has reported, and nobody has failed. That must not look healthy.
	v := policy.Evaluate(running(Outcomes{Cohort: 100, Unknown: 100}))
	if v.Action != ActionNone {
		t.Fatalf("verdict = %+v, want waiting", v)
	}
	v = policy.Evaluate(soaking(Outcomes{Cohort: 100, Succeeded: 90, Unknown: 10}, time.Hour))
	if v.Action == ActionPromote {
		t.Fatalf("verdict = %+v: 10%% unknown exceeds the 5%% limit and must not promote", v)
	}
}

func TestSoakMustElapseBeforePromotion(t *testing.T) {
	policy := evalPolicy(t, nil)
	o := Outcomes{Cohort: 100, Succeeded: 100}

	v := policy.Evaluate(soaking(o, 10*time.Minute-time.Second))
	if v.Action != ActionNone || !strings.Contains(v.Waiting, "soak") {
		t.Fatalf("verdict = %+v, want waiting on soak", v)
	}
	if v := policy.Evaluate(soaking(o, 10*time.Minute)); v.Action != ActionPromote {
		t.Fatalf("verdict at exactly the soak time = %+v, want promotion", v)
	}
}

func TestZeroSoakPromotesAtTheFirstEvaluationThatPasses(t *testing.T) {
	policy := evalPolicy(t, func(s *PolicySpec) { s.Stages[0].Soak = ptr(Duration(0)) })
	if v := policy.Evaluate(soaking(Outcomes{Cohort: 100, Succeeded: 100}, 0)); v.Action != ActionPromote {
		t.Fatalf("verdict = %+v", v)
	}
}

func TestUnknownLimitBoundaries(t *testing.T) {
	policy := evalPolicy(t, nil)
	if v := policy.Evaluate(soaking(Outcomes{Cohort: 100, Succeeded: 95, Unknown: 5}, time.Hour)); v.Action != ActionPromote {
		t.Fatalf("exactly at the unknown limit: verdict = %+v, want promotion", v)
	}
	v := policy.Evaluate(soaking(Outcomes{Cohort: 100, Succeeded: 94, Unknown: 6}, time.Hour))
	if v.Action != ActionNone || !strings.Contains(v.Waiting, "unknown") {
		t.Fatalf("just over the unknown limit: verdict = %+v, want waiting on unknown", v)
	}
}

func TestUnknownLimitAppliesAcrossAllExposedDevices(t *testing.T) {
	policy := evalPolicy(t, func(s *PolicySpec) { s.Stages[1].MinObservations = ptr(1) })
	e := soaking(Outcomes{Cohort: 100, Succeeded: 100}, time.Hour)
	e.CurrentStage = 2
	e.Stages = []StageOutcomes{
		{Ordinal: 1, Outcomes: Outcomes{Cohort: 100, Succeeded: 80, Unknown: 20}},
		{Ordinal: 2, Outcomes: Outcomes{Cohort: 100, Succeeded: 100}},
	}
	// The current stage is clean, but 10% of everything exposed is unknown.
	v := policy.Evaluate(e)
	if v.Action != ActionNone || !strings.Contains(v.Waiting, "unknown") {
		t.Fatalf("verdict = %+v, want waiting on unknown", v)
	}
}

func TestDeadlineHaltsWhenAGateIsUnmet(t *testing.T) {
	policy := evalPolicy(t, nil)

	expired := running(Outcomes{Cohort: 100, Succeeded: 50})
	expired.DeadlineAt = evalNow
	if v := policy.Evaluate(expired); v.Action != ActionHalt || v.Reason != HaltDeadline {
		t.Fatalf("coverage unmet at the deadline: verdict = %+v", v)
	}

	before := running(Outcomes{Cohort: 100, Succeeded: 50})
	before.DeadlineAt = evalNow.Add(time.Nanosecond)
	if v := policy.Evaluate(before); v.Action == ActionHalt {
		t.Fatalf("before the deadline: verdict = %+v", v)
	}

	blocked := soaking(Outcomes{Cohort: 100, Succeeded: 90, Unknown: 10}, time.Hour)
	blocked.DeadlineAt = evalNow
	if v := policy.Evaluate(blocked); v.Action != ActionHalt || v.Reason != HaltDeadline {
		t.Fatalf("too many unknown at the deadline: verdict = %+v", v)
	}
}

func TestDeadlineDoesNotInterruptAHealthySoak(t *testing.T) {
	policy := evalPolicy(t, nil)
	e := soaking(Outcomes{Cohort: 100, Succeeded: 100}, time.Minute)
	e.DeadlineAt = evalNow.Add(-time.Hour)
	if v := policy.Evaluate(e); v.Action != ActionNone {
		t.Fatalf("verdict = %+v, want the soak to continue", v)
	}
}

func TestPausedRolloutsStillHaltButDoNotProgress(t *testing.T) {
	policy := evalPolicy(t, nil)
	paused := func(o Outcomes) Evaluation {
		e := running(o)
		e.State = StatePaused
		return e
	}

	if v := policy.Evaluate(paused(Outcomes{Cohort: 100, Succeeded: 100})); v.Action != ActionNone {
		t.Fatalf("healthy pause: verdict = %+v, want no progress", v)
	}
	if v := policy.Evaluate(paused(Outcomes{Cohort: 100, Succeeded: 90, Failed: 10})); v.Action != ActionHalt {
		t.Fatalf("breach during a pause: verdict = %+v, want a halt", v)
	}

	// The deadline clock is stopped during a pause.
	expired := paused(Outcomes{Cohort: 100})
	expired.DeadlineAt = evalNow.Add(-time.Hour)
	if v := policy.Evaluate(expired); v.Action != ActionNone {
		t.Fatalf("expired deadline during a pause: verdict = %+v", v)
	}
}

func TestDetailStatesTheNumbers(t *testing.T) {
	policy := evalPolicy(t, nil)
	v := policy.Evaluate(running(Outcomes{Cohort: 100, Succeeded: 45, Failed: 5}))
	for _, want := range []string{"5", "50", "2.00%"} {
		if !strings.Contains(v.Detail, want) {
			t.Errorf("detail %q is missing %q", v.Detail, want)
		}
	}
}

func TestDeadlineDetailNamesUnknownDevicesInEarlierStages(t *testing.T) {
	policy := evalPolicy(t, func(s *PolicySpec) { s.Stages[1].MinObservations = ptr(1) })
	e := soaking(Outcomes{Cohort: 100, Succeeded: 100}, time.Minute)
	e.CurrentStage = 2
	e.DeadlineAt = evalNow
	e.Stages = []StageOutcomes{
		{Ordinal: 1, Outcomes: Outcomes{Cohort: 100, Succeeded: 80, Unknown: 20}},
		{Ordinal: 2, Outcomes: Outcomes{Cohort: 100, Succeeded: 100}},
	}
	v := policy.Evaluate(e)
	if v.Action != ActionHalt || v.Reason != HaltDeadline {
		t.Fatalf("verdict = %+v", v)
	}
	// The current stage is clean, so the real cause has to be visible to the operator.
	if !strings.Contains(v.Detail, "20") || !strings.Contains(v.Detail, "200") {
		t.Fatalf("detail %q should state the 20 unknown of 200 exposed", v.Detail)
	}
}
