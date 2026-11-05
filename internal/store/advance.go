package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

const engineActor = "engine"

// AdvanceResult says what one evaluation of a rollout did.
type AdvanceResult struct {
	Changed bool
	Event   rollout.Event
	From    rollout.State
	To      rollout.State
	Stage   int
	Detail  string
}

// ListActiveRollouts returns the rollouts the engine must evaluate. Paused rollouts
// are included, because safety checks keep running during a pause.
func (s *Store) ListActiveRollouts(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM rollouts WHERE state IN ('running', 'soaking', 'paused') ORDER BY started_at, id`)
	if err != nil {
		return nil, fmt.Errorf("query active rollouts: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("query active rollouts: %w", err)
	}
	return ids, nil
}

// AdvanceRollout evaluates a rollout once and applies the decision, all in one
// transaction under the engine lease and the rollout's row lock. It returns
// ErrLeaseLost, with nothing changed, if fence is no longer current.
func (s *Store) AdvanceRollout(ctx context.Context, id string, fence Fence) (AdvanceResult, error) {
	if !rollout.ValidUUID(id) {
		return AdvanceResult{}, notFound("rollout", id)
	}
	var result AdvanceResult
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := checkFence(ctx, tx, fence); err != nil {
			return err
		}
		current, err := lockRollout(ctx, tx, id)
		if err != nil {
			return err
		}
		switch current.state {
		case rollout.StateRunning, rollout.StateSoaking, rollout.StatePaused:
		default:
			return nil // Another instance or an operator finished it first.
		}

		now := s.now()
		r, err := readRollout(ctx, tx, id, now)
		if err != nil {
			return err
		}
		verdict := r.Policy.Evaluate(r.evaluation(now))
		switch verdict.Action {
		case rollout.ActionNone:
			return nil
		case rollout.ActionEnterSoak:
			result, err = enterSoak(ctx, tx, r, now)
		case rollout.ActionPromote:
			result, err = promote(ctx, tx, r, now)
		case rollout.ActionHalt:
			result, err = halt(ctx, tx, r, verdict, now)
		default:
			err = fmt.Errorf("unknown action %q", verdict.Action)
		}
		if err != nil {
			return err
		}
		result.Changed = true
		result.From = r.State
		result.Stage = *r.CurrentStage
		return appendTransition(ctx, tx, transitionRecord{
			rolloutID:        id,
			revision:         r.Revision + 1,
			from:             r.State,
			to:               result.To,
			event:            result.Event,
			actor:            engineActor,
			reason:           result.Detail,
			occurredAt:       now,
			engineGeneration: &fence.Generation,
		})
	})
	if err != nil {
		return AdvanceResult{}, err
	}
	return result, nil
}

// evaluation builds the input to a decision from a rollout's status. Skipped stages
// have nobody in them, so they're left out.
func (r Rollout) evaluation(now time.Time) rollout.Evaluation {
	e := rollout.Evaluation{State: r.State, CurrentStage: *r.CurrentStage, Now: now}
	for _, stage := range r.Stages {
		if stage.Ordinal > *r.CurrentStage || stage.State == StageSkipped {
			continue
		}
		e.Stages = append(e.Stages, rollout.StageOutcomes{
			Ordinal: stage.Ordinal,
			Outcomes: rollout.Outcomes{
				Cohort:    *stage.CohortSize,
				Succeeded: stage.Succeeded,
				Failed:    stage.Failed,
				Unknown:   stage.Unknown,
			},
		})
		if stage.Ordinal != *r.CurrentStage {
			continue
		}
		if stage.DeadlineAt != nil {
			e.DeadlineAt = *stage.DeadlineAt
		}
		e.SoakElapsed = stage.SoakAccumulated
		if stage.SoakStartedAt != nil {
			e.SoakElapsed += max(now.Sub(*stage.SoakStartedAt), 0)
		}
	}
	return e
}

func enterSoak(ctx context.Context, tx pgx.Tx, r Rollout, now time.Time) (AdvanceResult, error) {
	to, err := rollout.Next(r.State, rollout.EventCoverageMet, "")
	if err != nil {
		return AdvanceResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE rollout_stages SET state = 'soaking', soak_started_at = $3
		WHERE rollout_id = $1 AND ordinal = $2`, r.ID, *r.CurrentStage, now); err != nil {
		return AdvanceResult{}, fmt.Errorf("start soak: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE rollouts SET state = $2, revision = revision + 1 WHERE id = $1`, r.ID, to); err != nil {
		return AdvanceResult{}, fmt.Errorf("update rollout: %w", err)
	}
	return AdvanceResult{Event: rollout.EventCoverageMet, To: to, Detail: fmt.Sprintf("stage %d reached coverage", *r.CurrentStage)}, nil
}

// promote passes the current stage and exposes the next one with anybody in it. With
// no such stage left, the rollout is complete.
func promote(ctx context.Context, tx pgx.Tx, r Rollout, now time.Time) (AdvanceResult, error) {
	if _, err := tx.Exec(ctx, `
		UPDATE rollout_stages SET state = 'passed', finished_at = $3
		WHERE rollout_id = $1 AND ordinal = $2`, r.ID, *r.CurrentStage, now); err != nil {
		return AdvanceResult{}, fmt.Errorf("pass stage: %w", err)
	}

	next := 0
	for _, stage := range r.Stages {
		if stage.Ordinal > *r.CurrentStage && stage.State != StageSkipped {
			next = stage.Ordinal
			break
		}
	}
	if next == 0 {
		to, err := rollout.Next(r.State, rollout.EventFinalSoakPassed, "")
		if err != nil {
			return AdvanceResult{}, err
		}
		// Coverage can be met with devices still to start. A finished rollout has nothing
		// more to offer them, and an open assignment would keep them from a follow-up.
		if err := supersedeUnstarted(ctx, tx, r.ID, now); err != nil {
			return AdvanceResult{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE rollouts SET state = $2, revision = revision + 1, finished_at = $3 WHERE id = $1`,
			r.ID, to, now); err != nil {
			return AdvanceResult{}, fmt.Errorf("complete rollout: %w", err)
		}
		return AdvanceResult{Event: rollout.EventFinalSoakPassed, To: to, Detail: "the final stage passed"}, nil
	}

	to, err := rollout.Next(r.State, rollout.EventSoakPassed, "")
	if err != nil {
		return AdvanceResult{}, err
	}
	deadline := now.Add(r.Policy.Stages[next-1].Deadline.Std())
	if _, err := tx.Exec(ctx, `
		UPDATE rollout_stages SET state = 'active', started_at = $3, deadline_at = $4
		WHERE rollout_id = $1 AND ordinal = $2`, r.ID, next, now, deadline); err != nil {
		return AdvanceResult{}, fmt.Errorf("activate stage %d: %w", next, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE rollouts SET state = $2, revision = revision + 1, current_stage = $3 WHERE id = $1`,
		r.ID, to, next); err != nil {
		return AdvanceResult{}, fmt.Errorf("update rollout: %w", err)
	}
	return AdvanceResult{Event: rollout.EventSoakPassed, To: to, Detail: fmt.Sprintf("stage %d passed, exposing stage %d", *r.CurrentStage, next)}, nil
}

// halt stops the rollout and cancels work that hasn't reached installation, in one
// transaction. The current stage fails only if its own results are the reason. An earlier
// stage keeps its verdict, even if a late failure there is what caused the halt.
func halt(ctx context.Context, tx pgx.Tx, r Rollout, verdict rollout.Verdict, now time.Time) (AdvanceResult, error) {
	to, err := rollout.Next(r.State, rollout.EventBreach, "")
	if err != nil {
		return AdvanceResult{}, err
	}
	if err := supersedeUnstarted(ctx, tx, r.ID, now); err != nil {
		return AdvanceResult{}, err
	}
	stageState := StageFailed
	if verdict.Reason == rollout.HaltLateRegression || verdict.Reason == rollout.HaltRolloutBreach {
		stageState = StageStopped
	}
	if _, err := tx.Exec(ctx, `
		UPDATE rollout_stages SET state = $3, finished_at = $4
		WHERE rollout_id = $1 AND ordinal = $2`, r.ID, *r.CurrentStage, stageState, now); err != nil {
		return AdvanceResult{}, fmt.Errorf("end stage: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE rollouts
		SET state = $2, revision = revision + 1, paused_from = NULL, paused_at = NULL,
		    halt_reason = $3, halt_detail = $4
		WHERE id = $1`, r.ID, to, string(verdict.Reason), verdict.Detail); err != nil {
		return AdvanceResult{}, fmt.Errorf("halt rollout: %w", err)
	}
	return AdvanceResult{Event: rollout.EventBreach, To: to, Detail: string(verdict.Reason) + ": " + verdict.Detail}, nil
}
