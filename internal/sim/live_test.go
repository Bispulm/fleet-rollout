package sim_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/engine"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/signing/signingtest"
	"github.com/Bispulm/fleet-rollout/internal/sim"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

const liveAdminToken = "live-simulation-admin-token-0123456789"

// liveServer runs a control plane over real HTTP, with its engine, until the test ends. wrap,
// if set, sits in front of the API.
func liveServer(t *testing.T, wrap func(http.Handler) http.Handler) (url string, signer ed25519.PrivateKey) {
	t.Helper()
	pool := testdb.New(t)
	signer = signingtest.NewKey(t)
	logger := slog.New(slog.DiscardHandler)
	st := store.New(pool)
	var handler http.Handler = api.New(api.Options{
		Logger:            logger,
		Ready:             func(context.Context) error { return nil },
		Store:             st,
		AdminToken:        liveAdminToken,
		PollInterval:      time.Second,
		Trust:             signingtest.Trust(t, signer),
		Artifacts:         artifactstore.NewMemory(),
		MaxArtifactBytes:  1 << 20,
		MaxDeviceRequests: 256,
	})
	if wrap != nil {
		handler = wrap(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	engineCtx, stopEngine := context.WithCancel(t.Context())
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		rolloutEngine := engine.New(engine.Options{
			Store: st, Holder: "live-test", Interval: 200 * time.Millisecond, LeaseTTL: 2 * time.Second, Logger: logger,
		})
		if err := rolloutEngine.Run(engineCtx); err != nil && engineCtx.Err() == nil {
			t.Errorf("engine stopped: %v", err)
		}
	}()
	t.Cleanup(func() { stopEngine(); <-engineDone })
	return server.URL, signer
}

func TestALiveRunOverRealHTTPCompletesAndCleansUp(t *testing.T) {
	url, signer := liveServer(t, nil)
	trust := signingtest.Trust(t, signer)

	// Let the server's own goroutines settle before counting.
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()

	sc, err := sim.Preset("flaky", 5, 40, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sc.Faults.Latency = 20 * time.Millisecond
	sc.Fleet = "live"
	sc.MaxDuration = time.Minute
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	report, err := sim.RunLive(ctx, sim.LiveConfig{
		Scenario: sc, APIURL: url, AdminToken: liveAdminToken, Signer: signer, Trust: trust,
	})
	if err != nil {
		t.Fatalf("RunLive: %v", err)
	}

	requireNoBadInstalls(t, report)
	if report.State != rollout.StateCompleted || report.TimedOut {
		t.Fatalf("state = %s (%s: %s), timed out = %v, failures %v", report.State, report.HaltReason, report.HaltDetail,
			report.TimedOut, report.Failures)
	}
	requireEveryDeviceAccountedFor(t, report, sc.Devices)

	// Every device goroutine has stopped. Connections the server is still closing may take a
	// moment, so allow a few.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before+4 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+4 {
		t.Fatalf("%d goroutines before the run, %d after", before, after)
	}
}

func TestALiveRunStopsAtItsDeadlineWhenStatusIsUnavailable(t *testing.T) {
	// Everything works except reading the rollout's status, which is what the run watches.
	url, signer := liveServer(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/rollouts/") {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	sc, err := sim.Preset("clean", 6, 5, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sc.Fleet = "blind"
	sc.MaxDuration = 2 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	started := time.Now()
	report, err := sim.RunLive(ctx, sim.LiveConfig{
		Scenario: sc, APIURL: url, AdminToken: liveAdminToken, Signer: signer, Trust: signingtest.Trust(t, signer),
	})
	if err != nil {
		t.Fatalf("RunLive: %v, after %s", err, time.Since(started))
	}
	if !report.TimedOut {
		t.Fatalf("report = %+v, want it to have given up", report)
	}
}

func TestASimulatedFleetCarriesOutARolloutStartedElsewhere(t *testing.T) {
	url, signer := liveServer(t, nil)
	admin := client.New(url, liveAdminToken, nil)
	ctx := t.Context()
	if _, err := admin.CreateFleet(ctx, apiv1.CreateFleetRequest{Name: "existing"}); err != nil {
		t.Fatal(err)
	}
	specs := make([]apiv1.DeviceSpec, 8)
	for i := range specs {
		specs[i] = apiv1.DeviceSpec{ID: fmt.Sprintf("existing-%d", i), Platform: "linux/arm64"}
	}
	registered, err := admin.RegisterDevices(ctx, "existing", specs)
	if err != nil {
		t.Fatal(err)
	}
	creds := make([]sim.Credential, len(registered))
	for i, d := range registered {
		creds[i] = sim.Credential{ID: d.ID, Token: d.Token}
	}
	content := bytes.Repeat([]byte("release one "), 1000)
	_, envelope := signingtest.Signed(t, signer, signing.Meta{
		Name: "demo-app", Version: "1.0.0", Platform: "linux/arm64", ReleaseCounter: 1, ChunkSize: 1024,
	}, content)
	artifact, err := admin.CreateArtifact(ctx, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.UploadArtifactContent(ctx, artifact.ID, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	created, err := admin.CreateRollout(ctx, "existing", apiv1.CreateRolloutRequest{
		Name: "elsewhere", ArtifactID: artifact.ID, Policy: sim.StagedPolicy(time.Second, 1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Transition(ctx, created.ID, client.ActionStart, apiv1.TransitionRequest{}); err != nil {
		t.Fatal(err)
	}

	runCtx, stop := context.WithCancel(ctx)
	type result struct {
		report sim.FleetReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := sim.RunFleet(runCtx, sim.FleetConfig{
			APIURL: url, Devices: creds, Trust: signingtest.Trust(t, signer), Platform: "linux/arm64", Seed: 1,
		})
		done <- result{report, err}
	}()
	deadline := time.Now().Add(90 * time.Second)
	for {
		got, err := admin.GetRollout(ctx, created.ID)
		if err == nil && got.State == rollout.StateCompleted {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("the rollout never completed: %+v, %v", got.State, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	stop()
	res := <-done
	if res.err != nil {
		t.Fatalf("RunFleet: %v", res.err)
	}
	if res.report.Devices != 8 || res.report.Releases[1] != 8 {
		t.Fatalf("report = %+v, want all 8 devices on release 1", res.report)
	}
}
