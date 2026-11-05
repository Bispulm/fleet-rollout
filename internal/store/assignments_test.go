package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

const testLease = 5 * time.Minute

// mustAuthenticate returns the device the way the API would after checking its token.
func mustAuthenticate(t *testing.T, ctx context.Context, s *store.Store, id string) store.Device {
	t.Helper()
	device, err := s.AuthenticateDevice(ctx, sha256.Sum256([]byte(id)))
	if err != nil {
		t.Fatalf("AuthenticateDevice(%s): %v", id, err)
	}
	return device
}

// startedWorld returns a running rollout over n devices, with stages 10, 50, 100.
func startedWorld(t *testing.T, ctx context.Context, s *store.Store, n int) (world, store.Rollout) {
	t.Helper()
	w := newWorld(t, ctx, s, n)
	return w, mustStart(t, s, w.mustCreate(t, ctx, "demo"))
}

// memberAt returns the device at the given position in the rollout's order.
func memberAt(t *testing.T, ctx context.Context, s *store.Store, rolloutID string, position int) string {
	t.Helper()
	entries, err := s.ListCohort(ctx, rolloutID, store.CohortQuery{AfterPosition: position - 1, Limit: 1})
	if err != nil {
		t.Fatalf("ListCohort: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("no member at position %d", position)
	}
	return entries[0].DeviceID
}

func poll(t *testing.T, ctx context.Context, s *store.Store, deviceID string) *store.Assignment {
	t.Helper()
	assignment, err := s.PollAssignment(ctx, mustAuthenticate(t, ctx, s, deviceID), testLease)
	if err != nil {
		t.Fatalf("PollAssignment(%s): %v", deviceID, err)
	}
	return assignment
}

func TestAuthenticateDevice(t *testing.T) {
	s, ctx := newStore(t)
	newWorld(t, ctx, s, 2)

	got := mustAuthenticate(t, ctx, s, "sim-00001")
	if got.ID != "sim-00001" {
		t.Fatalf("authenticated as %s", got.ID)
	}

	if _, err := s.AuthenticateDevice(ctx, sha256.Sum256([]byte("unknown"))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown credential: error = %v, want ErrNotFound", err)
	}

	if _, err := s.SetDeviceStatus(ctx, "sim-00001", rollout.DeviceDecommissioned); err != nil {
		t.Fatalf("SetDeviceStatus: %v", err)
	}
	if _, err := s.AuthenticateDevice(ctx, sha256.Sum256([]byte("sim-00001"))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("decommissioned device: error = %v, want ErrNotFound", err)
	}

	if _, err := s.SetDeviceStatus(ctx, "sim-00000", rollout.DeviceExcluded); err != nil {
		t.Fatalf("SetDeviceStatus: %v", err)
	}
	if _, err := s.AuthenticateDevice(ctx, sha256.Sum256([]byte("sim-00000"))); err != nil {
		t.Fatalf("excluded device should still authenticate, to finish its work: %v", err)
	}
}

func TestPollIssuesAssignmentOnlyToExposedStages(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	w, r := startedWorld(t, ctx, s, 20)

	// With 20 devices, stage 1 is positions 1 and 2.
	first := memberAt(t, ctx, s, r.ID, 1)
	third := memberAt(t, ctx, s, r.ID, 3)

	assignment := poll(t, ctx, s, first)
	if assignment == nil {
		t.Fatal("a device in the first stage got no assignment")
	}
	if assignment.RolloutID != r.ID || assignment.DeviceID != first || assignment.Generation != 1 ||
		assignment.Stage != 1 || assignment.State != rollout.AssignmentAssigned ||
		assignment.Directive != rollout.DirectiveProceed {
		t.Fatalf("assignment = %+v", assignment)
	}
	if assignment.Artifact.ID != w.artifact.ID || assignment.Artifact.SHA256 != w.artifact.SHA256 {
		t.Fatalf("artifact = %+v", assignment.Artifact)
	}
	if want := clock.Now().Add(testLease); !assignment.ExpiresAt.Equal(want) {
		t.Fatalf("expires at %v, want %v", assignment.ExpiresAt, want)
	}

	if got := poll(t, ctx, s, third); got != nil {
		t.Fatalf("a device in a later stage got an assignment: %+v", got)
	}
}

func TestPollRecordsThatTheDeviceWasSeen(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	newWorld(t, ctx, s, 1)
	poll(t, ctx, s, "sim-00000")
	device := mustAuthenticate(t, ctx, s, "sim-00000")
	if device.LastSeenAt == nil || !device.LastSeenAt.Equal(clock.Now()) {
		t.Fatalf("last seen = %v, want %v", device.LastSeenAt, clock.Now())
	}
}

func TestRepeatedPollsReturnTheSameAssignmentAndRenewTheLease(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	_, r := startedWorld(t, ctx, s, 20)
	device := memberAt(t, ctx, s, r.ID, 1)

	first := poll(t, ctx, s, device)
	clock.Advance(time.Minute)
	second := poll(t, ctx, s, device)
	if second.ID != first.ID {
		t.Fatalf("second poll returned %s, want %s", second.ID, first.ID)
	}
	if want := clock.Now().Add(testLease); !second.ExpiresAt.Equal(want) {
		t.Fatalf("lease wasn't renewed: expires at %v, want %v", second.ExpiresAt, want)
	}
}

func TestConcurrentPollsIssueOneAssignment(t *testing.T) {
	s, ctx := newStore(t)
	_, r := startedWorld(t, ctx, s, 20)
	device := mustAuthenticate(t, ctx, s, memberAt(t, ctx, s, r.ID, 1))

	const attempts = 8
	ids := make([]string, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			assignment, err := s.PollAssignment(ctx, device, testLease)
			errs[i] = err
			if assignment != nil {
				ids[i] = assignment.ID
			}
		})
	}
	wg.Wait()
	for i := range attempts {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("attempts returned different assignments: %v", ids)
		}
	}
}

func TestPollGivesNothingToANonMemberOrWithoutARollout(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 5)

	if got := poll(t, ctx, s, "sim-00000"); got != nil {
		t.Fatalf("assignment without an active rollout: %+v", got)
	}
	draft := w.mustCreate(t, ctx, "demo")
	if got := poll(t, ctx, s, "sim-00000"); got != nil {
		t.Fatalf("assignment from a draft: %+v", got)
	}
	mustStart(t, s, draft)

	late := []store.NewDevice{{ID: "late", Platform: "linux/arm64", CredentialHash: sha256.Sum256([]byte("late"))}}
	if _, err := s.RegisterDevices(ctx, "lab-a", late); err != nil {
		t.Fatalf("RegisterDevices: %v", err)
	}
	if got := poll(t, ctx, s, "late"); got != nil {
		t.Fatalf("a device enrolled after start got an assignment: %+v", got)
	}
}

func TestDevicesExcludedAfterStartGetNoNewWork(t *testing.T) {
	s, ctx := newStore(t)
	_, r := startedWorld(t, ctx, s, 20)
	device := memberAt(t, ctx, s, r.ID, 1)
	if _, err := s.SetDeviceStatus(ctx, device, rollout.DeviceExcluded); err != nil {
		t.Fatalf("SetDeviceStatus: %v", err)
	}
	if got := poll(t, ctx, s, device); got != nil {
		t.Fatalf("assignment = %+v, want none for an excluded device", got)
	}
}

func TestPauseStopsNewAssignmentsAndHoldsUnstartedOnes(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	_, r := startedWorld(t, ctx, s, 20)
	holding := memberAt(t, ctx, s, r.ID, 1)
	unassigned := memberAt(t, ctx, s, r.ID, 2)
	before := poll(t, ctx, s, holding)

	if _, err := s.PauseRollout(ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("PauseRollout: %v", err)
	}
	if got := poll(t, ctx, s, unassigned); got != nil {
		t.Fatalf("a new assignment during a pause: %+v", got)
	}

	clock.Advance(time.Minute)
	held := poll(t, ctx, s, holding)
	if held == nil || held.Directive != rollout.DirectiveHold {
		t.Fatalf("assignment during a pause = %+v, want hold", held)
	}
	if !held.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("lease was renewed during a hold: %v, was %v", held.ExpiresAt, before.ExpiresAt)
	}

	if _, err := s.ResumeRollout(ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("ResumeRollout: %v", err)
	}
	if got := poll(t, ctx, s, unassigned); got == nil {
		t.Fatal("no assignment after resuming")
	}
	if got := poll(t, ctx, s, holding); got.Directive != rollout.DirectiveProceed {
		t.Fatalf("directive after resuming = %s", got.Directive)
	}
}

func TestPauseLetsInFlightDevicesFinish(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	_, r := startedWorld(t, ctx, s, 20)
	device := memberAt(t, ctx, s, r.ID, 1)
	assignment := poll(t, ctx, s, device)
	mustReport(t, ctx, s, device, assignment, 1, rollout.AssignmentInstalling)

	if _, err := s.PauseRollout(ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("PauseRollout: %v", err)
	}
	clock.Advance(time.Minute)
	got := poll(t, ctx, s, device)
	if got == nil || got.Directive != rollout.DirectiveProceed {
		t.Fatalf("in-flight assignment during a pause = %+v, want proceed", got)
	}
	if want := clock.Now().Add(testLease); !got.ExpiresAt.Equal(want) {
		t.Fatalf("lease = %v, want it renewed to %v", got.ExpiresAt, want)
	}
}

func TestAbortSupersedesUnstartedWorkAndLeavesInFlightWork(t *testing.T) {
	s, ctx := newStore(t)
	_, r := startedWorld(t, ctx, s, 20)
	waiting := memberAt(t, ctx, s, r.ID, 1)
	installing := memberAt(t, ctx, s, r.ID, 2)
	abandoned := poll(t, ctx, s, waiting)
	inFlight := poll(t, ctx, s, installing)
	mustReport(t, ctx, s, installing, inFlight, 1, rollout.AssignmentInstalling)

	if _, err := s.AbortRollout(ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("AbortRollout: %v", err)
	}

	// The device that hadn't started is told nothing further, and can't report progress.
	if got := poll(t, ctx, s, waiting); got != nil {
		t.Fatalf("superseded device got %+v", got)
	}
	result := mustReport(t, ctx, s, waiting, abandoned, 1, rollout.AssignmentInstalling)
	if result.Applied || result.IgnoredReason != rollout.IgnoredAssignmentFinished {
		t.Fatalf("report on a superseded assignment = %+v", result)
	}

	// The device already installing keeps going, and its result still counts.
	got := poll(t, ctx, s, installing)
	if got == nil || got.Directive != rollout.DirectiveProceed {
		t.Fatalf("in-flight device got %+v", got)
	}
	mustReport(t, ctx, s, installing, inFlight, 2, rollout.AssignmentHealthCheck)
	if result := mustReport(t, ctx, s, installing, inFlight, 3, rollout.AssignmentSucceeded); !result.Applied {
		t.Fatalf("final report = %+v", result)
	}
}

func TestAPauseCommittedBeforeAPollAlwaysWins(t *testing.T) {
	s, ctx := newStore(t)
	const devices = 40
	_, r := startedWorld(t, ctx, s, devices)
	// With stages 10, 50, 100, four devices are exposed in stage 1.
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = memberAt(t, ctx, s, r.ID, i+1)
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			device, err := s.AuthenticateDevice(ctx, sha256.Sum256([]byte(id)))
			if err != nil {
				t.Errorf("AuthenticateDevice: %v", err)
				return
			}
			if _, err := s.PollAssignment(ctx, device, testLease); err != nil {
				t.Errorf("PollAssignment: %v", err)
			}
		})
	}
	wg.Go(func() {
		if _, err := s.PauseRollout(ctx, r.ID, byAdmin); err != nil {
			t.Errorf("PauseRollout: %v", err)
		}
	})
	wg.Wait()

	// Whatever raced the pause is settled now. Nothing new can appear while paused.
	issued := func() int {
		count := 0
		for _, id := range ids {
			got := poll(t, ctx, s, id)
			if got != nil {
				count++
				if got.Directive != rollout.DirectiveHold {
					t.Fatalf("directive during a pause = %s", got.Directive)
				}
			}
		}
		return count
	}
	first := issued()
	if second := issued(); second != first {
		t.Fatalf("new assignments appeared during the pause: %d, then %d", first, second)
	}
}
