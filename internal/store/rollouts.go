package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// NewRollout is a rollout being created. ID may be empty, in which case Postgres
// generates one. Supplying it makes cohorts reproducible across databases.
type NewRollout struct {
	ID         string
	Name       string
	ArtifactID string
	Policy     rollout.Policy
}

// StageState is the progress of one stage.
type StageState string

// The stage states.
const (
	StagePending StageState = "pending"
	StageActive  StageState = "active"
	StageSoaking StageState = "soaking"
	StagePassed  StageState = "passed"
	StageFailed  StageState = "failed"
	// StageSkipped marks a stage that rounding left with no new devices.
	StageSkipped StageState = "skipped"
	// StageStopped marks a stage whose rollout stopped for a reason other than this
	// stage's own results, such as an abort or a late failure in an earlier stage.
	StageStopped StageState = "stopped"
)

// Stage is a stage's size and progress.
type Stage struct {
	Ordinal         int
	Percent         int
	TargetCount     *int
	CohortSize      *int
	State           StageState
	StartedAt       *time.Time
	DeadlineAt      *time.Time
	SoakStartedAt   *time.Time
	SoakAccumulated time.Duration
	FinishedAt      *time.Time

	// Outcomes so far, derived from assignments each time they're read. A device
	// in flight is also counted as pending or unknown.
	Succeeded  int
	Failed     int
	Superseded int
	InFlight   int
	Unknown    int
	Pending    int
}

// Rollout is a rollout's full status.
type Rollout struct {
	ID           string
	FleetID      string
	FleetName    string
	ArtifactID   string
	Name         string
	State        rollout.State
	Revision     int64
	Policy       rollout.Policy
	PolicyDigest [sha256.Size]byte
	CurrentStage *int
	MemberCount  *int
	PausedFrom   *rollout.State
	PausedAt     *time.Time
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
	Stages       []Stage
	Exclusions   map[rollout.ExclusionReason]int
	// HaltReason and HaltDetail say why a rollout halted, if it did.
	HaltReason *string
	HaltDetail *string
	// Waiting explains what an active rollout needs before it can move on, using the
	// same evaluation the engine decides with.
	Waiting string
}

// CreateRollout creates a draft. It rejects a policy that the fleet's currently
// eligible devices could never satisfy, with a *rollout.PolicyError.
//
// Creation is idempotent, so a client can retry safely: repeating a request with
// the same name, artifact, and policy returns the existing rollout with created
// set to false. The same name with anything different is a *ConflictError.
func (s *Store) CreateRollout(ctx context.Context, fleetName string, n NewRollout) (Rollout, bool, error) {
	if n.ID != "" && !rollout.ValidUUID(n.ID) {
		return Rollout{}, false, &rollout.PolicyError{Violations: []rollout.Violation{
			{Field: "id", Message: fmt.Sprintf("%q is not a canonical lowercase UUID", n.ID)},
		}}
	}
	if !rollout.ValidUUID(n.ArtifactID) {
		return Rollout{}, false, notFound("artifact", n.ArtifactID)
	}
	encoded, digest, err := n.Policy.Canonical()
	if err != nil {
		return Rollout{}, false, err
	}

	// An insert that loses a race on a unique key aborts its transaction, so it
	// can't look for the winner. Running again from the start does, and the lookup
	// then tells an identical retry from a genuine conflict.
	id, created, err := s.createRolloutOnce(ctx, fleetName, n, encoded, digest)
	if isUniqueViolation(err, "") {
		id, created, err = s.createRolloutOnce(ctx, fleetName, n, encoded, digest)
	}
	switch {
	case isUniqueViolation(err, "rollouts_pkey"):
		return Rollout{}, false, &ConflictError{Message: fmt.Sprintf("rollout ID %s is already in use", n.ID)}
	case isUniqueViolation(err, ""):
		return Rollout{}, false, &ConflictError{Message: fmt.Sprintf("rollout %q is being created concurrently; try again", n.Name)}
	case err != nil:
		return Rollout{}, false, err
	}

	out, err := s.GetRollout(ctx, id)
	if err != nil {
		return Rollout{}, false, err
	}
	return out, created, nil
}

// createRolloutOnce makes one attempt in one transaction. A lost race on a unique
// key is returned as the Postgres error, for CreateRollout to handle.
func (s *Store) createRolloutOnce(
	ctx context.Context, fleetName string, n NewRollout, encoded []byte, digest [sha256.Size]byte,
) (id string, created bool, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		var fleetID string
		err := tx.QueryRow(ctx, `SELECT id FROM fleets WHERE name = $1`, fleetName).Scan(&fleetID)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("fleet", fleetName)
		}
		if err != nil {
			return fmt.Errorf("query fleet: %w", err)
		}

		// An identical retry must succeed even if the fleet has changed since, so
		// look for an existing rollout before judging feasibility.
		var existingID, existingArtifact string
		var existingDigest []byte
		err = tx.QueryRow(ctx, `SELECT id, artifact_id, policy_digest FROM rollouts WHERE fleet_id = $1 AND name = $2`,
			fleetID, n.Name).Scan(&existingID, &existingArtifact, &existingDigest)
		switch {
		case err == nil:
			if existingArtifact != n.ArtifactID || !bytes.Equal(existingDigest, digest[:]) ||
				(n.ID != "" && n.ID != existingID) {
				return &ConflictError{Message: fmt.Sprintf(
					"rollout %q already exists in fleet %q with a different definition", n.Name, fleetName)}
			}
			id = existingID
			return nil
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("query existing rollout: %w", err)
		}

		target, err := loadArtifactTarget(ctx, tx, n.ArtifactID)
		if err != nil {
			return err
		}
		candidates, err := loadCandidates(ctx, tx, fleetID)
		if err != nil {
			return err
		}
		eligible := 0
		for _, c := range candidates {
			if n.Policy.Exclusion(c, target) == "" {
				eligible++
			}
		}
		if _, err := n.Policy.Plan(eligible); err != nil {
			return err
		}

		err = tx.QueryRow(ctx, `
			INSERT INTO rollouts (id, fleet_id, artifact_id, name, policy, policy_digest, created_at)
			VALUES (COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()), $2, $3, $4, $5, $6, $7)
			RETURNING id`,
			n.ID, fleetID, n.ArtifactID, n.Name, encoded, digest[:], s.now()).Scan(&id)
		if err != nil {
			return fmt.Errorf("insert rollout: %w", err)
		}
		created = true

		for i, stage := range n.Policy.Stages {
			if _, err := tx.Exec(ctx, `INSERT INTO rollout_stages (rollout_id, ordinal, percent) VALUES ($1, $2, $3)`,
				id, i+1, stage.Percent); err != nil {
				return fmt.Errorf("insert stage %d: %w", i+1, err)
			}
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return id, created, nil
}

// GetRollout returns a rollout's status from one consistent snapshot, so a
// concurrent transition can't produce a mix of old and new values.
func (s *Store) GetRollout(ctx context.Context, id string) (Rollout, error) {
	if !rollout.ValidUUID(id) {
		return Rollout{}, notFound("rollout", id)
	}
	var out Rollout
	err := pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			out, err = readRollout(ctx, tx, id, s.now())
			return err
		})
	if err != nil {
		return Rollout{}, err
	}
	return out, nil
}

func readRollout(ctx context.Context, tx pgx.Tx, id string, now time.Time) (Rollout, error) {
	r := Rollout{ID: id, Exclusions: map[rollout.ExclusionReason]int{}}
	var digest []byte
	err := tx.QueryRow(ctx, `
		SELECT r.fleet_id, f.name, r.artifact_id, r.name, r.state, r.revision, r.policy, r.policy_digest,
		       r.current_stage, r.member_count, r.paused_from, r.paused_at,
		       r.created_at, r.started_at, r.finished_at, r.halt_reason, r.halt_detail
		FROM rollouts r JOIN fleets f ON f.id = r.fleet_id
		WHERE r.id = $1`, id).Scan(
		&r.FleetID, &r.FleetName, &r.ArtifactID, &r.Name, &r.State, &r.Revision, &r.Policy, &digest,
		&r.CurrentStage, &r.MemberCount, &r.PausedFrom, &r.PausedAt,
		&r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.HaltReason, &r.HaltDetail)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rollout{}, notFound("rollout", id)
	}
	if err != nil {
		return Rollout{}, fmt.Errorf("query rollout: %w", err)
	}
	if copy(r.PolicyDigest[:], digest) != sha256.Size {
		return Rollout{}, fmt.Errorf("rollout %s has a malformed policy digest", id)
	}

	rows, err := tx.Query(ctx, `
		SELECT ordinal, percent, target_count, cohort_size, state, started_at, deadline_at,
		       soak_started_at, soak_accumulated_ms, finished_at
		FROM rollout_stages WHERE rollout_id = $1 ORDER BY ordinal`, id)
	if err != nil {
		return Rollout{}, fmt.Errorf("query stages: %w", err)
	}
	r.Stages, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Stage, error) {
		var st Stage
		var soakMS int64
		err := row.Scan(&st.Ordinal, &st.Percent, &st.TargetCount, &st.CohortSize, &st.State, &st.StartedAt,
			&st.DeadlineAt, &st.SoakStartedAt, &soakMS, &st.FinishedAt)
		st.SoakAccumulated = time.Duration(soakMS) * time.Millisecond
		return st, err
	})
	if err != nil {
		return Rollout{}, fmt.Errorf("query stages: %w", err)
	}

	if r.CurrentStage != nil {
		staleBefore := now.Add(-r.Policy.Safety.StaleAfter.Std())
		if err := readOutcomes(ctx, tx, id, *r.CurrentStage, staleBefore, r.Stages); err != nil {
			return Rollout{}, err
		}
		switch r.State {
		case rollout.StateRunning, rollout.StateSoaking, rollout.StatePaused:
			r.Waiting = r.Policy.Evaluate(r.evaluation(now)).Waiting
		}
	}

	rows, err = tx.Query(ctx, `SELECT reason, count(*) FROM rollout_exclusions WHERE rollout_id = $1 GROUP BY reason`, id)
	if err != nil {
		return Rollout{}, fmt.Errorf("query exclusions: %w", err)
	}
	var reason rollout.ExclusionReason
	var count int
	if _, err := pgx.ForEachRow(rows, []any{&reason, &count}, func() error {
		r.Exclusions[reason] = count
		return nil
	}); err != nil {
		return Rollout{}, fmt.Errorf("query exclusions: %w", err)
	}
	return r, nil
}

// readOutcomes fills in the outcome counts of every exposed stage, deriving them
// from assignments. A member with no assignment yet is included, so a device that
// never polls can't vanish from the numbers. It's unknown once neither it nor the
// stage's start has happened since staleBefore.
func readOutcomes(ctx context.Context, tx pgx.Tx, id string, currentStage int, staleBefore time.Time, stages []Stage) error {
	rows, err := tx.Query(ctx, `
		SELECT m.stage_ordinal,
		       count(*),
		       count(*) FILTER (WHERE a.state = 'succeeded'),
		       count(*) FILTER (WHERE a.state = 'failed'),
		       count(*) FILTER (WHERE a.state = 'superseded'),
		       count(*) FILTER (WHERE a.state IN ('installing', 'health_check', 'reverting')),
		       count(*) FILTER (WHERE a.finished_at IS NULL AND GREATEST(d.last_seen_at, s.started_at) < $3)
		FROM rollout_members m
		JOIN devices d ON d.id = m.device_id
		JOIN rollout_stages s ON s.rollout_id = m.rollout_id AND s.ordinal = m.stage_ordinal
		LEFT JOIN assignments a ON a.rollout_id = m.rollout_id AND a.device_id = m.device_id AND a.kind = 'forward'
		WHERE m.rollout_id = $1 AND m.stage_ordinal <= $2
		GROUP BY m.stage_ordinal`, id, currentStage, staleBefore)
	if err != nil {
		return fmt.Errorf("query outcomes: %w", err)
	}
	var ordinal, cohort, succeeded, failed, superseded, inFlight, unknown int
	_, err = pgx.ForEachRow(rows, []any{&ordinal, &cohort, &succeeded, &failed, &superseded, &inFlight, &unknown}, func() error {
		for i := range stages {
			if stages[i].Ordinal != ordinal {
				continue
			}
			stages[i].Succeeded = succeeded
			stages[i].Failed = failed
			stages[i].Superseded = superseded
			stages[i].InFlight = inFlight
			stages[i].Unknown = unknown
			stages[i].Pending = cohort - succeeded - failed - superseded - unknown
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("query outcomes: %w", err)
	}
	return nil
}

func loadArtifactTarget(ctx context.Context, tx pgx.Tx, artifactID string) (rollout.ArtifactTarget, error) {
	var target rollout.ArtifactTarget
	err := tx.QueryRow(ctx, `SELECT platform, release_counter FROM artifacts WHERE id = $1`, artifactID).
		Scan(&target.Platform, &target.Release)
	if errors.Is(err, pgx.ErrNoRows) {
		return target, notFound("artifact", artifactID)
	}
	if err != nil {
		return target, fmt.Errorf("query artifact: %w", err)
	}
	return target, nil
}

func loadCandidates(ctx context.Context, tx pgx.Tx, fleetID string) ([]rollout.Candidate, error) {
	rows, err := tx.Query(ctx, `SELECT id, platform, status, labels, installed_release FROM devices WHERE fleet_id = $1`, fleetID)
	if err != nil {
		return nil, fmt.Errorf("query devices: %w", err)
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (rollout.Candidate, error) {
		var c rollout.Candidate
		err := row.Scan(&c.ID, &c.Platform, &c.Status, &c.Labels, &c.InstalledRelease)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("query devices: %w", err)
	}
	return candidates, nil
}
