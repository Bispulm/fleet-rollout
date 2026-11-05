package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

var (
	// ErrReportConflict reports a sequence number already used for a different
	// report. Retries are always identical, so this means something else is wrong.
	ErrReportConflict = errors.New("this sequence number was already used for a different report")

	// ErrReportMismatch reports a report that names the wrong rollout or generation
	// for its assignment.
	ErrReportMismatch = errors.New("the report's rollout or generation doesn't match its assignment")
)

// Report is one status update from a device. DeviceID must come from the
// authenticated credential, never from the request.
type Report struct {
	DeviceID     string
	Sequence     int64
	RolloutID    string
	AssignmentID string
	Generation   int64
	State        rollout.AssignmentState
	Reason       string
}

// ReportResult says what became of a report. Duplicate means this exact report was
// already recorded, and the rest then describes what became of the original, so that a
// device that lost the first response learns the same thing from a retry. IgnoredReason
// explains a report that couldn't be applied.
type ReportResult struct {
	Applied       bool
	Duplicate     bool
	IgnoredReason string
}

// reportPayload is the content a retry must repeat exactly.
type reportPayload struct {
	RolloutID    string                  `json:"rollout_id"`
	AssignmentID string                  `json:"assignment_id"`
	Generation   int64                   `json:"generation"`
	State        rollout.AssignmentState `json:"state"`
	Reason       string                  `json:"reason"`
}

// openReportedAssignment is an assignment locked for applying a report.
type openReportedAssignment struct {
	deviceID     string
	rolloutID    string
	generation   int64
	kind         string
	state        rollout.AssignmentState
	lastSequence int64
	release      int64
	rolloutState rollout.State
	// restore is what a rollback assignment restores.
	restore *int64
}

// RecordReport applies a device's report. Every new report goes into the event log,
// applied or not, so a delayed or invalid one leaves a trace and can't change an
// outcome. Stage counts are derived from assignments, so a repeat can't count twice.
func (s *Store) RecordReport(ctx context.Context, r Report) (ReportResult, error) {
	if !rollout.ValidUUID(r.AssignmentID) {
		return ReportResult{}, notFound("assignment", r.AssignmentID)
	}
	payload, err := json.Marshal(reportPayload{
		RolloutID:    r.RolloutID,
		AssignmentID: r.AssignmentID,
		Generation:   r.Generation,
		State:        r.State,
		Reason:       r.Reason,
	})
	if err != nil {
		return ReportResult{}, fmt.Errorf("encode report: %w", err)
	}
	digest := sha256.Sum256(payload)

	var result ReportResult
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		now := s.now()
		if err := touchDevice(ctx, tx, r.DeviceID, now); err != nil {
			return err
		}
		if err := lockRolloutForClaim(ctx, tx, r); err != nil {
			return err
		}
		current, err := lockReportedAssignment(ctx, tx, r)
		if err != nil {
			return err
		}

		var existing []byte
		var applied bool
		var ignored string
		err = tx.QueryRow(ctx, `
			SELECT payload_digest, applied, coalesce(ignored_reason, '')
			FROM device_events WHERE device_id = $1 AND sequence = $2`,
			r.DeviceID, r.Sequence).Scan(&existing, &applied, &ignored)
		switch {
		case err == nil:
			if !bytes.Equal(existing, digest[:]) {
				return ErrReportConflict
			}
			result = ReportResult{Duplicate: true, Applied: applied, IgnoredReason: ignored}
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("query earlier report: %w", err)
		}

		result = decideReport(current, r)
		if result.Applied {
			if err := applyReport(ctx, tx, current, r, now); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO device_events
				(device_id, sequence, assignment_id, rollout_id, generation, type, payload, payload_digest,
				 received_at, applied, ignored_reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''))`,
			r.DeviceID, r.Sequence, r.AssignmentID, r.RolloutID, r.Generation, r.State, payload, digest[:],
			now, result.Applied, result.IgnoredReason)
		if err != nil {
			return fmt.Errorf("record report: %w", err)
		}
		return nil
	})
	if err != nil {
		return ReportResult{}, err
	}
	return result, nil
}

// lockRolloutForClaim locks the rollout against a change of state if this report asks to
// start installing. Halting, aborting, and completing all update the assignment, so its row
// lock already orders them against the report. A pause doesn't, so without this a device
// could start installing just after a pause committed. The rollout is locked before the
// assignment, in the order transitions use, and only for a claim, so that reports from
// devices already in flight never wait on the engine.
func lockRolloutForClaim(ctx context.Context, tx pgx.Tx, r Report) error {
	if !r.State.InFlight() {
		return nil
	}
	// The state can only move forward, so if this unlocked read finds the device in flight
	// or finished, it still is, and there's nothing to order.
	var state rollout.AssignmentState
	err := tx.QueryRow(ctx, `SELECT state FROM assignments WHERE id = $1 AND device_id = $2`,
		r.AssignmentID, r.DeviceID).Scan(&state)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil // lockReportedAssignment reports it.
	case err != nil:
		return fmt.Errorf("read assignment: %w", err)
	case state.InFlight() || state.IsTerminal():
		return nil
	}
	if _, err := tx.Exec(ctx, `
		SELECT 1 FROM rollouts ro JOIN assignments a ON a.rollout_id = ro.id
		WHERE a.id = $1 FOR SHARE OF ro`, r.AssignmentID); err != nil {
		return fmt.Errorf("lock rollout: %w", err)
	}
	return nil
}

// lockReportedAssignment checks that a report belongs to the device that sent it.
// Someone else's assignment is reported as not found, which reveals nothing about it.
func lockReportedAssignment(ctx context.Context, tx pgx.Tx, r Report) (openReportedAssignment, error) {
	var a openReportedAssignment
	err := tx.QueryRow(ctx, `
		SELECT a.device_id, a.rollout_id, a.generation, a.kind, a.state, a.last_sequence, ar.release_counter, ro.state,
		       a.restore_release
		FROM assignments a
		JOIN rollouts ro ON ro.id = a.rollout_id
		JOIN artifacts ar ON ar.id = ro.artifact_id
		WHERE a.id = $1
		FOR UPDATE OF a`, r.AssignmentID).Scan(
		&a.deviceID, &a.rolloutID, &a.generation, &a.kind, &a.state, &a.lastSequence, &a.release, &a.rolloutState,
		&a.restore)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && a.deviceID != r.DeviceID) {
		return a, notFound("assignment", r.AssignmentID)
	}
	if err != nil {
		return a, fmt.Errorf("lock assignment: %w", err)
	}
	if a.rolloutID != r.RolloutID || a.generation != r.Generation {
		return a, ErrReportMismatch
	}
	return a, nil
}

func decideReport(current openReportedAssignment, r Report) ReportResult {
	switch {
	case current.state.IsTerminal():
		return ReportResult{IgnoredReason: rollout.IgnoredAssignmentFinished}
	case r.Sequence <= current.lastSequence:
		return ReportResult{IgnoredReason: rollout.IgnoredOutOfOrder}
	case !rollout.CanProgress(current.state, r.State):
		return ReportResult{IgnoredReason: rollout.IgnoredInvalidProgression}
	case r.State.InFlight() && !current.state.InFlight() &&
		rollout.DirectiveFor(current.rolloutState, current.state) == rollout.DirectiveHold:
		// The device asks to start installing, and it may not yet.
		return ReportResult{IgnoredReason: rollout.IgnoredOnHold}
	}
	return ReportResult{Applied: true}
}

func applyReport(ctx context.Context, tx pgx.Tx, current openReportedAssignment, r Report, now time.Time) error {
	var finishedAt *time.Time
	if r.State.IsTerminal() {
		finishedAt = &now
	}
	_, err := tx.Exec(ctx, `
		UPDATE assignments SET state = $2, last_sequence = $3, last_report_at = $4, finished_at = $5
		WHERE id = $1`, r.AssignmentID, r.State, r.Sequence, now, finishedAt)
	if err != nil {
		return fmt.Errorf("update assignment: %w", err)
	}
	switch {
	case r.State != rollout.AssignmentSucceeded:
	case current.kind == "forward":
		_, err := tx.Exec(ctx, `UPDATE devices SET installed_release = GREATEST(installed_release, $2) WHERE id = $1`,
			r.DeviceID, current.release)
		if err != nil {
			return fmt.Errorf("record installed release: %w", err)
		}
	case current.restore != nil:
		// A rollback is the one time a device's release goes down, to exactly what it had.
		if _, err := tx.Exec(ctx, `UPDATE devices SET installed_release = $2 WHERE id = $1`, r.DeviceID, *current.restore); err != nil {
			return fmt.Errorf("record restored release: %w", err)
		}
	}
	return nil
}
