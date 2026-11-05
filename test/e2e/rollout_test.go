// Package e2e drives whole rollouts through the real HTTP API, store, and engine on real
// Postgres. Time comes from a fake clock and the engine is ticked by hand, so scenarios
// are exact and fast. Devices here are scripted, because the real agent doesn't exist yet.
package e2e

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/engine"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

const (
	adminToken = "e2e-admin-credential-value"
	fleetName  = "lab-a"
	leaseTTL   = time.Minute
	stageSoak  = 10 * time.Minute
)

type clock struct{ nanos atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.nanos.Store(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano())
	return c
}

func (c *clock) Now() time.Time          { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *clock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

// controlPlane is one control plane process: a store, an API server, and an engine. A
// scenario can run several against one database, and none of them shares any memory.
type controlPlane struct {
	store  *store.Store
	engine *engine.Engine
	admin  *client.Client
	base   string
}

// device is a scripted device with its real credential, and a persisted sequence.
type device struct {
	id       string
	client   *client.DeviceClient
	sequence int64
}

// fixture is one scenario's database, clock, and fleet.
type fixture struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	clock   *clock
	cp      *controlPlane
	devices map[string]*device
	release int64
}

func newFixture(t *testing.T, devices int) *fixture {
	t.Helper()
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	f := &fixture{t: t, ctx: ctx, pool: testdb.New(t), clock: newClock(), devices: map[string]*device{}}
	f.cp = f.newControlPlane("control-plane-a")

	if _, err := f.cp.admin.CreateFleet(ctx, apiv1.CreateFleetRequest{Name: fleetName}); err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	specs := make([]apiv1.DeviceSpec, devices)
	for i := range specs {
		specs[i] = apiv1.DeviceSpec{ID: fmt.Sprintf("sim-%05d", i), Platform: "linux/arm64"}
	}
	for batch := range slices.Chunk(specs, 1000) {
		registered, err := f.cp.admin.RegisterDevices(ctx, fleetName, batch)
		if err != nil {
			t.Fatalf("RegisterDevices: %v", err)
		}
		for _, d := range registered {
			f.devices[d.ID] = &device{id: d.ID, client: client.NewDevice(f.cp.base, d.Token, nil)}
		}
	}
	return f
}

func (f *fixture) newControlPlane(holder string) *controlPlane {
	f.t.Helper()
	st := store.New(f.pool, store.WithClock(f.clock.Now))
	logger := slog.New(slog.DiscardHandler)
	ts := httptest.NewServer(api.New(api.Options{
		Logger:     logger,
		Ready:      func(context.Context) error { return nil },
		Store:      st,
		AdminToken: adminToken,
	}))
	f.t.Cleanup(ts.Close)
	return &controlPlane{
		store: st,
		engine: engine.New(engine.Options{
			Store: st, Holder: holder, Interval: time.Second, LeaseTTL: leaseTTL, Logger: logger,
		}),
		admin: client.New(ts.URL, adminToken, nil),
		base:  ts.URL,
	}
}

// startRollout starts a rollout of a new artifact. By default its stages are 1, 10, 50,
// and 100 percent, a 10 minute soak, a 2% failure limit, and full required coverage.
func (f *fixture) startRollout(name string, edit func(*rollout.PolicySpec)) apiv1.Rollout {
	f.t.Helper()
	f.release++
	artifact, err := f.cp.admin.CreateArtifact(f.ctx, apiv1.CreateArtifactRequest{
		Name: "demo-app", Version: fmt.Sprintf("1.%d.0", f.release), Platform: "linux/arm64",
		ReleaseCounter: f.release, SizeBytes: 1, SHA256: strings.Repeat("a", 64),
	})
	if err != nil {
		f.t.Fatalf("CreateArtifact: %v", err)
	}
	return f.startRolloutOf(name, artifact.ID, edit)
}

func (f *fixture) startRolloutOf(name, artifactID string, edit func(*rollout.PolicySpec)) apiv1.Rollout {
	f.t.Helper()
	soak := rollout.Duration(stageSoak)
	limit, coverage := 200, rollout.FullBPS
	policy := rollout.PolicySpec{
		Stages: []rollout.StageSpec{
			{Percent: 1, Soak: &soak}, {Percent: 10, Soak: &soak}, {Percent: 50, Soak: &soak}, {Percent: 100, Soak: &soak},
		},
		Safety: rollout.SafetySpec{MaxFailureBPS: &limit, RequiredCoverageBPS: &coverage},
	}
	if edit != nil {
		edit(&policy)
	}
	created, err := f.cp.admin.CreateRollout(f.ctx, fleetName, apiv1.CreateRolloutRequest{
		Name: name, ArtifactID: artifactID, Policy: policy,
	})
	if err != nil {
		f.t.Fatalf("CreateRollout: %v", err)
	}
	started, err := f.cp.admin.Transition(f.ctx, created.ID, client.ActionStart, apiv1.TransitionRequest{})
	if err != nil {
		f.t.Fatalf("start: %v", err)
	}
	return started
}

func (f *fixture) status(id string) apiv1.Rollout {
	f.t.Helper()
	r, err := f.cp.admin.GetRollout(f.ctx, id)
	if err != nil {
		f.t.Fatalf("GetRollout: %v", err)
	}
	return r
}

// order returns device IDs in the rollout's exposure order.
func (f *fixture) order(id string) []string {
	f.t.Helper()
	var out []string
	after := 0
	for {
		page, err := f.cp.admin.ListCohort(f.ctx, id, 0, after, 1000)
		if err != nil {
			f.t.Fatalf("ListCohort: %v", err)
		}
		for _, entry := range page.Entries {
			out = append(out, entry.DeviceID)
		}
		if page.NextAfter == 0 {
			return out
		}
		after = page.NextAfter
	}
}

func (d *device) poll(f *fixture) *apiv1.DeviceAssignment {
	f.t.Helper()
	polled, err := d.client.Poll(f.ctx)
	if err != nil {
		f.t.Fatalf("Poll(%s): %v", d.id, err)
	}
	return polled.Assignment
}

func (d *device) report(f *fixture, a *apiv1.DeviceAssignment, state rollout.AssignmentState) apiv1.ReportRequest {
	f.t.Helper()
	d.sequence++
	req := apiv1.ReportRequest{
		Sequence: d.sequence, RolloutID: a.RolloutID, AssignmentID: a.ID, Generation: a.Generation, State: state,
	}
	if _, err := d.client.Report(f.ctx, req); err != nil {
		f.t.Fatalf("Report(%s, %s): %v", d.id, state, err)
	}
	return req
}

// succeed takes an assignment all the way through a healthy install.
func (d *device) succeed(f *fixture, a *apiv1.DeviceAssignment) {
	f.t.Helper()
	for _, state := range []rollout.AssignmentState{
		rollout.AssignmentDownloading, rollout.AssignmentVerifying, rollout.AssignmentInstalling,
		rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded,
	} {
		d.report(f, a, state)
	}
}

// pollAll has every device poll, and hands out what each is assigned. It returns how
// many devices were given work.
func (f *fixture) pollAll(handle func(*device, *apiv1.DeviceAssignment)) int {
	f.t.Helper()
	assigned := 0
	ids := make([]string, 0, len(f.devices))
	for id := range f.devices {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		d := f.devices[id]
		if a := d.poll(f); a != nil {
			assigned++
			handle(d, a)
		}
	}
	return assigned
}

func healthy(f *fixture) func(*device, *apiv1.DeviceAssignment) {
	return func(d *device, a *apiv1.DeviceAssignment) { d.succeed(f, a) }
}

// runToEnd alternates between devices polling, the engine ticking, and time passing, until
// the rollout finishes. The bound stops a stuck rollout from hanging the test.
func (f *fixture) runToEnd(id string, handle func(*device, *apiv1.DeviceAssignment)) apiv1.Rollout {
	f.t.Helper()
	for range 200 {
		f.pollAll(handle)
		f.cp.engine.Tick(f.ctx)
		r := f.status(id)
		if r.State == rollout.StateCompleted || r.State == rollout.StateHalted {
			return r
		}
		f.clock.Advance(stageSoak / 2)
	}
	f.t.Fatalf("the rollout never finished: %+v", f.status(id))
	return apiv1.Rollout{}
}

func TestGoodReleaseReachesEveryDeviceStageByStage(t *testing.T) {
	f := newFixture(t, 100)
	r := f.startRollout("good", nil)
	order := f.order(r.ID)

	// Exposure must follow the rollout's order: a device gets work only once its stage
	// is exposed, however often it polls.
	exposed := map[string]bool{}
	final := func() apiv1.Rollout {
		for range 200 {
			current := f.status(r.ID)
			if current.State == rollout.StateCompleted || current.State == rollout.StateHalted {
				return current
			}
			target := *current.Stages[*current.CurrentStage-1].TargetCount
			f.pollAll(func(d *device, a *apiv1.DeviceAssignment) {
				position := slices.Index(order, d.id)
				if position >= target {
					t.Fatalf("%s at position %d got work with only %d devices exposed", d.id, position+1, target)
				}
				if exposed[d.id] {
					t.Fatalf("%s was assigned twice", d.id)
				}
				exposed[d.id] = true
				d.succeed(f, a)
			})
			f.cp.engine.Tick(f.ctx)
			f.clock.Advance(stageSoak / 2)
		}
		t.Fatal("the rollout never finished")
		return apiv1.Rollout{}
	}()

	if final.State != rollout.StateCompleted {
		t.Fatalf("state = %s (%s)", final.State, final.HaltDetail)
	}
	if len(exposed) != 100 {
		t.Fatalf("%d devices were updated, want all 100", len(exposed))
	}
	for _, stage := range final.Stages {
		if stage.State != "passed" || stage.Failed != 0 || stage.Succeeded != *stage.CohortSize {
			t.Errorf("stage %d = %+v", stage.Ordinal, stage)
		}
	}

	// Every device now records the release, so this can't be rolled out to them again.
	page, err := f.cp.admin.ListDevices(f.ctx, fleetName, "", 1000)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	for _, d := range page.Devices {
		if d.InstalledRelease != f.release {
			t.Errorf("%s has release %d, want %d", d.ID, d.InstalledRelease, f.release)
		}
	}

	log, err := f.cp.admin.ListTransitions(f.ctx, r.ID)
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	events := make([]rollout.Event, len(log))
	for i, entry := range log {
		events[i] = entry.Event
	}
	want := []rollout.Event{
		rollout.EventStart,
		rollout.EventCoverageMet, rollout.EventSoakPassed,
		rollout.EventCoverageMet, rollout.EventSoakPassed,
		rollout.EventCoverageMet, rollout.EventSoakPassed,
		rollout.EventCoverageMet, rollout.EventFinalSoakPassed,
	}
	if !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestBadReleaseStopsEarlyAndExposesNobodyElse(t *testing.T) {
	f := newFixture(t, 500)
	r := f.startRollout("bad", func(p *rollout.PolicySpec) {
		minimum := 5
		p.Stages[0].MinObservations = &minimum
	})

	// Every device that installs this release fails its health check.
	assignedEver := 0
	final := f.runToEnd(r.ID, func(d *device, a *apiv1.DeviceAssignment) {
		assignedEver++
		d.report(f, a, rollout.AssignmentInstalling)
		d.report(f, a, rollout.AssignmentFailed)
	})

	if final.State != rollout.StateHalted {
		t.Fatalf("state = %s", final.State)
	}
	if final.HaltReason == "" {
		t.Fatal("no reason recorded for the halt")
	}
	// Stage 1 is 1% of 500. Nothing past it was ever offered.
	if assignedEver > 5 {
		t.Fatalf("%d of 500 devices were exposed, want at most the 5 in stage 1", assignedEver)
	}
	if *final.CurrentStage != 1 || final.Stages[0].State != "failed" {
		t.Fatalf("stages = %+v", final.Stages)
	}

	// It stays stopped, however many devices keep polling and however much time passes.
	f.clock.Advance(24 * time.Hour)
	if got := f.pollAll(func(*device, *apiv1.DeviceAssignment) { t.Fatal("assigned after the halt") }); got != 0 {
		t.Fatalf("%d assignments after the halt", got)
	}
	f.cp.engine.Tick(f.ctx)
	if got := f.status(r.ID).State; got != rollout.StateHalted {
		t.Fatalf("state = %s", got)
	}

	// A halted rollout holds the fleet, so nothing else starts until someone decides.
	if _, err := f.cp.admin.Transition(f.ctx, r.ID, client.ActionResume, apiv1.TransitionRequest{}); err == nil {
		t.Fatal("resumed a halted rollout")
	}
	if _, err := f.cp.admin.Transition(f.ctx, r.ID, client.ActionAbort, apiv1.TransitionRequest{Reason: "bad release"}); err != nil {
		t.Fatalf("abort: %v", err)
	}
	fixed := f.startRollout("fixed", nil)
	if got := f.runToEnd(fixed.ID, healthy(f)); got.State != rollout.StateCompleted {
		t.Fatalf("the fixed release: state = %s (%s)", got.State, got.HaltDetail)
	}
}

func TestFailuresInALaterStageAreNotHiddenByEarlierSuccess(t *testing.T) {
	f := newFixture(t, 200)
	r := f.startRollout("regression", nil)
	order := f.order(r.ID)
	const goodDevices = 20 // Stages 1 and 2 are healthy. The rest of the fleet fails.

	final := f.runToEnd(r.ID, func(d *device, a *apiv1.DeviceAssignment) {
		if slices.Index(order, d.id) < goodDevices {
			d.succeed(f, a)
			return
		}
		d.report(f, a, rollout.AssignmentFailed)
	})
	if final.State != rollout.StateHalted || *final.CurrentStage != 3 {
		t.Fatalf("state = %s at stage %v", final.State, final.CurrentStage)
	}
	if final.Stages[0].State != "passed" || final.Stages[1].State != "passed" {
		t.Fatalf("earlier stages lost their verdicts: %+v", final.Stages[:2])
	}
	// Across the rollout only a minority of outcomes failed, and it still halted, because
	// stage 3 is judged by its own cohort. The last stage never saw this release.
	if final.Stages[3].Succeeded+final.Stages[3].Failed != 0 {
		t.Fatalf("the last stage was exposed: %+v", final.Stages[3])
	}
}

func TestRepeatedReportsDoNotChangeTheOutcome(t *testing.T) {
	f := newFixture(t, 100)
	r := f.startRollout("retries", nil)

	final := f.runToEnd(r.ID, func(d *device, a *apiv1.DeviceAssignment) {
		for _, state := range []rollout.AssignmentState{rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded} {
			req := d.report(f, a, state)
			// A flaky network delivers every report three times.
			for range 2 {
				got, err := d.client.Report(f.ctx, req)
				if err != nil {
					t.Fatalf("retry: %v", err)
				}
				if !got.Duplicate {
					t.Fatalf("retry = %+v, want a duplicate", got)
				}
			}
		}
	})
	if final.State != rollout.StateCompleted {
		t.Fatalf("state = %s", final.State)
	}
	total := 0
	for _, stage := range final.Stages {
		total += stage.Succeeded
	}
	if total != 100 {
		t.Fatalf("%d successes counted for 100 devices", total)
	}
}

func TestSilentFleetNeverPromotes(t *testing.T) {
	f := newFixture(t, 100)
	r := f.startRollout("silent", nil)

	// No device ever reports anything. Silence must not look like health.
	for range 13 {
		f.cp.engine.Tick(f.ctx)
		got := f.status(r.ID)
		if got.State == rollout.StateHalted {
			break
		}
		if *got.CurrentStage != 1 || got.State != rollout.StateRunning {
			t.Fatalf("progress without any reports: %+v", got)
		}
		f.clock.Advance(6 * time.Minute)
	}
	got := f.status(r.ID)
	if got.State != rollout.StateHalted || got.HaltReason != string(rollout.HaltDeadline) {
		t.Fatalf("state = %s, reason = %q, want a deadline halt", got.State, got.HaltReason)
	}
	if got.Stages[0].Succeeded != 0 {
		t.Fatalf("stage = %+v", got.Stages[0])
	}
}

func TestPauseFreezesExposureUntilResumed(t *testing.T) {
	f := newFixture(t, 100)
	r := f.startRollout("paused", nil)
	if _, err := f.cp.admin.Transition(f.ctx, r.ID, client.ActionPause, apiv1.TransitionRequest{Reason: "checking"}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := f.pollAll(func(*device, *apiv1.DeviceAssignment) {}); got != 0 {
		t.Fatalf("%d assignments during a pause", got)
	}

	// A week-long pause doesn't use up the stage's deadline.
	f.clock.Advance(7 * 24 * time.Hour)
	f.cp.engine.Tick(f.ctx)
	if got := f.status(r.ID).State; got != rollout.StatePaused {
		t.Fatalf("state during a long pause = %s", got)
	}

	if _, err := f.cp.admin.Transition(f.ctx, r.ID, client.ActionResume, apiv1.TransitionRequest{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := f.runToEnd(r.ID, healthy(f)); got.State != rollout.StateCompleted {
		t.Fatalf("state = %s (%s)", got.State, got.HaltDetail)
	}
}

func (f *fixture) expireLease() {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE engine_lease SET expires_at = clock_timestamp() - interval '1 second'`); err != nil {
		f.t.Fatalf("expire lease: %v", err)
	}
}

func TestAnotherInstanceTakesOverWhenTheLeaderDies(t *testing.T) {
	f := newFixture(t, 100)
	r := f.startRollout("failover", nil)
	first := f.cp

	// The first instance runs part of the rollout, then disappears without a word.
	f.pollAll(healthy(f))
	first.engine.Tick(f.ctx)
	if got := f.status(r.ID).State; got != rollout.StateSoaking {
		t.Fatalf("state = %s, want soaking", got)
	}

	// While its lease is valid, a standby does nothing.
	standby := f.newControlPlane("control-plane-b")
	f.clock.Advance(stageSoak)
	standby.engine.Tick(f.ctx)
	if got := f.status(r.ID).State; got != rollout.StateSoaking {
		t.Fatalf("a standby acted while the leader held the lease: %s", got)
	}

	f.expireLease()
	standby.engine.Tick(f.ctx)
	after := f.status(r.ID)
	if after.State != rollout.StateRunning || *after.CurrentStage != 2 {
		t.Fatalf("the standby didn't take over: %s at stage %v", after.State, after.CurrentStage)
	}

	// The old leader wakes up, believing it still leads. It can't change anything.
	revision := after.Revision
	f.pollAll(healthy(f))
	first.engine.Tick(f.ctx)
	if got := f.status(r.ID); got.Revision != revision {
		t.Fatalf("the old leader changed the rollout: revision %d, was %d", got.Revision, revision)
	}

	// The new leader carries on, and the audit trail shows who decided what.
	f.cp = standby
	if got := f.runToEnd(r.ID, healthy(f)); got.State != rollout.StateCompleted {
		t.Fatalf("state = %s (%s)", got.State, got.HaltDetail)
	}
	log, err := standby.admin.ListTransitions(f.ctx, r.ID)
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	var generations []int64
	for _, entry := range log {
		if entry.EngineGeneration != nil && !slices.Contains(generations, *entry.EngineGeneration) {
			generations = append(generations, *entry.EngineGeneration)
		}
	}
	if !slices.IsSorted(generations) || len(generations) < 2 {
		t.Fatalf("generations in the audit log = %v, want them increasing across the failover", generations)
	}
}

func TestRestartDuringAnActiveRolloutLosesNothing(t *testing.T) {
	f := newFixture(t, 100)
	r := f.startRollout("restart", nil)
	order := f.order(r.ID)
	f.pollAll(healthy(f))
	f.cp.engine.Tick(f.ctx)
	f.clock.Advance(stageSoak)
	f.cp.engine.Tick(f.ctx)

	// A device is partway through an assignment when everything restarts.
	var midway *device
	var midwayAssignment *apiv1.DeviceAssignment
	f.pollAll(func(d *device, a *apiv1.DeviceAssignment) {
		if midway == nil {
			midway, midwayAssignment = d, a
			d.report(f, a, rollout.AssignmentDownloading)
			return
		}
		d.succeed(f, a)
	})
	if midway == nil {
		t.Fatal("no device was assigned in stage 2")
	}
	before := f.status(r.ID)

	// Every process is gone. Only the database survives.
	f.expireLease()
	f.cp = f.newControlPlane("control-plane-restarted")
	after := f.status(r.ID)
	if after.State != before.State || after.Revision != before.Revision || *after.CurrentStage != *before.CurrentStage {
		t.Fatalf("state changed across the restart: %+v, was %+v", after, before)
	}
	if !slices.Equal(f.order(r.ID), order) {
		t.Fatal("the cohort changed across the restart")
	}

	// The device gets the same assignment back, and nobody gets a second one.
	again := midway.poll(f)
	if again == nil || again.ID != midwayAssignment.ID || again.State != rollout.AssignmentDownloading {
		t.Fatalf("assignment after the restart = %+v, was %+v", again, midwayAssignment)
	}
	midway.succeed(f, again)
	if got := f.runToEnd(r.ID, healthy(f)); got.State != rollout.StateCompleted {
		t.Fatalf("state = %s (%s)", got.State, got.HaltDetail)
	}
	var count int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM assignments WHERE rollout_id = $1`, r.ID).Scan(&count); err != nil {
		t.Fatalf("count assignments: %v", err)
	}
	if count != 100 {
		t.Fatalf("%d assignments for 100 devices", count)
	}
}

func TestFollowUpRolloutReachesOnlyTheDevicesLeftBehind(t *testing.T) {
	f := newFixture(t, 100)
	ninety := 9000
	soak := rollout.Duration(0)
	configure := func(p *rollout.PolicySpec) {
		p.Stages = []rollout.StageSpec{{Percent: 100, Soak: &soak}}
		p.Safety.RequiredCoverageBPS = &ninety
		maxUnknown := 1000
		p.Safety.MaxUnknownBPS = &maxUnknown
	}
	r := f.startRollout("first-pass", configure)
	order := f.order(r.ID)
	offline := map[string]bool{}
	for _, id := range order[90:] {
		offline[id] = true
	}

	// Ten devices are offline for the whole rollout, and 90% coverage is enough.
	online := make(map[string]*device, 90)
	for id, d := range f.devices {
		if !offline[id] {
			online[id] = d
		}
	}
	everyone := f.devices
	f.devices = online
	final := f.runToEnd(r.ID, healthy(f))
	if final.State != rollout.StateCompleted {
		t.Fatalf("state = %s (%s)", final.State, final.HaltDetail)
	}

	// A completed rollout stops offering work, even to devices that come back.
	f.devices = everyone
	if got := f.pollAll(func(*device, *apiv1.DeviceAssignment) { t.Fatal("assigned after completion") }); got != 0 {
		t.Fatalf("%d assignments after completion", got)
	}

	// A follow-up of the same artifact finds exactly the devices left behind.
	followUp := f.startRolloutOf("second-pass", r.ArtifactID, configure)
	if *followUp.MemberCount != 10 || followUp.Exclusions[rollout.ExclusionAlreadyCurrent] != 90 {
		t.Fatalf("follow-up members = %d, exclusions = %v", *followUp.MemberCount, followUp.Exclusions)
	}
	if got := f.runToEnd(followUp.ID, healthy(f)); got.State != rollout.StateCompleted {
		t.Fatalf("follow-up: state = %s (%s)", got.State, got.HaltDetail)
	}
	for id := range offline {
		if d := f.devices[id]; d.poll(f) != nil {
			t.Fatalf("%s still has work", id)
		}
	}
}
