package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

// engineWorld is a store with a fake clock and a held engine lease.
type engineWorld struct {
	t     *testing.T
	ctx   context.Context
	store *store.Store
	clock *fakeClock
	fence store.Fence
	world world
}

func newEngineWorld(t *testing.T, devices int) *engineWorld {
	t.Helper()
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	return &engineWorld{
		t: t, ctx: ctx, store: s, clock: clock,
		fence: mustAcquire(t, ctx, s, "engine-a"),
		world: newWorld(t, ctx, s, devices),
	}
}

// startRollout creates and starts a rollout. Stages are 10, 50, and 100 percent, each
// needing all of its cohort to report, soaking for 10 minutes, with a 1 hour deadline.
func (e *engineWorld) startRollout(edit func(*rollout.PolicySpec)) store.Rollout {
	e.t.Helper()
	input := e.world.newRollout(e.t, "demo")
	input.Policy = mustPolicy(e.t, func(p *rollout.PolicySpec) {
		p.Safety.RequiredCoverageBPS = ptr(rollout.FullBPS)
		if edit != nil {
			edit(p)
		}
	})
	created, _, err := e.store.CreateRollout(e.ctx, "lab-a", input)
	if err != nil {
		e.t.Fatalf("CreateRollout: %v", err)
	}
	return mustStart(e.t, e.store, created)
}

func (e *engineWorld) advance(id string) store.AdvanceResult {
	e.t.Helper()
	result, err := e.store.AdvanceRollout(e.ctx, id, e.fence)
	if err != nil {
		e.t.Fatalf("AdvanceRollout: %v", err)
	}
	return result
}

func (e *engineWorld) get(id string) store.Rollout {
	e.t.Helper()
	r, err := e.store.GetRollout(e.ctx, id)
	if err != nil {
		e.t.Fatalf("GetRollout: %v", err)
	}
	return r
}

// finish makes the devices at these positions succeed, or fail if told to.
func (e *engineWorld) finish(id string, outcome rollout.AssignmentState, positions ...int) {
	e.t.Helper()
	for _, position := range positions {
		device := memberAt(e.t, e.ctx, e.store, id, position)
		a := poll(e.t, e.ctx, e.store, device)
		if a == nil {
			e.t.Fatalf("position %d (%s) has no assignment", position, device)
		}
		if outcome == rollout.AssignmentFailed {
			mustReport(e.t, e.ctx, e.store, device, a, 1, rollout.AssignmentFailed)
			continue
		}
		mustReport(e.t, e.ctx, e.store, device, a, 1, rollout.AssignmentHealthCheck)
		mustReport(e.t, e.ctx, e.store, device, a, 2, rollout.AssignmentSucceeded)
	}
}

func span(from, to int) []int {
	out := make([]int, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

func TestCoverageEntersSoakAndStartsTheClock(t *testing.T) {
	e := newEngineWorld(t, 20)
	r := e.startRollout(nil)

	e.finish(r.ID, rollout.AssignmentSucceeded, 1)
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("advanced with 1 of 2 outcomes: %+v", got)
	}

	e.clock.Advance(time.Minute)
	e.finish(r.ID, rollout.AssignmentSucceeded, 2)
	got := e.advance(r.ID)
	if !got.Changed || got.Event != rollout.EventCoverageMet || got.To != rollout.StateSoaking {
		t.Fatalf("result = %+v", got)
	}
	after := e.get(r.ID)
	stage := after.Stages[0]
	if after.State != rollout.StateSoaking || stage.State != store.StageSoaking {
		t.Fatalf("rollout = %s, stage = %s", after.State, stage.State)
	}
	if stage.SoakStartedAt == nil || !stage.SoakStartedAt.Equal(e.clock.Now()) {
		t.Fatalf("soak started at %v, want %v", stage.SoakStartedAt, e.clock.Now())
	}
}

func TestSoakThenPromotionExposesTheNextStage(t *testing.T) {
	e := newEngineWorld(t, 20)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)
	e.advance(r.ID)

	// A device in the next stage isn't exposed yet.
	third := memberAt(t, e.ctx, e.store, r.ID, 3)
	if got := poll(t, e.ctx, e.store, third); got != nil {
		t.Fatalf("a stage 2 device got an assignment during stage 1: %+v", got)
	}

	e.clock.Advance(10*time.Minute - time.Second)
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("promoted before the soak finished: %+v", got)
	}

	e.clock.Advance(time.Second)
	got := e.advance(r.ID)
	if !got.Changed || got.Event != rollout.EventSoakPassed || got.To != rollout.StateRunning {
		t.Fatalf("result = %+v", got)
	}
	after := e.get(r.ID)
	if *after.CurrentStage != 2 || after.State != rollout.StateRunning {
		t.Fatalf("rollout = %+v", after)
	}
	if after.Stages[0].State != store.StagePassed || after.Stages[0].FinishedAt == nil {
		t.Fatalf("first stage = %+v", after.Stages[0])
	}
	second := after.Stages[1]
	if second.State != store.StageActive || !second.StartedAt.Equal(e.clock.Now()) {
		t.Fatalf("second stage = %+v", second)
	}
	if want := e.clock.Now().Add(time.Hour); !second.DeadlineAt.Equal(want) {
		t.Fatalf("deadline = %v, want %v", second.DeadlineAt, want)
	}

	if got := poll(t, e.ctx, e.store, third); got == nil {
		t.Fatal("a stage 2 device got nothing after promotion")
	}
}

func TestFinalStageCompletesAndFreesTheFleet(t *testing.T) {
	e := newEngineWorld(t, 2)
	r := e.startRollout(func(p *rollout.PolicySpec) {
		p.Stages = []rollout.StageSpec{{Percent: 100, Soak: ptr(rollout.Duration(0))}}
	})
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)
	e.advance(r.ID)
	got := e.advance(r.ID)
	if got.Event != rollout.EventFinalSoakPassed || got.To != rollout.StateCompleted {
		t.Fatalf("result = %+v", got)
	}
	after := e.get(r.ID)
	if after.State != rollout.StateCompleted || after.FinishedAt == nil || after.Stages[0].State != store.StagePassed {
		t.Fatalf("rollout = %+v", after)
	}
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("a finished rollout changed: %+v", got)
	}

	second := e.world.newRollout(t, "next")
	created, _, err := e.store.CreateRollout(e.ctx, "lab-a", second)
	var policyErr *rollout.PolicyError
	if !errors.As(err, &policyErr) {
		// Every device is already on this release, so there's nobody left to update.
		t.Fatalf("follow-up with nobody eligible: created = %+v, err = %v", created, err)
	}
}

func TestEmptyStagesAreSkippedOnTheWayThrough(t *testing.T) {
	e := newEngineWorld(t, 3)
	r := e.startRollout(func(p *rollout.PolicySpec) {
		zero := ptr(rollout.Duration(0))
		p.Stages = []rollout.StageSpec{{Percent: 1, Soak: zero}, {Percent: 10, Soak: zero}, {Percent: 50, Soak: zero}, {Percent: 100, Soak: zero}}
	})
	// Targets are 1, 1, 2, and 3, so stage 2 exposes nobody.
	e.finish(r.ID, rollout.AssignmentSucceeded, 1)
	e.advance(r.ID)
	e.advance(r.ID)
	if got := *e.get(r.ID).CurrentStage; got != 3 {
		t.Fatalf("current stage = %d, want 3, skipping the empty stage 2", got)
	}

	e.finish(r.ID, rollout.AssignmentSucceeded, 2)
	e.advance(r.ID)
	e.advance(r.ID)
	e.finish(r.ID, rollout.AssignmentSucceeded, 3)
	e.advance(r.ID)
	if got := e.advance(r.ID); got.To != rollout.StateCompleted {
		t.Fatalf("result = %+v", got)
	}
}

func TestFinalStagesThatAreEmptyStillComplete(t *testing.T) {
	e := newEngineWorld(t, 1)
	r := e.startRollout(func(p *rollout.PolicySpec) {
		zero := ptr(rollout.Duration(0))
		p.Stages = []rollout.StageSpec{{Percent: 1, Soak: zero}, {Percent: 50, Soak: zero}, {Percent: 100, Soak: zero}}
	})
	e.finish(r.ID, rollout.AssignmentSucceeded, 1)
	e.advance(r.ID)
	if got := e.advance(r.ID); got.To != rollout.StateCompleted {
		t.Fatalf("result = %+v, want completion, with the remaining stages empty", got)
	}
}

func TestBreachHaltsAndBoundsFurtherExposure(t *testing.T) {
	e := newEngineWorld(t, 100)
	r := e.startRollout(func(p *rollout.PolicySpec) {
		p.Stages[0].MinObservations = ptr(5)
	})
	// Stage 1 is 10 devices: 5 report, 2 of them fail, 1 is installing, 1 has its
	// assignment but hasn't started, and 1 hasn't polled at all.
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2, 3)
	e.finish(r.ID, rollout.AssignmentFailed, 4, 5)

	installing := memberAt(t, e.ctx, e.store, r.ID, 6)
	inFlight := poll(t, e.ctx, e.store, installing)
	mustReport(t, e.ctx, e.store, installing, inFlight, 1, rollout.AssignmentInstalling)
	waiting := memberAt(t, e.ctx, e.store, r.ID, 7)
	abandoned := poll(t, e.ctx, e.store, waiting)
	neverPolled := memberAt(t, e.ctx, e.store, r.ID, 8)

	got := e.advance(r.ID)
	if !got.Changed || got.Event != rollout.EventBreach || got.To != rollout.StateHalted {
		t.Fatalf("result = %+v", got)
	}

	after := e.get(r.ID)
	if after.HaltReason == nil || *after.HaltReason != string(rollout.HaltStageBreach) {
		t.Fatalf("halt reason = %v", after.HaltReason)
	}
	if after.HaltDetail == nil || !strings.Contains(*after.HaltDetail, "2 failed") {
		t.Fatalf("halt detail = %v", after.HaltDetail)
	}
	stage := after.Stages[0]
	if stage.State != store.StageFailed || stage.FinishedAt == nil {
		t.Fatalf("stage = %+v", stage)
	}
	if stage.Succeeded != 3 || stage.Failed != 2 || stage.InFlight != 1 || stage.Superseded != 1 {
		t.Fatalf("outcomes = %+v", stage)
	}

	// No new exposure, and work that hadn't started is cancelled.
	if a := poll(t, e.ctx, e.store, neverPolled); a != nil {
		t.Fatalf("a new assignment after the halt: %+v", a)
	}
	if a := poll(t, e.ctx, e.store, waiting); a != nil {
		t.Fatalf("a superseded device was told to continue: %+v", a)
	}
	result := mustReport(t, e.ctx, e.store, waiting, abandoned, 1, rollout.AssignmentInstalling)
	if result.Applied {
		t.Fatalf("a report on superseded work applied: %+v", result)
	}

	// The device past the point of no return finishes, and it still counts.
	if a := poll(t, e.ctx, e.store, installing); a == nil || a.Directive != rollout.DirectiveProceed {
		t.Fatalf("in-flight device got %+v", a)
	}
	mustReport(t, e.ctx, e.store, installing, inFlight, 2, rollout.AssignmentHealthCheck)
	mustReport(t, e.ctx, e.store, installing, inFlight, 3, rollout.AssignmentSucceeded)
	if got := e.get(r.ID).Stages[0].Succeeded; got != 4 {
		t.Fatalf("succeeded = %d, want the in-flight device counted", got)
	}

	// Halted is sticky. More good news doesn't resume it.
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("a halted rollout changed: %+v", got)
	}
	if _, err := e.store.ResumeRollout(e.ctx, r.ID, byAdmin); err == nil {
		t.Fatal("resumed a halted rollout")
	}
}

func TestLateFailureInAPassedStageHaltsWithoutRewritingItsVerdict(t *testing.T) {
	e := newEngineWorld(t, 100)
	r := e.startRollout(func(p *rollout.PolicySpec) {
		p.Safety.RequiredCoverageBPS = ptr(9000)
		p.Safety.RolloutMaxFailureBPS = ptr(rollout.FullBPS) // Isolate the late-regression check.
		zero := ptr(rollout.Duration(0))
		p.Stages[0].Soak = zero
	})
	// 9 of 10 report success, which meets the 90% coverage, so stage 1 passes.
	e.finish(r.ID, rollout.AssignmentSucceeded, span(1, 9)...)
	straggler := memberAt(t, e.ctx, e.store, r.ID, 10)
	late := poll(t, e.ctx, e.store, straggler)
	e.advance(r.ID)
	e.advance(r.ID)
	if got := e.get(r.ID); *got.CurrentStage != 2 || got.Stages[0].State != store.StagePassed {
		t.Fatalf("stage 1 should have passed: %+v", got)
	}

	// The straggler fails after its stage passed: 1 in 10 is above the 2% limit.
	mustReport(t, e.ctx, e.store, straggler, late, 1, rollout.AssignmentFailed)
	got := e.advance(r.ID)
	if got.Event != rollout.EventBreach {
		t.Fatalf("result = %+v", got)
	}
	after := e.get(r.ID)
	if after.HaltReason == nil || *after.HaltReason != string(rollout.HaltLateRegression) {
		t.Fatalf("halt reason = %v", after.HaltReason)
	}
	if after.Stages[0].State != store.StagePassed {
		t.Fatalf("stage 1's verdict was rewritten to %q", after.Stages[0].State)
	}
	if after.Stages[1].State != store.StageFailed {
		t.Fatalf("stage 2 = %q, want failed", after.Stages[1].State)
	}
}

func TestDeadlineHaltsAStageThatNeverGetsCoverage(t *testing.T) {
	e := newEngineWorld(t, 20)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1)

	e.clock.Advance(time.Hour - time.Second)
	// The other device was heard from, so the only problem is coverage.
	poll(t, e.ctx, e.store, memberAt(t, e.ctx, e.store, r.ID, 2))
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("halted before the deadline: %+v", got)
	}

	e.clock.Advance(time.Second)
	got := e.advance(r.ID)
	if got.Event != rollout.EventBreach {
		t.Fatalf("result = %+v", got)
	}
	if reason := e.get(r.ID).HaltReason; reason == nil || *reason != string(rollout.HaltDeadline) {
		t.Fatalf("halt reason = %v", reason)
	}
}

func TestUnknownDevicesBlockPromotionUntilTheDeadlineHalts(t *testing.T) {
	e := newEngineWorld(t, 100)
	r := e.startRollout(func(p *rollout.PolicySpec) {
		p.Safety.RequiredCoverageBPS = ptr(8000)
		p.Safety.MaxUnknownBPS = ptr(1000)
		p.Safety.StaleAfter = ptr(rollout.Duration(15 * time.Minute))
		p.Stages[0].Soak = ptr(rollout.Duration(30 * time.Minute))
	})
	// 8 of 10 report, meeting coverage. The other 2 go quiet, and 20% is over the 10% limit.
	e.finish(r.ID, rollout.AssignmentSucceeded, span(1, 8)...)
	if got := e.advance(r.ID); got.Event != rollout.EventCoverageMet {
		t.Fatalf("result = %+v", got)
	}

	e.clock.Advance(30 * time.Minute)
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("promoted with too many unknown devices: %+v", got)
	}
	if waiting := e.get(r.ID).Waiting; !strings.Contains(waiting, "unknown") {
		t.Fatalf("waiting = %q, want it to explain the unknown devices", waiting)
	}

	e.clock.Advance(30 * time.Minute)
	got := e.advance(r.ID)
	if got.Event != rollout.EventBreach {
		t.Fatalf("result = %+v, want the deadline to end the wait", got)
	}
	if reason := e.get(r.ID).HaltReason; reason == nil || *reason != string(rollout.HaltDeadline) {
		t.Fatalf("halt reason = %v", reason)
	}
}

func TestADeviceComingBackClearsItsUnknownStatus(t *testing.T) {
	e := newEngineWorld(t, 100)
	r := e.startRollout(func(p *rollout.PolicySpec) {
		p.Safety.RequiredCoverageBPS = ptr(8000)
		p.Safety.MaxUnknownBPS = ptr(1000)
	})
	e.finish(r.ID, rollout.AssignmentSucceeded, span(1, 8)...)
	e.advance(r.ID)
	e.clock.Advance(20 * time.Minute)
	if got := e.get(r.ID).Stages[0].Unknown; got != 2 {
		t.Fatalf("unknown = %d, want 2", got)
	}

	poll(t, e.ctx, e.store, memberAt(t, e.ctx, e.store, r.ID, 9))
	if got := e.get(r.ID).Stages[0].Unknown; got != 1 {
		t.Fatalf("unknown = %d after a device came back, want 1", got)
	}
	if got := e.advance(r.ID); !got.Changed || got.Event != rollout.EventSoakPassed {
		t.Fatalf("result = %+v, want promotion at exactly the unknown limit", got)
	}
}

func TestPausedRolloutsDoNotProgressButStillHalt(t *testing.T) {
	e := newEngineWorld(t, 20)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)
	if _, err := e.store.PauseRollout(e.ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("PauseRollout: %v", err)
	}
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("progress during a pause: %+v", got)
	}

	// The deadline is stopped during a pause, so this mustn't halt.
	e.clock.Advance(3 * time.Hour)
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("a paused rollout timed out: %+v", got)
	}
	if _, err := e.store.ResumeRollout(e.ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("ResumeRollout: %v", err)
	}
	if got := e.advance(r.ID); got.Event != rollout.EventCoverageMet {
		t.Fatalf("result after resuming = %+v", got)
	}
}

func TestBreachDuringAPauseHalts(t *testing.T) {
	e := newEngineWorld(t, 20)
	r := e.startRollout(nil)
	device := memberAt(t, e.ctx, e.store, r.ID, 1)
	a := poll(t, e.ctx, e.store, device)
	mustReport(t, e.ctx, e.store, device, a, 1, rollout.AssignmentInstalling)
	if _, err := e.store.PauseRollout(e.ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("PauseRollout: %v", err)
	}
	// In-flight work continues during a pause, and it can fail.
	mustReport(t, e.ctx, e.store, device, a, 2, rollout.AssignmentFailed)

	got := e.advance(r.ID)
	if got.Event != rollout.EventBreach || got.From != rollout.StatePaused {
		t.Fatalf("result = %+v", got)
	}
	after := e.get(r.ID)
	if after.State != rollout.StateHalted || after.PausedAt != nil || after.PausedFrom != nil {
		t.Fatalf("rollout = %+v", after)
	}
}

func TestPauseDuringSoakDoesNotCountTowardIt(t *testing.T) {
	e := newEngineWorld(t, 20)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)
	e.advance(r.ID)

	e.clock.Advance(4 * time.Minute)
	if _, err := e.store.PauseRollout(e.ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("PauseRollout: %v", err)
	}
	e.clock.Advance(time.Hour)
	if _, err := e.store.ResumeRollout(e.ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("ResumeRollout: %v", err)
	}

	e.clock.Advance(6*time.Minute - time.Second)
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("promoted early: %+v", got)
	}
	e.clock.Advance(time.Second)
	if got := e.advance(r.ID); got.Event != rollout.EventSoakPassed {
		t.Fatalf("result = %+v, want promotion after 10 soaking minutes", got)
	}
}

func TestStaleLeaderCannotAdvance(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := newFakeClock()
	pool := testdb.New(t)
	s := store.New(pool, store.WithClock(clock.Now))
	w := newWorld(t, ctx, s, 20)
	r := mustStart(t, s, w.mustCreate(t, ctx, "demo"))
	e := &engineWorld{t: t, ctx: ctx, store: s, clock: clock, world: w}
	e.finish(r.ID, rollout.AssignmentSucceeded, span(1, 2)...)
	before := e.get(r.ID)

	oldLeader := mustAcquire(t, ctx, s, "engine-a")
	if _, err := pool.Exec(ctx, `UPDATE engine_lease SET expires_at = clock_timestamp() - interval '1 second'`); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	// Expired but not yet taken over: it still can't act, since it can't know.
	if _, err := s.AdvanceRollout(ctx, r.ID, oldLeader); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("expired lease: error = %v, want ErrLeaseLost", err)
	}

	newLeader := mustAcquire(t, ctx, s, "engine-b")
	if _, err := s.AdvanceRollout(ctx, r.ID, oldLeader); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("after takeover: error = %v, want ErrLeaseLost", err)
	}
	unchanged := e.get(r.ID)
	if unchanged.Revision != before.Revision || unchanged.State != before.State {
		t.Fatalf("a stale leader changed the rollout: %+v", unchanged)
	}

	if got, err := s.AdvanceRollout(ctx, r.ID, newLeader); err != nil || !got.Changed {
		t.Fatalf("new leader: result = %+v, err = %v", got, err)
	}
}

func TestConcurrentAdvancesDecideOnce(t *testing.T) {
	e := newEngineWorld(t, 20)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)

	const attempts = 8
	results := make([]store.AdvanceResult, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() { results[i], errs[i] = e.store.AdvanceRollout(e.ctx, r.ID, e.fence) })
	}
	wg.Wait()

	changed := 0
	for i := range attempts {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if results[i].Changed {
			changed++
		}
	}
	if changed != 1 {
		t.Fatalf("%d attempts changed the rollout, want 1", changed)
	}
	log, err := e.store.ListTransitions(e.ctx, r.ID)
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	if last := log[len(log)-1]; len(log) != 2 || last.Event != rollout.EventCoverageMet {
		t.Fatalf("log = %+v", log)
	}
}

func TestEngineDecisionsAreAuditedWithTheirGeneration(t *testing.T) {
	e := newEngineWorld(t, 20)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)
	e.advance(r.ID)

	log, err := e.store.ListTransitions(e.ctx, r.ID)
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	last := log[len(log)-1]
	if last.Actor != "engine" || last.EngineGeneration == nil || *last.EngineGeneration != e.fence.Generation {
		t.Fatalf("entry = %+v", last)
	}
	if log[0].EngineGeneration != nil {
		t.Fatalf("an operator's action has a generation: %+v", log[0])
	}
}

func TestRestartRebuildsEverythingFromTheDatabase(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := newFakeClock()
	pool := testdb.New(t)
	before := store.New(pool, store.WithClock(clock.Now))
	w := newWorld(t, ctx, before, 20)
	r := mustStart(t, before, w.mustCreate(t, ctx, "demo"))
	e := &engineWorld{t: t, ctx: ctx, store: before, clock: clock, world: w, fence: mustAcquire(t, ctx, before, "engine-a")}
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)
	e.advance(r.ID)
	clock.Advance(4 * time.Minute)

	// A new process with no memory of any of that: only Postgres carries over.
	after := store.New(pool, store.WithClock(clock.Now))
	if _, err := pool.Exec(ctx, `UPDATE engine_lease SET expires_at = clock_timestamp() - interval '1 second'`); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	fence := mustAcquire(t, ctx, after, "engine-b")

	if got, err := after.AdvanceRollout(ctx, r.ID, fence); err != nil || got.Changed {
		t.Fatalf("after restart: result = %+v, err = %v, want the soak still running", got, err)
	}
	clock.Advance(6 * time.Minute)
	got, err := after.AdvanceRollout(ctx, r.ID, fence)
	if err != nil || got.Event != rollout.EventSoakPassed {
		t.Fatalf("result = %+v, err = %v, want time before the restart counted", got, err)
	}
	// Assignments aren't duplicated either: the first devices have nothing further.
	if a, err := after.PollAssignment(ctx, mustAuthenticate(t, ctx, after, memberAt(t, ctx, after, r.ID, 1)), testLease); err != nil || a != nil {
		t.Fatalf("poll after restart = %+v, err = %v", a, err)
	}
}

func TestListActiveRollouts(t *testing.T) {
	e := newEngineWorld(t, 20)
	draft := e.world.newRollout(t, "draft-only")
	if _, _, err := e.store.CreateRollout(e.ctx, "lab-a", draft); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	ids, err := e.store.ListActiveRollouts(e.ctx)
	if err != nil {
		t.Fatalf("ListActiveRollouts: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("listed a draft: %v", ids)
	}

	r := e.startRollout(nil)
	ids, err = e.store.ListActiveRollouts(e.ctx)
	if err != nil {
		t.Fatalf("ListActiveRollouts: %v", err)
	}
	if len(ids) != 1 || ids[0] != r.ID {
		t.Fatalf("ids = %v, want %s", ids, r.ID)
	}

	if _, err := e.store.PauseRollout(e.ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("PauseRollout: %v", err)
	}
	if ids, _ := e.store.ListActiveRollouts(e.ctx); len(ids) != 1 {
		t.Fatalf("a paused rollout must stay listed, so safety checks keep running: %v", ids)
	}

	if _, err := e.store.AbortRollout(e.ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("AbortRollout: %v", err)
	}
	if ids, _ := e.store.ListActiveRollouts(e.ctx); len(ids) != 0 {
		t.Fatalf("listed an aborted rollout: %v", ids)
	}
}

// waitForBlockedSessions waits until this many sessions are stuck waiting on a lock.
func waitForBlockedSessions(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("check blocked sessions: %v", err)
		}
		if waiting >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions blocked, want %d", waiting, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The engine's own transaction must hold the lease row against a takeover. Otherwise a
// process could pass the check, stall, and commit after someone else took over.
func TestTakeoverWaitsForARealAdvanceInProgress(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clock := newFakeClock()
	pool := testdb.New(t)
	s := store.New(pool, store.WithClock(clock.Now))
	w := newWorld(t, ctx, s, 20)
	r := mustStart(t, s, w.mustCreate(t, ctx, "demo"))
	e := &engineWorld{t: t, ctx: ctx, store: s, clock: clock, world: w}
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)

	const shortTTL = 200 * time.Millisecond
	fence, ok, err := s.AcquireLease(ctx, "engine-a", shortTTL)
	if err != nil || !ok {
		t.Fatalf("AcquireLease: ok = %v, err = %v", ok, err)
	}

	// Stall the old leader partway through, after its check, by holding the rollout row.
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM rollouts WHERE id = $1 FOR UPDATE`, r.ID); err != nil {
		t.Fatalf("hold rollout row: %v", err)
	}
	type advanceResult struct {
		result store.AdvanceResult
		err    error
	}
	advanced := make(chan advanceResult, 1)
	go func() {
		result, err := s.AdvanceRollout(ctx, r.ID, fence)
		advanced <- advanceResult{result, err}
	}()
	waitForBlockedSessions(t, ctx, pool, 1)

	// Its lease runs out while it's stalled, and another instance tries to take over.
	time.Sleep(2 * shortTTL)
	type acquireResult struct {
		fence store.Fence
		ok    bool
		err   error
	}
	takeover := make(chan acquireResult, 1)
	go func() {
		next, ok, err := s.AcquireLease(ctx, "engine-b", testLeaseTTL)
		takeover <- acquireResult{next, ok, err}
	}()
	waitForBlockedSessions(t, ctx, pool, 2)

	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release rollout row: %v", err)
	}

	// The decision that began under the old lease lands first, then the takeover.
	got := <-advanced
	if got.err != nil || !got.result.Changed {
		t.Fatalf("advance = %+v", got)
	}
	next := <-takeover
	if next.err != nil || !next.ok || next.fence.Generation != fence.Generation+1 {
		t.Fatalf("takeover = %+v", next)
	}
	if _, err := s.AdvanceRollout(ctx, r.ID, fence); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("old leader afterward: error = %v, want ErrLeaseLost", err)
	}
}

// A poll racing a pause must wait to see how it ends. Reading the old state and issuing
// an assignment would expose a device after the operator stopped the rollout.
func TestPollWaitsForAPauseThatIsCommitting(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testdb.New(t)
	s := store.New(pool)
	w := newWorld(t, ctx, s, 20)
	r := mustStart(t, s, w.mustCreate(t, ctx, "demo"))
	device := mustAuthenticate(t, ctx, s, memberAt(t, ctx, s, r.ID, 1))

	pausing, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = pausing.Rollback(ctx) }()
	if _, err := pausing.Exec(ctx, `
		UPDATE rollouts SET state = 'paused', paused_from = 'running', paused_at = now(), revision = revision + 1
		WHERE id = $1`, r.ID); err != nil {
		t.Fatalf("pause: %v", err)
	}

	type pollResult struct {
		assignment *store.Assignment
		err        error
	}
	polled := make(chan pollResult, 1)
	go func() {
		assignment, err := s.PollAssignment(ctx, device, testLease)
		polled <- pollResult{assignment, err}
	}()
	waitForBlockedSessions(t, ctx, pool, 1)

	if err := pausing.Commit(ctx); err != nil {
		t.Fatalf("commit pause: %v", err)
	}
	got := <-polled
	if got.err != nil {
		t.Fatalf("poll: %v", got.err)
	}
	if got.assignment != nil {
		t.Fatalf("issued an assignment after the pause committed: %+v", got.assignment)
	}
}
