package sim

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/fleet-rollout/internal/agent"
	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/engine"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

const (
	platform       = "linux/arm64"
	initialRelease = 1
	newRelease     = 2
	adminToken     = "simulator-admin-credential"
	// minStep keeps a device that asks to run again immediately from stalling the clock.
	minStep = time.Millisecond
)

// Scenario describes a simulated rollout.
type Scenario struct {
	// Name is the preset it came from, if any.
	Name          string             `json:"name,omitempty"`
	Seed          int64              `json:"seed"`
	Fleet         string             `json:"fleet"`
	Devices       int                `json:"devices"`
	ArtifactBytes int                `json:"artifact_bytes"`
	ChunkSize     int64              `json:"chunk_size"`
	Policy        rollout.PolicySpec `json:"policy"`
	Faults        Faults             `json:"faults"`
	// The rest only matter in deterministic mode, where the simulator runs the server.
	PollInterval   time.Duration `json:"poll_interval,omitempty"`
	EngineInterval time.Duration `json:"engine_interval,omitempty"`
	// MaxDuration bounds the run, in simulated time for deterministic mode, and real time
	// for live mode.
	MaxDuration time.Duration `json:"max_duration"`
}

func (s *Scenario) defaults() {
	if s.Fleet == "" {
		s.Fleet = "sim"
	}
	if s.ArtifactBytes == 0 {
		s.ArtifactBytes = 64 << 10
	}
	if s.ChunkSize == 0 {
		s.ChunkSize = 4 << 10
	}
	if s.PollInterval == 0 {
		s.PollInterval = time.Minute
	}
	if s.EngineInterval == 0 {
		s.EngineInterval = 5 * time.Second
	}
	if s.MaxDuration == 0 {
		s.MaxDuration = 24 * time.Hour
	}
	if s.Faults.RestartWithin == 0 {
		s.Faults.RestartWithin = 2 * time.Minute
	}
	if s.Faults.OfflineWithin == 0 {
		s.Faults.OfflineWithin = time.Hour
	}
}

// StageResult is how one stage ended.
type StageResult struct {
	Ordinal    int    `json:"ordinal"`
	State      string `json:"state"`
	Cohort     int    `json:"cohort"`
	Succeeded  int    `json:"succeeded"`
	Failed     int    `json:"failed"`
	Superseded int    `json:"superseded"`
	InFlight   int    `json:"in_flight"`
	Unknown    int    `json:"unknown"`
	// Pending counts members that were never assigned, which a completed rollout can leave
	// behind once it has enough coverage. A follow-up rollout reaches them.
	Pending int `json:"pending"`
}

// DeviceResult is how one device ended.
type DeviceResult struct {
	ID      string `json:"id"`
	Release int64  `json:"release"`
	Crashes int    `json:"crashes"`
	Steps   int    `json:"steps"`
}

// Report describes a finished run.
type Report struct {
	Mode       string        `json:"mode"`
	Scenario   Scenario      `json:"scenario"`
	RolloutID  string        `json:"rollout_id"`
	State      rollout.State `json:"state"`
	HaltReason string        `json:"halt_reason,omitempty"`
	HaltDetail string        `json:"halt_detail,omitempty"`
	Stages     []StageResult `json:"stages"`
	// Updated is how many devices end up running the new release.
	Updated int `json:"updated"`
	// BadInstalls counts devices running content that isn't the signed artifact. It must
	// always be zero, whatever faults were injected.
	BadInstalls int            `json:"bad_installs"`
	Faults      map[string]int `json:"faults"`
	// Failures counts the failures devices reported, by reason.
	Failures map[string]int `json:"failures"`
	Crashes  int            `json:"crashes"`
	// BytesDownloaded is everything devices received, including what they had to discard.
	BytesDownloaded int64         `json:"bytes_downloaded"`
	ArtifactBytes   int64         `json:"artifact_bytes"`
	Elapsed         time.Duration `json:"elapsed"`
	// TimedOut means the run hit MaxDuration before the rollout finished.
	TimedOut bool           `json:"timed_out"`
	Devices  []DeviceResult `json:"devices"`
	// Digest summarizes the outcome. Two deterministic runs of one scenario must agree.
	Digest string `json:"digest"`
}

// deterministicKey derives a signing key from the seed, so that every deterministic run of a
// scenario signs the same manifest.
func deterministicKey(seed int64) ed25519.PrivateKey {
	material := sha256.Sum256([]byte(fmt.Sprintf("fleet-rollout/sim/signer/%d", seed)))
	return ed25519.NewKeyFromSeed(material[:])
}

// rolloutID derives the rollout's ID from the seed, which fixes the cohort order.
func rolloutID(seed int64, fleet string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("fleet-rollout/sim/rollout/%d/%s", seed, fleet)))
	sum[6] = sum[6]&0x0f | 0x40
	sum[8] = sum[8]&0x3f | 0x80
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func artifactContent(seed int64, size int) []byte {
	rng := rand.New(rand.NewPCG(uint64(seed), 0x5eed)) //nolint:gosec // Synthetic content, not security.
	content := make([]byte, size+8)
	for i := 0; i < size; i += 8 {
		binary.LittleEndian.PutUint64(content[i:], rng.Uint64())
	}
	return content[:size]
}

// fleetSetup is what setUp creates.
type fleetSetup struct {
	rolloutID string
	content   []byte
	tokens    []apiv1.Device
}

// setUp registers the fleet and its devices, signs and uploads the artifact, and starts the
// rollout, all through the admin API.
func setUp(ctx context.Context, admin *client.Client, sc Scenario, signer ed25519.PrivateKey) (fleetSetup, error) {
	if _, err := admin.CreateFleet(ctx, apiv1.CreateFleetRequest{Name: sc.Fleet}); err != nil {
		return fleetSetup{}, fmt.Errorf("create fleet: %w", err)
	}
	var tokens []apiv1.Device
	for start := 0; start < sc.Devices; start += 1000 {
		specs := make([]apiv1.DeviceSpec, min(1000, sc.Devices-start))
		for i := range specs {
			specs[i] = apiv1.DeviceSpec{
				ID: fmt.Sprintf("%s-%05d", sc.Fleet, start+i), Platform: platform, InstalledRelease: initialRelease,
			}
		}
		registered, err := admin.RegisterDevices(ctx, sc.Fleet, specs)
		if err != nil {
			return fleetSetup{}, fmt.Errorf("register devices: %w", err)
		}
		tokens = append(tokens, registered...)
	}
	slices.SortFunc(tokens, func(a, b apiv1.Device) int { return strings.Compare(a.ID, b.ID) })

	content := artifactContent(sc.Seed, sc.ArtifactBytes)
	manifest, err := signing.BuildManifest(signing.Meta{
		Name: "sim-app", Version: "2.0.0", Platform: platform, ReleaseCounter: newRelease, ChunkSize: sc.ChunkSize,
	}, bytes.NewReader(content))
	if err != nil {
		return fleetSetup{}, err
	}
	envelope, err := signing.Sign(signer, manifest)
	if err != nil {
		return fleetSetup{}, err
	}
	artifact, err := admin.CreateArtifact(ctx, envelope)
	if err != nil {
		return fleetSetup{}, fmt.Errorf("register artifact: %w", err)
	}
	if !artifact.ContentReady {
		if err := admin.UploadArtifactContent(ctx, artifact.ID, bytes.NewReader(content), int64(len(content))); err != nil {
			return fleetSetup{}, fmt.Errorf("upload artifact: %w", err)
		}
	}
	id := rolloutID(sc.Seed, sc.Fleet)
	created, err := admin.CreateRollout(ctx, sc.Fleet, apiv1.CreateRolloutRequest{
		ID: id, Name: "sim-" + id[:8], ArtifactID: artifact.ID, Policy: sc.Policy,
	})
	if err != nil {
		return fleetSetup{}, fmt.Errorf("create rollout: %w", err)
	}
	if _, err := admin.Transition(ctx, created.ID, client.ActionStart, apiv1.TransitionRequest{Reason: "simulation"}); err != nil {
		return fleetSetup{}, fmt.Errorf("start rollout: %w", err)
	}
	return fleetSetup{rolloutID: id, content: content, tokens: tokens}, nil
}

func finished(state rollout.State) bool {
	return state == rollout.StateCompleted || state == rollout.StateHalted
}

// summarize turns the final state into a report.
func summarize(report *Report, status apiv1.Rollout, devices []*device, content []byte) {
	report.State = status.State
	report.HaltReason, report.HaltDetail = status.HaltReason, status.HaltDetail
	for _, stage := range status.Stages {
		cohort := 0
		if stage.CohortSize != nil {
			cohort = *stage.CohortSize
		}
		report.Stages = append(report.Stages, StageResult{
			Ordinal: stage.Ordinal, State: stage.State, Cohort: cohort, Succeeded: stage.Succeeded,
			Failed: stage.Failed, Superseded: stage.Superseded, InFlight: stage.InFlight, Unknown: stage.Unknown,
			Pending: stage.Pending,
		})
	}
	digest := sha256.Sum256(content)
	want := hex.EncodeToString(digest[:])
	report.Faults = map[string]int{}
	report.Failures = map[string]int{}
	report.ArtifactBytes = int64(len(content))
	for _, d := range devices {
		cp, _, _ := d.checkpoints.Load()
		if cp.Active.Release == newRelease {
			report.Updated++
			if cp.Active.SHA256 != want || !bytes.Equal(d.installer.Content(cp.ActiveSlot), content) {
				report.BadInstalls++
			}
		}
		for kind, count := range d.faults {
			report.Faults[kind] += count
		}
		for _, reason := range d.failures {
			report.Failures[reason]++
		}
		report.Crashes += d.crashes
		report.BytesDownloaded += d.bytes
		report.Devices = append(report.Devices, DeviceResult{ID: d.id, Release: cp.Active.Release, Crashes: d.crashes, Steps: d.steps})
	}
	outcome, _ := json.Marshal(struct {
		State    rollout.State
		Reason   string
		Stages   []StageResult
		Faults   map[string]int
		Failures map[string]int
		Devices  []DeviceResult
	}{report.State, report.HaltReason, report.Stages, report.Faults, report.Failures, report.Devices})
	sum := sha256.Sum256(outcome)
	report.Digest = hex.EncodeToString(sum[:])
}

// schedule orders devices by when they next want to run, and then by position, so the order
// never depends on anything but the simulation itself.
type schedule []scheduled

type scheduled struct {
	at    time.Time
	index int
}

func (s schedule) Len() int { return len(s) }
func (s schedule) Less(i, j int) bool {
	if !s[i].at.Equal(s[j].at) {
		return s[i].at.Before(s[j].at)
	}
	return s[i].index < s[j].index
}
func (s schedule) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s *schedule) Push(x any)   { *s = append(*s, x.(scheduled)) } //nolint:forcetypeassert // Only scheduled is pushed.
func (s *schedule) Pop() any {
	old := *s
	item := old[len(old)-1]
	*s = old[:len(old)-1]
	return item
}

// Run simulates a scenario deterministically. It runs the control plane in process against
// pool, which should be an empty, migrated database, with the engine and every device
// advanced from this goroutine on a fake clock. Nothing is concurrent, so a scenario always
// produces the same report.
func Run(ctx context.Context, pool *pgxpool.Pool, sc Scenario) (Report, error) {
	sc.defaults()
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	logger := slog.New(slog.DiscardHandler)
	signer := deterministicKey(sc.Seed)
	trust, err := trustFor(signer)
	if err != nil {
		return Report{}, err
	}
	st := store.New(pool, store.WithClock(clock.Now))
	server := api.New(api.Options{
		Logger:            logger,
		Ready:             func(context.Context) error { return nil },
		Store:             st,
		AdminToken:        adminToken,
		PollInterval:      sc.PollInterval,
		Trust:             trust,
		Artifacts:         artifactstore.NewMemory(),
		MaxDeviceRequests: 1 << 20,
	})
	transport := handlerTransport{handler: server}
	const base = "http://simulator"
	admin := client.New(base, adminToken, &http.Client{Transport: transport})

	setup, err := setUp(ctx, admin, sc, signer)
	if err != nil {
		return Report{}, err
	}
	env := &environment{
		clock: clock, decide: decider{seed: sc.Seed}, faults: sc.Faults, start: clock.now,
		config:    agent.Config{Platform: platform, Trust: trust, InitialRelease: initialRelease},
		logger:    logger,
		transport: transport,
		baseURL:   base,
	}
	devices := make([]*device, len(setup.tokens))
	queue := make(schedule, 0, len(devices))
	for i, registered := range setup.tokens {
		devices[i] = newDevice(env, registered.ID, registered.Token, sc.Seed)
		// Devices don't all start at the same instant.
		offset := time.Duration(env.decide.value(registered.ID, "start", 0) * float64(sc.PollInterval))
		queue = append(queue, scheduled{at: clock.now.Add(offset), index: i})
	}
	heap.Init(&queue)

	rolloutEngine := engine.New(engine.Options{
		Store: st, Holder: "simulator", Interval: sc.EngineInterval, LeaseTTL: time.Hour, Logger: logger,
	})
	report := Report{Mode: "deterministic", Scenario: sc, RolloutID: setup.rolloutID}
	deadline := clock.now.Add(sc.MaxDuration)
	nextTick := clock.now
	var status apiv1.Rollout
	for {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		item := queue[0]
		if !nextTick.After(item.at) {
			clock.now = nextTick
			rolloutEngine.Tick(ctx)
			nextTick = nextTick.Add(sc.EngineInterval)
			status, err = admin.GetRollout(ctx, setup.rolloutID)
			if err != nil {
				return Report{}, fmt.Errorf("read rollout: %w", err)
			}
			if finished(status.State) && allQuiet(devices) {
				break
			}
			if !clock.now.Before(deadline) {
				report.TimedOut = true
				break
			}
			continue
		}
		heap.Pop(&queue)
		clock.now = item.at
		wait := max(devices[item.index].step(ctx), minStep)
		heap.Push(&queue, scheduled{at: clock.now.Add(wait), index: item.index})
	}
	report.Elapsed = clock.now.Sub(env.start)
	summarize(&report, status, devices, setup.content)
	return report, nil
}

func allQuiet(devices []*device) bool {
	for _, d := range devices {
		if !d.quiet() {
			return false
		}
	}
	return true
}

func trustFor(signer ed25519.PrivateKey) (signing.TrustSet, error) {
	public, ok := signer.Public().(ed25519.PublicKey)
	if !ok {
		return signing.TrustSet{}, errors.New("not an Ed25519 key")
	}
	encoded, err := signing.MarshalPublicKey(public)
	if err != nil {
		return signing.TrustSet{}, err
	}
	return signing.ParseTrustSet(encoded)
}

// LiveConfig describes a live run against a server that's already running.
type LiveConfig struct {
	Scenario   Scenario
	APIURL     string
	AdminToken string
	// Signer signs the artifact, and must be on the server's trust list. Trust is what the
	// devices trust, which would normally be the same key.
	Signer ed25519.PrivateKey
	Trust  signing.TrustSet
	// Progress, if set, gets a line about the rollout every second.
	Progress io.Writer
}

// RunLive simulates a scenario in real time, one goroutine per device, over real HTTP. Each
// device's fault decisions are reproducible, but how devices interleave depends on scheduling,
// so two runs can differ in which devices finish before a halt.
func RunLive(ctx context.Context, cfg LiveConfig) (Report, error) {
	sc := cfg.Scenario
	sc.defaults()
	logger := slog.New(slog.DiscardHandler)
	transport := &http.Transport{MaxIdleConnsPerHost: 64, MaxConnsPerHost: 64, IdleConnTimeout: 30 * time.Second}
	defer transport.CloseIdleConnections()
	admin := client.New(cfg.APIURL, cfg.AdminToken, &http.Client{Transport: transport})
	setup, err := setUp(ctx, admin, sc, cfg.Signer)
	if err != nil {
		return Report{}, err
	}

	env := &environment{
		clock: realClock{}, decide: decider{seed: sc.Seed}, faults: sc.Faults, start: time.Now(),
		config:    agent.Config{Platform: platform, Trust: cfg.Trust, InitialRelease: initialRelease},
		logger:    logger,
		transport: transport,
		baseURL:   cfg.APIURL,
		live:      true,
	}
	devices := make([]*device, len(setup.tokens))
	quiet := make([]atomic.Bool, len(devices))
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var wg sync.WaitGroup
	for i, registered := range setup.tokens {
		d := newDevice(env, registered.ID, registered.Token, sc.Seed)
		devices[i] = d
		wg.Go(func() {
			// Devices don't all start at the same instant.
			wait := time.Duration(env.decide.value(d.id, "start", 0) * float64(time.Second))
			for {
				select {
				case <-runCtx.Done():
					return
				case <-time.After(wait):
				}
				wait = d.step(runCtx)
				quiet[i].Store(d.quiet())
			}
		})
	}

	report := Report{Mode: "live", Scenario: sc, RolloutID: setup.rolloutID}
	deadline := time.Now().Add(sc.MaxDuration)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var status apiv1.Rollout
	for {
		select {
		case <-ctx.Done():
			stop()
			wg.Wait()
			return Report{}, ctx.Err()
		case <-ticker.C:
		}
		if time.Now().After(deadline) {
			report.TimedOut = true
			break
		}
		status, err = admin.GetRollout(ctx, setup.rolloutID)
		if err != nil {
			continue // The server may be shedding load. Its devices keep running.
		}
		if cfg.Progress != nil {
			_, _ = fmt.Fprintf(cfg.Progress, "%s %s stage %v, waiting: %s\n", time.Now().Format(time.TimeOnly), status.State,
				status.CurrentStage, status.Waiting)
		}
		allQuiet := true
		for i := range quiet {
			allQuiet = allQuiet && quiet[i].Load()
		}
		if finished(status.State) && allQuiet {
			break
		}
	}
	stop()
	wg.Wait()
	report.Elapsed = time.Since(env.start)
	summarize(&report, status, devices, setup.content)
	return report, nil
}
