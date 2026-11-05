package store_test

import (
	"crypto/sha256"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

// fakeClock is a settable clock for testing timers without sleeping.
type fakeClock struct{ nanos atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *fakeClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

var byAdmin = store.TransitionOptions{Actor: "test"}

func mustStart(t *testing.T, s *store.Store, r store.Rollout) store.Rollout {
	t.Helper()
	started, err := s.StartRollout(t.Context(), r.ID, byAdmin)
	if err != nil {
		t.Fatalf("StartRollout: %v", err)
	}
	return started
}

func TestStartFreezesMembershipAndActivatesFirstStage(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	w := newWorld(t, ctx, s, 20)
	started := mustStart(t, s, w.mustCreate(t, ctx, "demo"))

	if started.State != rollout.StateRunning || started.Revision != 2 {
		t.Fatalf("rollout = %+v", started)
	}
	if started.MemberCount == nil || *started.MemberCount != 20 {
		t.Fatalf("member count = %v", started.MemberCount)
	}
	if started.CurrentStage == nil || *started.CurrentStage != 1 {
		t.Fatalf("current stage = %v", started.CurrentStage)
	}
	if started.StartedAt == nil || !started.StartedAt.Equal(clock.Now()) {
		t.Fatalf("started at = %v, want %v", started.StartedAt, clock.Now())
	}

	wantCohorts := []int{2, 8, 10}
	wantTargets := []int{2, 10, 20}
	for i, stage := range started.Stages {
		if *stage.CohortSize != wantCohorts[i] || *stage.TargetCount != wantTargets[i] {
			t.Errorf("stage %d = cohort %d target %d", i+1, *stage.CohortSize, *stage.TargetCount)
		}
	}
	first := started.Stages[0]
	if first.State != store.StageActive || first.StartedAt == nil {
		t.Fatalf("first stage = %+v", first)
	}
	if want := clock.Now().Add(time.Hour); first.DeadlineAt == nil || !first.DeadlineAt.Equal(want) {
		t.Fatalf("deadline = %v, want %v", first.DeadlineAt, want)
	}
	if started.Stages[1].State != store.StagePending {
		t.Fatalf("second stage state = %q", started.Stages[1].State)
	}
}

func TestCohortMatchesTheDomainRankingExactly(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 25)
	r := mustStart(t, s, w.mustCreate(t, ctx, "demo"))

	entries, err := s.ListCohort(ctx, r.ID, store.CohortQuery{Limit: 100})
	if err != nil {
		t.Fatalf("ListCohort: %v", err)
	}
	ids := make([]string, 25)
	for i, d := range newDevices(25) {
		ids[i] = d.ID
	}
	want := rollout.RankDevices(r.ID, ids)
	if len(entries) != len(want) {
		t.Fatalf("cohort has %d entries, want %d", len(entries), len(want))
	}
	for i, entry := range entries {
		if entry.DeviceID != want[i].DeviceID || entry.Position != want[i].Position || entry.RankKey != want[i].Key {
			t.Fatalf("entry %d = %+v, want %+v", i, entry, want[i])
		}
	}
}

func TestClientChosenIDReproducesCohortAcrossDatabases(t *testing.T) {
	t.Parallel()
	const id = "11111111-1111-4111-8111-111111111111"

	order := func() []string {
		s := store.New(testdb.New(t))
		ctx := t.Context()
		w := newWorld(t, ctx, s, 30)
		input := w.newRollout(t, "demo")
		input.ID = id
		r, _, err := s.CreateRollout(ctx, "lab-a", input)
		if err != nil {
			t.Fatalf("CreateRollout: %v", err)
		}
		mustStart(t, s, r)
		entries, err := s.ListCohort(ctx, id, store.CohortQuery{Limit: 100})
		if err != nil {
			t.Fatalf("ListCohort: %v", err)
		}
		out := make([]string, len(entries))
		for i, e := range entries {
			out[i] = e.DeviceID
		}
		return out
	}
	if first, second := order(), order(); !slices.Equal(first, second) {
		t.Fatalf("cohort order differs across databases:\n%v\n%v", first, second)
	}
}

func TestListCohortFiltersByStageAndPages(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 20)
	r := mustStart(t, s, w.mustCreate(t, ctx, "demo"))

	first, err := s.ListCohort(ctx, r.ID, store.CohortQuery{Stage: 1, Limit: 100})
	if err != nil {
		t.Fatalf("ListCohort: %v", err)
	}
	if len(first) != 2 || first[0].Position != 1 || first[1].Stage != 1 {
		t.Fatalf("stage 1 = %+v", first)
	}

	page, err := s.ListCohort(ctx, r.ID, store.CohortQuery{AfterPosition: 2, Limit: 3})
	if err != nil {
		t.Fatalf("ListCohort: %v", err)
	}
	if len(page) != 3 || page[0].Position != 3 || page[2].Position != 5 || page[0].Stage != 2 {
		t.Fatalf("page = %+v", page)
	}
}

func TestListCohortRequiresFrozenMembership(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	draft := w.mustCreate(t, ctx, "demo")
	if _, err := s.ListCohort(ctx, draft.ID, store.CohortQuery{Limit: 10}); !errors.Is(err, store.ErrMembershipNotFrozen) {
		t.Fatalf("draft error = %v, want ErrMembershipNotFrozen", err)
	}
	if _, err := s.ListCohort(ctx, "00000000-0000-4000-8000-000000000000", store.CohortQuery{Limit: 10}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown rollout error = %v, want ErrNotFound", err)
	}
}

func TestStartRecordsExclusionsAndLeavesThemOutOfCohort(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 0)
	devices := newDevices(5)
	devices[0].Platform = "linux/amd64"
	devices[1].InstalledRelease = 14
	devices[2].Labels = map[string]string{"site": "field"}
	if _, err := s.RegisterDevices(ctx, "lab-a", devices); err != nil {
		t.Fatalf("RegisterDevices: %v", err)
	}
	input := w.newRollout(t, "demo")
	input.Policy = mustPolicy(t, func(p *rollout.PolicySpec) {
		p.Selector = map[string]string{"site": "lab"}
		p.ExcludeDevices = []string{"sim-00003"}
	})
	r, _, err := s.CreateRollout(ctx, "lab-a", input)
	if err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	started := mustStart(t, s, r)

	if *started.MemberCount != 1 {
		t.Fatalf("member count = %d, want 1", *started.MemberCount)
	}
	want := map[rollout.ExclusionReason]int{
		rollout.ExclusionPlatform:       1,
		rollout.ExclusionAlreadyCurrent: 1,
		rollout.ExclusionSelector:       1,
		rollout.ExclusionRollout:        1,
	}
	for reason, count := range want {
		if started.Exclusions[reason] != count {
			t.Errorf("exclusions[%s] = %d, want %d (all: %v)", reason, started.Exclusions[reason], count, started.Exclusions)
		}
	}
	entries, err := s.ListCohort(ctx, r.ID, store.CohortQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListCohort: %v", err)
	}
	if len(entries) != 1 || entries[0].DeviceID != "sim-00004" {
		t.Fatalf("cohort = %+v", entries)
	}
}

func TestStartMarksEmptyStagesSkipped(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 3)
	input := w.newRollout(t, "demo")
	input.Policy = mustPolicy(t, func(p *rollout.PolicySpec) {
		p.Stages = []rollout.StageSpec{{Percent: 1}, {Percent: 10}, {Percent: 50}, {Percent: 100}}
	})
	r, _, err := s.CreateRollout(ctx, "lab-a", input)
	if err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	started := mustStart(t, s, r)
	want := []store.StageState{store.StageActive, store.StageSkipped, store.StagePending, store.StagePending}
	for i, stage := range started.Stages {
		if stage.State != want[i] {
			t.Errorf("stage %d state = %q, want %q", i+1, stage.State, want[i])
		}
	}
}

func TestStartRevalidatesAgainstFleetAtStartTime(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	input := w.newRollout(t, "demo")
	input.Policy = mustPolicy(t, func(p *rollout.PolicySpec) {
		// With 10 devices the stage sizes are 1, 4, and 5, so this just fits.
		p.Stages[1].MinObservations = ptr(4)
	})
	r, _, err := s.CreateRollout(ctx, "lab-a", input)
	if err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}

	// The fleet can shrink between creation and start. With 8 devices, the second
	// stage exposes only 3, so its minimum of 4 can no longer be met.
	for _, id := range []string{"sim-00000", "sim-00001"} {
		if _, err := s.SetDeviceStatus(ctx, id, rollout.DeviceExcluded); err != nil {
			t.Fatalf("SetDeviceStatus: %v", err)
		}
	}
	_, err = s.StartRollout(ctx, r.ID, byAdmin)
	var policyErr *rollout.PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("StartRollout error = %v, want *rollout.PolicyError", err)
	}

	// The failed start leaves no trace, and the rollout stays a draft.
	got, err := s.GetRollout(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRollout: %v", err)
	}
	if got.State != rollout.StateDraft || got.Revision != 1 || len(got.Exclusions) != 0 {
		t.Fatalf("rollout after failed start = %+v", got)
	}
}

func TestEnrollmentAndStatusChangesAfterStartDoNotChangeMembership(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	r := mustStart(t, s, w.mustCreate(t, ctx, "demo"))
	before, err := s.ListCohort(ctx, r.ID, store.CohortQuery{Limit: 100})
	if err != nil {
		t.Fatalf("ListCohort: %v", err)
	}

	late := []store.NewDevice{{ID: "late-enrollee", Platform: "linux/arm64", CredentialHash: sha256.Sum256([]byte("late-enrollee"))}}
	if _, err := s.RegisterDevices(ctx, "lab-a", late); err != nil {
		t.Fatalf("RegisterDevices: %v", err)
	}
	if _, err := s.SetDeviceStatus(ctx, before[0].DeviceID, rollout.DeviceDecommissioned); err != nil {
		t.Fatalf("SetDeviceStatus: %v", err)
	}

	after, err := s.ListCohort(ctx, r.ID, store.CohortQuery{Limit: 100})
	if err != nil {
		t.Fatalf("ListCohort: %v", err)
	}
	if !slices.Equal(after, before) {
		t.Fatalf("membership changed after start:\nbefore %+v\nafter  %+v", before, after)
	}
}

func TestInvalidTransitionsAreRejectedAndChangeNothing(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	draft := w.mustCreate(t, ctx, "demo")

	_, err := s.PauseRollout(ctx, draft.ID, byAdmin)
	var invalid *rollout.InvalidTransitionError
	if !errors.As(err, &invalid) {
		t.Fatalf("pausing a draft: error = %v, want *InvalidTransitionError", err)
	}
	if _, err := s.ResumeRollout(ctx, draft.ID, byAdmin); !errors.As(err, &invalid) {
		t.Fatalf("resuming a draft: error = %v", err)
	}

	mustStart(t, s, draft)
	if _, err := s.StartRollout(ctx, draft.ID, byAdmin); !errors.As(err, &invalid) {
		t.Fatalf("restarting: error = %v", err)
	}

	got, err := s.GetRollout(ctx, draft.ID)
	if err != nil {
		t.Fatalf("GetRollout: %v", err)
	}
	if got.Revision != 2 {
		t.Fatalf("revision = %d, want 2: rejected transitions must not change it", got.Revision)
	}
}

func TestTransitionsReportUnknownRollout(t *testing.T) {
	s, ctx := newStore(t)
	const id = "00000000-0000-4000-8000-000000000000"
	if _, err := s.StartRollout(ctx, id, byAdmin); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("start error = %v", err)
	}
	if _, err := s.PauseRollout(ctx, id, byAdmin); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("pause error = %v", err)
	}
	if _, err := s.PauseRollout(ctx, "not-a-uuid", byAdmin); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("pause with a malformed ID error = %v", err)
	}
}

func TestExpectedRevisionIsEnforced(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	r := w.mustCreate(t, ctx, "demo")

	_, err := s.StartRollout(ctx, r.ID, store.TransitionOptions{Actor: "test", ExpectedRevision: ptr(int64(99))})
	var mismatch *store.RevisionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want *RevisionMismatchError", err)
	}
	if mismatch.Expected != 99 || mismatch.Actual != 1 {
		t.Fatalf("mismatch = %+v", mismatch)
	}

	started, err := s.StartRollout(ctx, r.ID, store.TransitionOptions{Actor: "test", ExpectedRevision: ptr(int64(1))})
	if err != nil {
		t.Fatalf("StartRollout with the right revision: %v", err)
	}
	if started.Revision != 2 {
		t.Fatalf("revision = %d", started.Revision)
	}
}

func TestConcurrentStartsOfOneRolloutHaveOneWinner(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 200)
	r := w.mustCreate(t, ctx, "demo")

	const attempts = 8
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() { _, errs[i] = s.StartRollout(ctx, r.ID, byAdmin) })
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		var invalid *rollout.InvalidTransitionError
		switch {
		case err == nil:
			wins++
		case !errors.As(err, &invalid):
			t.Errorf("loser got %v, want *InvalidTransitionError", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d starts succeeded, want exactly 1", wins)
	}
	entries, err := s.ListCohort(ctx, r.ID, store.CohortQuery{Limit: 1000})
	if err != nil {
		t.Fatalf("ListCohort: %v", err)
	}
	if len(entries) != 200 {
		t.Fatalf("cohort has %d entries, want 200", len(entries))
	}
}

func TestConcurrentTransitionsWithSameRevisionHaveOneWinner(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	r := mustStart(t, s, w.mustCreate(t, ctx, "demo"))

	const attempts = 8
	errs := make([]error, attempts)
	opts := store.TransitionOptions{Actor: "test", ExpectedRevision: ptr(r.Revision)}
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() { _, errs[i] = s.PauseRollout(ctx, r.ID, opts) })
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		var mismatch *store.RevisionMismatchError
		switch {
		case err == nil:
			wins++
		case !errors.As(err, &mismatch):
			t.Errorf("loser got %v, want *RevisionMismatchError", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d pauses succeeded, want exactly 1", wins)
	}
	got, err := s.GetRollout(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRollout: %v", err)
	}
	if got.Revision != r.Revision+1 {
		t.Fatalf("revision = %d, want %d", got.Revision, r.Revision+1)
	}
}

func TestOnlyOneRolloutCanRunPerFleet(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	first := mustStart(t, s, w.mustCreate(t, ctx, "first"))
	second := w.mustCreate(t, ctx, "second")

	_, err := s.StartRollout(ctx, second.ID, byAdmin)
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want *ConflictError", err)
	}

	if _, err := s.AbortRollout(ctx, first.ID, byAdmin); err != nil {
		t.Fatalf("AbortRollout: %v", err)
	}
	mustStart(t, s, second)
}

func TestConcurrentStartsOnOneFleetHaveOneWinner(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 100)
	names := []string{"a", "b", "c", "d"}
	ids := make([]string, len(names))
	for i, name := range names {
		ids[i] = w.mustCreate(t, ctx, name).ID
	}

	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() { _, errs[i] = s.StartRollout(ctx, id, byAdmin) })
	}
	wg.Wait()

	wins := 0
	for _, err := range errs {
		var conflict *store.ConflictError
		switch {
		case err == nil:
			wins++
		case !errors.As(err, &conflict):
			t.Errorf("loser got %v, want *ConflictError", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d rollouts started, want exactly 1", wins)
	}
}

func TestPauseAndResumeMoveTheDeadlineByThePauseLength(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	w := newWorld(t, ctx, s, 10)
	r := mustStart(t, s, w.mustCreate(t, ctx, "demo"))
	originalDeadline := *r.Stages[0].DeadlineAt

	clock.Advance(5 * time.Minute)
	paused, err := s.PauseRollout(ctx, r.ID, byAdmin)
	if err != nil {
		t.Fatalf("PauseRollout: %v", err)
	}
	if paused.State != rollout.StatePaused || *paused.PausedFrom != rollout.StateRunning || !paused.PausedAt.Equal(clock.Now()) {
		t.Fatalf("paused = %+v", paused)
	}

	const pauseLength = 40 * time.Minute
	clock.Advance(pauseLength)
	resumed, err := s.ResumeRollout(ctx, r.ID, byAdmin)
	if err != nil {
		t.Fatalf("ResumeRollout: %v", err)
	}
	if resumed.State != rollout.StateRunning || resumed.PausedFrom != nil || resumed.PausedAt != nil {
		t.Fatalf("resumed = %+v", resumed)
	}
	if want := originalDeadline.Add(pauseLength); !resumed.Stages[0].DeadlineAt.Equal(want) {
		t.Fatalf("deadline = %v, want %v", resumed.Stages[0].DeadlineAt, want)
	}
}

func TestPauseStopsTheSoakClock(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	pool := testdb.New(t)
	s := store.New(pool, store.WithClock(clock.Now))
	ctx := t.Context()
	w := newWorld(t, ctx, s, 10)
	r := mustStart(t, s, w.mustCreate(t, ctx, "demo"))

	// The engine that enters soaking doesn't exist yet, so set the state directly.
	if _, err := pool.Exec(ctx, `UPDATE rollouts SET state = 'soaking', revision = revision + 1 WHERE id = $1`, r.ID); err != nil {
		t.Fatalf("enter soaking: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE rollout_stages SET state = 'soaking', soak_started_at = $2
		WHERE rollout_id = $1 AND ordinal = 1`, r.ID, clock.Now()); err != nil {
		t.Fatalf("start soak: %v", err)
	}

	clock.Advance(3 * time.Minute)
	paused, err := s.PauseRollout(ctx, r.ID, byAdmin)
	if err != nil {
		t.Fatalf("PauseRollout: %v", err)
	}
	stage := paused.Stages[0]
	if stage.SoakStartedAt != nil || stage.SoakAccumulated != 3*time.Minute {
		t.Fatalf("paused stage = %+v, want 3m accumulated and the timer stopped", stage)
	}

	clock.Advance(time.Hour)
	resumed, err := s.ResumeRollout(ctx, r.ID, byAdmin)
	if err != nil {
		t.Fatalf("ResumeRollout: %v", err)
	}
	if resumed.State != rollout.StateSoaking {
		t.Fatalf("resumed state = %q, want soaking", resumed.State)
	}
	stage = resumed.Stages[0]
	if stage.SoakAccumulated != 3*time.Minute {
		t.Fatalf("accumulated soak = %v: pause time must not count", stage.SoakAccumulated)
	}
	if stage.SoakStartedAt == nil || !stage.SoakStartedAt.Equal(clock.Now()) {
		t.Fatalf("soak restarted at = %v, want %v", stage.SoakStartedAt, clock.Now())
	}
}

func TestAbortFinishesTheRollout(t *testing.T) {
	clock := newFakeClock()
	s, ctx := newStore(t, store.WithClock(clock.Now))
	w := newWorld(t, ctx, s, 10)

	t.Run("from draft", func(t *testing.T) {
		r := w.mustCreate(t, ctx, "draft-one")
		aborted, err := s.AbortRollout(ctx, r.ID, byAdmin)
		if err != nil {
			t.Fatalf("AbortRollout: %v", err)
		}
		if aborted.State != rollout.StateAborted || aborted.FinishedAt == nil {
			t.Fatalf("aborted = %+v", aborted)
		}
	})
	t.Run("from paused", func(t *testing.T) {
		r := mustStart(t, s, w.mustCreate(t, ctx, "paused-one"))
		if _, err := s.PauseRollout(ctx, r.ID, byAdmin); err != nil {
			t.Fatalf("PauseRollout: %v", err)
		}
		aborted, err := s.AbortRollout(ctx, r.ID, byAdmin)
		if err != nil {
			t.Fatalf("AbortRollout: %v", err)
		}
		if aborted.State != rollout.StateAborted || aborted.PausedAt != nil || aborted.PausedFrom != nil {
			t.Fatalf("aborted = %+v", aborted)
		}
		if _, err := s.ResumeRollout(ctx, r.ID, byAdmin); err == nil {
			t.Fatal("resumed an aborted rollout")
		}
	})
}

func TestEveryTransitionIsAudited(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	r := w.mustCreate(t, ctx, "demo")
	withReason := store.TransitionOptions{Actor: "oncall", Reason: "investigating"}

	if _, err := s.StartRollout(ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("StartRollout: %v", err)
	}
	if _, err := s.PauseRollout(ctx, r.ID, withReason); err != nil {
		t.Fatalf("PauseRollout: %v", err)
	}
	if _, err := s.ResumeRollout(ctx, r.ID, byAdmin); err != nil {
		t.Fatalf("ResumeRollout: %v", err)
	}
	// A rejected transition must leave no audit row.
	if _, err := s.StartRollout(ctx, r.ID, byAdmin); err == nil {
		t.Fatal("restart succeeded")
	}

	log, err := s.ListTransitions(ctx, r.ID)
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	want := []store.Transition{
		{Revision: 2, From: rollout.StateDraft, To: rollout.StateRunning, Event: rollout.EventStart, Actor: "test"},
		{Revision: 3, From: rollout.StateRunning, To: rollout.StatePaused, Event: rollout.EventPause, Actor: "oncall", Reason: "investigating"},
		{Revision: 4, From: rollout.StatePaused, To: rollout.StateRunning, Event: rollout.EventResume, Actor: "test"},
	}
	if len(log) != len(want) {
		t.Fatalf("log = %+v", log)
	}
	for i := range want {
		got := log[i]
		got.OccurredAt = time.Time{}
		if got != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got, want[i])
		}
	}
}
