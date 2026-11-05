package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// Assignment is what a device is told to do.
type Assignment struct {
	ID         string
	RolloutID  string
	DeviceID   string
	Generation int64
	Stage      int
	State      rollout.AssignmentState
	IssuedAt   time.Time
	// ExpiresAt is when the validity lease runs out. A device must not begin
	// installing after this without polling again.
	ExpiresAt time.Time
	Directive rollout.Directive
	Artifact  Artifact
}

// AuthenticateDevice finds the device a credential belongs to. Decommissioned
// devices are not found, which is how their credentials are revoked.
func (s *Store) AuthenticateDevice(ctx context.Context, credentialHash [sha256.Size]byte) (Device, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+deviceColumns+` FROM devices
		WHERE credential_hash = $1 AND status <> 'decommissioned'`, credentialHash[:])
	if err != nil {
		return Device{}, fmt.Errorf("query device: %w", err)
	}
	device, err := pgx.CollectExactlyOneRow(rows, scanDevice)
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, fmt.Errorf("device credential: %w", ErrNotFound)
	}
	if err != nil {
		return Device{}, fmt.Errorf("query device: %w", err)
	}
	return device, nil
}

// PollAssignment records that a device was heard from and returns its open
// assignment, issuing one if it's due. It returns nil if there's nothing to do.
//
// Everything a device does starts by updating its row, which serializes its polls
// and reports, and gives them all the same lock order.
func (s *Store) PollAssignment(ctx context.Context, device Device, lease time.Duration) (*Assignment, error) {
	var out *Assignment
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		now := s.now()
		if err := touchDevice(ctx, tx, device.ID, now); err != nil {
			return err
		}
		var err error
		out, err = renewOpenAssignment(ctx, tx, device.ID, now, lease)
		if err != nil || out != nil {
			return err
		}
		if device.Status != rollout.DeviceActive {
			return nil
		}
		out, err = issueAssignment(ctx, tx, device, now, lease)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func touchDevice(ctx context.Context, tx pgx.Tx, deviceID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE devices SET last_seen_at = $2 WHERE id = $1`, deviceID, now); err != nil {
		return fmt.Errorf("record device contact: %w", err)
	}
	return nil
}

// renewOpenAssignment returns the device's open assignment with its directive,
// renewing the lease only if the device may proceed.
func renewOpenAssignment(ctx context.Context, tx pgx.Tx, deviceID string, now time.Time, lease time.Duration) (*Assignment, error) {
	a := Assignment{DeviceID: deviceID}
	var rolloutState rollout.State
	var artifactID string
	err := tx.QueryRow(ctx, `
		SELECT a.id, a.rollout_id, a.generation, a.stage_ordinal, a.state, a.issued_at, a.expires_at, r.state, r.artifact_id
		FROM assignments a JOIN rollouts r ON r.id = a.rollout_id
		WHERE a.device_id = $1 AND a.finished_at IS NULL
		FOR UPDATE OF a`, deviceID).Scan(
		&a.ID, &a.RolloutID, &a.Generation, &a.Stage, &a.State, &a.IssuedAt, &a.ExpiresAt, &rolloutState, &artifactID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query open assignment: %w", err)
	}

	a.Directive = rollout.DirectiveFor(rolloutState, a.State)
	if a.Directive == rollout.DirectiveProceed {
		a.ExpiresAt = now.Add(lease)
		if _, err := tx.Exec(ctx, `UPDATE assignments SET expires_at = $2 WHERE id = $1`, a.ID, a.ExpiresAt); err != nil {
			return nil, fmt.Errorf("renew assignment lease: %w", err)
		}
	}
	a.Artifact, err = readArtifact(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// issueAssignment creates a forward assignment if the device is due one. The shared
// lock on the rollout row serializes this with pause, halt, and abort. If one of
// those is committing, Postgres waits for it and then rechecks the state condition,
// so an assignment can't be issued once the rollout has stopped.
func issueAssignment(ctx context.Context, tx pgx.Tx, device Device, now time.Time, lease time.Duration) (*Assignment, error) {
	a := Assignment{
		DeviceID:   device.ID,
		Generation: 1,
		State:      rollout.AssignmentAssigned,
		IssuedAt:   now,
		ExpiresAt:  now.Add(lease),
		Directive:  rollout.DirectiveProceed,
	}
	var artifactID string
	err := tx.QueryRow(ctx, `
		SELECT r.id, r.artifact_id, m.stage_ordinal
		FROM rollouts r
		JOIN rollout_members m ON m.rollout_id = r.id AND m.device_id = $2
		WHERE r.fleet_id = $1
		  AND r.state IN ('running', 'soaking')
		  AND m.stage_ordinal <= r.current_stage
		  AND NOT EXISTS (
		      SELECT 1 FROM assignments x
		      WHERE x.rollout_id = r.id AND x.device_id = m.device_id AND x.kind = 'forward')
		FOR SHARE OF r`, device.FleetID, device.ID).Scan(&a.RolloutID, &artifactID, &a.Stage)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find due assignment: %w", err)
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO assignments (rollout_id, device_id, generation, kind, stage_ordinal, issued_at, expires_at)
		VALUES ($1, $2, $3, 'forward', $4, $5, $6)
		RETURNING id`, a.RolloutID, a.DeviceID, a.Generation, a.Stage, a.IssuedAt, a.ExpiresAt).Scan(&a.ID)
	if err != nil {
		return nil, fmt.Errorf("insert assignment: %w", err)
	}
	a.Artifact, err = readArtifact(ctx, tx, artifactID)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// supersedeUnstarted cancels a rollout's open forward assignments that haven't
// reached installation. It runs in the transaction that stops the rollout. Work past
// that point is left to finish and report, because the device can't be recalled.
func supersedeUnstarted(ctx context.Context, tx pgx.Tx, rolloutID string, now time.Time) error {
	_, err := tx.Exec(ctx, `
		UPDATE assignments SET state = 'superseded', finished_at = $2
		WHERE rollout_id = $1 AND kind = 'forward' AND finished_at IS NULL
		  AND state IN ('assigned', 'downloading', 'verifying')`, rolloutID, now)
	if err != nil {
		return fmt.Errorf("supersede assignments: %w", err)
	}
	return nil
}
