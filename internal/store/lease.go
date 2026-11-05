package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const engineLeaseName = "rollout-engine"

// ErrLeaseLost reports that a caller no longer holds the engine lease, or can't be
// sure that it does. Either way, it must stop making decisions.
var ErrLeaseLost = errors.New("engine lease lost")

// Fence identifies one period of engine leadership. A new holder always gets a higher
// generation, so a stale process can be told apart from the current one.
type Fence struct {
	Holder     string
	Generation int64
}

// AcquireLease takes the engine lease if it's free or expired. The second result is
// false if another instance holds it. All expiry uses Postgres's clock, so clock skew
// between instances doesn't matter.
func (s *Store) AcquireLease(ctx context.Context, holder string, ttl time.Duration) (Fence, bool, error) {
	fence := Fence{Holder: holder}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO engine_lease AS l (name, holder, generation, expires_at)
		VALUES ($1, $2, 1, clock_timestamp() + ($3::bigint * interval '1 microsecond'))
		ON CONFLICT (name) DO UPDATE
		    SET holder = EXCLUDED.holder,
		        generation = l.generation + 1,
		        expires_at = EXCLUDED.expires_at
		    WHERE l.expires_at <= clock_timestamp()
		RETURNING generation`,
		engineLeaseName, holder, ttl.Microseconds()).Scan(&fence.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return Fence{}, false, nil
	}
	if err != nil {
		return Fence{}, false, fmt.Errorf("acquire engine lease: %w", err)
	}
	return fence, true, nil
}

// RenewLease extends a lease that's still valid. An expired lease isn't renewed, even
// if nobody else took it, because the holder can't know that nothing happened in the
// meantime. It has to acquire the lease again, which raises the generation.
func (s *Store) RenewLease(ctx context.Context, fence Fence, ttl time.Duration) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE engine_lease
		SET expires_at = clock_timestamp() + ($4::bigint * interval '1 microsecond')
		WHERE name = $1 AND holder = $2 AND generation = $3 AND expires_at > clock_timestamp()`,
		engineLeaseName, fence.Holder, fence.Generation, ttl.Microseconds())
	if err != nil {
		return fmt.Errorf("renew engine lease: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// ReleaseLease gives up the lease so another instance can take over without waiting
// for it to expire. Releasing a lease that's already gone does nothing.
func (s *Store) ReleaseLease(ctx context.Context, fence Fence) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE engine_lease SET expires_at = clock_timestamp()
		WHERE name = $1 AND holder = $2 AND generation = $3`,
		engineLeaseName, fence.Holder, fence.Generation)
	if err != nil {
		return fmt.Errorf("release engine lease: %w", err)
	}
	return nil
}

// checkFence must be the first thing an engine transaction does. It fails unless this
// leadership is still valid, and its shared lock makes a takeover wait for this
// transaction. So once a new holder has the lease, no older decision can still commit.
func checkFence(ctx context.Context, tx pgx.Tx, fence Fence) error {
	var generation int64
	err := tx.QueryRow(ctx, `
		SELECT generation FROM engine_lease
		WHERE name = $1 AND holder = $2 AND generation = $3 AND expires_at > clock_timestamp()
		FOR SHARE`, engineLeaseName, fence.Holder, fence.Generation).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return fmt.Errorf("check engine lease: %w", err)
	}
	return nil
}
