// Package store persists fleets, artifacts, and rollouts in Postgres. Its methods
// own their transactions, so callers can't split work that must commit together.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const sqlStateUniqueViolation = "23505"

// ErrNotFound reports that a requested resource doesn't exist.
var ErrNotFound = errors.New("not found")

// ConflictError reports a request that can't apply to the current state.
type ConflictError struct {
	Message string
}

func (e *ConflictError) Error() string { return e.Message }

// Store is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// Option configures a Store.
type Option func(*Store)

// WithClock sets the clock used for rollout timers.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// New returns a Store backed by pool.
func New(pool *pgxpool.Pool, opts ...Option) *Store {
	s := &Store{pool: pool, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// inTx runs fn in a transaction, committing only if it returns nil.
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlStateUniqueViolation {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

func notFound(kind, name string) error {
	return fmt.Errorf("%s %q: %w", kind, name, ErrNotFound)
}
