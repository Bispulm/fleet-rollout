package sim_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
	"github.com/Bispulm/fleet-rollout/internal/engine"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing/signingtest"
	"github.com/Bispulm/fleet-rollout/internal/sim"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

const liveAdminToken = "live-simulation-admin-token-0123456789"

func TestALiveRunOverRealHTTPCompletesAndCleansUp(t *testing.T) {
	pool := testdb.New(t)
	signer := signingtest.NewKey(t)
	trust := signingtest.Trust(t, signer)
	logger := slog.New(slog.DiscardHandler)
	st := store.New(pool)
	server := httptest.NewServer(api.New(api.Options{
		Logger:            logger,
		Ready:             func(context.Context) error { return nil },
		Store:             st,
		AdminToken:        liveAdminToken,
		PollInterval:      time.Second,
		Trust:             trust,
		Artifacts:         artifactstore.NewMemory(),
		MaxArtifactBytes:  1 << 20,
		MaxDeviceRequests: 256,
	}))
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
		Scenario: sc, APIURL: server.URL, AdminToken: liveAdminToken, Signer: signer, Trust: trust,
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
