package engine_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Bispulm/fleet-rollout/internal/engine"
	"github.com/Bispulm/fleet-rollout/internal/metrics"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

// fakeStore scripts the outcomes of lease and rollout calls, and records them.
type fakeStore struct {
	mu sync.Mutex

	acquireOK  bool
	acquireErr error
	renewErr   error
	listErr    error
	rollouts   []string
	// steps is how many times each rollout reports a change before settling.
	steps      map[string]int
	advanceErr map[string]error
	// result, if set, is what each change reports.
	result *store.AdvanceResult

	acquired int
	renewed  int
	released []store.Fence
	listed   int
	advanced []string
}

func (f *fakeStore) AcquireLease(context.Context, string, time.Duration) (store.Fence, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired++
	if f.acquireErr != nil || !f.acquireOK {
		return store.Fence{}, false, f.acquireErr
	}
	return store.Fence{Holder: "test", Generation: int64(f.acquired)}, true, nil
}

func (f *fakeStore) RenewLease(context.Context, store.Fence, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewed++
	return f.renewErr
}

func (f *fakeStore) ReleaseLease(_ context.Context, fence store.Fence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, fence)
	return nil
}

func (f *fakeStore) ListActiveRollouts(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
	return f.rollouts, f.listErr
}

func (f *fakeStore) AdvanceRollout(_ context.Context, id string, _ store.Fence) (store.AdvanceResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advanced = append(f.advanced, id)
	if err := f.advanceErr[id]; err != nil {
		return store.AdvanceResult{}, err
	}
	if f.steps[id] > 0 {
		f.steps[id]--
		if f.result != nil {
			return *f.result, nil
		}
		return store.AdvanceResult{Changed: true, Event: rollout.EventCoverageMet}, nil
	}
	return store.AdvanceResult{}, nil
}

func newEngine(f *fakeStore) *engine.Engine {
	return engine.New(engine.Options{
		Store:    f,
		Holder:   "test",
		Interval: time.Millisecond,
		LeaseTTL: time.Minute,
		Logger:   slog.New(slog.DiscardHandler),
	})
}

func TestStandbyDoesNoWork(t *testing.T) {
	f := &fakeStore{acquireOK: false, rollouts: []string{"r1"}}
	newEngine(f).Tick(t.Context())
	if f.listed != 0 || len(f.advanced) != 0 {
		t.Fatalf("a standby did work: listed %d, advanced %v", f.listed, f.advanced)
	}
}

func TestLeaderKeepsTheLeaseAcrossTicks(t *testing.T) {
	f := &fakeStore{acquireOK: true}
	e := newEngine(f)
	e.Tick(t.Context())
	e.Tick(t.Context())
	e.Tick(t.Context())
	if f.acquired != 1 || f.renewed != 2 {
		t.Fatalf("acquired %d times and renewed %d times, want 1 and 2", f.acquired, f.renewed)
	}
}

func TestLeaderAdvancesEachRolloutUntilItSettles(t *testing.T) {
	f := &fakeStore{
		acquireOK: true,
		rollouts:  []string{"r1", "r2"},
		steps:     map[string]int{"r1": 2},
	}
	newEngine(f).Tick(t.Context())
	// r1 changes twice and settles on the third call, and r2 is settled at once. So a
	// stage with no soak moves on within one tick, rather than one step per tick.
	if want := []string{"r1", "r1", "r1", "r2"}; !slices.Equal(f.advanced, want) {
		t.Fatalf("advanced = %v, want %v", f.advanced, want)
	}
}

func TestOneTickCannotLoopForever(t *testing.T) {
	f := &fakeStore{acquireOK: true, rollouts: []string{"r1"}, steps: map[string]int{"r1": 1_000_000}}
	newEngine(f).Tick(t.Context())
	if len(f.advanced) > 100 {
		t.Fatalf("one tick advanced %d times", len(f.advanced))
	}
}

func TestOneFailingRolloutDoesNotStopTheOthers(t *testing.T) {
	f := &fakeStore{
		acquireOK:  true,
		rollouts:   []string{"bad", "good"},
		steps:      map[string]int{"good": 1},
		advanceErr: map[string]error{"bad": errors.New("boom")},
	}
	newEngine(f).Tick(t.Context())
	if !slices.Contains(f.advanced, "good") {
		t.Fatalf("advanced = %v, want the healthy rollout reached", f.advanced)
	}
}

func TestLosingTheLeaseStopsWorkImmediately(t *testing.T) {
	f := &fakeStore{
		acquireOK:  true,
		rollouts:   []string{"r1", "r2"},
		advanceErr: map[string]error{"r1": store.ErrLeaseLost},
	}
	e := newEngine(f)
	e.Tick(t.Context())
	if slices.Contains(f.advanced, "r2") {
		t.Fatalf("kept working after losing the lease: %v", f.advanced)
	}

	// It has to win the lease again before doing anything else.
	f.advanced = nil
	f.acquireOK = false
	e.Tick(t.Context())
	if len(f.advanced) != 0 {
		t.Fatalf("worked without the lease: %v", f.advanced)
	}
}

func TestUncertainOwnershipStopsWork(t *testing.T) {
	for name, renewErr := range map[string]error{
		"lost":        store.ErrLeaseLost,
		"unreachable": errors.New("connection refused"),
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeStore{acquireOK: true, rollouts: []string{"r1"}}
			e := newEngine(f)
			e.Tick(t.Context())
			advancedBefore := len(f.advanced)

			f.renewErr = renewErr
			f.acquireOK = false
			e.Tick(t.Context())
			e.Tick(t.Context())
			if len(f.advanced) != advancedBefore {
				t.Fatalf("worked without knowing it still leads: %v", f.advanced)
			}
			if f.acquired < 2 {
				t.Fatalf("acquired %d times, want it to try to win the lease back", f.acquired)
			}
		})
	}
}

func TestDatabaseErrorsWhileAcquiringAreSurvivable(t *testing.T) {
	f := &fakeStore{acquireErr: errors.New("connection refused"), rollouts: []string{"r1"}}
	e := newEngine(f)
	e.Tick(t.Context())
	if len(f.advanced) != 0 {
		t.Fatalf("worked without the lease: %v", f.advanced)
	}
	f.acquireErr = nil
	f.acquireOK = true
	e.Tick(t.Context())
	if len(f.advanced) == 0 {
		t.Fatal("didn't recover once the database came back")
	}
}

func TestRunStopsOnCancelAndReleasesTheLease(t *testing.T) {
	f := &fakeStore{acquireOK: true}
	e := newEngine(f)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		renewed := f.renewed
		f.mu.Unlock()
		if renewed > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the engine never ticked twice")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't stop after cancellation")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.released) != 1 {
		t.Fatalf("released %d times, want the lease given up on the way out", len(f.released))
	}
}

func TestRunWithoutTheLeaseReleasesNothing(t *testing.T) {
	f := &fakeStore{acquireOK: false}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- newEngine(f).Run(ctx) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.released) != 0 {
		t.Fatalf("released a lease it didn't hold: %v", f.released)
	}
}

func TestDecisionsHaltsAndLeadershipAreCounted(t *testing.T) {
	f := &fakeStore{acquireOK: true, rollouts: []string{"r1"}, steps: map[string]int{"r1": 1},
		result: &store.AdvanceResult{
			Changed: true, Event: rollout.EventBreach, HaltReason: rollout.HaltStageBreach, RolledBack: true,
		}}
	m := metrics.New()
	e := engine.New(engine.Options{
		Store: f, Holder: "test", Interval: time.Millisecond, LeaseTTL: time.Minute,
		Logger: slog.New(slog.DiscardHandler), Metrics: m,
	})
	e.Tick(t.Context())

	scrape := func() string {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
		return rec.Body.String()
	}
	body := scrape()
	for _, want := range []string{
		`fleet_rollout_engine_decisions_total{event="breach"} 1`,
		`fleet_rollout_engine_decisions_total{event="rollback"} 1`,
		`fleet_rollout_rollout_halts_total{reason="stage_breach"} 1`,
		"fleet_rollout_engine_leader 1",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("metrics lack %q", want)
		}
	}

	f.mu.Lock()
	f.renewErr = errors.New("connection reset")
	f.mu.Unlock()
	e.Tick(t.Context())
	if body := scrape(); !strings.Contains(body, "fleet_rollout_engine_leader 0\n") {
		t.Error("an instance that lost the lease still reports leading")
	}
}

func TestGivingUpTheLeaseStopsReportingLeadership(t *testing.T) {
	f := &fakeStore{acquireOK: true}
	m := metrics.New()
	e := engine.New(engine.Options{
		Store: f, Holder: "test", Interval: time.Hour, LeaseTTL: time.Minute,
		Logger: slog.New(slog.DiscardHandler), Metrics: m,
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- e.Run(ctx) }()
	// The first tick runs at once, and takes the lease.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(scrapeMetrics(t, m), "fleet_rollout_engine_leader 1\n") {
		if time.Now().After(deadline) {
			t.Fatal("the engine never took the lease")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if body := scrapeMetrics(t, m); !strings.Contains(body, "fleet_rollout_engine_leader 0\n") {
		t.Error("an instance that gave up the lease on the way out still reports leading")
	}
}

func scrapeMetrics(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestEachEvaluationIsTraced(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	f := &fakeStore{acquireOK: true, rollouts: []string{"r1"}, steps: map[string]int{"r1": 1}}
	e := engine.New(engine.Options{
		Store: f, Holder: "test", Interval: time.Millisecond, LeaseTTL: time.Minute,
		Logger: slog.New(slog.DiscardHandler), Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)),
	})
	e.Tick(t.Context())
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Name() != "engine.advance" {
		t.Fatalf("spans = %d, want one engine.advance", len(spans))
	}
	if events := spans[0].Events(); len(events) != 1 || events[0].Name != string(rollout.EventCoverageMet) {
		t.Fatalf("events = %+v, want the decision", events)
	}
}
