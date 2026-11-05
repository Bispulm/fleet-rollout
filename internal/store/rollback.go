package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// RollbackProgress is how far a rollback has got. See ADR 0011.
type RollbackProgress struct {
	// Attempt counts rollbacks started, and the current one's assignments use generation
	// Attempt+1.
	Attempt    int
	DeadlineAt *time.Time
	// Needed counts members whose forward install succeeded, so they run the release, leaving out
	// decommissioned devices, which can't take part.
	Needed int
	// Restored counts those that have confirmed a rollback.
	Restored int
	// Failed and InFlight count rollback assignments of the current attempt.
	Failed   int
	InFlight int
	// Unresolved is Needed less Restored.
	Unresolved int
	// ForwardInFlight counts forward installs still running, which have to finish first.
	ForwardInFlight int
}

// RollbackRollout starts a rollback, or a new attempt at one. See ADR 0011.
func (s *Store) RollbackRollout(ctx context.Context, id string, opts TransitionOptions) (Rollout, error) {
	return s.transition(ctx, id, rollout.EventRollback, opts, func(tx pgx.Tx, current lockedRollout, _ rollout.State, now time.Time) error {
		return beginRollback(ctx, tx, id, current.policy, current.currentStage, now)
	})
}

// beginRollback supersedes work that hasn't started, stops the current stage, and starts a
// new attempt with its own deadline. The caller records the transition.
func beginRollback(ctx context.Context, tx pgx.Tx, id string, policy rollout.Policy, currentStage *int, now time.Time) error {
	if err := supersedeUnstarted(ctx, tx, id, now); err != nil {
		return err
	}
	if err := supersedeUnstartedRollbacks(ctx, tx, id, now); err != nil {
		return err
	}
	if currentStage != nil {
		// A stage that already has a verdict keeps it.
		if _, err := tx.Exec(ctx, `
			UPDATE rollout_stages SET state = 'stopped', finished_at = $3
			WHERE rollout_id = $1 AND ordinal = $2 AND state IN ('active', 'soaking')`,
			id, *currentStage, now); err != nil {
			return fmt.Errorf("stop stage: %w", err)
		}
	}
	deadline := policy.Safety.RollbackDeadline.Std()
	if deadline <= 0 {
		deadline = time.Hour // A policy frozen before rollback existed.
	}
	if _, err := tx.Exec(ctx, `
		UPDATE rollouts
		SET state = 'rolling_back', revision = revision + 1, paused_from = NULL, paused_at = NULL,
		    rollback_attempt = rollback_attempt + 1, rollback_deadline_at = $2
		WHERE id = $1`, id, now.Add(deadline)); err != nil {
		return fmt.Errorf("start rollback: %w", err)
	}
	return nil
}

// supersedeUnstartedRollbacks cancels rollback assignments a device hasn't acted on.
func supersedeUnstartedRollbacks(ctx context.Context, tx pgx.Tx, rolloutID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `
		UPDATE assignments SET state = 'superseded', finished_at = $2
		WHERE rollout_id = $1 AND kind = 'rollback' AND finished_at IS NULL AND state = 'assigned'`,
		rolloutID, now); err != nil {
		return fmt.Errorf("supersede rollback assignments: %w", err)
	}
	return nil
}

// issueRollback creates a rollback assignment if the device needs one: its fleet's rollout is
// rolling back, its forward install succeeded, and it hasn't been restored or offered this
// attempt. The shared lock on the rollout orders this against the rollback ending.
func issueRollback(ctx context.Context, tx pgx.Tx, device Device, now time.Time) (*Assignment, error) {
	a := Assignment{
		DeviceID:  device.ID,
		Kind:      "rollback",
		State:     rollout.AssignmentAssigned,
		IssuedAt:  now,
		Directive: rollout.DirectiveProceed,
	}
	var artifactID string
	var restore int64
	err := tx.QueryRow(ctx, `
		SELECT r.id, r.artifact_id, m.stage_ordinal, m.prior_release, r.rollback_attempt + 1
		FROM rollouts r
		JOIN rollout_members m ON m.rollout_id = r.id AND m.device_id = $2
		WHERE r.fleet_id = $1 AND r.state = 'rolling_back'
		  AND EXISTS (
		      SELECT 1 FROM assignments f
		      WHERE f.rollout_id = r.id AND f.device_id = m.device_id AND f.kind = 'forward' AND f.state = 'succeeded')
		  AND NOT EXISTS (
		      SELECT 1 FROM assignments b
		      WHERE b.rollout_id = r.id AND b.device_id = m.device_id AND b.kind = 'rollback'
		        AND (b.state = 'succeeded' OR b.generation = r.rollback_attempt + 1))
		FOR SHARE OF r`, device.FleetID, device.ID).Scan(&a.RolloutID, &artifactID, &a.Stage, &restore, &a.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find due rollback: %w", err)
	}
	a.RestoreRelease = &restore
	err = tx.QueryRow(ctx, `
		INSERT INTO assignments (rollout_id, device_id, generation, kind, stage_ordinal, issued_at, restore_release)
		VALUES ($1, $2, $3, 'rollback', $4, $5, $6)
		RETURNING id`, a.RolloutID, a.DeviceID, a.Generation, a.Stage, a.IssuedAt, restore).Scan(&a.ID)
	if err != nil {
		return nil, fmt.Errorf("insert rollback assignment: %w", err)
	}
	a.Artifact, err = readArtifact(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// readRollback fills in a rollback's progress.
func readRollback(ctx context.Context, tx pgx.Tx, id string, attempt int, deadline *time.Time) (*RollbackProgress, error) {
	p := RollbackProgress{Attempt: attempt, DeadlineAt: deadline}
	err := tx.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE f.state = 'succeeded'),
		  count(*) FILTER (WHERE f.state = 'succeeded' AND EXISTS (
		      SELECT 1 FROM assignments b WHERE b.rollout_id = f.rollout_id AND b.device_id = f.device_id
		        AND b.kind = 'rollback' AND b.state = 'succeeded')),
		  count(*) FILTER (WHERE f.state = 'succeeded' AND EXISTS (
		      SELECT 1 FROM assignments b WHERE b.rollout_id = f.rollout_id AND b.device_id = f.device_id
		        AND b.kind = 'rollback' AND b.generation = $2 AND b.state = 'failed')),
		  count(*) FILTER (WHERE f.state = 'succeeded' AND EXISTS (
		      SELECT 1 FROM assignments b WHERE b.rollout_id = f.rollout_id AND b.device_id = f.device_id
		        AND b.kind = 'rollback' AND b.generation = $2 AND b.state IN ('installing', 'health_check', 'reverting'))),
		  count(*) FILTER (WHERE f.state IN ('installing', 'health_check', 'reverting'))
		FROM assignments f
		JOIN devices d ON d.id = f.device_id
		-- A decommissioned device can't poll, so it can never confirm a rollback.
		WHERE f.rollout_id = $1 AND f.kind = 'forward' AND d.status <> 'decommissioned'`, id, attempt+1).Scan(
		&p.Needed, &p.Restored, &p.Failed, &p.InFlight, &p.ForwardInFlight)
	if err != nil {
		return nil, fmt.Errorf("query rollback progress: %w", err)
	}
	p.Unresolved = p.Needed - p.Restored
	return &p, nil
}

// advanceRollback finishes a rollback once every device that needed one has confirmed, and no
// forward install is still running, or marks it incomplete at its deadline.
func advanceRollback(ctx context.Context, tx pgx.Tx, r Rollout, now time.Time) (AdvanceResult, bool, error) {
	p := r.Rollback
	var event rollout.Event
	var detail string
	switch {
	case p.Unresolved == 0 && p.ForwardInFlight == 0:
		event, detail = rollout.EventRollbackDone, fmt.Sprintf("%d devices restored", p.Restored)
	case p.DeadlineAt != nil && !now.Before(*p.DeadlineAt):
		event, detail = rollout.EventRollbackExpired, fmt.Sprintf("%d of %d devices restored by the deadline", p.Restored, p.Needed)
	default:
		return AdvanceResult{}, false, nil
	}
	to, err := rollout.Next(r.State, event, "")
	if err != nil {
		return AdvanceResult{}, false, err
	}
	var finishedAt *time.Time
	if event == rollout.EventRollbackDone {
		finishedAt = &now
	} else if err := supersedeUnstartedRollbacks(ctx, tx, r.ID, now); err != nil {
		return AdvanceResult{}, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE rollouts SET state = $2, revision = revision + 1, finished_at = $3 WHERE id = $1`,
		r.ID, to, finishedAt); err != nil {
		return AdvanceResult{}, false, fmt.Errorf("finish rollback: %w", err)
	}
	return AdvanceResult{Event: event, To: to, Detail: detail}, true, nil
}

// rollbackWaiting says what a rollback is waiting for or, once it ran out of time, what to do next.
func rollbackWaiting(state rollout.State, p RollbackProgress) string {
	if state == rollout.StateRollbackIncomplete {
		failed := ""
		if p.Failed > 0 {
			failed = fmt.Sprintf(", %d failed", p.Failed)
		}
		return fmt.Sprintf("rollback incomplete: %d of %d devices restored%s, %d unresolved at the deadline; "+
			"roll back again, or abort", p.Restored, p.Needed, failed, p.Unresolved)
	}
	if p.ForwardInFlight > 0 && p.Unresolved == 0 {
		return fmt.Sprintf("rollback: %d installs still finishing before they can be rolled back", p.ForwardInFlight)
	}
	return fmt.Sprintf("rollback: %d of %d devices restored", p.Restored, p.Needed)
}
