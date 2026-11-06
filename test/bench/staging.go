package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/sim"
)

type stagingResult struct {
	BadShare float64       `json:"bad_share"`
	Staged   bool          `json:"staged"`
	State    rollout.State `json:"state"`
	Halt     string        `json:"halt,omitempty"`
	// Installed counts devices that installed the bad release, and Failed those it failed on.
	Installed int           `json:"installed"`
	Failed    int           `json:"failed"`
	Devices   int           `json:"devices"`
	Elapsed   time.Duration `json:"simulated_time"`
}

// runStaging measures how many devices a bad release reaches before the rollout halts, with
// staged exposure and with every device at once. Both arms use the same seed, fleet, and
// safety limits, so staging is the only difference.
func runStaging(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("bench staging", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := databaseFlag(flags)
	devices := flags.Int("devices", 1000, "simulated devices")
	shares := flags.String("bad-shares", "1,0.3", "share of devices the release fails on, comma-separated")
	minObservations := flags.Int("min-observations", 10, "outcomes a stage needs before its failure rate counts; the first stage needs at least this many devices")
	seed := flags.Int64("seed", 1, "seed for every fault decision")
	out := flags.String("json", "", "also write the results to this file as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	serverURL, err := databaseServer(*server, getenv)
	if err != nil {
		return err
	}
	badShares, err := parseFloats(*shares)
	if err != nil {
		return err
	}

	m := describeMachine()
	var results []stagingResult
	for _, share := range badShares {
		for _, staged := range []bool{true, false} {
			report, err := deterministic(ctx, serverURL, stagingScenario(*seed, *devices, *minObservations, share, staged))
			if err != nil {
				return err
			}
			r := stagingResult{
				BadShare: share, Staged: staged, State: report.State, Devices: *devices, Elapsed: report.Elapsed,
				Installed: report.Updated,
			}
			for _, count := range report.Failures {
				r.Installed += count
			}
			r.Failed = report.Failures["unhealthy"]
			if report.HaltReason != "" {
				r.Halt = report.HaltReason + ": " + report.HaltDetail
			}
			results = append(results, r)
			_, _ = fmt.Fprintf(stderr, "bad share %g, staged %v: %s\n", share, staged, report.State)
		}
	}
	m.LoadTo = loadAverage()

	_, _ = fmt.Fprintf(stdout, "Bad releases: %d devices, halting above 5%% failures once a stage has %d outcomes, "+
		"polling every minute, the engine every 5 seconds, seed %d.\n", *devices, *minObservations, *seed)
	printMachine(stdout, m)
	_, _ = fmt.Fprintln(stdout)
	rows := make([][]string, 0, len(results))
	for _, r := range results {
		staging := "every device at once"
		if r.Staged {
			staging = "1%, 10%, 50%, 100%"
		}
		rows = append(rows, []string{
			percent(r.BadShare, 1), staging, string(r.State),
			strconv.Itoa(r.Installed), percent(float64(r.Installed), float64(r.Devices)), strconv.Itoa(r.Failed),
		})
	}
	table(stdout, []string{"Release fails on", "Stages", "Outcome", "Devices that installed it", "Share of fleet", "Devices it failed on"}, rows)
	return writeJSON(*out, struct {
		Machine machine         `json:"machine"`
		Results []stagingResult `json:"results"`
	}{m, results})
}

// stagingScenario rolls a release that fails on share of devices out to a clean fleet, staged
// or to everyone in one stage, under the same safety limits.
func stagingScenario(seed int64, devices, minObservations int, share float64, staged bool) sim.Scenario {
	stage := func(percent int) rollout.StageSpec {
		return rollout.StageSpec{
			Percent: percent, Soak: durationPtr(10 * time.Minute), Deadline: durationPtr(4 * time.Hour), MinObservations: &minObservations,
		}
	}
	stages := []rollout.StageSpec{stage(100)}
	if staged {
		stages = []rollout.StageSpec{stage(1), stage(10), stage(50), stage(100)}
	}
	return sim.Scenario{
		Seed: seed, Fleet: "staging", Devices: devices,
		Policy: rollout.PolicySpec{
			Stages: stages,
			Safety: rollout.SafetySpec{MaxFailureBPS: new(500), StaleAfter: durationPtr(30 * time.Minute)},
		},
		Faults:         sim.Faults{Unhealthy: share},
		PollInterval:   time.Minute,
		EngineInterval: 5 * time.Second,
		MaxDuration:    24 * time.Hour,
	}
}
