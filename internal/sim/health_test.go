package sim

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/agent"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
)

func TestOnlyTheBadReleaseFailsItsHealthCheck(t *testing.T) {
	env := &environment{
		clock: realClock{}, decide: decider{seed: 1}, start: time.Now(),
		faults: Faults{Unhealthy: 1, UnhealthyRelease: 3},
	}
	d := newDevice(env, "dev-1", "token", 1)
	installer := faultyInstaller{MemoryInstaller: d.installer, device: d}
	ctx := context.Background()
	for _, slot := range []string{"slot-a", "slot-b"} {
		if err := d.installer.Install(ctx, slot, bytes.NewReader([]byte("x")), 1); err != nil {
			t.Fatal(err)
		}
	}
	check := func(cp agent.Checkpoint, slot string) error {
		t.Helper()
		if err := d.checkpoints.Save(cp); err != nil {
			t.Fatal(err)
		}
		return installer.HealthCheck(ctx, slot)
	}
	installing := func(release int64) agent.Checkpoint {
		return agent.Checkpoint{
			ActiveSlot: "slot-a", Active: agent.Installed{Release: 1},
			Work: &agent.Work{Kind: agent.KindForward, Artifact: apiv1.Artifact{ReleaseCounter: release}, PendingSlot: "slot-b"},
		}
	}
	if err := check(installing(2), "slot-b"); err != nil {
		t.Errorf("release 2 failed its health check, though only release 3 is bad: %v", err)
	}
	if err := check(installing(3), "slot-b"); err == nil {
		t.Error("release 3 passed its health check on a device where it's bad")
	}
	// Rolling back from release 3 checks the release in the previous slot, 2, which is fine.
	restoring := agent.Checkpoint{
		ActiveSlot: "slot-b", Active: agent.Installed{Release: 3}, Previous: &agent.Installed{Release: 2, ArtifactID: "a2"},
		Work: &agent.Work{Kind: agent.KindRollback, Artifact: apiv1.Artifact{ReleaseCounter: 3}, RestoreRelease: 2, PendingSlot: "slot-a"},
	}
	if err := check(restoring, "slot-a"); err != nil {
		t.Errorf("restoring release 2 failed its health check: %v", err)
	}

	// Without a bad release named, every release is bad on this device, as before.
	env.faults.UnhealthyRelease = 0
	if err := check(installing(2), "slot-b"); err == nil {
		t.Error("with no release named, release 2 should fail too")
	}
}
