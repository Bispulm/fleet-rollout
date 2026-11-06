package sim_test

import (
	"context"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/sim"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

func preset(t *testing.T, name string, seed int64, devices int) sim.Scenario {
	t.Helper()
	sc, err := sim.Preset(name, seed, devices, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func run(t *testing.T, sc sim.Scenario) sim.Report {
	t.Helper()
	// Evaluating every 30 seconds is plenty for 10-minute soaks, and much cheaper than 5.
	if sc.EngineInterval == 0 {
		sc.EngineInterval = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	report, err := sim.Run(ctx, testdb.New(t), sc)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return report
}

// requireEveryDeviceAccountedFor checks that the devices running the new release are exactly
// those the server counts as succeeded, and that every member of a stage that opened ended
// some other way.
func requireEveryDeviceAccountedFor(t *testing.T, report sim.Report, devices int) {
	t.Helper()
	var succeeded, members int
	for _, stage := range report.Stages {
		members += stage.Cohort
		succeeded += stage.Succeeded
		if stage.State == "pending" {
			continue // It never opened, so its members never heard of the rollout.
		}
		if ended := stage.Succeeded + stage.Failed + stage.Superseded + stage.Pending; ended != stage.Cohort {
			t.Errorf("stage %d accounts for %d of its %d members: %+v", stage.Ordinal, ended, stage.Cohort, stage)
		}
	}
	if report.Updated != succeeded {
		t.Errorf("%d devices run the new release, but the server counts %d successes", report.Updated, succeeded)
	}
	if members != devices {
		t.Errorf("stages hold %d of %d devices: %+v", members, devices, report.Stages)
	}
}

func requireNoBadInstalls(t *testing.T, report sim.Report) {
	t.Helper()
	if report.BadInstalls != 0 {
		t.Fatalf("%d devices are running content that isn't the signed artifact", report.BadInstalls)
	}
}

func TestAFaultyFleetStillCompletes(t *testing.T) {
	t.Parallel()
	// Stages of 20 devices or more, so that one or two interrupted installs don't halt it.
	sc := preset(t, "flaky", 7, 200)

	first := run(t, sc)
	requireNoBadInstalls(t, first)
	if first.State != rollout.StateCompleted || first.TimedOut {
		t.Fatalf("state = %s (%s: %s), timed out = %v, failures %v, want completed",
			first.State, first.HaltReason, first.HaltDetail, first.TimedOut, first.Failures)
	}
	requireEveryDeviceAccountedFor(t, first, sc.Devices)
	if first.Updated < sc.Devices*95/100 {
		t.Errorf("only %d of %d devices updated", first.Updated, sc.Devices)
	}
	for _, kind := range []string{"drop", "lost_response", "corrupt", "truncate", "duplicate_report", "crash", "offline_refusal"} {
		if first.Faults[kind] == 0 {
			t.Errorf("no %s faults were injected, so the run didn't test them: %v", kind, first.Faults)
		}
	}
	if first.BytesDownloaded <= int64(sc.Devices)*first.ArtifactBytes {
		t.Errorf("devices downloaded %d bytes, which can't include any retried chunks", first.BytesDownloaded)
	}
}

func TestASeedReproducesTheWholeRun(t *testing.T) {
	t.Parallel()
	sc := preset(t, "flaky", 21, 60)
	first := run(t, sc)
	if first.State != rollout.StateCompleted {
		t.Fatalf("state = %s (%s: %s), want completed", first.State, first.HaltReason, first.HaltDetail)
	}
	if second := run(t, sc); second.Digest != first.Digest {
		t.Fatalf("the same seed gave digests %s and %s", first.Digest, second.Digest)
	}
	sc.Seed = 22
	if other := run(t, sc); other.Digest == first.Digest {
		t.Fatalf("seeds 21 and 22 gave the same digest, so it can't be summarizing the run")
	}
}

func TestABadReleaseHaltsBeforeItReachesMostOfTheFleet(t *testing.T) {
	t.Parallel()
	report := run(t, preset(t, "bad-release", 11, 200))

	requireNoBadInstalls(t, report)
	requireEveryDeviceAccountedFor(t, report, 200)
	breach := report.HaltReason == string(rollout.HaltStageBreach) || report.HaltReason == string(rollout.HaltCertainBreach)
	if report.State != rollout.StateHalted || !breach {
		t.Fatalf("state = %s, reason = %q (%s), want halted on failures", report.State, report.HaltReason, report.HaltDetail)
	}
	// The first stage is 20 devices, and it should stop there.
	if report.Updated > 20 {
		t.Errorf("%d devices kept the bad release", report.Updated)
	}
	for _, stage := range report.Stages[1:] {
		if stage.Succeeded+stage.Failed > 0 {
			t.Errorf("stage %d installed on %d devices after the first stage showed failures",
				stage.Ordinal, stage.Succeeded+stage.Failed)
		}
	}
}

func TestACleanRunUpdatesEveryDevice(t *testing.T) {
	t.Parallel()
	report := run(t, preset(t, "clean", 1, 50))
	requireNoBadInstalls(t, report)
	if report.State != rollout.StateCompleted || report.Updated != 50 || len(report.Failures) != 0 {
		t.Fatalf("state = %s, %d updated, failures %v, want every device updated", report.State, report.Updated, report.Failures)
	}
	if report.BytesDownloaded != 50*report.ArtifactBytes {
		t.Errorf("downloaded %d bytes, want exactly one copy per device", report.BytesDownloaded)
	}
}

func TestTheFleetRecoversFromEveryDeviceReconnectingAtOnce(t *testing.T) {
	t.Parallel()
	report := run(t, preset(t, "storm", 3, 200))

	requireNoBadInstalls(t, report)
	if report.State != rollout.StateCompleted {
		t.Fatalf("state = %s (%s: %s), failures %v, want completed",
			report.State, report.HaltReason, report.HaltDetail, report.Failures)
	}
	requireEveryDeviceAccountedFor(t, report, 200)
	if report.Elapsed < 50*time.Minute {
		t.Errorf("the rollout finished after %s, during the outage", report.Elapsed)
	}
}

func TestAnUnknownPresetIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := sim.Preset("nope", 1, 10, time.Minute); err == nil {
		t.Fatal("an unknown preset was accepted")
	}
}

func TestABadReleaseIsRolledBackWhenThePolicySaysTo(t *testing.T) {
	t.Parallel()
	sc := preset(t, "bad-release", 11, 200)
	sc.Policy.Safety.OnBreach = rollout.OnBreachRollback
	report := run(t, sc)

	requireNoBadInstalls(t, report)
	if report.State != rollout.StateRolledBack {
		t.Fatalf("state = %s (%s: %s), rollback = %+v, want rolled back", report.State, report.HaltReason, report.HaltDetail, report.Rollback)
	}
	if report.Rollback == nil || report.Rollback.Needed == 0 || report.Rollback.Restored != report.Rollback.Needed {
		t.Fatalf("rollback = %+v, want every updated device restored", report.Rollback)
	}
	if report.Updated != 0 {
		t.Fatalf("%d devices still run the bad release", report.Updated)
	}
}

// downloadScenario is one stage of every device, on a network that breaks content
// responses partway through at the given rate.
func downloadScenario(breakRate float64) sim.Scenario {
	soak := rollout.Duration(time.Minute)
	deadline := rollout.Duration(24 * time.Hour)
	return sim.Scenario{
		Seed: 11, Devices: 30, ArtifactBytes: 256 << 10, ChunkSize: 16 << 10,
		Policy: rollout.PolicySpec{
			Stages: []rollout.StageSpec{{Percent: 100, Soak: &soak, Deadline: &deadline}},
			Safety: rollout.SafetySpec{MaxFailureBPS: new(1000)},
		},
		Faults: sim.Faults{Break: breakRate},
	}
}

func TestBrokenDownloadsResumeFromTheLastVerifiedChunk(t *testing.T) {
	t.Parallel()
	clean := run(t, downloadScenario(0))
	broken := run(t, downloadScenario(0.2))
	for _, report := range []sim.Report{clean, broken} {
		requireNoBadInstalls(t, report)
		if report.State != rollout.StateCompleted || report.Updated != 30 {
			t.Fatalf("state = %s, updated %d of 30", report.State, report.Updated)
		}
	}
	if want := 30 * clean.ArtifactBytes; clean.BytesDownloaded != want {
		t.Fatalf("a clean network downloaded %d bytes, want exactly %d", clean.BytesDownloaded, want)
	}
	if broken.Faults["break"] == 0 {
		t.Fatal("no response was broken")
	}
	// A break loses at most the chunk it cut short.
	if most := clean.BytesDownloaded + int64(broken.Faults["break"])*(16<<10); broken.BytesDownloaded <= clean.BytesDownloaded ||
		broken.BytesDownloaded > most {
		t.Fatalf("downloaded %d bytes with %d breaks, want more than %d and at most %d",
			broken.BytesDownloaded, broken.Faults["break"], clean.BytesDownloaded, most)
	}
}

func TestWithoutResumeABrokenDownloadStartsOver(t *testing.T) {
	t.Parallel()
	resumed := run(t, downloadScenario(0.2))
	sc := downloadScenario(0.2)
	sc.RestartDownloads = true
	restarted := run(t, sc)
	requireNoBadInstalls(t, restarted)
	if restarted.State != rollout.StateCompleted || restarted.Updated != 30 {
		t.Fatalf("state = %s, updated %d of 30", restarted.State, restarted.Updated)
	}
	// Starting over from the first chunk after a break costs far more than resuming.
	if restarted.BytesDownloaded < 2*resumed.BytesDownloaded {
		t.Fatalf("without resume downloaded %d bytes, with resume %d; want at least twice as many",
			restarted.BytesDownloaded, resumed.BytesDownloaded)
	}
}
