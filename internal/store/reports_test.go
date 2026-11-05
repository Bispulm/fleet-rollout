package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

func reportFor(device string, a *store.Assignment, sequence int64, state rollout.AssignmentState) store.Report {
	return store.Report{
		DeviceID:     device,
		Sequence:     sequence,
		RolloutID:    a.RolloutID,
		AssignmentID: a.ID,
		Generation:   a.Generation,
		State:        state,
	}
}

func mustReport(t *testing.T, ctx context.Context, s *store.Store, device string, a *store.Assignment, sequence int64, state rollout.AssignmentState) store.ReportResult {
	t.Helper()
	result, err := s.RecordReport(ctx, reportFor(device, a, sequence, state))
	if err != nil {
		t.Fatalf("RecordReport(seq %d, %s): %v", sequence, state, err)
	}
	return result
}

// assignedDevice returns a stage 1 device that has polled for its assignment.
func assignedDevice(t *testing.T, ctx context.Context, s *store.Store) (string, *store.Assignment, store.Rollout) {
	t.Helper()
	_, r := startedWorld(t, ctx, s, 20)
	device := memberAt(t, ctx, s, r.ID, 1)
	return device, poll(t, ctx, s, device), r
}

func TestReportsAdvanceAnAssignmentToSuccess(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	device, a, _ := assignedDevice(t, ctx, s)

	steps := []rollout.AssignmentState{
		rollout.AssignmentDownloading, rollout.AssignmentVerifying, rollout.AssignmentInstalling,
		rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded,
	}
	for i, state := range steps {
		result := mustReport(t, ctx, s, device, a, int64(i+1), state)
		if !result.Applied || result.Duplicate {
			t.Fatalf("report %d = %+v", i+1, result)
		}
	}

	// The assignment is finished, so the device has nothing further to do.
	if got := poll(t, ctx, s, device); got != nil {
		t.Fatalf("poll after success = %+v", got)
	}
	authed := mustAuthenticate(t, ctx, s, device)
	if authed.InstalledRelease != 14 {
		t.Fatalf("installed release = %d, want the artifact's 14", authed.InstalledRelease)
	}
}

func TestFailureDoesNotChangeTheInstalledRelease(t *testing.T) {
	s, ctx := newStore(t)
	device, a, _ := assignedDevice(t, ctx, s)
	mustReport(t, ctx, s, device, a, 1, rollout.AssignmentFailed)
	if got := mustAuthenticate(t, ctx, s, device).InstalledRelease; got != 0 {
		t.Fatalf("installed release = %d after a failure", got)
	}
	if got := poll(t, ctx, s, device); got != nil {
		t.Fatalf("a failed device was reassigned: %+v", got)
	}
}

func TestIdenticalRepeatIsAcknowledgedAndChangesNothing(t *testing.T) {
	s, ctx := newStore(t)
	device, a, _ := assignedDevice(t, ctx, s)
	first := mustReport(t, ctx, s, device, a, 1, rollout.AssignmentDownloading)
	if !first.Applied {
		t.Fatalf("first = %+v", first)
	}
	again := mustReport(t, ctx, s, device, a, 1, rollout.AssignmentDownloading)
	if !again.Duplicate || again.Applied {
		t.Fatalf("repeat = %+v, want a duplicate that wasn't applied", again)
	}
}

func TestRepeatedFailureCountsOnce(t *testing.T) {
	s, ctx := newStore(t)
	device, a, r := assignedDevice(t, ctx, s)
	for range 3 {
		mustReport(t, ctx, s, device, a, 1, rollout.AssignmentFailed)
	}
	got, err := s.GetRollout(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRollout: %v", err)
	}
	if failed := got.Stages[0].Failed; failed != 1 {
		t.Fatalf("failed = %d, want 1 after three identical reports", failed)
	}
}

func TestSameSequenceWithADifferentPayloadIsRejected(t *testing.T) {
	s, ctx := newStore(t)
	device, a, _ := assignedDevice(t, ctx, s)
	mustReport(t, ctx, s, device, a, 1, rollout.AssignmentDownloading)

	_, err := s.RecordReport(ctx, reportFor(device, a, 1, rollout.AssignmentFailed))
	if !errors.Is(err, store.ErrReportConflict) {
		t.Fatalf("error = %v, want ErrReportConflict", err)
	}
	withReason := reportFor(device, a, 1, rollout.AssignmentDownloading)
	withReason.Reason = "different"
	if _, err := s.RecordReport(ctx, withReason); !errors.Is(err, store.ErrReportConflict) {
		t.Fatalf("different reason: error = %v, want ErrReportConflict", err)
	}
}

func TestDelayedOlderReportDoesNotUndoANewerOne(t *testing.T) {
	s, ctx := newStore(t)
	device, a, _ := assignedDevice(t, ctx, s)
	mustReport(t, ctx, s, device, a, 5, rollout.AssignmentInstalling)

	result := mustReport(t, ctx, s, device, a, 2, rollout.AssignmentDownloading)
	if result.Applied || result.IgnoredReason != store.IgnoredOutOfOrder {
		t.Fatalf("delayed report = %+v", result)
	}
	// The newer state still stands: the device can move on from installing.
	if result := mustReport(t, ctx, s, device, a, 6, rollout.AssignmentHealthCheck); !result.Applied {
		t.Fatalf("next report = %+v", result)
	}
}

func TestFinishedAssignmentsAcceptNoFurtherChange(t *testing.T) {
	s, ctx := newStore(t)
	device, a, r := assignedDevice(t, ctx, s)
	mustReport(t, ctx, s, device, a, 1, rollout.AssignmentHealthCheck)
	mustReport(t, ctx, s, device, a, 2, rollout.AssignmentSucceeded)

	result := mustReport(t, ctx, s, device, a, 3, rollout.AssignmentFailed)
	if result.Applied || result.IgnoredReason != store.IgnoredAssignmentFinished {
		t.Fatalf("report after finishing = %+v", result)
	}
	got, err := s.GetRollout(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRollout: %v", err)
	}
	if got.Stages[0].Failed != 0 || got.Stages[0].Succeeded != 1 {
		t.Fatalf("stage = %+v, want the success to stand", got.Stages[0])
	}
}

func TestInvalidProgressionIsRecordedButNotApplied(t *testing.T) {
	s, ctx := newStore(t)
	device, a, _ := assignedDevice(t, ctx, s)
	result := mustReport(t, ctx, s, device, a, 1, rollout.AssignmentSucceeded)
	if result.Applied || result.IgnoredReason != store.IgnoredInvalidProgression {
		t.Fatalf("success without a health check = %+v", result)
	}
	// It used its sequence, so repeating it is a duplicate rather than a fresh attempt.
	if again := mustReport(t, ctx, s, device, a, 1, rollout.AssignmentSucceeded); !again.Duplicate {
		t.Fatalf("repeat = %+v", again)
	}
}

func TestReportsAreBoundToTheOwningDevice(t *testing.T) {
	s, ctx := newStore(t)
	device, a, r := assignedDevice(t, ctx, s)
	other := memberAt(t, ctx, s, r.ID, 2)

	// Another device can't report on this assignment, or learn that it exists.
	if _, err := s.RecordReport(ctx, reportFor(other, a, 1, rollout.AssignmentFailed)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other device: error = %v, want ErrNotFound", err)
	}
	wrongRollout := reportFor(device, a, 1, rollout.AssignmentDownloading)
	wrongRollout.RolloutID = "00000000-0000-4000-8000-000000000000"
	if _, err := s.RecordReport(ctx, wrongRollout); !errors.Is(err, store.ErrReportMismatch) {
		t.Fatalf("wrong rollout: error = %v, want ErrReportMismatch", err)
	}
	wrongGeneration := reportFor(device, a, 1, rollout.AssignmentDownloading)
	wrongGeneration.Generation = 2
	if _, err := s.RecordReport(ctx, wrongGeneration); !errors.Is(err, store.ErrReportMismatch) {
		t.Fatalf("wrong generation: error = %v, want ErrReportMismatch", err)
	}
	unknown := reportFor(device, a, 1, rollout.AssignmentDownloading)
	unknown.AssignmentID = "00000000-0000-4000-8000-000000000000"
	if _, err := s.RecordReport(ctx, unknown); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown assignment: error = %v, want ErrNotFound", err)
	}
}

func TestReportUpdatesLastSeen(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	device, a, _ := assignedDevice(t, ctx, s)
	clock.Advance(testLease)
	mustReport(t, ctx, s, device, a, 1, rollout.AssignmentDownloading)
	if got := mustAuthenticate(t, ctx, s, device).LastSeenAt; !got.Equal(clock.Now()) {
		t.Fatalf("last seen = %v, want %v", got, clock.Now())
	}
}

func TestConcurrentIdenticalReportsApplyOnce(t *testing.T) {
	s, ctx := newStore(t)
	device, a, r := assignedDevice(t, ctx, s)

	const attempts = 8
	results := make([]store.ReportResult, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() { results[i], errs[i] = s.RecordReport(ctx, reportFor(device, a, 1, rollout.AssignmentFailed)) })
	}
	wg.Wait()

	applied := 0
	for i := range attempts {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if results[i].Applied {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("%d reports applied, want 1", applied)
	}
	got, err := s.GetRollout(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRollout: %v", err)
	}
	if got.Stages[0].Failed != 1 {
		t.Fatalf("failed = %d", got.Stages[0].Failed)
	}
}

func TestStatusCountsEveryKindOfOutcome(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	w := newWorld(t, ctx, s, 10)
	input := w.newRollout(t, "demo")
	input.Policy = mustPolicy(t, func(p *rollout.PolicySpec) {
		p.Stages = []rollout.StageSpec{{Percent: 100}}
		p.Safety.StaleAfter = ptr(rollout.Duration(testLease))
	})
	created, _, err := s.CreateRollout(ctx, "lab-a", input)
	if err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	r := mustStart(t, s, created)

	at := func(position int) string { return memberAt(t, ctx, s, r.ID, position) }
	report := func(position int, states ...rollout.AssignmentState) {
		device := at(position)
		a := poll(t, ctx, s, device)
		for i, state := range states {
			mustReport(t, ctx, s, device, a, int64(i+1), state)
		}
	}
	report(1, rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded)
	report(2, rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded)
	report(3, rollout.AssignmentFailed)
	report(4, rollout.AssignmentInstalling)
	report(5) // Assigned, and heard from.

	// Everyone goes quiet except positions 5 and 6, which are heard from late on.
	clock.Advance(testLease + time.Second)
	poll(t, ctx, s, at(5))
	poll(t, ctx, s, at(6))

	got, err := s.GetRollout(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRollout: %v", err)
	}
	stage := got.Stages[0]
	if stage.Succeeded != 2 || stage.Failed != 1 || stage.InFlight != 1 {
		t.Fatalf("stage = %+v", stage)
	}
	// Quiet with no outcome: positions 4, 7, 8, 9, and 10.
	if stage.Unknown != 5 {
		t.Fatalf("unknown = %d, want 5", stage.Unknown)
	}
	// Heard from recently with no outcome: positions 5 and 6.
	if stage.Pending != 2 {
		t.Fatalf("pending = %d, want 2", stage.Pending)
	}
}
