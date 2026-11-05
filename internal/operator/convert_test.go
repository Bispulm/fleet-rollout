package operator

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Bispulm/fleet-rollout/api/v1alpha1"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

func ptr[T any](v T) *T { return &v }

func TestAPolicyComesFromTheSpec(t *testing.T) {
	spec := v1alpha1.RolloutSpec{
		Stages: []v1alpha1.Stage{
			{Percent: 10, Soak: &metav1.Duration{Duration: 10 * time.Minute}, MinObservations: ptr(5)},
			{Percent: 100, Deadline: &metav1.Duration{Duration: 2 * time.Hour}},
		},
		Safety: v1alpha1.Safety{
			MaxFailureRate: "2%", RolloutMaxFailureRate: "1.5%", RequiredCoverage: "95%", MaxUnknown: "0.25%",
			StaleAfter: &metav1.Duration{Duration: 20 * time.Minute}, OnBreach: "Halt",
		},
		Selector:       map[string]string{"ring": "canary"},
		ExcludeDevices: []string{"sim-00001"},
	}
	got, err := policyFor(spec)
	if err != nil {
		t.Fatalf("policyFor: %v", err)
	}
	if len(got.Stages) != 2 || got.Stages[0].Percent != 10 || got.Stages[0].Soak.Std() != 10*time.Minute ||
		*got.Stages[0].MinObservations != 5 || got.Stages[0].Deadline != nil || got.Stages[1].Deadline.Std() != 2*time.Hour {
		t.Fatalf("stages = %+v", got.Stages)
	}
	s := got.Safety
	if *s.MaxFailureBPS != 200 || *s.RolloutMaxFailureBPS != 150 || *s.RequiredCoverageBPS != 9500 ||
		*s.MaxUnknownBPS != 25 || s.StaleAfter.Std() != 20*time.Minute || s.OnBreach != rollout.OnBreachHalt {
		t.Fatalf("safety = %+v", s)
	}
	if got.Selector["ring"] != "canary" || len(got.ExcludeDevices) != 1 {
		t.Fatalf("selector = %v, excluded = %v", got.Selector, got.ExcludeDevices)
	}
}

func TestOptionalSafetyLimitsAreLeftToTheControlPlane(t *testing.T) {
	got, err := policyFor(v1alpha1.RolloutSpec{
		Stages: []v1alpha1.Stage{{Percent: 100}}, Safety: v1alpha1.Safety{MaxFailureRate: "5%"},
	})
	if err != nil {
		t.Fatalf("policyFor: %v", err)
	}
	s := got.Safety
	if *s.MaxFailureBPS != 500 || s.RolloutMaxFailureBPS != nil || s.RequiredCoverageBPS != nil ||
		s.MaxUnknownBPS != nil || s.StaleAfter != nil || s.OnBreach != "" {
		t.Fatalf("safety = %+v, want only the failure limit set", s)
	}
}

func TestABadRateIsReportedByField(t *testing.T) {
	_, err := policyFor(v1alpha1.RolloutSpec{
		Stages: []v1alpha1.Stage{{Percent: 100}}, Safety: v1alpha1.Safety{MaxFailureRate: "2%", MaxUnknown: "150%"},
	})
	if err == nil || !strings.Contains(err.Error(), "maxUnknown") {
		t.Fatalf("error = %v, want one naming maxUnknown", err)
	}
}

func TestControlPlaneNamesAreValidAndUnique(t *testing.T) {
	uid := types.UID("1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d")
	for name, want := range map[string]string{
		"demo-app":                          "demo-app-1a2b3c4d",
		"demo-app.1.4.0":                    "demo-app-1-4-0-1a2b3c4d",
		strings.Repeat("a", 100):            strings.Repeat("a", 54) + "-1a2b3c4d",
		"x" + strings.Repeat("-", 60) + "y": "x-1a2b3c4d",
	} {
		got := controlPlaneName(name, uid)
		if got != want {
			t.Errorf("controlPlaneName(%q) = %q, want %q", name, got, want)
		}
		if !rollout.ValidName(got) {
			t.Errorf("controlPlaneName(%q) = %q, which the control plane won't accept", name, got)
		}
	}
}

func TestStatusMirrorsTheControlPlane(t *testing.T) {
	cohort := 10
	current := 1
	got := statusFrom(apiv1.Rollout{
		ID: "r1", ArtifactID: "a1", State: rollout.StateHalted, CurrentStage: &current,
		Waiting: "", HaltReason: "stage_breach", HaltDetail: "stage 1: 2 failed",
		Stages: []apiv1.Stage{{Ordinal: 1, Percent: 10, State: "failed", CohortSize: &cohort, Succeeded: 7, Failed: 2, Unknown: 1}},
	})
	if got.RolloutID != "r1" || got.ArtifactID != "a1" || got.Phase != "halted" || *got.CurrentStage != 1 ||
		got.HaltReason != "stage_breach" || got.HaltDetail != "stage 1: 2 failed" {
		t.Fatalf("status = %+v", got)
	}
	if len(got.Stages) != 1 || *got.Stages[0].Cohort != 10 || got.Stages[0].Failed != 2 || got.Stages[0].Unknown != 1 {
		t.Fatalf("stages = %+v", got.Stages)
	}
}
