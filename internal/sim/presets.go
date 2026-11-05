package sim

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// flaky is a network and fleet that go wrong in every way the simulator knows, at rates a
// rollout should be able to absorb. Times scale with the soak time, so the same preset
// works on a fake clock with long soaks and in real time with short ones.
func flaky(soak time.Duration) Faults {
	return Faults{
		Drop:            0.05,
		LostResponse:    0.05,
		Corrupt:         0.03,
		Truncate:        0.03,
		DuplicateReport: 0.05,
		// A crash during an install counts against the release, because a bad release that
		// reboots the device looks just the same. This rate puts about 1% of devices there.
		Crash:         0.002,
		RestartWithin: soak / 5,
		Offline:       0.1,
		OfflineFor:    2 * soak,
		OfflineWithin: 6 * soak,
	}
}

var presets = map[string]func(soak time.Duration) Faults{
	"clean": func(time.Duration) Faults { return Faults{} },
	"flaky": flaky,
	"bad-release": func(soak time.Duration) Faults {
		f := flaky(soak)
		f.Unhealthy = 0.3
		return f
	},
	"storm": func(soak time.Duration) Faults {
		f := flaky(soak)
		f.StormAt, f.StormFor = soak/2, 9*soak/2
		return f
	},
}

// Presets lists the scenario presets.
func Presets() []string {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Preset returns a named scenario: a four-stage rollout of the given number of devices that
// halts when more than 10% of a stage fails, with soak as each stage's soak time.
func Preset(name string, seed int64, devices int, soak time.Duration) (Scenario, error) {
	faults, ok := presets[name]
	if !ok {
		return Scenario{}, fmt.Errorf("unknown preset %q; choose from %s", name, strings.Join(Presets(), ", "))
	}
	return Scenario{Name: name, Seed: seed, Devices: devices, Policy: StagedPolicy(soak, 1000), Faults: faults(soak)}, nil
}

// StagedPolicy rolls out to 10%, 25%, 50%, and then everyone, soaking each stage, and halts
// when more than maxFailureBPS of a stage fails.
func StagedPolicy(soak time.Duration, maxFailureBPS int) rollout.PolicySpec {
	stage := func(percent int) rollout.StageSpec {
		return rollout.StageSpec{Percent: percent, Soak: durationPtr(soak), Deadline: durationPtr(max(4*time.Hour, 20*soak))}
	}
	return rollout.PolicySpec{
		Stages: []rollout.StageSpec{stage(10), stage(25), stage(50), stage(100)},
		Safety: rollout.SafetySpec{MaxFailureBPS: &maxFailureBPS, StaleAfter: durationPtr(max(30*time.Minute, 3*soak))},
	}
}

func durationPtr(d time.Duration) *rollout.Duration {
	v := rollout.Duration(d)
	return &v
}
