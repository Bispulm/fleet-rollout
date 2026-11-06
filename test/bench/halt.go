package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/db"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/sim"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

type haltResult struct {
	Trial int `json:"trial"`
	// TippedAt is when rolloutd applied the report that put the rollout over its limit, and
	// HaltedAt when the engine halted it.
	TippedAt time.Time     `json:"tipped_at"`
	HaltedAt time.Time     `json:"halted_at"`
	Latency  time.Duration `json:"latency"`
	Reason   string        `json:"reason"`
	// Installed counts devices that installed the release, all of which it failed on.
	Installed int `json:"installed"`
}

// runHalt measures how long a breach takes to halt a rollout, against real rolloutd over real
// HTTP: from the moment rolloutd applies the report that crosses the limit, to the moment the
// engine halts the rollout. Both times are rolloutd's own, read from Postgres afterward.
func runHalt(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("bench halt", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := databaseFlag(flags)
	binary := flags.String("rolloutd", "bin/rolloutd", "rolloutd binary to run")
	trials := flags.Int("trials", 20, "rollouts to halt")
	devices := flags.Int("devices", 200, "simulated devices in each")
	engineInterval := flags.Duration("engine-interval", 5*time.Second, "ROLLOUTD_ENGINE_INTERVAL")
	pollInterval := flags.Duration("poll-interval", 2*time.Second, "ROLLOUTD_POLL_INTERVAL")
	seed := flags.Int64("seed", 1, "seed of the first trial; each trial adds one")
	out := flags.String("json", "", "also write the results to this file as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	serverURL, err := databaseServer(*server, getenv)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "fleet-rollout-bench-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	m := describeMachine()
	env := map[string]string{
		"ROLLOUTD_ENGINE_INTERVAL": engineInterval.String(),
		"ROLLOUTD_POLL_INTERVAL":   pollInterval.String(),
	}
	var results []haltResult
	for trial := range *trials {
		r, err := haltTrial(ctx, *binary, serverURL, dir, env, *seed+int64(trial), *devices, *engineInterval)
		if err != nil {
			return fmt.Errorf("trial %d: %w", trial+1, err)
		}
		r.Trial = trial + 1
		results = append(results, r)
		_, _ = fmt.Fprintf(stderr, "trial %d: halted %s after the breach (%s)\n", r.Trial, r.Latency.Round(time.Millisecond), r.Reason)
	}
	m.LoadTo = loadAverage()

	latencies := make([]time.Duration, len(results))
	installed := make([]time.Duration, len(results))
	for i, r := range results {
		latencies[i] = r.Latency
		installed[i] = time.Duration(r.Installed)
	}
	latencies, installed = sortedCopy(latencies), sortedCopy(installed)
	_, _ = fmt.Fprintf(stdout, "Time from breach to halt: %d rollouts of %d devices each, a release that fails on every "+
		"device, the engine every %s, devices polling every %s.\n", len(results), *devices, *engineInterval, *pollInterval)
	printMachine(stdout, m)
	_, _ = fmt.Fprintln(stdout)
	table(stdout, []string{"Measure", "Min", "p50", "p95", "Max"}, [][]string{
		{"Breach to halt (s)", formatSeconds(quantile(latencies, 0)), formatSeconds(quantile(latencies, 0.5)),
			formatSeconds(quantile(latencies, 0.95)), formatSeconds(quantile(latencies, 1))},
		{"Devices that installed it", strconv.Itoa(int(quantile(installed, 0))), strconv.Itoa(int(quantile(installed, 0.5))),
			strconv.Itoa(int(quantile(installed, 0.95))), strconv.Itoa(int(quantile(installed, 1)))},
	})
	return writeJSON(*out, struct {
		Machine machine      `json:"machine"`
		Results []haltResult `json:"results"`
	}{m, results})
}

func haltTrial(ctx context.Context, binary, databaseServer, dir string, env map[string]string, seed int64, devices int,
	engineInterval time.Duration,
) (haltResult, error) {
	srv, err := startServer(ctx, binary, databaseServer, dir, env)
	if err != nil {
		return haltResult{}, err
	}
	defer func() { _ = srv.cleanUp() }()
	// The engine ticks on a schedule that starts with rolloutd, and setting a rollout up takes
	// about the same time every trial, so without this every breach would land at the same
	// point between ticks. Waiting a random part of an interval spreads them over it.
	rng := rand.New(rand.NewPCG(uint64(seed), 0x4a17)) //nolint:gosec // Spreading start times, not security.
	select {
	case <-ctx.Done():
		return haltResult{}, ctx.Err()
	case <-time.After(time.Duration(rng.Int64N(int64(engineInterval)))):
	}
	stage := func(percent int) rollout.StageSpec {
		return rollout.StageSpec{Percent: percent, Soak: durationPtr(time.Minute), Deadline: durationPtr(time.Hour), MinObservations: new(5)}
	}
	report, err := sim.RunLive(ctx, sim.LiveConfig{
		Scenario: sim.Scenario{
			Seed: seed, Fleet: "halt", Devices: devices,
			Policy: rollout.PolicySpec{
				Stages: []rollout.StageSpec{stage(10), stage(100)},
				Safety: rollout.SafetySpec{MaxFailureBPS: new(1000)},
			},
			Faults:      sim.Faults{Unhealthy: 1},
			MaxDuration: 3 * time.Minute,
		},
		APIURL: srv.URL, AdminToken: srv.Token, Signer: srv.Signer, Trust: srv.Trust,
	})
	if _, stopErr := srv.stop(); err == nil {
		err = stopErr
	}
	if err != nil {
		return haltResult{}, err
	}
	if report.State != rollout.StateHalted {
		return haltResult{}, fmt.Errorf("the rollout ended %s, not halted", report.State)
	}
	r, err := measureHalt(ctx, srv.DBURL, report.RolloutID)
	if err != nil {
		return haltResult{}, err
	}
	for _, count := range report.Failures {
		r.Installed += count
	}
	return r, nil
}

// measureHalt finds the report that crossed the rollout's limit by replaying the outcomes rolloutd
// applied, in order, through the same policy evaluation the engine uses.
func measureHalt(ctx context.Context, dbURL, rolloutID string) (haltResult, error) {
	pool, err := db.Open(ctx, dbURL, 2)
	if err != nil {
		return haltResult{}, err
	}
	defer pool.Close()
	r, err := store.New(pool).GetRollout(ctx, rolloutID)
	if err != nil {
		return haltResult{}, err
	}
	if r.CurrentStage == nil || r.HaltReason == nil {
		return haltResult{}, errors.New("the rollout has no halted stage")
	}
	result := haltResult{Reason: *r.HaltReason}
	if err := pool.QueryRow(ctx, `SELECT occurred_at FROM rollout_transitions
		WHERE rollout_id = $1 AND to_state = 'halted' ORDER BY revision LIMIT 1`, rolloutID).Scan(&result.HaltedAt); err != nil {
		return haltResult{}, fmt.Errorf("read the halt: %w", err)
	}

	stages := make([]rollout.StageOutcomes, 0, *r.CurrentStage)
	for _, stage := range r.Stages {
		if stage.Ordinal <= *r.CurrentStage {
			stages = append(stages, rollout.StageOutcomes{Ordinal: stage.Ordinal, Outcomes: rollout.Outcomes{Cohort: memberCount(stage)}})
		}
	}
	rows, err := pool.Query(ctx, `SELECT stage_ordinal, state, finished_at FROM assignments
		WHERE rollout_id = $1 AND kind = 'forward' AND state IN ('succeeded', 'failed') AND finished_at <= $2
		ORDER BY finished_at, id`, rolloutID, result.HaltedAt)
	if err != nil {
		return haltResult{}, err
	}
	var ordinal int
	var state string
	var finished time.Time
	_, err = pgx.ForEachRow(rows, []any{&ordinal, &state, &finished}, func() error {
		if !result.TippedAt.IsZero() {
			return nil
		}
		for i := range stages {
			if stages[i].Ordinal != ordinal {
				continue
			}
			if state == "failed" {
				stages[i].Outcomes.Failed++
			} else {
				stages[i].Outcomes.Succeeded++
			}
		}
		verdict := r.Policy.Evaluate(rollout.Evaluation{
			State: rollout.StateRunning, CurrentStage: *r.CurrentStage, Stages: stages,
			DeadlineAt: finished.Add(time.Hour), Now: finished,
		})
		if verdict.Action == rollout.ActionHalt {
			result.TippedAt = finished
		}
		return nil
	})
	if err != nil {
		return haltResult{}, err
	}
	if result.TippedAt.IsZero() {
		return haltResult{}, errors.New("no report crossed the limit before the halt")
	}
	result.Latency = result.HaltedAt.Sub(result.TippedAt)
	return result, nil
}

// memberCount is a stage's cohort, which is every member however its assignment ended.
func memberCount(stage store.Stage) int {
	return stage.Succeeded + stage.Failed + stage.Superseded + stage.Unknown + stage.Pending
}
