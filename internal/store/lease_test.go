package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

const testLeaseTTL = time.Minute

func mustAcquire(t *testing.T, ctx context.Context, s *store.Store, holder string) store.Fence {
	t.Helper()
	fence, ok, err := s.AcquireLease(ctx, holder, testLeaseTTL)
	if err != nil {
		t.Fatalf("AcquireLease(%s): %v", holder, err)
	}
	if !ok {
		t.Fatalf("AcquireLease(%s) found the lease held", holder)
	}
	return fence
}

// leaseHarness exposes the pool, so tests can expire a lease without waiting for it.
func leaseHarness(t *testing.T) (*store.Store, context.Context, func()) {
	t.Helper()
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool := testdb.New(t)
	expire := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE engine_lease SET expires_at = clock_timestamp() - interval '1 second'`); err != nil {
			t.Fatalf("expire lease: %v", err)
		}
	}
	return store.New(pool), ctx, expire
}

func TestOnlyOneHolderAtATime(t *testing.T) {
	s, ctx, _ := leaseHarness(t)
	first := mustAcquire(t, ctx, s, "a")
	if first.Generation != 1 || first.Holder != "a" {
		t.Fatalf("fence = %+v", first)
	}
	if _, ok, err := s.AcquireLease(ctx, "b", testLeaseTTL); err != nil || ok {
		t.Fatalf("second holder: ok = %v, err = %v, want the lease refused", ok, err)
	}
}

func TestExpiredLeaseCanBeTakenOverWithANewGeneration(t *testing.T) {
	s, ctx, expire := leaseHarness(t)
	first := mustAcquire(t, ctx, s, "a")
	expire()
	second := mustAcquire(t, ctx, s, "b")
	if second.Generation != first.Generation+1 {
		t.Fatalf("generation = %d, want %d", second.Generation, first.Generation+1)
	}
}

func TestRenewKeepsTheLeaseAndFailsOnceItIsLost(t *testing.T) {
	s, ctx, expire := leaseHarness(t)
	first := mustAcquire(t, ctx, s, "a")
	if err := s.RenewLease(ctx, first, testLeaseTTL); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if _, ok, err := s.AcquireLease(ctx, "b", testLeaseTTL); err != nil || ok {
		t.Fatalf("takeover of a renewed lease: ok = %v, err = %v", ok, err)
	}

	expire()
	// An expired lease isn't renewed, even with nobody else waiting. The holder can't
	// know that nobody took over in the meantime.
	if err := s.RenewLease(ctx, first, testLeaseTTL); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("renewing an expired lease: error = %v, want ErrLeaseLost", err)
	}

	second := mustAcquire(t, ctx, s, "b")
	if err := s.RenewLease(ctx, first, testLeaseTTL); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("renewing after a takeover: error = %v, want ErrLeaseLost", err)
	}
	if err := s.RenewLease(ctx, second, testLeaseTTL); err != nil {
		t.Fatalf("new holder renewing: %v", err)
	}
}

func TestReleaseLetsAnotherInstanceTakeOverImmediately(t *testing.T) {
	s, ctx, _ := leaseHarness(t)
	first := mustAcquire(t, ctx, s, "a")
	if err := s.ReleaseLease(ctx, first); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	second := mustAcquire(t, ctx, s, "b")
	if second.Generation != first.Generation+1 {
		t.Fatalf("generation = %d", second.Generation)
	}
	// A stale holder releasing must not take the lease from the new one.
	if err := s.ReleaseLease(ctx, first); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	if err := s.RenewLease(ctx, second, testLeaseTTL); err != nil {
		t.Fatalf("the new holder lost its lease to a stale release: %v", err)
	}
}

// An engine transaction holds a shared lock on the lease row, so a takeover has to
// wait for it. That's what stops a decision committing after leadership changes.
func TestTakeoverWaitsForAnInFlightEngineTransaction(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testdb.New(t)
	s := store.New(pool)

	const shortTTL = 100 * time.Millisecond
	first, ok, err := s.AcquireLease(ctx, "a", shortTTL)
	if err != nil || !ok {
		t.Fatalf("AcquireLease: ok = %v, err = %v", ok, err)
	}
	inFlight, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = inFlight.Rollback(ctx) }()
	if _, err := inFlight.Exec(ctx, `SELECT generation FROM engine_lease WHERE generation = $1 FOR SHARE`, first.Generation); err != nil {
		t.Fatalf("hold shared lock: %v", err)
	}

	// Let the lease expire while the transaction is open.
	time.Sleep(2 * shortTTL)

	type result struct {
		fence store.Fence
		ok    bool
		err   error
	}
	takeover := make(chan result, 1)
	go func() {
		fence, ok, err := s.AcquireLease(ctx, "b", testLeaseTTL)
		takeover <- result{fence, ok, err}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("check blocked sessions: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the takeover never blocked on the in-flight transaction")
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case got := <-takeover:
		t.Fatalf("takeover finished while the transaction was open: %+v", got)
	default:
	}
	if err := inFlight.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got := <-takeover
	if got.err != nil || !got.ok || got.fence.Generation != first.Generation+1 {
		t.Fatalf("takeover = %+v", got)
	}
}
