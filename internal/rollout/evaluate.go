package rollout

import (
	"fmt"
	"time"
)

// Outcomes summarizes one stage's cohort at a moment in time.
type Outcomes struct {
	Cohort    int
	Succeeded int
	Failed    int
	Unknown   int
}

// Observed is how many devices in the cohort have a terminal outcome. Missing
// reports aren't in it, so they can't pass for successes.
func (o Outcomes) Observed() int { return o.Succeeded + o.Failed }

// StageOutcomes pairs a stage with its outcomes.
type StageOutcomes struct {
	Ordinal  int
	Outcomes Outcomes
}

// Evaluation is everything a decision depends on. Stages holds every non-skipped
// stage up to and including the current one.
type Evaluation struct {
	State        State
	CurrentStage int
	Stages       []StageOutcomes
	DeadlineAt   time.Time
	SoakElapsed  time.Duration
	Now          time.Time
}

// Action is what the engine should do with a rollout.
type Action string

// The actions.
const (
	ActionNone      Action = "none"
	ActionEnterSoak Action = "enter_soak"
	ActionPromote   Action = "promote"
	ActionHalt      Action = "halt"
)

// HaltReason says why a rollout was halted.
type HaltReason string

// The halt reasons.
const (
	HaltStageBreach    HaltReason = "stage_breach"
	HaltCertainBreach  HaltReason = "certain_breach"
	HaltRolloutBreach  HaltReason = "rollout_breach"
	HaltLateRegression HaltReason = "late_regression"
	HaltDeadline       HaltReason = "stage_deadline"
)

// Verdict is the result of an evaluation. Waiting explains why nothing is happening
// when the action is ActionNone, and Detail explains a halt.
type Verdict struct {
	Action  Action
	Reason  HaltReason
	Detail  string
	Waiting string
}

// exceeds reports whether count/total is above a basis-point limit, using exact
// integer arithmetic. A value exactly at the limit does not exceed it.
func exceeds(count, total, limitBPS int) bool {
	return int64(count)*FullBPS > int64(limitBPS)*int64(total)
}

// Evaluate decides what should happen to a rollout. It's a pure function of its
// input, and it implements the stage progression rules in docs/rollout-semantics.md.
func (p Policy) Evaluate(e Evaluation) Verdict {
	if v, halted := p.checkBreaches(e); halted {
		return v
	}
	if e.State == StatePaused {
		return Verdict{Action: ActionNone, Waiting: "paused"}
	}

	var current Outcomes
	var everyone Outcomes
	for _, stage := range e.Stages {
		if stage.Ordinal == e.CurrentStage {
			current = stage.Outcomes
		}
		everyone.Cohort += stage.Outcomes.Cohort
		everyone.Unknown += stage.Outcomes.Unknown
	}
	spec := p.Stages[e.CurrentStage-1]

	covered := current.Observed() >= requiredObservations(current.Cohort, p.Safety.RequiredCoverageBPS, spec.MinObservations)
	unknownOK := !exceeds(current.Unknown, current.Cohort, p.Safety.MaxUnknownBPS) &&
		!exceeds(everyone.Unknown, everyone.Cohort, p.Safety.MaxUnknownBPS)

	if !e.Now.Before(e.DeadlineAt) && (!covered || !unknownOK) {
		return Verdict{
			Action: ActionHalt,
			Reason: HaltDeadline,
			Detail: fmt.Sprintf("stage %d reached its deadline with %d of %d outcomes and %d unknown",
				e.CurrentStage, current.Observed(), current.Cohort, current.Unknown),
		}
	}

	if e.State == StateRunning {
		if !covered {
			return Verdict{Action: ActionNone, Waiting: fmt.Sprintf(
				"coverage: %d of %d devices have an outcome, need %d",
				current.Observed(), current.Cohort, requiredObservations(current.Cohort, p.Safety.RequiredCoverageBPS, spec.MinObservations))}
		}
		return Verdict{Action: ActionEnterSoak}
	}

	if e.SoakElapsed < spec.Soak.Std() {
		return Verdict{Action: ActionNone, Waiting: fmt.Sprintf("soak: %s of %s", e.SoakElapsed, spec.Soak.Std())}
	}
	if !unknownOK {
		return Verdict{Action: ActionNone, Waiting: fmt.Sprintf(
			"unknown devices: %d in this stage and %d overall, above the %s limit",
			current.Unknown, everyone.Unknown, formatBPS(p.Safety.MaxUnknownBPS))}
	}
	return Verdict{Action: ActionPromote}
}

// checkBreaches applies the failure checks, which run in every state the engine
// evaluates, including while paused.
func (p Policy) checkBreaches(e Evaluation) (Verdict, bool) {
	var everyone Outcomes
	for _, stage := range e.Stages {
		o := stage.Outcomes
		everyone.Cohort += o.Cohort
		everyone.Succeeded += o.Succeeded
		everyone.Failed += o.Failed

		minimum := p.Stages[stage.Ordinal-1].MinObservations
		observed := o.Observed() >= minimum && exceeds(o.Failed, o.Observed(), p.Safety.MaxFailureBPS)
		certain := exceeds(o.Failed, o.Cohort, p.Safety.MaxFailureBPS)
		if !observed && !certain {
			continue
		}
		detail := fmt.Sprintf("stage %d has %d failures in %d outcomes from a cohort of %d, above the %s limit",
			stage.Ordinal, o.Failed, o.Observed(), o.Cohort, formatBPS(p.Safety.MaxFailureBPS))
		switch {
		case stage.Ordinal != e.CurrentStage:
			return Verdict{Action: ActionHalt, Reason: HaltLateRegression, Detail: detail}, true
		case observed:
			return Verdict{Action: ActionHalt, Reason: HaltStageBreach, Detail: detail}, true
		default:
			return Verdict{Action: ActionHalt, Reason: HaltCertainBreach, Detail: detail}, true
		}
	}

	minimum := p.Stages[e.CurrentStage-1].MinObservations
	if everyone.Observed() >= minimum && exceeds(everyone.Failed, everyone.Observed(), p.Safety.RolloutMaxFailureBPS) {
		return Verdict{
			Action: ActionHalt,
			Reason: HaltRolloutBreach,
			Detail: fmt.Sprintf("%d failures in %d outcomes across the rollout, above the %s limit",
				everyone.Failed, everyone.Observed(), formatBPS(p.Safety.RolloutMaxFailureBPS)),
		}, true
	}
	return Verdict{}, false
}

// requiredObservations is the smallest observed count that meets the coverage gate:
// the policy's minimum, or the coverage share of the cohort rounded up.
func requiredObservations(cohort, coverageBPS, minimum int) int {
	return max(minimum, int((int64(coverageBPS)*int64(cohort)+FullBPS-1)/FullBPS))
}

func formatBPS(bps int) string {
	return fmt.Sprintf("%d.%02d%%", bps/100, bps%100)
}
