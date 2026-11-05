package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

const oneActivePerFleetIndex = "rollouts_one_active_per_fleet"

// TransitionOptions describes who is changing a rollout, and why. ExpectedRevision
// is optional: a caller acting on a value it read should set it, while an operator
// pausing during an incident may want the pause to apply regardless.
type TransitionOptions struct {
	Actor            string
	Reason           string
	ExpectedRevision *int64
}

// RevisionMismatchError reports that a rollout changed since the caller read it.
type RevisionMismatchError struct {
	Expected int64
	Actual   int64
}

func (e *RevisionMismatchError) Error() string {
	return fmt.Sprintf("rollout is at revision %d, expected %d", e.Actual, e.Expected)
}

// Transition is one entry in a rollout's audit log.
type Transition struct {
	Revision   int64
	From       rollout.State
	To         rollout.State
	Event      rollout.Event
	Actor      string
	Reason     string
	OccurredAt time.Time
	// EngineGeneration is the leadership generation that made this decision. It's
	// nil for an operator's action.
	EngineGeneration *int64
}

// lockedRollout is a rollout row held under FOR UPDATE for one transaction.
type lockedRollout struct {
	id           string
	fleetID      string
	artifactID   string
	state        rollout.State
	revision     int64
	policy       rollout.Policy
	pausedFrom   *rollout.State
	pausedAt     *time.Time
	currentStage *int
}

// transition applies one event in a single transaction: lock the rollout, check
// the revision and the state machine, make the event's changes, and append to the
// audit log. If anything fails, nothing is written.
func (s *Store) transition(
	ctx context.Context, id string, event rollout.Event, opts TransitionOptions,
	apply func(tx pgx.Tx, current lockedRollout, to rollout.State, now time.Time) error,
) (Rollout, error) {
	if !rollout.ValidUUID(id) {
		return Rollout{}, notFound("rollout", id)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		current, err := lockRollout(ctx, tx, id)
		if err != nil {
			return err
		}

		if opts.ExpectedRevision != nil && *opts.ExpectedRevision != current.revision {
			return &RevisionMismatchError{Expected: *opts.ExpectedRevision, Actual: current.revision}
		}
		var pausedFrom rollout.State
		if current.pausedFrom != nil {
			pausedFrom = *current.pausedFrom
		}
		to, err := rollout.Next(current.state, event, pausedFrom)
		if err != nil {
			return err
		}

		now := s.now()
		if err := apply(tx, current, to, now); err != nil {
			return err
		}
		return appendTransition(ctx, tx, transitionRecord{
			rolloutID:  id,
			revision:   current.revision + 1,
			from:       current.state,
			to:         to,
			event:      event,
			actor:      opts.Actor,
			reason:     opts.Reason,
			occurredAt: now,
		})
	})
	if err != nil {
		return Rollout{}, err
	}
	return s.GetRollout(ctx, id)
}

// lockRollout takes the rollout's row lock for a state change. FOR NO KEY UPDATE is the
// right strength, since no key changes. It still blocks a poll's FOR SHARE, so nothing
// can be issued mid-transition. Plain FOR UPDATE would also block the key-share lock a
// report's foreign key check takes on this row, while that report holds an assignment a
// halt needs to cancel, and the two would deadlock.
func lockRollout(ctx context.Context, tx pgx.Tx, id string) (lockedRollout, error) {
	current := lockedRollout{id: id}
	err := tx.QueryRow(ctx, `
		SELECT fleet_id, artifact_id, state, revision, policy, paused_from, paused_at, current_stage
		FROM rollouts WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(
		&current.fleetID, &current.artifactID, &current.state, &current.revision, &current.policy,
		&current.pausedFrom, &current.pausedAt, &current.currentStage)
	if errors.Is(err, pgx.ErrNoRows) {
		return current, notFound("rollout", id)
	}
	if err != nil {
		return current, fmt.Errorf("lock rollout: %w", err)
	}
	return current, nil
}

// transitionRecord is one row of the audit log.
type transitionRecord struct {
	rolloutID        string
	revision         int64
	from             rollout.State
	to               rollout.State
	event            rollout.Event
	actor            string
	reason           string
	occurredAt       time.Time
	engineGeneration *int64
}

func appendTransition(ctx context.Context, tx pgx.Tx, t transitionRecord) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO rollout_transitions
			(rollout_id, revision, from_state, to_state, event, actor, reason, occurred_at, engine_generation)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		t.rolloutID, t.revision, t.from, t.to, t.event, t.actor, t.reason, t.occurredAt, t.engineGeneration)
	if err != nil {
		return fmt.Errorf("record transition: %w", err)
	}
	return nil
}

// StartRollout freezes the rollout's membership and activates its first stage. If
// the eligible devices can no longer satisfy the policy, it returns a
// *rollout.PolicyError and the rollout stays a draft.
func (s *Store) StartRollout(ctx context.Context, id string, opts TransitionOptions) (Rollout, error) {
	return s.transition(ctx, id, rollout.EventStart, opts, func(tx pgx.Tx, current lockedRollout, to rollout.State, now time.Time) error {
		// A friendlier message than the index violation, and it avoids writing the
		// membership first. The unique index below remains what enforces this.
		var activeName string
		err := tx.QueryRow(ctx, `SELECT name FROM rollouts
			WHERE fleet_id = $1 AND state IN ('running', 'soaking', 'paused', 'halted', 'rolling_back', 'rollback_incomplete')`,
			current.fleetID).Scan(&activeName)
		switch {
		case err == nil:
			return fleetBusy(activeName)
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("check active rollouts: %w", err)
		}

		target, err := loadArtifactTarget(ctx, tx, current.artifactID)
		if err != nil {
			return err
		}
		candidates, err := loadCandidates(ctx, tx, current.fleetID)
		if err != nil {
			return err
		}
		membership, err := current.policy.Freeze(id, target, candidates)
		if err != nil {
			return err
		}
		if err := writeMembership(ctx, tx, id, membership); err != nil {
			return err
		}
		if err := activateStages(ctx, tx, id, current.policy, membership.Plan, now); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `
			UPDATE rollouts SET state = $2, revision = revision + 1, started_at = $3, member_count = $4, current_stage = 1
			WHERE id = $1`, id, to, now, len(membership.Members))
		if isUniqueViolation(err, oneActivePerFleetIndex) {
			// Another rollout on this fleet started after the check above.
			return fleetBusy("another rollout")
		}
		if err != nil {
			return fmt.Errorf("start rollout: %w", err)
		}
		return nil
	})
}

func fleetBusy(active string) error {
	return &ConflictError{Message: fmt.Sprintf(
		"fleet already has an active rollout (%s); it must finish before another can start", active)}
}

func writeMembership(ctx context.Context, tx pgx.Tx, id string, m rollout.Membership) error {
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"rollout_members"},
		[]string{"rollout_id", "device_id", "position", "stage_ordinal", "rank_key"},
		pgx.CopyFromSlice(len(m.Members), func(i int) ([]any, error) {
			member := m.Members[i]
			return []any{id, member.DeviceID, member.Position, member.Stage, member.Key[:]}, nil
		}))
	if err != nil {
		return fmt.Errorf("write members: %w", err)
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"rollout_exclusions"},
		[]string{"rollout_id", "device_id", "reason"},
		pgx.CopyFromSlice(len(m.Exclusions), func(i int) ([]any, error) {
			excluded := m.Exclusions[i]
			return []any{id, excluded.DeviceID, string(excluded.Reason)}, nil
		}))
	if err != nil {
		return fmt.Errorf("write exclusions: %w", err)
	}
	return nil
}

// activateStages records each stage's size, marks empty stages skipped, and starts
// the first stage. Rounding up guarantees the first stage isn't empty.
func activateStages(ctx context.Context, tx pgx.Tx, id string, policy rollout.Policy, plan []rollout.StagePlan, now time.Time) error {
	for i, stage := range plan {
		state := StagePending
		var startedAt, deadlineAt *time.Time
		switch {
		case stage.CohortSize == 0:
			state = StageSkipped
		case i == 0:
			state = StageActive
			deadline := now.Add(policy.Stages[i].Deadline.Std())
			startedAt, deadlineAt = &now, &deadline
		}
		if _, err := tx.Exec(ctx, `
			UPDATE rollout_stages
			SET target_count = $3, cohort_size = $4, state = $5, started_at = $6, deadline_at = $7
			WHERE rollout_id = $1 AND ordinal = $2`,
			id, stage.Ordinal, stage.Target, stage.CohortSize, state, startedAt, deadlineAt); err != nil {
			return fmt.Errorf("update stage %d: %w", stage.Ordinal, err)
		}
	}
	return nil
}

// PauseRollout stops a running or soaking rollout, and stops its soak timer.
func (s *Store) PauseRollout(ctx context.Context, id string, opts TransitionOptions) (Rollout, error) {
	return s.transition(ctx, id, rollout.EventPause, opts, func(tx pgx.Tx, current lockedRollout, to rollout.State, now time.Time) error {
		if current.state == rollout.StateSoaking {
			// Bank the soak time so far. GREATEST guards against a clock that moved
			// backward between the replicas that started and paused the soak.
			if _, err := tx.Exec(ctx, `
				UPDATE rollout_stages
				SET soak_accumulated_ms = soak_accumulated_ms
				        + GREATEST(0, (extract(epoch FROM $3::timestamptz - soak_started_at) * 1000)::bigint),
				    soak_started_at = NULL
				WHERE rollout_id = $1 AND ordinal = $2`, id, current.currentStage, now); err != nil {
				return fmt.Errorf("stop soak timer: %w", err)
			}
		}
		_, err := tx.Exec(ctx, `
			UPDATE rollouts SET state = $2, revision = revision + 1, paused_from = $3, paused_at = $4
			WHERE id = $1`, id, to, current.state, now)
		if err != nil {
			return fmt.Errorf("pause rollout: %w", err)
		}
		return nil
	})
}

// ResumeRollout returns a paused rollout to the state it was paused from. The
// stage deadline moves later by the length of the pause, and the soak timer
// restarts, so time spent paused counts toward neither.
func (s *Store) ResumeRollout(ctx context.Context, id string, opts TransitionOptions) (Rollout, error) {
	return s.transition(ctx, id, rollout.EventResume, opts, func(tx pgx.Tx, current lockedRollout, to rollout.State, now time.Time) error {
		pauseLength := max(now.Sub(*current.pausedAt), 0)
		var soakStartedAt *time.Time
		if to == rollout.StateSoaking {
			soakStartedAt = &now
		}
		if _, err := tx.Exec(ctx, `
			UPDATE rollout_stages
			SET deadline_at = deadline_at + ($3::bigint * interval '1 microsecond'),
			    soak_started_at = COALESCE($4, soak_started_at)
			WHERE rollout_id = $1 AND ordinal = $2`,
			id, current.currentStage, pauseLength.Microseconds(), soakStartedAt); err != nil {
			return fmt.Errorf("resume stage timers: %w", err)
		}
		_, err := tx.Exec(ctx, `
			UPDATE rollouts SET state = $2, revision = revision + 1, paused_from = NULL, paused_at = NULL
			WHERE id = $1`, id, to)
		if err != nil {
			return fmt.Errorf("resume rollout: %w", err)
		}
		return nil
	})
}

// AbortRollout finishes a rollout without rolling it back. Assignments that haven't
// reached installation are superseded. Devices keep whatever they have installed, and
// the fleet is free for another rollout.
func (s *Store) AbortRollout(ctx context.Context, id string, opts TransitionOptions) (Rollout, error) {
	return s.transition(ctx, id, rollout.EventAbort, opts, func(tx pgx.Tx, _ lockedRollout, to rollout.State, now time.Time) error {
		if err := supersedeUnstarted(ctx, tx, id, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE rollouts SET state = $2, revision = revision + 1, paused_from = NULL, paused_at = NULL, finished_at = $3
			WHERE id = $1`, id, to, now)
		if err != nil {
			return fmt.Errorf("abort rollout: %w", err)
		}
		return nil
	})
}

// ListTransitions returns a rollout's audit log, oldest first.
func (s *Store) ListTransitions(ctx context.Context, id string) ([]Transition, error) {
	if !rollout.ValidUUID(id) {
		return nil, notFound("rollout", id)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT revision, from_state, to_state, event, actor, reason, occurred_at, engine_generation
		FROM rollout_transitions WHERE rollout_id = $1 ORDER BY revision`, id)
	if err != nil {
		return nil, fmt.Errorf("query transitions: %w", err)
	}
	log, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Transition, error) {
		var t Transition
		err := row.Scan(&t.Revision, &t.From, &t.To, &t.Event, &t.Actor, &t.Reason, &t.OccurredAt, &t.EngineGeneration)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("query transitions: %w", err)
	}
	return log, nil
}
