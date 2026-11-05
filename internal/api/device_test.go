package api_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// deviceFixture is a running rollout, with the tokens of the devices in its first stage.
type deviceFixture struct {
	harness
	rollout apiv1.Rollout
	first   string // Token of the device at position 1.
	second  string // Token of the device at position 2.
	later   string // Token of a device in a later stage.
}

func newDeviceFixture(t *testing.T) deviceFixture {
	t.Helper()
	return newDeviceFixtureWith(t, nil)
}

// newDeviceFixtureWith lets a test change the server's options.
func newDeviceFixtureWith(t *testing.T, edit func(*api.Options)) deviceFixture {
	t.Helper()
	h := newHarnessWith(t, edit)
	h.createFleet("lab-a")
	devices := h.registerDevices("lab-a", 20)
	tokens := make(map[string]string, len(devices))
	for _, d := range devices {
		tokens[d.ID] = d.Token
	}
	var r apiv1.Rollout
	h.expect(http.StatusCreated, http.MethodPost, "/v1/fleets/lab-a/rollouts", rolloutRequest(h.createArtifact().ID), &r)
	h.expect(http.StatusOK, http.MethodPost, "/v1/rollouts/"+r.ID+"/start", nil, &r)

	var page apiv1.CohortPage
	h.expect(http.StatusOK, http.MethodGet, "/v1/rollouts/"+r.ID+"/cohort", nil, &page)
	return deviceFixture{
		harness: h,
		rollout: r,
		first:   tokens[page.Entries[0].DeviceID],
		second:  tokens[page.Entries[1].DeviceID],
		later:   tokens[page.Entries[5].DeviceID],
	}
}

// on returns a fixture that reports failures to t, for use in subtests.
func (f deviceFixture) on(t *testing.T) deviceFixture {
	t.Helper()
	f.harness = f.harness.on(t)
	return f
}

func (f deviceFixture) poll(token string) apiv1.PollResponse {
	f.t.Helper()
	status, raw := f.doWithAuth("Bearer "+token, http.MethodPost, "/v1/device/poll", nil)
	if status != http.StatusOK {
		f.t.Fatalf("poll: status = %d, body: %s", status, raw)
	}
	var out apiv1.PollResponse
	decode(f.t, raw, &out)
	return out
}

func (f deviceFixture) report(token string, req apiv1.ReportRequest) (int, []byte) {
	f.t.Helper()
	return f.doWithAuth("Bearer "+token, http.MethodPost, "/v1/device/reports", req)
}

func reportFor(a *apiv1.DeviceAssignment, sequence int64, state rollout.AssignmentState) apiv1.ReportRequest {
	return apiv1.ReportRequest{
		Sequence:     sequence,
		RolloutID:    a.RolloutID,
		AssignmentID: a.ID,
		Generation:   a.Generation,
		State:        state,
	}
}

func TestDeviceRoutesRequireADeviceToken(t *testing.T) {
	f := newDeviceFixture(t)
	for name, authorization := range map[string]string{
		"missing":     "",
		"unknown":     "Bearer frd_notarealtoken",
		"admin token": "Bearer " + testAdminToken,
		"empty":       "Bearer ",
		"wrong type":  "Basic " + f.first,
	} {
		t.Run(name, func(t *testing.T) {
			status, raw := f.on(t).doWithAuth(authorization, http.MethodPost, "/v1/device/poll", nil)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, body: %s", status, raw)
			}
		})
	}
}

func TestDecommissioningADeviceRevokesItsToken(t *testing.T) {
	h := newHarness(t)
	h.createFleet("lab-a")
	devices := h.registerDevices("lab-a", 2)
	poll := func(token string) int {
		status, _ := h.doWithAuth("Bearer "+token, http.MethodPost, "/v1/device/poll", nil)
		return status
	}
	if got := poll(devices[0].Token); got != http.StatusOK {
		t.Fatalf("before: status = %d", got)
	}
	h.expect(http.StatusOK, http.MethodPatch, "/v1/devices/"+devices[0].ID, `{"status":"decommissioned"}`, nil)
	if got := poll(devices[0].Token); got != http.StatusUnauthorized {
		t.Fatalf("after: status = %d, want its token revoked", got)
	}
	if got := poll(devices[1].Token); got != http.StatusOK {
		t.Fatalf("another device: status = %d", got)
	}
}

func TestPollReturnsAnAssignmentWithItsArtifactAndALease(t *testing.T) {
	f := newDeviceFixture(t)
	got := f.poll(f.first)
	if got.PollAfterSeconds <= 0 {
		t.Fatalf("poll_after_seconds = %d", got.PollAfterSeconds)
	}
	a := got.Assignment
	if a == nil {
		t.Fatal("no assignment for a device in the first stage")
	}
	if a.RolloutID != f.rollout.ID || a.Directive != rollout.DirectiveProceed || a.State != rollout.AssignmentAssigned || a.Stage != 1 {
		t.Fatalf("assignment = %+v", a)
	}
	if a.Artifact.ID != f.rollout.ArtifactID || len(a.Artifact.SHA256) != 64 {
		t.Fatalf("artifact = %+v", a.Artifact)
	}
	if got := f.poll(f.later); got.Assignment != nil {
		t.Fatalf("a later-stage device got %+v", got.Assignment)
	}
}

func TestReportLifecycleAndStatusCounts(t *testing.T) {
	f := newDeviceFixture(t)
	a := f.poll(f.first).Assignment

	for i, state := range []rollout.AssignmentState{rollout.AssignmentDownloading, rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded} {
		status, raw := f.report(f.first, reportFor(a, int64(i+1), state))
		if status != http.StatusOK {
			t.Fatalf("report %s: status = %d, body: %s", state, status, raw)
		}
		var result apiv1.ReportResponse
		decode(t, raw, &result)
		if !result.Applied {
			t.Fatalf("report %s = %+v", state, result)
		}
	}

	var r apiv1.Rollout
	f.expect(http.StatusOK, http.MethodGet, "/v1/rollouts/"+f.rollout.ID, nil, &r)
	if r.Stages[0].Succeeded != 1 || r.Stages[0].Pending != 1 {
		t.Fatalf("first stage = %+v", r.Stages[0])
	}
	if !strings.Contains(r.Waiting, "coverage") {
		t.Fatalf("waiting = %q", r.Waiting)
	}
	if got := f.poll(f.first); got.Assignment != nil {
		t.Fatalf("a finished device got %+v", got.Assignment)
	}
}

func TestDuplicateAndConflictingReports(t *testing.T) {
	f := newDeviceFixture(t)
	a := f.poll(f.first).Assignment
	req := reportFor(a, 1, rollout.AssignmentFailed)

	if status, raw := f.report(f.first, req); status != http.StatusOK {
		t.Fatalf("first: status = %d, body: %s", status, raw)
	}

	status, raw := f.report(f.first, req)
	if status != http.StatusOK {
		t.Fatalf("repeat: status = %d, body: %s", status, raw)
	}
	var repeat apiv1.ReportResponse
	decode(t, raw, &repeat)
	// A device that lost the first response learns from the repeat that it was applied.
	if !repeat.Duplicate || !repeat.Applied {
		t.Fatalf("repeat = %+v", repeat)
	}

	var r apiv1.Rollout
	f.expect(http.StatusOK, http.MethodGet, "/v1/rollouts/"+f.rollout.ID, nil, &r)
	if r.Stages[0].Failed != 1 {
		t.Fatalf("failed = %d, want 1 after a repeated report", r.Stages[0].Failed)
	}

	different := req
	different.Reason = "something else"
	status, raw = f.report(f.first, different)
	if status != http.StatusConflict {
		t.Fatalf("conflict: status = %d, body: %s", status, raw)
	}
	var problem apiv1.Problem
	decode(t, raw, &problem)
	if problem.Code != apiv1.CodeReportConflict {
		t.Fatalf("code = %q", problem.Code)
	}
}

func TestReportsAreBoundToTheAuthenticatedDevice(t *testing.T) {
	f := newDeviceFixture(t)
	a := f.poll(f.first).Assignment
	f.poll(f.second)

	// The second device can't touch the first device's assignment.
	status, raw := f.report(f.second, reportFor(a, 1, rollout.AssignmentFailed))
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, body: %s", status, raw)
	}

	wrong := reportFor(a, 1, rollout.AssignmentDownloading)
	wrong.Generation = 2
	status, raw = f.report(f.first, wrong)
	if status != http.StatusConflict {
		t.Fatalf("wrong generation: status = %d, body: %s", status, raw)
	}
	var problem apiv1.Problem
	decode(t, raw, &problem)
	if problem.Code != apiv1.CodeReportMismatch {
		t.Fatalf("code = %q", problem.Code)
	}
}

func TestReportValidation(t *testing.T) {
	f := newDeviceFixture(t)
	a := f.poll(f.first).Assignment
	cases := map[string]func(*apiv1.ReportRequest){
		"zero sequence":      func(r *apiv1.ReportRequest) { r.Sequence = 0 },
		"negative sequence":  func(r *apiv1.ReportRequest) { r.Sequence = -1 },
		"zero generation":    func(r *apiv1.ReportRequest) { r.Generation = 0 },
		"bad rollout":        func(r *apiv1.ReportRequest) { r.RolloutID = "nope" },
		"bad assignment":     func(r *apiv1.ReportRequest) { r.AssignmentID = "nope" },
		"unreportable state": func(r *apiv1.ReportRequest) { r.State = rollout.AssignmentSuperseded },
		"assigned state":     func(r *apiv1.ReportRequest) { r.State = rollout.AssignmentAssigned },
		"unknown state":      func(r *apiv1.ReportRequest) { r.State = "exploded" },
		"long reason":        func(r *apiv1.ReportRequest) { r.Reason = strings.Repeat("x", 1025) },
		"NUL in reason":      func(r *apiv1.ReportRequest) { r.Reason = "a\x00b" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			req := reportFor(a, 1, rollout.AssignmentDownloading)
			edit(&req)
			status, raw := f.on(t).report(f.first, req)
			if status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body: %s", status, raw)
			}
		})
	}

	status, _ := f.doWithAuth("Bearer "+f.first, http.MethodPost, "/v1/device/reports", `{"sequence":`)
	if status != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status = %d", status)
	}
	oversized := `{"reason":"` + strings.Repeat("x", 128<<10) + `"}`
	status, _ = f.doWithAuth("Bearer "+f.first, http.MethodPost, "/v1/device/reports", oversized)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized report: status = %d", status)
	}
}

func TestStatusCountsDevicesInFlight(t *testing.T) {
	f := newDeviceFixture(t)
	a := f.poll(f.first).Assignment
	if status, raw := f.report(f.first, reportFor(a, 1, rollout.AssignmentInstalling)); status != http.StatusOK {
		t.Fatalf("report: status = %d, body: %s", status, raw)
	}
	var r apiv1.Rollout
	f.expect(http.StatusOK, http.MethodGet, "/v1/rollouts/"+f.rollout.ID, nil, &r)
	if r.Stages[0].InFlight != 1 {
		t.Fatalf("in flight = %d", r.Stages[0].InFlight)
	}
}

// A fleet reconnecting all at once must be turned away quickly, rather than queuing without a
// limit. Device requests share a bounded number of slots, and the rest get told when to retry.
func TestDeviceRequestsBeyondTheLimitAreShedWithRetryAfter(t *testing.T) {
	h := newHarnessWith(t, func(o *api.Options) { o.MaxDeviceRequests = 1 })
	h.createFleet("lab-a")
	devices := h.registerDevices("lab-a", 2)

	// One device holds the only slot, with a request whose body never finishes arriving. It's
	// written by hand, because an HTTP client buffers a streamed request and wouldn't send it.
	var conn net.Conn
	hold := func() {
		t.Helper()
		var err error
		conn, err = (&net.Dialer{}).DialContext(t.Context(), "tcp", strings.TrimPrefix(h.base, "http://"))
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if _, err := fmt.Fprintf(conn, "POST /v1/device/reports HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer %s\r\n"+
			"Content-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"sequence\":", devices[0].Token); err != nil {
			t.Fatalf("write held request: %v", err)
		}
	}
	// held reports whether the held request still has the slot. If a poll got there first,
	// the held request was the one turned away, and the server has answered it.
	held := func() bool {
		_ = conn.SetReadDeadline(time.Now().Add(time.Millisecond))
		n, _ := conn.Read(make([]byte, 1))
		return n == 0
	}
	hold()
	// This runs before the server's own cleanup, so a failure here can't leave the server
	// waiting forever on the held request.
	t.Cleanup(func() { _ = conn.Close() })
	// A broken limiter would make a poll wait for the slot, so a timeout turns that into a failure.
	poller := &http.Client{Timeout: 2 * time.Second}

	var shed http.Header
	deadline := time.Now().Add(5 * time.Second)
	for shed == nil && time.Now().Before(deadline) {
		if !held() {
			_ = conn.Close()
			hold()
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.base+"/v1/device/poll", http.NoBody)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+devices[1].Token)
		resp, err := poller.Do(req)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable {
			if !strings.Contains(string(raw), apiv1.CodeOverloaded) {
				t.Fatalf("body = %s", raw)
			}
			shed = resp.Header
		}
		time.Sleep(5 * time.Millisecond)
	}
	if shed == nil {
		t.Fatal("the second device was never turned away")
	}
	seconds, err := strconv.Atoi(shed.Get("Retry-After"))
	if err != nil || seconds < 1 || seconds > 30 {
		t.Fatalf("Retry-After = %q", shed.Get("Retry-After"))
	}

	// Once the slot is free, the other device gets through.
	_ = conn.Close()
	deadline = time.Now().Add(5 * time.Second)
	for {
		status, _ := h.doWithAuth("Bearer "+devices[1].Token, http.MethodPost, "/v1/device/poll", nil)
		if status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the slot freed up: status = %d", status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Admin requests don't compete with devices for slots.
	h.expect(http.StatusOK, http.MethodGet, "/v1/fleets/lab-a", nil, nil)
}

// gatedStore holds every Open until release is closed, so a download can be kept in progress.
type gatedStore struct {
	artifactstore.Store
	entered chan struct{}
	release chan struct{}
}

func (g *gatedStore) Open(ctx context.Context, id string) (artifactstore.Object, error) {
	g.entered <- struct{}{}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.Store.Open(ctx, id)
}

func TestDownloadsDontTakeTheSlotsPollsAndReportsNeed(t *testing.T) {
	gate := &gatedStore{entered: make(chan struct{}, 4), release: make(chan struct{})}
	f := newDeviceFixtureWith(t, func(o *api.Options) {
		o.MaxDeviceRequests = 1
		o.MaxDeviceDownloads = 1
		gate.Store = o.Artifacts
		o.Artifacts = gate
	})
	a := f.poll(f.first).Assignment
	content := "/v1/device/artifacts/" + a.Artifact.ID + "/content"

	downloaded := make(chan int, 1)
	go func() {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.base+content, http.NoBody)
		if err != nil {
			downloaded <- 0
			return
		}
		req.Header.Set("Authorization", "Bearer "+f.first)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			downloaded <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		downloaded <- resp.StatusCode
	}()
	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the download never started")
	}

	// A download in progress leaves room for a device to poll, and to report.
	f.poll(f.second)
	if status, raw := f.report(f.first, reportFor(a, 1, rollout.AssignmentDownloading)); status != http.StatusOK {
		t.Fatalf("report during a download: status = %d, body: %s", status, raw)
	}
	// Another download waits for a download slot.
	got, raw := f.deviceGet(f.first, content, "", "")
	if got.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), apiv1.CodeOverloaded) {
		t.Fatalf("second download: status = %d, body: %s", got.StatusCode, raw)
	}

	close(gate.release)
	if status := <-downloaded; status != http.StatusOK {
		t.Fatalf("first download: status = %d", status)
	}
}
