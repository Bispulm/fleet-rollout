// Package engine advances rollouts. Any number of instances can run. One holds the
// lease and does the work, and the others wait to take over. It keeps nothing about
// rollouts in memory: each decision is made from Postgres, in a transaction that's
// rejected if this instance no longer holds the lease.
package engine

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/store"
)

const (
	// maxStepsPerRollout bounds one tick's work on a rollout. A rollout takes at most
	// two steps per stage, and the policy allows at most 20 stages.
	maxStepsPerRollout = 42
	releaseTimeout     = 2 * time.Second
)

// Store is what the engine needs from persistence.
type Store interface {
	AcquireLease(ctx context.Context, holder string, ttl time.Duration) (store.Fence, bool, error)
	RenewLease(ctx context.Context, fence store.Fence, ttl time.Duration) error
	ReleaseLease(ctx context.Context, fence store.Fence) error
	ListActiveRollouts(ctx context.Context) ([]string, error)
	AdvanceRollout(ctx context.Context, id string, fence store.Fence) (store.AdvanceResult, error)
}

// Options configures an Engine. LeaseTTL should be several times Interval, so that a
// slow tick or two doesn't cost this instance its lease.
type Options struct {
	Store    Store
	Holder   string
	Interval time.Duration
	LeaseTTL time.Duration
	Logger   *slog.Logger
}

// Engine runs the evaluation loop. Run and Tick must not be called concurrently.
type Engine struct {
	store    Store
	holder   string
	interval time.Duration
	leaseTTL time.Duration
	logger   *slog.Logger
	fence    *store.Fence
}

// New returns an Engine.
func New(opts Options) *Engine {
	return &Engine{
		store:    opts.Store,
		holder:   opts.Holder,
		interval: opts.Interval,
		leaseTTL: opts.LeaseTTL,
		logger:   opts.Logger,
	}
}

// Run ticks until ctx is canceled, then gives up the lease so that another instance
// doesn't have to wait for it to expire.
func (e *Engine) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		e.Tick(ctx)
		select {
		case <-ctx.Done():
			e.release(ctx)
			return nil
		case <-ticker.C:
		}
	}
}

// Tick confirms leadership and evaluates every active rollout once. If this instance
// isn't certain it leads, it does nothing. The lease check inside each transaction is
// what makes that safe. Stopping here just avoids pointless work.
func (e *Engine) Tick(ctx context.Context) {
	// A stuck query mustn't hold a tick open past the point the lease could be lost.
	ctx, cancel := context.WithTimeout(ctx, e.leaseTTL)
	defer cancel()

	if !e.ensureLease(ctx) {
		return
	}
	ids, err := e.store.ListActiveRollouts(ctx)
	if err != nil {
		e.logger.LogAttrs(ctx, slog.LevelError, "list active rollouts", slog.Any("error", err))
		return
	}
	for _, id := range ids {
		if !e.advance(ctx, id) {
			return
		}
	}
}

// ensureLease reports whether this instance holds the lease, acquiring or renewing it.
// A failed renewal drops the lease even if it was only a network problem, because the
// instance can no longer be sure that it leads.
func (e *Engine) ensureLease(ctx context.Context) bool {
	if e.fence != nil {
		err := e.store.RenewLease(ctx, *e.fence, e.leaseTTL)
		if err == nil {
			return true
		}
		e.dropLease(ctx, err)
		return false
	}
	fence, acquired, err := e.store.AcquireLease(ctx, e.holder, e.leaseTTL)
	if err != nil {
		e.logger.LogAttrs(ctx, slog.LevelWarn, "acquire engine lease", slog.Any("error", err))
		return false
	}
	if !acquired {
		return false
	}
	e.fence = &fence
	e.logger.LogAttrs(ctx, slog.LevelInfo, "became engine leader",
		slog.String("holder", fence.Holder), slog.Int64("generation", fence.Generation))
	return true
}

func (e *Engine) dropLease(ctx context.Context, cause error) {
	e.logger.LogAttrs(ctx, slog.LevelWarn, "no longer certain of engine leadership",
		slog.Int64("generation", e.fence.Generation), slog.Any("error", cause))
	e.fence = nil
}

// advance evaluates one rollout until it settles, so that a stage with no soak moves
// on within a tick. It returns false if the lease was lost and all work must stop. Any
// other failure is logged, and leaves this rollout for the next tick without affecting
// the others.
func (e *Engine) advance(ctx context.Context, id string) bool {
	for range maxStepsPerRollout {
		result, err := e.store.AdvanceRollout(ctx, id, *e.fence)
		if errors.Is(err, store.ErrLeaseLost) {
			e.dropLease(ctx, err)
			return false
		}
		if err != nil {
			e.logger.LogAttrs(ctx, slog.LevelError, "advance rollout",
				slog.String("rollout_id", id), slog.Any("error", err))
			return true
		}
		if !result.Changed {
			return true
		}
		e.logger.LogAttrs(ctx, slog.LevelInfo, "rollout changed",
			slog.String("rollout_id", id),
			slog.String("event", string(result.Event)),
			slog.String("from", string(result.From)),
			slog.String("to", string(result.To)),
			slog.Int("stage", result.Stage),
			slog.String("detail", result.Detail))
	}
	return true
}

func (e *Engine) release(ctx context.Context) {
	if e.fence == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := e.store.ReleaseLease(ctx, *e.fence); err != nil {
		// It will expire by itself, so this only delays a takeover.
		e.logger.LogAttrs(ctx, slog.LevelWarn, "release engine lease", slog.Any("error", err))
	}
	e.fence = nil
}
