package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// ErrMembershipNotFrozen reports that a rollout hasn't started, so it has no
// membership to show.
var ErrMembershipNotFrozen = errors.New("membership is frozen when a rollout starts")

// CohortQuery selects a page of a rollout's frozen membership.
type CohortQuery struct {
	// Stage limits results to one stage's cohort. Zero means all stages.
	Stage int
	// AfterPosition is the last position already seen.
	AfterPosition int
	Limit         int
}

// CohortEntry is one member of a rollout.
type CohortEntry struct {
	DeviceID string
	Position int
	Stage    int
	RankKey  [sha256.Size]byte
}

// ListCohort returns members in position order.
func (s *Store) ListCohort(ctx context.Context, id string, q CohortQuery) ([]CohortEntry, error) {
	if !rollout.ValidUUID(id) {
		return nil, notFound("rollout", id)
	}
	var memberCount *int
	err := s.pool.QueryRow(ctx, `SELECT member_count FROM rollouts WHERE id = $1`, id).Scan(&memberCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("rollout", id)
	}
	if err != nil {
		return nil, fmt.Errorf("query rollout: %w", err)
	}
	if memberCount == nil {
		return nil, fmt.Errorf("rollout %s: %w", id, ErrMembershipNotFrozen)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT device_id, position, stage_ordinal, rank_key
		FROM rollout_members
		WHERE rollout_id = $1 AND position > $2 AND ($3::int = 0 OR stage_ordinal = $3)
		ORDER BY position LIMIT $4`, id, q.AfterPosition, q.Stage, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("query members: %w", err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (CohortEntry, error) {
		var entry CohortEntry
		var key []byte
		if err := row.Scan(&entry.DeviceID, &entry.Position, &entry.Stage, &key); err != nil {
			return entry, err
		}
		if copy(entry.RankKey[:], key) != sha256.Size {
			return entry, fmt.Errorf("member %q has a malformed rank key", entry.DeviceID)
		}
		return entry, nil
	})
	if err != nil {
		return nil, fmt.Errorf("query members: %w", err)
	}
	return entries, nil
}
