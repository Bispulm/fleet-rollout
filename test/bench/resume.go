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

type resumeResult struct {
	BreakRate float64 `json:"break_rate"`
	Breaks    int     `json:"breaks"`
	// Bytes per device that installed the release, resuming and starting over.
	ResumedPerDevice   float64 `json:"resumed_bytes_per_device"`
	RestartedPerDevice float64 `json:"restarted_bytes_per_device"`
	ArtifactBytes      int64   `json:"artifact_bytes"`
}

// runResume measures the bytes devices download to install one artifact over a network that
// breaks responses partway through, resuming from the last verified chunk, and starting over
// as a client without resume would. Both are the deterministic simulator, so a seed always
// gives the same numbers.
func runResume(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("bench resume", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := databaseFlag(flags)
	devices := flags.Int("devices", 100, "simulated devices")
	artifactBytes := flags.Int("artifact-bytes", 1<<20, "artifact size in bytes")
	chunkSize := flags.Int64("chunk-size", 64<<10, "chunk size in bytes")
	rates := flags.String("break-rates", "0,0.01,0.05,0.1,0.2", "share of content responses broken partway, comma-separated")
	seed := flags.Int64("seed", 1, "seed for every fault decision")
	out := flags.String("json", "", "also write the results to this file as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	serverURL, err := databaseServer(*server, getenv)
	if err != nil {
		return err
	}
	breakRates, err := parseFloats(*rates)
	if err != nil {
		return err
	}

	m := describeMachine()
	var results []resumeResult
	for _, rate := range breakRates {
		var reports [2]sim.Report
		for i, restart := range []bool{false, true} {
			sc := resumeScenario(*seed, *devices, *artifactBytes, *chunkSize, rate)
			sc.RestartDownloads = restart
			report, err := deterministic(ctx, serverURL, sc)
			if err != nil {
				return err
			}
			if report.State != rollout.StateCompleted || report.Updated != *devices || report.BadInstalls != 0 {
				return fmt.Errorf("break rate %g, restart %v: state %s, %d of %d updated, %d bad installs",
					rate, restart, report.State, report.Updated, *devices, report.BadInstalls)
			}
			reports[i] = report
		}
		results = append(results, resumeResult{
			BreakRate: rate, Breaks: reports[0].Faults["break"],
			ResumedPerDevice:   float64(reports[0].BytesDownloaded) / float64(*devices),
			RestartedPerDevice: float64(reports[1].BytesDownloaded) / float64(*devices),
			ArtifactBytes:      reports[0].ArtifactBytes,
		})
		_, _ = fmt.Fprintf(stderr, "break rate %g done\n", rate)
	}
	m.LoadTo = loadAverage()

	_, _ = fmt.Fprintf(stdout, "Resumable downloads: %d devices, a %d KiB artifact in %d KiB chunks, seed %d.\n",
		*devices, *artifactBytes>>10, *chunkSize>>10, *seed)
	printMachine(stdout, m)
	_, _ = fmt.Fprintln(stdout)
	rows := make([][]string, 0, len(results))
	for _, r := range results {
		artifact := float64(r.ArtifactBytes)
		rows = append(rows, []string{
			percent(r.BreakRate, 1), strconv.Itoa(r.Breaks),
			fmt.Sprintf("%.2f×", r.ResumedPerDevice/artifact), fmt.Sprintf("%.2f×", r.RestartedPerDevice/artifact),
			percent(r.RestartedPerDevice-r.ResumedPerDevice, r.RestartedPerDevice),
		})
	}
	table(stdout, []string{"Responses broken", "Breaks (resuming)", "Downloaded, resuming", "Downloaded, starting over", "Saved by resuming"}, rows)
	return writeJSON(*out, struct {
		Machine machine        `json:"machine"`
		Results []resumeResult `json:"results"`
	}{m, results})
}

// resumeScenario is one stage of every device, with no fault but broken downloads, so that
// the bytes downloaded depend only on how downloads recover.
func resumeScenario(seed int64, devices, artifactBytes int, chunkSize int64, breakRate float64) sim.Scenario {
	return sim.Scenario{
		Seed: seed, Fleet: "resume", Devices: devices, ArtifactBytes: artifactBytes, ChunkSize: chunkSize,
		Policy: rollout.PolicySpec{
			Stages: []rollout.StageSpec{{Percent: 100, Soak: durationPtr(time.Minute), Deadline: durationPtr(24 * time.Hour)}},
			Safety: rollout.SafetySpec{MaxFailureBPS: new(1000)},
		},
		Faults:         sim.Faults{Break: breakRate},
		EngineInterval: 30 * time.Second,
	}
}
