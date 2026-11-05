package api_test

import (
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

func ptr[T any](v T) *T { return &v }

func rolloutRequest(artifactID string) apiv1.CreateRolloutRequest {
	return apiv1.CreateRolloutRequest{
		Name:       "demo",
		ArtifactID: artifactID,
		Policy: rollout.PolicySpec{
			Stages: []rollout.StageSpec{{Percent: 10}, {Percent: 50}, {Percent: 100}},
			Safety: rollout.SafetySpec{MaxFailureBPS: ptr(200)},
		},
	}
}

// newRollout sets up a fleet of n devices with a draft rollout.
func (h harness) newRollout(n int) apiv1.Rollout {
	h.t.Helper()
	h.createFleet("lab-a")
	h.registerDevices("lab-a", n)
	var r apiv1.Rollout
	h.expect(http.StatusCreated, http.MethodPost, "/v1/fleets/lab-a/rollouts", rolloutRequest(h.createArtifact().ID), &r)
	return r
}

func TestCreateRolloutReturnsDraftStatus(t *testing.T) {
	h := newHarness(t)
	r := h.newRollout(20)
	if r.State != rollout.StateDraft || r.Revision != 1 || r.Fleet != "lab-a" || len(r.Stages) != 3 {
		t.Fatalf("rollout = %+v", r)
	}
	if r.Policy.Safety.RequiredCoverageBPS != 9000 {
		t.Fatalf("defaults missing from the status: %+v", r.Policy.Safety)
	}
	if len(r.PolicyDigest) != 64 {
		t.Fatalf("policy digest = %q", r.PolicyDigest)
	}

	var got apiv1.Rollout
	h.expect(http.StatusOK, http.MethodGet, "/v1/rollouts/"+r.ID, nil, &got)
	if got.ID != r.ID || got.PolicyDigest != r.PolicyDigest {
		t.Fatalf("got = %+v", got)
	}
}

func TestCreateRolloutIdempotency(t *testing.T) {
	h := newHarness(t)
	r := h.newRollout(20)

	req := rolloutRequest(r.ArtifactID)
	var again apiv1.Rollout
	h.expect(http.StatusOK, http.MethodPost, "/v1/fleets/lab-a/rollouts", req, &again)
	if again.ID != r.ID {
		t.Fatalf("retry returned %s, want %s", again.ID, r.ID)
	}

	req.Policy.Safety.MaxFailureBPS = ptr(100)
	if p := h.problem(http.StatusConflict, http.MethodPost, "/v1/fleets/lab-a/rollouts", req); p.Code != apiv1.CodeConflict {
		t.Fatalf("code = %q", p.Code)
	}
}

func TestCreateRolloutRejectsBadPolicyWithAllViolations(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	h.registerDevices("lab-a", 10)
	req := rolloutRequest(h.createArtifact().ID)
	req.Policy.Stages[0].Percent = 0
	req.Policy.Safety.MaxFailureBPS = nil

	p := h.problem(http.StatusUnprocessableEntity, http.MethodPost, "/v1/fleets/lab-a/rollouts", req)
	if p.Code != apiv1.CodeInvalidPolicy || len(p.Violations) < 2 {
		t.Fatalf("problem = %+v", p)
	}
}

func TestCreateRolloutRejectsUnknownSafetySetting(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	body := `{"name":"demo","artifact_id":"00000000-0000-4000-8000-000000000000",
		"policy":{"stages":[{"percent":100}],"safety":{"max_failure_bps":200,"max_unknown_percent":1}}}`
	h.problem(http.StatusBadRequest, http.MethodPost, "/v1/fleets/lab-a/rollouts", body)
}

func TestCreateRolloutValidation(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	h.registerDevices("lab-a", 10)
	artifact := h.createArtifact()

	bad := rolloutRequest(artifact.ID)
	bad.Name = "Not Valid"
	h.problem(http.StatusUnprocessableEntity, http.MethodPost, "/v1/fleets/lab-a/rollouts", bad)

	bad = rolloutRequest(artifact.ID)
	bad.ID = "not-a-uuid"
	h.problem(http.StatusUnprocessableEntity, http.MethodPost, "/v1/fleets/lab-a/rollouts", bad)

	bad = rolloutRequest("00000000-0000-4000-8000-000000000000")
	h.problem(http.StatusNotFound, http.MethodPost, "/v1/fleets/lab-a/rollouts", bad)

	h.problem(http.StatusNotFound, http.MethodPost, "/v1/fleets/missing/rollouts", rolloutRequest(artifact.ID))
}

func TestCreateRolloutRejectsImpossibleSampleSize(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	h.registerDevices("lab-a", 10)
	req := rolloutRequest(h.createArtifact().ID)
	req.Policy.Stages[0].MinObservations = ptr(5)

	p := h.problem(http.StatusUnprocessableEntity, http.MethodPost, "/v1/fleets/lab-a/rollouts", req)
	if len(p.Violations) != 1 || p.Violations[0].Field != "stages[0].min_observations" {
		t.Fatalf("violations = %+v", p.Violations)
	}
}

func TestStartExposesFrozenDeterministicCohort(t *testing.T) {
	h := newHarness(t)
	r := h.newRollout(20)

	h.problem(http.StatusConflict, http.MethodGet, "/v1/rollouts/"+r.ID+"/cohort", nil)

	var started apiv1.Rollout
	h.expect(http.StatusOK, http.MethodPost, "/v1/rollouts/"+r.ID+"/start", nil, &started)
	if started.State != rollout.StateRunning || started.Revision != 2 || *started.MemberCount != 20 {
		t.Fatalf("started = %+v", started)
	}
	if started.Stages[0].State != "active" || *started.Stages[0].CohortSize != 2 {
		t.Fatalf("first stage = %+v", started.Stages[0])
	}

	var page apiv1.CohortPage
	h.expect(http.StatusOK, http.MethodGet, "/v1/rollouts/"+r.ID+"/cohort", nil, &page)
	if len(page.Entries) != 20 {
		t.Fatalf("cohort has %d entries", len(page.Entries))
	}
	ids := make([]string, 20)
	for i, spec := range deviceSpecs(20) {
		ids[i] = spec.ID
	}
	want := rollout.RankDevices(r.ID, ids)
	for i, entry := range page.Entries {
		if entry.DeviceID != want[i].DeviceID || entry.Position != i+1 {
			t.Fatalf("entry %d = %+v, want device %s", i, entry, want[i].DeviceID)
		}
		if entry.RankKey != hex.EncodeToString(want[i].Key[:]) {
			t.Fatalf("entry %d rank key = %s", i, entry.RankKey)
		}
	}

	var firstStage apiv1.CohortPage
	h.expect(http.StatusOK, http.MethodGet, "/v1/rollouts/"+r.ID+"/cohort?stage=1", nil, &firstStage)
	if len(firstStage.Entries) != 2 || firstStage.Entries[0].DeviceID != want[0].DeviceID {
		t.Fatalf("stage 1 = %+v", firstStage)
	}

	var paged apiv1.CohortPage
	h.expect(http.StatusOK, http.MethodGet, "/v1/rollouts/"+r.ID+"/cohort?limit=5", nil, &paged)
	if len(paged.Entries) != 5 || paged.NextAfter != 5 {
		t.Fatalf("page = %+v", paged)
	}
	h.expect(http.StatusOK, http.MethodGet, fmt.Sprintf("/v1/rollouts/%s/cohort?limit=5&after=%d", r.ID, paged.NextAfter), nil, &paged)
	if paged.Entries[0].Position != 6 {
		t.Fatalf("second page = %+v", paged)
	}

	for _, q := range []string{"stage=-1", "stage=x", "after=-1", "after=x", "limit=0"} {
		h.problem(http.StatusBadRequest, http.MethodGet, "/v1/rollouts/"+r.ID+"/cohort?"+q, nil)
	}
}

func TestLifecycleTransitionsAndErrorCodes(t *testing.T) {
	h := newHarness(t)
	r := h.newRollout(10)
	path := "/v1/rollouts/" + r.ID

	if p := h.problem(http.StatusConflict, http.MethodPost, path+"/pause", nil); p.Code != apiv1.CodeInvalidTransition {
		t.Fatalf("pause of a draft code = %q", p.Code)
	}
	stale := apiv1.TransitionRequest{ExpectedRevision: ptr(int64(99))}
	if p := h.problem(http.StatusConflict, http.MethodPost, path+"/start", stale); p.Code != apiv1.CodeRevisionMismatch {
		t.Fatalf("stale revision code = %q", p.Code)
	}

	h.expect(http.StatusOK, http.MethodPost, path+"/start", apiv1.TransitionRequest{ExpectedRevision: ptr(int64(1))}, nil)

	var paused apiv1.Rollout
	h.expect(http.StatusOK, http.MethodPost, path+"/pause", apiv1.TransitionRequest{Reason: "investigating"}, &paused)
	if paused.State != rollout.StatePaused || *paused.PausedFrom != rollout.StateRunning {
		t.Fatalf("paused = %+v", paused)
	}

	var resumed apiv1.Rollout
	h.expect(http.StatusOK, http.MethodPost, path+"/resume", nil, &resumed)
	if resumed.State != rollout.StateRunning {
		t.Fatalf("resumed = %+v", resumed)
	}

	var aborted apiv1.Rollout
	h.expect(http.StatusOK, http.MethodPost, path+"/abort", nil, &aborted)
	if aborted.State != rollout.StateAborted {
		t.Fatalf("aborted = %+v", aborted)
	}
	if p := h.problem(http.StatusConflict, http.MethodPost, path+"/resume", nil); p.Code != apiv1.CodeInvalidTransition {
		t.Fatalf("resume of an aborted rollout code = %q", p.Code)
	}

	var log apiv1.TransitionLog
	h.expect(http.StatusOK, http.MethodGet, path+"/transitions", nil, &log)
	events := make([]rollout.Event, len(log.Transitions))
	for i, entry := range log.Transitions {
		events[i] = entry.Event
		if entry.Actor != "admin" {
			t.Errorf("entry %d actor = %q", i, entry.Actor)
		}
	}
	want := fmt.Sprint([]rollout.Event{rollout.EventStart, rollout.EventPause, rollout.EventResume, rollout.EventAbort})
	if fmt.Sprint(events) != want {
		t.Fatalf("events = %v, want %s", events, want)
	}
	if log.Transitions[1].Reason != "investigating" {
		t.Fatalf("pause reason = %q", log.Transitions[1].Reason)
	}
}

func TestTransitionValidation(t *testing.T) {
	h := newHarness(t)
	r := h.newRollout(10)
	h.problem(http.StatusBadRequest, http.MethodPost, "/v1/rollouts/"+r.ID+"/start", `{"expected_revison":1}`)
	h.problem(http.StatusUnprocessableEntity, http.MethodPost, "/v1/rollouts/"+r.ID+"/start",
		apiv1.TransitionRequest{Reason: strings.Repeat("x", 1025)})
	h.problem(http.StatusNotFound, http.MethodPost, "/v1/rollouts/00000000-0000-4000-8000-000000000000/start", nil)
	h.problem(http.StatusNotFound, http.MethodGet, "/v1/rollouts/not-a-uuid", nil)
	h.problem(http.StatusNotFound, http.MethodGet, "/v1/rollouts/00000000-0000-4000-8000-000000000000/cohort", nil)
	h.problem(http.StatusNotFound, http.MethodGet, "/v1/rollouts/00000000-0000-4000-8000-000000000000/transitions", nil)
}

func TestConcurrentStartRequestsHaveOneWinner(t *testing.T) {
	h := newHarness(t)
	r := h.newRollout(100)

	const attempts = 8
	statuses := make([]int, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Go(func() {
			statuses[i], _ = h.do(http.MethodPost, "/v1/rollouts/"+r.ID+"/start", nil)
		})
	}
	wg.Wait()

	wins := 0
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			wins++
		case http.StatusConflict:
		default:
			t.Errorf("unexpected status %d", status)
		}
	}
	if wins != 1 {
		t.Fatalf("%d requests started the rollout: %v", wins, statuses)
	}
}
