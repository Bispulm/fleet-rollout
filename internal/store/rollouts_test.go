package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

func ptr[T any](v T) *T { return &v }

// world is a fleet of size devices with an artifact ready to roll out.
type world struct {
	store    *store.Store
	artifact store.Artifact
}

func newWorld(t *testing.T, ctx context.Context, s *store.Store, devices int) world {
	t.Helper()
	if _, err := s.CreateFleet(ctx, "lab-a", nil); err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	if devices > 0 {
		if _, err := s.RegisterDevices(ctx, "lab-a", newDevices(devices)); err != nil {
			t.Fatalf("RegisterDevices: %v", err)
		}
	}
	return world{store: s, artifact: mustRegisterArtifact(t, ctx, s, newArtifact("1.4.0", 14))}
}

func mustPolicy(t *testing.T, edit func(*rollout.PolicySpec)) rollout.Policy {
	t.Helper()
	spec := rollout.PolicySpec{
		Stages: []rollout.StageSpec{{Percent: 10}, {Percent: 50}, {Percent: 100}},
		Safety: rollout.SafetySpec{MaxFailureBPS: ptr(200)},
	}
	if edit != nil {
		edit(&spec)
	}
	p, err := spec.Normalize()
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return p
}

func (w world) newRollout(t *testing.T, name string) store.NewRollout {
	t.Helper()
	return store.NewRollout{Name: name, ArtifactID: w.artifact.ID, Policy: mustPolicy(t, nil)}
}

func (w world) mustCreate(t *testing.T, ctx context.Context, name string) store.Rollout {
	t.Helper()
	r, created, err := w.store.CreateRollout(ctx, "lab-a", w.newRollout(t, name))
	if err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if !created {
		t.Fatalf("CreateRollout(%q) found an existing rollout", name)
	}
	return r
}

func TestCreateRolloutPersistsADraftWithItsPolicy(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 100)

	created := w.mustCreate(t, ctx, "demo")
	got, err := s.GetRollout(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetRollout: %v", err)
	}
	if got.State != rollout.StateDraft || got.Revision != 1 || got.FleetName != "lab-a" || got.Name != "demo" {
		t.Fatalf("rollout = %+v", got)
	}
	if got.Policy.Safety.MaxFailureBPS != 200 || got.Policy.Safety.RequiredCoverageBPS != 9000 {
		t.Fatalf("policy = %+v", got.Policy.Safety)
	}
	if len(got.Stages) != 3 || got.Stages[0].Percent != 10 || got.Stages[2].Percent != 100 {
		t.Fatalf("stages = %+v", got.Stages)
	}
	if got.MemberCount != nil || got.StartedAt != nil {
		t.Fatalf("a draft has no membership yet: %+v", got)
	}
	_, want, err := got.Policy.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	if got.PolicyDigest != want {
		t.Fatal("stored digest doesn't match the policy read back from Postgres")
	}
}

func TestCreateRolloutAcceptsAClientChosenID(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	input := w.newRollout(t, "demo")
	input.ID = "11111111-1111-4111-8111-111111111111"

	r, created, err := s.CreateRollout(ctx, "lab-a", input)
	if err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if !created || r.ID != input.ID {
		t.Fatalf("created = %v, id = %s", created, r.ID)
	}
}

func TestCreateRolloutRejectsNonCanonicalID(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	input := w.newRollout(t, "demo")
	// Rank keys hash the ID's text, so a second spelling of one UUID must not exist.
	input.ID = "AAAAAAAA-1111-4111-8111-111111111111"
	_, _, err := s.CreateRollout(ctx, "lab-a", input)
	var policyErr *rollout.PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("error = %v, want *rollout.PolicyError", err)
	}
}

func TestCreateRolloutIsIdempotentForAnIdenticalRequest(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	first := w.mustCreate(t, ctx, "demo")

	again, created, err := s.CreateRollout(ctx, "lab-a", w.newRollout(t, "demo"))
	if err != nil {
		t.Fatalf("second CreateRollout: %v", err)
	}
	if created {
		t.Fatal("an identical retry reported creating a new rollout")
	}
	if again.ID != first.ID {
		t.Fatalf("retry returned %s, want %s", again.ID, first.ID)
	}
}

func TestCreateRolloutRejectsDifferentRequestWithSameName(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 100)
	w.mustCreate(t, ctx, "demo")

	changed := w.newRollout(t, "demo")
	changed.Policy = mustPolicy(t, func(p *rollout.PolicySpec) { p.Safety.MaxFailureBPS = ptr(100) })
	_, _, err := s.CreateRollout(ctx, "lab-a", changed)
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want *ConflictError", err)
	}
}

func TestCreateRolloutRejectsImpossiblePolicyForCurrentFleet(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	input := w.newRollout(t, "demo")
	// The 10% stage exposes one device, so it can never observe five outcomes.
	input.Policy = mustPolicy(t, func(p *rollout.PolicySpec) { p.Stages[0].MinObservations = ptr(5) })

	_, _, err := s.CreateRollout(ctx, "lab-a", input)
	var policyErr *rollout.PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("error = %v, want *rollout.PolicyError", err)
	}
	if !strings.Contains(policyErr.Error(), "stages[0].min_observations") {
		t.Fatalf("error %q should name the stage", policyErr)
	}
}

func TestCreateRolloutRejectsFleetWithNoEligibleDevices(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 0)
	_, _, err := s.CreateRollout(ctx, "lab-a", w.newRollout(t, "demo"))
	var policyErr *rollout.PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("error = %v, want *rollout.PolicyError", err)
	}
}

func TestCreateRolloutReportsUnknownReferences(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)

	input := w.newRollout(t, "demo")
	if _, _, err := s.CreateRollout(ctx, "missing", input); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown fleet error = %v, want ErrNotFound", err)
	}
	input.ArtifactID = "00000000-0000-4000-8000-000000000000"
	if _, _, err := s.CreateRollout(ctx, "lab-a", input); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown artifact error = %v, want ErrNotFound", err)
	}
}

func TestGetRolloutReportsNotFound(t *testing.T) {
	s, ctx := newStore(t)
	for _, id := range []string{"00000000-0000-4000-8000-000000000000", "not-a-uuid"} {
		if _, err := s.GetRollout(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("GetRollout(%q) error = %v, want ErrNotFound", id, err)
		}
	}
}

// A client that times out and retries can have its retry race the first attempt,
// which may still be in flight. The retry must find that rollout, not a conflict.
func TestRetryRacingAnInFlightCreateReturnsTheRollout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testdb.New(t)
	s := store.New(pool)
	w := newWorld(t, ctx, s, 10)
	input := w.newRollout(t, "demo")
	encoded, digest, err := input.Policy.Canonical()
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}

	// The first attempt: inserted, but not committed yet.
	first, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = first.Rollback(ctx) }()
	var firstID string
	if err := first.QueryRow(ctx, `
		INSERT INTO rollouts (fleet_id, artifact_id, name, policy, policy_digest, created_at)
		SELECT id, $1, 'demo', $2, $3, now() FROM fleets WHERE name = 'lab-a' RETURNING id`,
		w.artifact.ID, encoded, digest[:]).Scan(&firstID); err != nil {
		t.Fatalf("insert first attempt: %v", err)
	}
	for i, stage := range input.Policy.Stages {
		if _, err := first.Exec(ctx, `INSERT INTO rollout_stages (rollout_id, ordinal, percent) VALUES ($1, $2, $3)`,
			firstID, i+1, stage.Percent); err != nil {
			t.Fatalf("insert stage: %v", err)
		}
	}

	type result struct {
		rollout store.Rollout
		created bool
		err     error
	}
	retry := make(chan result, 1)
	go func() {
		r, created, err := s.CreateRollout(ctx, "lab-a", input)
		retry <- result{r, created, err}
	}()

	// Wait until the retry is really blocked on the first attempt's unique key.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		if err != nil {
			t.Fatalf("check blocked sessions: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the retry never blocked on the first attempt")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit first attempt: %v", err)
	}

	got := <-retry
	if got.err != nil {
		t.Fatalf("retry failed: %v", got.err)
	}
	if got.created || got.rollout.ID != firstID {
		t.Fatalf("retry returned created = %v, id = %s; want the first attempt's %s", got.created, got.rollout.ID, firstID)
	}
}

func TestConcurrentIdenticalCreatesReturnOneRollout(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)

	const attempts = 8
	ids := make([]string, attempts)
	created := make([]bool, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			var r store.Rollout
			r, created[i], errs[i] = s.CreateRollout(ctx, "lab-a", w.newRollout(t, "demo"))
			ids[i] = r.ID
		})
	}
	wg.Wait()

	newCount := 0
	for i := range attempts {
		if errs[i] != nil {
			t.Errorf("attempt %d: %v", i, errs[i])
			continue
		}
		if ids[i] != ids[0] {
			t.Errorf("attempt %d returned %s, want %s", i, ids[i], ids[0])
		}
		if created[i] {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("%d attempts reported creating the rollout, want 1", newCount)
	}
}

func TestDifferentDefinitionRacingACreateStillConflicts(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 100)

	limits := []int{100, 200}
	errs := make([]error, len(limits))
	var wg sync.WaitGroup
	for i, limit := range limits {
		input := w.newRollout(t, "demo")
		input.Policy = mustPolicy(t, func(p *rollout.PolicySpec) { p.Safety.MaxFailureBPS = ptr(limit) })
		wg.Go(func() { _, _, errs[i] = s.CreateRollout(ctx, "lab-a", input) })
	}
	wg.Wait()

	conflicts := 0
	for _, err := range errs {
		var conflict *store.ConflictError
		switch {
		case err == nil:
		case errors.As(err, &conflict):
			conflicts++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if conflicts != 1 {
		t.Fatalf("errors = %v, want exactly one conflict", errs)
	}
}

func TestRolloutsRequireAnArtifactWithItsContent(t *testing.T) {
	s, ctx := newStore(t)
	w := newWorld(t, ctx, s, 10)
	unready, _, err := s.RegisterArtifact(ctx, newArtifact("2.0.0", 20))
	if err != nil {
		t.Fatalf("RegisterArtifact: %v", err)
	}
	input := w.newRollout(t, "demo")
	input.ArtifactID = unready.ID

	_, _, err = s.CreateRollout(ctx, "lab-a", input)
	var conflict *store.ConflictError
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Error(), "content") {
		t.Fatalf("error = %v, want a conflict about the missing content", err)
	}

	if err := s.MarkArtifactContentReady(ctx, unready.ID); err != nil {
		t.Fatalf("MarkArtifactContentReady: %v", err)
	}
	if _, _, err := s.CreateRollout(ctx, "lab-a", input); err != nil {
		t.Fatalf("CreateRollout once the content arrived: %v", err)
	}
}
