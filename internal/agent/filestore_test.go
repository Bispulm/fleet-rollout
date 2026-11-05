package agent_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Bispulm/fleet-rollout/internal/agent"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

func TestFileCheckpointsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := agent.NewFileCheckpoints(dir)
	if err != nil {
		t.Fatalf("NewFileCheckpoints: %v", err)
	}
	if _, found, err := store.Load(); err != nil || found {
		t.Fatalf("empty store: found = %v, err = %v", found, err)
	}
	want := agent.Checkpoint{
		Sequence:   7,
		ActiveSlot: "b",
		Active:     agent.Installed{Release: 14, SHA256: "abc"},
		Outbox:     []apiv1.ReportRequest{{Sequence: 7, State: rollout.AssignmentSucceeded}},
	}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reopened, err := agent.NewFileCheckpoints(dir)
	if err != nil {
		t.Fatalf("NewFileCheckpoints: %v", err)
	}
	got, found, err := reopened.Load()
	if err != nil || !found {
		t.Fatalf("Load: found = %v, err = %v", found, err)
	}
	if got.Sequence != 7 || got.ActiveSlot != "b" || got.Active.Release != 14 || len(got.Outbox) != 1 {
		t.Fatalf("got = %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "checkpoint.json"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestALeftoverTemporaryFileDoesNotAffectTheCheckpoint(t *testing.T) {
	dir := t.TempDir()
	store, err := agent.NewFileCheckpoints(dir)
	if err != nil {
		t.Fatalf("NewFileCheckpoints: %v", err)
	}
	if err := store.Save(agent.Checkpoint{Sequence: 3}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// What a crash partway through writing the next checkpoint would leave.
	if err := os.WriteFile(filepath.Join(dir, ".checkpoint-123.tmp"), []byte(`{"sequence":`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _, err := store.Load()
	if err != nil || got.Sequence != 3 {
		t.Fatalf("got = %+v, err = %v", got, err)
	}
}

func TestACorruptCheckpointIsAnErrorNotAFreshStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := agent.NewFileCheckpoints(dir)
	if err != nil {
		t.Fatalf("NewFileCheckpoints: %v", err)
	}
	// Starting over would reuse report sequences and forget what's installed.
	if _, _, err := store.Load(); err == nil {
		t.Fatal("loaded a corrupt checkpoint")
	}
}

func TestFilePartials(t *testing.T) {
	partials, err := agent.NewFilePartials(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilePartials: %v", err)
	}
	const id = "11111111-1111-4111-8111-111111111111"
	if err := partials.Truncate(id, 0); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	for _, chunk := range []string{"first ", "second ", "third"} {
		if err := partials.Append(id, []byte(chunk)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if got := readAll(t, partials, id); got != "first second third" {
		t.Fatalf("content = %q", got)
	}
	if err := partials.Truncate(id, 6); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if got := readAll(t, partials, id); got != "first " {
		t.Fatalf("after truncating, content = %q", got)
	}
	if err := partials.Truncate(id, 100); !errors.Is(err, agent.ErrPartialShort) {
		t.Fatalf("truncating beyond the end, which would invent bytes: error = %v, want ErrPartialShort", err)
	}

	const other = "22222222-2222-4222-8222-222222222222"
	if err := partials.Append(other, []byte("x")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := partials.RemoveExcept(id); err != nil {
		t.Fatalf("RemoveExcept: %v", err)
	}
	if _, err := partials.Open(other); err == nil {
		t.Fatal("RemoveExcept kept another partial")
	}
	if err := partials.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := partials.Remove(id); err != nil {
		t.Fatalf("removing twice: %v", err)
	}
}

func readAll(t *testing.T, partials agent.Partials, id string) string {
	t.Helper()
	r, err := partials.Open(id)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(data)
}

func TestFilePartialsRejectNamesThatCouldEscape(t *testing.T) {
	dir := t.TempDir()
	partials, err := agent.NewFilePartials(filepath.Join(dir, "partials"))
	if err != nil {
		t.Fatalf("NewFilePartials: %v", err)
	}
	for _, id := range []string{"../escape", "a/b", "", ".."} {
		if err := partials.Append(id, []byte("x")); err == nil {
			t.Errorf("Append(%q) succeeded", id)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("something was written outside: %v", entries)
	}
}

func TestFileInstaller(t *testing.T) {
	dir := t.TempDir()
	installer, err := agent.NewFileInstaller(dir)
	if err != nil {
		t.Fatalf("NewFileInstaller: %v", err)
	}
	ctx := context.Background()
	if err := installer.HealthCheck(ctx, "a"); err == nil {
		t.Fatal("an empty slot passed its health check")
	}
	content := []byte(strings.Repeat("release ", 100))
	if err := installer.Install(ctx, "b", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := installer.HealthCheck(ctx, "b"); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "slot-b"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("slot content = %d bytes, err = %v", len(got), err)
	}
	if err := installer.Install(ctx, "a", bytes.NewReader(content[:10]), int64(len(content))); err == nil {
		t.Fatal("installed a short artifact")
	}
	if _, err := os.Stat(filepath.Join(dir, "slot-a")); err == nil {
		t.Fatal("a failed install left a slot behind")
	}

	// A marker file is how a person running the agent by hand makes a release unhealthy.
	if err := os.WriteFile(filepath.Join(dir, "unhealthy"), nil, 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := installer.HealthCheck(ctx, "b"); err == nil {
		t.Fatal("the unhealthy marker was ignored")
	}
	if err := installer.Discard(ctx, "b"); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if err := installer.Discard(ctx, "b"); err != nil {
		t.Fatalf("discarding twice: %v", err)
	}
	for _, slot := range []string{"c", "../a", ""} {
		if err := installer.Install(ctx, slot, bytes.NewReader(content), int64(len(content))); err == nil {
			t.Errorf("Install(%q) succeeded", slot)
		}
	}
}

// The agent's own scenarios run on memory stores. This one runs it on real files, including a
// restart partway through a download.
func TestTheAgentRunsOnRealFilesAcrossARestart(t *testing.T) {
	f := newFixture(t)
	stateDir := t.TempDir()
	open := func() agent.Deps {
		checkpoints, err := agent.NewFileCheckpoints(stateDir)
		if err != nil {
			t.Fatalf("NewFileCheckpoints: %v", err)
		}
		partials, err := agent.NewFilePartials(filepath.Join(stateDir, "partials"))
		if err != nil {
			t.Fatalf("NewFilePartials: %v", err)
		}
		installer, err := agent.NewFileInstaller(filepath.Join(stateDir, "slots"))
		if err != nil {
			t.Fatalf("NewFileInstaller: %v", err)
		}
		deps := f.deps()
		deps.Checkpoints, deps.Partials, deps.Installer = checkpoints, partials, installer
		return deps
	}
	step := func(a *agent.Agent) {
		wait, err := a.Step(f.ctx)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		f.clock.now = f.clock.now.Add(wait)
	}

	first, err := agent.New(f.config, open())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range 200 {
		if work := first.Checkpoint().Work; work != nil && work.VerifiedChunks == 3 {
			break
		}
		step(first)
	}

	second, err := agent.New(f.config, open())
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}
	for range 500 {
		if cp := second.Checkpoint(); cp.Work == nil && len(cp.Outbox) == 0 && f.server.assignment == nil {
			break
		}
		step(second)
	}
	if got := f.server.states(); !slices.Equal(got, healthyPath) {
		t.Fatalf("reports = %v", got)
	}
	installed, err := os.ReadFile(filepath.Join(stateDir, "slots", "slot-"+second.Checkpoint().ActiveSlot))
	if err != nil || !bytes.Equal(installed, f.server.content) {
		t.Fatalf("installed %d bytes, err = %v", len(installed), err)
	}
	if got := countOf(f.server.fetched, 0); got != 1 {
		t.Fatalf("the first chunk was fetched %d times; the restart should have resumed", got)
	}
}
