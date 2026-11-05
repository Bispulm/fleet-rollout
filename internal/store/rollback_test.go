package store_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

// priorRelease is what the rollback tests' devices run before the rollout's release 14.
const priorRelease = 9

// newRollbackWorld is an engine world whose 20 devices run release 9, so that restoring them
// is distinguishable from never having had anything.
func newRollbackWorld(t *testing.T) *engineWorld {
	t.Helper()
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	if _, err := s.CreateFleet(ctx, "lab-a", nil); err != nil {
		t.Fatal(err)
	}
	devices := make([]store.NewDevice, 20)
	for i := range devices {
		id := fmt.Sprintf("sim-%05d", i)
		devices[i] = store.NewDevice{ID: id, Platform: "linux/arm64", InstalledRelease: priorRelease, CredentialHash: sha256.Sum256([]byte(id))}
	}
	if _, err := s.RegisterDevices(ctx, "lab-a", devices); err != nil {
		t.Fatal(err)
	}
	return &engineWorld{
		t: t, ctx: ctx, store: s, clock: clock, fence: mustAcquire(t, ctx, s, "engine-a"),
		world: world{store: s, artifact: mustRegisterArtifact(t, ctx, s, newArtifact("1.4.0", 14))},
	}
}

// restore takes a device through its rollback assignment, using report sequences from first.
func (e *engineWorld) restore(id string, position int, first int64) {
	e.t.Helper()
	device := memberAt(e.t, e.ctx, e.store, id, position)
	a := poll(e.t, e.ctx, e.store, device)
	if a == nil || a.Kind != "rollback" {
		e.t.Fatalf("position %d: assignment = %+v, want a rollback", position, a)
	}
	for i, state := range []rollout.AssignmentState{rollout.AssignmentInstalling, rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded} {
		if got := mustReport(e.t, e.ctx, e.store, device, a, first+int64(i), state); !got.Applied {
			e.t.Fatalf("position %d: %s = %+v", position, state, got)
		}
	}
}

func (e *engineWorld) rollback(id string) store.Rollout {
	e.t.Helper()
	r, err := e.store.RollbackRollout(e.ctx, id, byAdmin)
	if err != nil {
		e.t.Fatalf("RollbackRollout: %v", err)
	}
	return r
}

func TestARollbackIsOfferedOnlyToDevicesThatUpdated(t *testing.T) {
	e := newRollbackWorld(t)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1)
	e.finish(r.ID, rollout.AssignmentFailed, 2)

	rb := e.rollback(r.ID)
	if rb.State != rollout.StateRollingBack || rb.Rollback == nil || rb.Rollback.Attempt != 1 ||
		!rb.Rollback.DeadlineAt.Equal(e.clock.Now().Add(time.Hour)) {
		t.Fatalf("rollout = %s, rollback = %+v", rb.State, rb.Rollback)
	}
	if rb.Rollback.Needed != 1 || rb.Rollback.Restored != 0 {
		t.Fatalf("rollback = %+v, want one device needing it", rb.Rollback)
	}

	updated := poll(t, e.ctx, e.store, memberAt(t, e.ctx, e.store, r.ID, 1))
	if updated == nil || updated.Kind != "rollback" || updated.Generation != 2 || updated.RestoreRelease == nil ||
		*updated.RestoreRelease != priorRelease || updated.Directive != rollout.DirectiveProceed {
		t.Fatalf("assignment = %+v, want a rollback to release %d", updated, priorRelease)
	}
	for _, position := range []int{2, 3} { // One reverted on its own, and one was never reached.
		if got := poll(t, e.ctx, e.store, memberAt(t, e.ctx, e.store, r.ID, position)); got != nil {
			t.Fatalf("position %d was offered %+v", position, got)
		}
	}
}

func TestARollbackFinishesWhenEveryUpdatedDeviceIsRestored(t *testing.T) {
	e := newRollbackWorld(t)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)
	e.rollback(r.ID)

	e.restore(r.ID, 1, 3)
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("advanced to %s with a device still to restore", got.To)
	}
	e.restore(r.ID, 2, 3)
	got := e.advance(r.ID)
	if got.To != rollout.StateRolledBack || got.Event != rollout.EventRollbackDone {
		t.Fatalf("advance = %+v, want rolled back", got)
	}
	final := e.get(r.ID)
	if final.FinishedAt == nil || final.Rollback.Restored != 2 || final.Rollback.Needed != 2 {
		t.Fatalf("rollout = %+v, rollback = %+v", final, final.Rollback)
	}
	for _, position := range []int{1, 2} {
		device := mustAuthenticate(t, e.ctx, e.store, memberAt(t, e.ctx, e.store, r.ID, position))
		if device.InstalledRelease != priorRelease {
			t.Fatalf("position %d: installed release = %d, want it back to %d", position, device.InstalledRelease, priorRelease)
		}
	}
	if ids, _ := e.store.ListActiveRollouts(e.ctx); len(ids) != 0 {
		t.Fatalf("a rolled back rollout is still active: %v", ids)
	}
}

func TestARollbackWaitsForAnInstallStillInFlight(t *testing.T) {
	e := newRollbackWorld(t)
	r := e.startRollout(nil)
	installing := memberAt(t, e.ctx, e.store, r.ID, 1)
	forward := poll(t, e.ctx, e.store, installing)
	mustReport(t, e.ctx, e.store, installing, forward, 1, rollout.AssignmentInstalling)
	e.finish(r.ID, rollout.AssignmentSucceeded, 2)

	e.rollback(r.ID)
	e.restore(r.ID, 2, 3)
	if got := e.advance(r.ID); got.Changed {
		t.Fatalf("advanced to %s while an install was in flight", got.To)
	}
	// The install can't be recalled, so it finishes, and then that device rolls back too.
	mustReport(t, e.ctx, e.store, installing, forward, 2, rollout.AssignmentHealthCheck)
	mustReport(t, e.ctx, e.store, installing, forward, 3, rollout.AssignmentSucceeded)
	e.restore(r.ID, 1, 4)
	if got := e.advance(r.ID); got.To != rollout.StateRolledBack {
		t.Fatalf("advance = %+v, want rolled back", got)
	}
}

func TestARollbackPastItsDeadlineIsIncompleteAndCanBeTriedAgain(t *testing.T) {
	e := newRollbackWorld(t)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)
	e.rollback(r.ID)
	e.restore(r.ID, 1, 3)
	slow := memberAt(t, e.ctx, e.store, r.ID, 2)
	if a := poll(t, e.ctx, e.store, slow); a == nil || a.Kind != "rollback" {
		t.Fatalf("assignment = %+v", a)
	}

	e.clock.Advance(time.Hour + time.Second)
	got := e.advance(r.ID)
	if got.To != rollout.StateRollbackIncomplete || got.Event != rollout.EventRollbackExpired {
		t.Fatalf("advance = %+v, want rollback_incomplete", got)
	}
	incomplete := e.get(r.ID)
	if incomplete.Rollback.Needed != 2 || incomplete.Rollback.Restored != 1 || incomplete.Rollback.Unresolved != 1 {
		t.Fatalf("rollback = %+v, want one device unresolved", incomplete.Rollback)
	}
	if want := "rollback incomplete: 1 of 2 devices restored, 1 unresolved at the deadline; roll back again, or abort"; incomplete.Waiting != want {
		t.Fatalf("waiting = %q, want %q", incomplete.Waiting, want)
	}
	if a := poll(t, e.ctx, e.store, slow); a != nil {
		t.Fatalf("an expired rollback is still offered: %+v", a)
	}

	retry := e.rollback(r.ID)
	if retry.State != rollout.StateRollingBack || retry.Rollback.Attempt != 2 {
		t.Fatalf("retry = %s, %+v", retry.State, retry.Rollback)
	}
	again := poll(t, e.ctx, e.store, slow)
	if again == nil || again.Generation != 3 {
		t.Fatalf("assignment = %+v, want the second attempt's generation 3", again)
	}
	if a := poll(t, e.ctx, e.store, memberAt(t, e.ctx, e.store, r.ID, 1)); a != nil {
		t.Fatalf("a restored device was offered %+v", a)
	}
	for i, state := range []rollout.AssignmentState{rollout.AssignmentInstalling, rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded} {
		mustReport(t, e.ctx, e.store, slow, again, int64(10+i), state)
	}
	if got := e.advance(r.ID); got.To != rollout.StateRolledBack {
		t.Fatalf("advance = %+v, want rolled back", got)
	}
}

func TestABreachRollsBackWhenThePolicySaysTo(t *testing.T) {
	e := newRollbackWorld(t)
	r := e.startRollout(func(p *rollout.PolicySpec) { p.Safety.OnBreach = rollout.OnBreachRollback })
	e.finish(r.ID, rollout.AssignmentSucceeded, 1)
	e.finish(r.ID, rollout.AssignmentFailed, 2)

	got := e.advance(r.ID)
	if got.To != rollout.StateRollingBack {
		t.Fatalf("advance = %+v, want rolling back", got)
	}
	rolling := e.get(r.ID)
	if rolling.HaltReason == nil || *rolling.HaltReason != string(rollout.HaltStageBreach) {
		t.Fatalf("halt reason = %v, want the breach recorded", rolling.HaltReason)
	}
	log, err := e.store.ListTransitions(e.ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := log[len(log)-2:]
	if last[0].Event != rollout.EventBreach || last[0].To != rollout.StateHalted ||
		last[1].Event != rollout.EventRollback || last[1].To != rollout.StateRollingBack ||
		last[0].Actor != "engine" || last[1].Actor != "engine" {
		t.Fatalf("last transitions = %+v, want a halt and then a rollback, both by the engine", last)
	}
	e.restore(r.ID, 1, 3)
}

func TestRollbackIsRefusedWhereTheStateMachineDoesntAllowIt(t *testing.T) {
	e := newRollbackWorld(t)
	draft := e.world.mustCreate(t, e.ctx, "draft")
	_, err := e.store.RollbackRollout(e.ctx, draft.ID, byAdmin)
	var invalid *rollout.InvalidTransitionError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want an invalid transition", err)
	}
}

func TestARollbackStopsDevicesThatHaventStartedInstalling(t *testing.T) {
	e := newRollbackWorld(t)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1)
	waiting := memberAt(t, e.ctx, e.store, r.ID, 2)
	forward := poll(t, e.ctx, e.store, waiting)

	e.rollback(r.ID)
	if got := poll(t, e.ctx, e.store, waiting); got != nil {
		t.Fatalf("a device that never started was given %+v", got)
	}
	if got := mustReport(t, e.ctx, e.store, waiting, forward, 1, rollout.AssignmentInstalling); got.Applied {
		t.Fatalf("a request to install after the rollback started = %+v, want it refused", got)
	}
}

func TestADecommissionedDeviceDoesntHoldUpARollback(t *testing.T) {
	e := newRollbackWorld(t)
	r := e.startRollout(nil)
	e.finish(r.ID, rollout.AssignmentSucceeded, 1, 2)
	// It can never poll again, so it can never confirm a rollback.
	if _, err := e.store.SetDeviceStatus(e.ctx, memberAt(t, e.ctx, e.store, r.ID, 2), rollout.DeviceDecommissioned); err != nil {
		t.Fatal(err)
	}
	e.rollback(r.ID)
	e.restore(r.ID, 1, 3)
	got := e.advance(r.ID)
	if got.To != rollout.StateRolledBack {
		t.Fatalf("advance = %+v, rollback = %+v; want rolled back without the decommissioned device", got, e.get(r.ID).Rollback)
	}
}
