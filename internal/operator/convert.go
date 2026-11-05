// Package operator reconciles Fleet and Rollout resources against the control plane's API. It
// holds no rollout rules of its own: it turns resources into API calls, and copies what the
// control plane reports into their status.
package operator

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Bispulm/fleet-rollout/api/v1alpha1"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// policyFor turns a Rollout's spec into the control plane's policy. Anything left unset stays
// unset, so the control plane applies its own defaults and validation.
func policyFor(spec v1alpha1.RolloutSpec) (rollout.PolicySpec, error) {
	out := rollout.PolicySpec{
		Stages:         make([]rollout.StageSpec, len(spec.Stages)),
		Selector:       spec.Selector,
		ExcludeDevices: spec.ExcludeDevices,
	}
	for i, stage := range spec.Stages {
		out.Stages[i] = rollout.StageSpec{
			Percent:         stage.Percent,
			Soak:            duration(stage.Soak),
			Deadline:        duration(stage.Deadline),
			MinObservations: stage.MinObservations,
		}
	}

	safety := spec.Safety
	maxFailure, err := rollout.ParseRate(safety.MaxFailureRate)
	if err != nil {
		return rollout.PolicySpec{}, fmt.Errorf("spec.safety.maxFailureRate: %w", err)
	}
	out.Safety.MaxFailureBPS = &maxFailure
	for _, rate := range []struct {
		field string
		value string
		dst   **int
	}{
		{"rolloutMaxFailureRate", safety.RolloutMaxFailureRate, &out.Safety.RolloutMaxFailureBPS},
		{"requiredCoverage", safety.RequiredCoverage, &out.Safety.RequiredCoverageBPS},
		{"maxUnknown", safety.MaxUnknown, &out.Safety.MaxUnknownBPS},
	} {
		if rate.value == "" {
			continue
		}
		bps, err := rollout.ParseRate(rate.value)
		if err != nil {
			return rollout.PolicySpec{}, fmt.Errorf("spec.safety.%s: %w", rate.field, err)
		}
		*rate.dst = &bps
	}
	out.Safety.StaleAfter = duration(safety.StaleAfter)
	out.Safety.OnBreach = rollout.OnBreach(strings.ToLower(safety.OnBreach))
	return out, nil
}

func duration(d *metav1.Duration) *rollout.Duration {
	if d == nil {
		return nil
	}
	v := rollout.Duration(d.Duration)
	return &v
}

// maxNamePrefix leaves room for a hyphen and eight characters of the UID within the 63 a
// control plane name allows.
const maxNamePrefix = 54

// controlPlaneName gives a resource a rollout name the control plane accepts. Those names are
// unique per fleet, and a resource deleted and created again has the same name, so part of
// the UID goes on the end.
func controlPlaneName(name string, uid types.UID) string {
	prefix := strings.ReplaceAll(name, ".", "-")
	if len(prefix) > maxNamePrefix {
		prefix = prefix[:maxNamePrefix]
	}
	prefix = strings.TrimRight(prefix, "-")
	suffix := strings.ReplaceAll(string(uid), "-", "")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return prefix + "-" + suffix
}

// statusFrom copies what the control plane reports into a Rollout's status. Conditions and the
// observed generation are the reconciler's to set.
func statusFrom(r apiv1.Rollout) v1alpha1.RolloutStatus {
	out := v1alpha1.RolloutStatus{
		RolloutID:    r.ID,
		ArtifactID:   r.ArtifactID,
		Phase:        string(r.State),
		CurrentStage: r.CurrentStage,
		Waiting:      r.Waiting,
		HaltReason:   r.HaltReason,
		HaltDetail:   r.HaltDetail,
		Stages:       make([]v1alpha1.StageStatus, len(r.Stages)),
	}
	for i, stage := range r.Stages {
		out.Stages[i] = v1alpha1.StageStatus{
			Ordinal: stage.Ordinal, Percent: stage.Percent, State: stage.State, Cohort: stage.CohortSize,
			Succeeded: stage.Succeeded, Failed: stage.Failed, InFlight: stage.InFlight, Pending: stage.Pending,
			Unknown: stage.Unknown,
		}
	}
	return out
}
