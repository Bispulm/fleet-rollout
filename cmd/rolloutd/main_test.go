package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

// syncWriter lets the server's log output be collected without a data race.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func testEnv(dbURL string) func(string) string {
	vals := map[string]string{
		"ROLLOUTD_DATABASE_URL": dbURL,
		"ROLLOUTD_ADMIN_TOKEN":  strings.Repeat("x", 32),
		"ROLLOUTD_LISTEN_ADDR":  "127.0.0.1:0",
	}
	return func(k string) string { return vals[k] }
}

func httpStatus(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func TestServeReportsUnreadyUntilMigratedThenShutsDownCleanly(t *testing.T) {
	dbURL := testdb.NewURL(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	listening := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() {
		done <- run(ctx, []string{"serve"}, env{
			getenv:      testEnv(dbURL),
			stdout:      io.Discard,
			stderr:      &syncWriter{w: &logs},
			onListening: func(addr net.Addr) { listening <- addr },
		})
	}()

	var base string
	select {
	case addr := <-listening:
		base = "http://" + addr.String()
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start listening")
	}

	if got := httpStatus(t, base+"/healthz"); got != http.StatusOK {
		t.Errorf("healthz = %d, want 200", got)
	}
	if got := httpStatus(t, base+"/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("readyz before migrating = %d, want 503", got)
	}

	migrateErr := run(ctx, []string{"migrate"}, env{getenv: testEnv(dbURL), stdout: io.Discard, stderr: io.Discard})
	if migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	if got := httpStatus(t, base+"/readyz"); got != http.StatusOK {
		t.Errorf("readyz after migrating = %d, want 200", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after shutdown, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestRunRejectsBadInvocations(t *testing.T) {
	cases := map[string][]string{
		"no command":      nil,
		"unknown command": {"explode"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			err := run(t.Context(), args, env{getenv: func(string) string { return "" }, stdout: io.Discard, stderr: &stderr})
			if err == nil {
				t.Fatal("run succeeded, want an error")
			}
			if !strings.Contains(err.Error(), "serve") {
				t.Fatalf("error %q should list the available commands", err)
			}
		})
	}
}

func TestRunExplainsMissingConfiguration(t *testing.T) {
	err := run(t.Context(), []string{"serve"}, env{getenv: func(string) string { return "" }, stdout: io.Discard, stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "ROLLOUTD_DATABASE_URL") {
		t.Fatalf("run error = %v, want it to name the missing setting", err)
	}
}

// serveForTest runs the real server command, with fast timers, on a migrated database.
func serveForTest(t *testing.T) (*client.Client, string, func() error) {
	t.Helper()
	dbURL := testdb.NewURL(t)
	const adminToken = "serve-test-admin-credential"
	vars := map[string]string{
		"ROLLOUTD_DATABASE_URL":     dbURL,
		"ROLLOUTD_ADMIN_TOKEN":      adminToken,
		"ROLLOUTD_LISTEN_ADDR":      "127.0.0.1:0",
		"ROLLOUTD_ENGINE_INTERVAL":  "20ms",
		"ROLLOUTD_ENGINE_LEASE_TTL": "2s",
	}
	getenv := func(k string) string { return vars[k] }

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	if err := run(ctx, []string{"migrate"}, env{getenv: getenv, stdout: io.Discard, stderr: io.Discard}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	listening := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"serve"}, env{
			getenv: getenv, stdout: io.Discard, stderr: io.Discard,
			onListening: func(addr net.Addr) { listening <- addr },
		})
	}()
	var base string
	select {
	case addr := <-listening:
		base = "http://" + addr.String()
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the server didn't start listening")
	}
	stop := func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(20 * time.Second):
			return errors.New("the server didn't shut down")
		}
	}
	return client.New(base, adminToken, nil), base, stop
}

func setUpRollout(t *testing.T, ctx context.Context, admin *client.Client, policy rollout.PolicySpec, devices int) (apiv1.Rollout, []apiv1.Device) {
	t.Helper()
	if _, err := admin.CreateFleet(ctx, apiv1.CreateFleetRequest{Name: "lab-a"}); err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	specs := make([]apiv1.DeviceSpec, devices)
	for i := range specs {
		specs[i] = apiv1.DeviceSpec{ID: fmt.Sprintf("sim-%05d", i), Platform: "linux/arm64"}
	}
	registered, err := admin.RegisterDevices(ctx, "lab-a", specs)
	if err != nil {
		t.Fatalf("RegisterDevices: %v", err)
	}
	artifact, err := admin.CreateArtifact(ctx, apiv1.CreateArtifactRequest{
		Name: "demo-app", Version: "1.4.0", Platform: "linux/arm64", ReleaseCounter: 14, SizeBytes: 1,
		SHA256: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	created, err := admin.CreateRollout(ctx, "lab-a", apiv1.CreateRolloutRequest{Name: "demo", ArtifactID: artifact.ID, Policy: policy})
	if err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	started, err := admin.Transition(ctx, created.ID, client.ActionStart, apiv1.TransitionRequest{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	return started, registered
}

// runDevices polls every device until the rollout finishes, reporting each assignment
// with the outcome that outcome chooses. It returns the final status.
func runDevices(t *testing.T, ctx context.Context, admin *client.Client, base string, r apiv1.Rollout, devices []apiv1.Device, outcome func(apiv1.DeviceAssignment) rollout.AssignmentState) apiv1.Rollout {
	t.Helper()
	sequences := make(map[string]int64, len(devices))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, err := admin.GetRollout(ctx, r.ID)
		if err != nil {
			t.Fatalf("GetRollout: %v", err)
		}
		if status.State == rollout.StateCompleted || status.State == rollout.StateHalted {
			return status
		}
		for _, d := range devices {
			device := client.NewDevice(base, d.Token, nil)
			polled, err := device.Poll(ctx)
			if err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if polled.Assignment == nil {
				continue
			}
			a := *polled.Assignment
			states := []rollout.AssignmentState{rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded}
			if outcome(a) == rollout.AssignmentFailed {
				states = []rollout.AssignmentState{rollout.AssignmentFailed}
			}
			for _, state := range states {
				sequences[d.ID]++
				if _, err := device.Report(ctx, apiv1.ReportRequest{
					Sequence: sequences[d.ID], RolloutID: a.RolloutID, AssignmentID: a.ID,
					Generation: a.Generation, State: state,
				}); err != nil {
					t.Fatalf("Report: %v", err)
				}
			}
		}
	}
	t.Fatal("the rollout never finished")
	return apiv1.Rollout{}
}

func intPtr(v int) *int { return &v }

func TestServeRunsARolloutToCompletionOnTheRealClock(t *testing.T) {
	admin, base, stop := serveForTest(t)
	ctx := t.Context()
	noSoak := rollout.Duration(0)
	policy := rollout.PolicySpec{
		Stages: []rollout.StageSpec{{Percent: 10, Soak: &noSoak}, {Percent: 50, Soak: &noSoak}, {Percent: 100, Soak: &noSoak}},
		Safety: rollout.SafetySpec{MaxFailureBPS: intPtr(200), RequiredCoverageBPS: intPtr(rollout.FullBPS)},
	}
	r, devices := setUpRollout(t, ctx, admin, policy, 20)

	final := runDevices(t, ctx, admin, base, r, devices, func(apiv1.DeviceAssignment) rollout.AssignmentState {
		return rollout.AssignmentSucceeded
	})
	if final.State != rollout.StateCompleted {
		t.Fatalf("state = %s, halt = %s", final.State, final.HaltDetail)
	}
	succeeded := 0
	for _, stage := range final.Stages {
		succeeded += stage.Succeeded
		if stage.State != "passed" {
			t.Errorf("stage %d = %s", stage.Ordinal, stage.State)
		}
	}
	if succeeded != 20 {
		t.Fatalf("succeeded = %d, want all 20", succeeded)
	}
	if err := stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestServeHaltsABadReleaseInItsFirstStage(t *testing.T) {
	admin, base, stop := serveForTest(t)
	ctx := t.Context()
	noSoak := rollout.Duration(0)
	policy := rollout.PolicySpec{
		Stages: []rollout.StageSpec{{Percent: 10, Soak: &noSoak}, {Percent: 100, Soak: &noSoak}},
		Safety: rollout.SafetySpec{MaxFailureBPS: intPtr(200)},
	}
	r, devices := setUpRollout(t, ctx, admin, policy, 100)

	final := runDevices(t, ctx, admin, base, r, devices, func(apiv1.DeviceAssignment) rollout.AssignmentState {
		return rollout.AssignmentFailed
	})
	if final.State != rollout.StateHalted {
		t.Fatalf("state = %s", final.State)
	}
	if final.Stages[1].Succeeded+final.Stages[1].Failed != 0 {
		t.Fatalf("the second stage saw exposure: %+v", final.Stages[1])
	}
	if exposed := final.Stages[0].Failed; exposed > 10 {
		t.Fatalf("%d devices failed, more than the 10 in the first stage", exposed)
	}
	if final.HaltReason == "" || final.HaltDetail == "" {
		t.Fatalf("no explanation for the halt: %+v", final)
	}
	if err := stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestServeCanRunWithoutTheEngine(t *testing.T) {
	dbURL := testdb.NewURL(t)
	vars := map[string]string{
		"ROLLOUTD_DATABASE_URL":   dbURL,
		"ROLLOUTD_ADMIN_TOKEN":    strings.Repeat("x", 32),
		"ROLLOUTD_LISTEN_ADDR":    "127.0.0.1:0",
		"ROLLOUTD_ENGINE_ENABLED": "false",
	}
	getenv := func(k string) string { return vars[k] }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := run(ctx, []string{"migrate"}, env{getenv: getenv, stdout: io.Discard, stderr: io.Discard}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	listening := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() {
		done <- run(ctx, []string{"serve"}, env{
			getenv: getenv, stdout: io.Discard, stderr: &syncWriter{w: &logs},
			onListening: func(addr net.Addr) { listening <- addr },
		})
	}()
	<-listening
	// Long enough for a leader to appear, if an engine were running.
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if strings.Contains(logs.String(), "became engine leader") {
		t.Fatalf("an engine ran while disabled:\n%s", logs.String())
	}
}
