package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/agent"
	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/engine"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/signing/signingtest"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

const (
	agentChunk    = 32 << 10
	agentChunks   = 64
	agentPlatform = "linux/arm64"
	// chunkDelay slows each chunk down, so that a download takes long enough to interrupt.
	chunkDelay = 40 * time.Millisecond
)

// buildAgent compiles the rollout-agent command, so the test runs the real binary.
func buildAgent(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "rollout-agent")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", out, "github.com/Bispulm/fleet-rollout/cmd/rollout-agent")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build rollout-agent: %v\n%s", err, output)
	}
	return out
}

// downloads records where each device's content requests start, and slows them down.
type downloads struct {
	mu      sync.Mutex
	offsets map[string][]int64
}

func (d *downloads) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			var start int64
			_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
			d.mu.Lock()
			d.offsets[r.Header.Get("Authorization")] = append(d.offsets[r.Header.Get("Authorization")], start)
			d.mu.Unlock()
			time.Sleep(chunkDelay)
		}
		next.ServeHTTP(w, r)
	})
}

func (d *downloads) of(token string) []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]int64(nil), d.offsets["Bearer "+token]...)
}

// syncBuffer collects a process's output for the failure message.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type agentProcess struct {
	id, token, dir string
	cmd            *exec.Cmd
	output         *syncBuffer
}

func TestRealAgentsUpdateOverHTTPAndResumeAfterBeingKilled(t *testing.T) {
	t.Parallel()
	bin := buildAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	pool := testdb.New(t)
	signer := signingtest.NewKey(t)
	logger := slog.New(slog.DiscardHandler)
	st := store.New(pool)
	log := &downloads{offsets: map[string][]int64{}}
	server := httptest.NewServer(log.wrap(api.New(api.Options{
		Logger:       logger,
		Ready:        func(context.Context) error { return nil },
		Store:        st,
		AdminToken:   adminToken,
		PollInterval: time.Second,
		Trust:        signingtest.Trust(t, signer),
		Artifacts:    artifactstore.NewMemory(),
	})))
	t.Cleanup(server.Close)
	engineCtx, stopEngine := context.WithCancel(ctx)
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		e := engine.New(engine.Options{Store: st, Holder: "agent-test", Interval: 200 * time.Millisecond, LeaseTTL: 2 * time.Second, Logger: logger})
		_ = e.Run(engineCtx)
	}()
	t.Cleanup(func() { stopEngine(); <-engineDone })

	admin := client.New(server.URL, adminToken, nil)
	if _, err := admin.CreateFleet(ctx, apiv1.CreateFleetRequest{Name: "agents"}); err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	registered, err := admin.RegisterDevices(ctx, "agents", []apiv1.DeviceSpec{
		{ID: "agent-0", Platform: agentPlatform}, {ID: "agent-1", Platform: agentPlatform}, {ID: "agent-2", Platform: agentPlatform},
	})
	if err != nil {
		t.Fatalf("RegisterDevices: %v", err)
	}

	content := make([]byte, agentChunk*agentChunks)
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // Test content.
	for i := range content {
		content[i] = byte(rng.UintN(256)) //nolint:gosec // Always below 256.
	}
	_, envelope := signingtest.Signed(t, signer, signing.Meta{
		Name: "demo-app", Version: "2.0.0", Platform: agentPlatform, ReleaseCounter: 1, ChunkSize: agentChunk,
	}, content)
	artifact, err := admin.CreateArtifact(ctx, envelope)
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if err := admin.UploadArtifactContent(ctx, artifact.ID, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("UploadArtifactContent: %v", err)
	}
	trustFile := filepath.Join(t.TempDir(), "trust.pub")
	if err := os.WriteFile(trustFile, signingtest.PublicPEM(t, signer), 0o600); err != nil {
		t.Fatal(err)
	}

	start := func(p *agentProcess) {
		t.Helper()
		p.output = &syncBuffer{}
		p.cmd = exec.CommandContext(ctx, bin, "--api", server.URL, "--device-id", p.id, "--state-dir", p.dir, //nolint:gosec // The test built it.
			"--platform", agentPlatform, "--trust-keys", trustFile, "--log-level", "debug")
		p.cmd.Env = append(os.Environ(), "ROLLOUT_AGENT_TOKEN="+p.token)
		p.cmd.Stderr = p.output
		if err := p.cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", p.id, err)
		}
	}
	agents := make([]*agentProcess, len(registered))
	for i, d := range registered {
		agents[i] = &agentProcess{id: d.ID, token: d.Token, dir: t.TempDir()}
		start(agents[i])
	}
	t.Cleanup(func() {
		for _, p := range agents {
			_ = p.cmd.Process.Kill()
			_ = p.cmd.Wait()
		}
	})

	soak := rollout.Duration(time.Second)
	created, err := admin.CreateRollout(ctx, "agents", apiv1.CreateRolloutRequest{
		Name: "agents-v2", ArtifactID: artifact.ID, Policy: rollout.PolicySpec{
			Stages: []rollout.StageSpec{{Percent: 100, Soak: &soak, Deadline: ptr(rollout.Duration(5 * time.Minute))}},
			Safety: rollout.SafetySpec{MaxFailureBPS: ptr(100), StaleAfter: ptr(rollout.Duration(time.Minute))},
		},
	})
	if err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if _, err := admin.Transition(ctx, created.ID, client.ActionStart, apiv1.TransitionRequest{Reason: "agent test"}); err != nil {
		t.Fatalf("start rollout: %v", err)
	}

	// Kill one agent outright partway through its download, the way a power cut would.
	victim := agents[0]
	partial := filepath.Join(victim.dir, "partials", artifact.ID+".part")
	waitFor(t, ctx, "a partial download", func() bool {
		info, err := os.Stat(partial)
		return err == nil && info.Size() >= 4*agentChunk && info.Size() < int64(len(content))
	})
	if err := victim.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}
	_ = victim.cmd.Wait()
	before := len(log.of(victim.token))
	start(victim)

	waitFor(t, ctx, "the rollout to complete", func() bool {
		got, err := admin.GetRollout(ctx, created.ID)
		if err != nil {
			return false
		}
		if got.State == rollout.StateHalted {
			t.Fatalf("halted: %s, %s\n%s", got.HaltReason, got.HaltDetail, victim.output.String())
		}
		return got.State == rollout.StateCompleted
	})

	// The restarted agent carried on from what it had verified, rather than starting over.
	offsets := log.of(victim.token)
	if resumed := offsets[before]; resumed == 0 {
		t.Errorf("after the restart, the download started again from byte 0: %v", offsets)
	}
	if fetched := len(offsets); fetched > agentChunks+8 {
		t.Errorf("%d chunk requests for %d chunks", fetched, agentChunks)
	}

	for _, p := range agents {
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("stop %s: %v", p.id, err)
		}
		if err := p.cmd.Wait(); err != nil {
			t.Errorf("%s exited with %v\n%s", p.id, err, p.output.String())
		}
		raw, err := os.ReadFile(filepath.Join(p.dir, "checkpoint.json"))
		if err != nil {
			t.Fatalf("%s: %v", p.id, err)
		}
		var cp agent.Checkpoint
		if err := json.Unmarshal(raw, &cp); err != nil {
			t.Fatalf("%s: %v", p.id, err)
		}
		if cp.Active.Release != 1 || cp.Work != nil || len(cp.Outbox) != 0 {
			t.Errorf("%s: checkpoint = %+v", p.id, cp)
		}
		slot, err := os.ReadFile(filepath.Join(p.dir, "slots", "slot-"+cp.ActiveSlot))
		if err != nil || !bytes.Equal(slot, content) {
			t.Errorf("%s: the active slot doesn't hold the release (%v)", p.id, err)
		}
	}
	list, err := admin.ListDevices(ctx, "agents", "", 10)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	for _, d := range list.Devices {
		if d.InstalledRelease != 1 {
			t.Errorf("the server thinks %s runs release %d", d.ID, d.InstalledRelease)
		}
	}
}

func waitFor(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("gave up waiting for %s", what)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func ptr[T any](v T) *T { return &v }
