package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/agent"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

var (
	errDropped      = errors.New("simulated: connection refused")
	errResponseLost = errors.New("simulated: connection reset before the response")
	errOffline      = errors.New("simulated: network unreachable")
	errTruncated    = errors.New("simulated: connection reset mid-download")
)

// crash is what fault wrappers panic with, to stop an agent exactly where it is.
type crash struct{}

// clock is the simulation's notion of time.
type clock interface {
	Now() time.Time
}

// fakeClock is moved forward by the deterministic runner.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// environment is what every device in a run shares.
type environment struct {
	clock     clock
	decide    decider
	faults    Faults
	start     time.Time
	config    agent.Config
	logger    *slog.Logger
	transport http.RoundTripper
	baseURL   string
	// live makes latency real.
	live bool
	// restartDownloads throws a download away whenever a chunk fails. See Scenario.
	restartDownloads bool
	// startWithin spreads live devices' first steps over this long, a second if it's zero.
	startWithin time.Duration
}

// device is one simulated device: a real agent, and the faults around it.
type device struct {
	env   *environment
	id    string
	token string

	checkpoints *agent.MemoryCheckpoints
	partials    *agent.MemoryPartials
	installer   *agent.MemoryInstaller
	rng         *rand.Rand
	agent       *agent.Agent

	// downUntil is when a crashed agent restarts.
	downUntil time.Time
	// offlineFrom and offlineUntil are this device's own time off the network, if any.
	offlineFrom, offlineUntil time.Time
	unhealthy                 bool

	occurrences map[string]int
	faults      map[string]int
	// failures records the reason for each failure the device reported, by sequence, so
	// that retries count once.
	failures map[int64]string
	crashes  int
	bytes    int64
	steps    int
	// chunkFailed records that a content request failed during the current step.
	chunkFailed bool
}

func newDevice(env *environment, id, token string, seed int64) *device {
	d := &device{
		env:         env,
		id:          id,
		token:       token,
		checkpoints: agent.NewMemoryCheckpoints(),
		partials:    agent.NewMemoryPartials(),
		installer:   agent.NewMemoryInstaller(),
		rng:         rand.New(rand.NewPCG(uint64(seed), uint64(seedFor(id)))), //nolint:gosec // Jitter, not security.
		occurrences: map[string]int{},
		faults:      map[string]int{},
		failures:    map[int64]string{},
		unhealthy:   env.decide.hit(id, "unhealthy", 0, env.faults.Unhealthy),
	}
	if env.decide.hit(id, "offline", 0, env.faults.Offline) {
		d.offlineFrom = env.start.Add(time.Duration(env.decide.value(id, "offline-at", 0) * float64(env.faults.OfflineWithin)))
		d.offlineUntil = d.offlineFrom.Add(env.faults.OfflineFor)
	}
	return d
}

func seedFor(id string) int64 {
	var h int64
	for _, r := range id {
		h = h*31 + int64(r)
	}
	return h
}

// next counts an occurrence of a kind of operation, which is what fault decisions key on.
func (d *device) next(kind string) int {
	n := d.occurrences[kind]
	d.occurrences[kind]++
	return n
}

func (d *device) hit(kind string, rate float64) bool {
	return d.env.decide.hit(d.id, kind, d.next(kind), rate)
}

func (d *device) offline(now time.Time) bool {
	if !now.Before(d.offlineFrom) && now.Before(d.offlineUntil) {
		return true
	}
	f := d.env.faults
	stormFrom := d.env.start.Add(f.StormAt)
	return f.StormFor > 0 && !now.Before(stormFrom) && now.Before(stormFrom.Add(f.StormFor))
}

func (d *device) deps() agent.Deps {
	return agent.Deps{
		Transport:   client.NewDevice(d.env.baseURL, d.token, &http.Client{Transport: &faultTransport{next: d.env.transport, device: d}}),
		Checkpoints: crashingCheckpoints{MemoryCheckpoints: d.checkpoints, device: d},
		Partials:    d.partials,
		Installer:   faultyInstaller{MemoryInstaller: d.installer, device: d},
		Clock:       d.env.clock,
		Rand:        d.rng.Float64,
		Logger:      d.env.logger,
	}
}

// step advances the device once, and returns when it next wants to run. A crash throws the
// agent away. It restarts later from whatever it had saved, as a rebooted device would.
func (d *device) step(ctx context.Context) time.Duration {
	now := d.env.clock.Now()
	if d.agent == nil {
		if now.Before(d.downUntil) {
			return d.downUntil.Sub(now)
		}
		var a *agent.Agent
		var err error
		// Starting up saves state too, so it can crash as well.
		if crashed := survive(func() { a, err = agent.New(d.env.config, d.deps()) }); crashed {
			return d.crashed(now)
		}
		if err != nil {
			d.env.logger.LogAttrs(ctx, slog.LevelError, "agent failed to start", slog.String("device_id", d.id), slog.Any("error", err))
			return time.Minute
		}
		d.agent = a
	}
	d.steps++
	// Only a failure in this step counts, not one left over from a step that crashed.
	d.chunkFailed = false
	var wait time.Duration
	var err error
	if crashed := survive(func() { wait, err = d.agent.Step(ctx) }); crashed {
		return d.crashed(now)
	}
	if err != nil {
		d.env.logger.LogAttrs(ctx, slog.LevelError, "agent step failed", slog.String("device_id", d.id), slog.Any("error", err))
		return 30 * time.Second
	}
	if d.chunkFailed {
		d.chunkFailed = false
		if d.env.restartDownloads {
			return d.startDownloadOver()
		}
	}
	return wait
}

// startDelay is how long a live device waits before its first step, so that devices don't all
// start at the same instant.
func (d *device) startDelay() time.Duration {
	within := d.env.startWithin
	if within <= 0 {
		within = time.Second
	}
	return time.Duration(d.env.decide.value(d.id, "start", 0) * float64(within))
}

// startDownloadOver discards the partial download and restarts the agent, which then finds
// less than it had verified, and starts from the first chunk.
func (d *device) startDownloadOver() time.Duration {
	if work := d.agent.Checkpoint().Work; work != nil {
		if err := d.partials.Remove(work.Artifact.ID); err != nil {
			panic(fmt.Sprintf("remove a simulated partial download: %v", err))
		}
	}
	d.agent = nil
	return 0
}

// crashed throws the agent away, and schedules its restart.
func (d *device) crashed(now time.Time) time.Duration {
	d.agent = nil
	d.crashes++
	// It takes at least a tenth of the longest restart to come back.
	within := float64(d.env.faults.RestartWithin)
	delay := time.Duration(within/10 + d.env.decide.value(d.id, "restart", d.crashes)*within*9/10)
	d.downUntil = now.Add(delay)
	return delay
}

// survive runs fn, and reports whether a simulated crash stopped it. Any other panic is a
// real bug, and keeps going.
func survive(fn func()) (crashed bool) {
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(crash); !ok {
				panic(v)
			}
			crashed = true
		}
	}()
	fn()
	return false
}

// quiet reports whether the device has nothing in progress and nothing left to report.
func (d *device) quiet() bool {
	if d.agent == nil {
		return false
	}
	cp := d.agent.Checkpoint()
	return cp.Work == nil && len(cp.Outbox) == 0
}

// crashingCheckpoints kills the agent just before or just after a save.
type crashingCheckpoints struct {
	*agent.MemoryCheckpoints
	device *device
}

func (c crashingCheckpoints) Save(cp agent.Checkpoint) error {
	d := c.device
	if d.hit("crash-save", d.env.faults.Crash) {
		d.faults["crash"]++
		if d.env.decide.value(d.id, "crash-side", d.crashes) < 0.5 {
			panic(crash{})
		}
		if err := c.MemoryCheckpoints.Save(cp); err != nil {
			return err
		}
		panic(crash{})
	}
	return c.MemoryCheckpoints.Save(cp)
}

// faultyInstaller can crash during an install, and fails health checks on devices where
// this release is unhealthy.
type faultyInstaller struct {
	*agent.MemoryInstaller
	device *device
}

func (f faultyInstaller) Install(ctx context.Context, slot string, content io.Reader, size int64) error {
	d := f.device
	if d.hit("crash-install", d.env.faults.Crash) {
		d.faults["crash"]++
		panic(crash{})
	}
	return f.MemoryInstaller.Install(ctx, slot, content, size)
}

func (f faultyInstaller) HealthCheck(ctx context.Context, slot string) error {
	if err := f.MemoryInstaller.HealthCheck(ctx, slot); err != nil {
		return err
	}
	if f.device.unhealthy && f.device.isBadRelease(slot) {
		return errors.New("simulated: the release fails on this device")
	}
	return nil
}

// isBadRelease reports whether the release in slot is the one that's unhealthy, which is every
// release unless Faults.UnhealthyRelease names one.
func (d *device) isBadRelease(slot string) bool {
	bad := d.env.faults.UnhealthyRelease
	if bad == 0 {
		return true
	}
	cp, _, err := d.checkpoints.Load()
	if err != nil {
		return true
	}
	// A forward install checks the release it's installing. A rollback checks the release
	// it's going back to, in the previous slot.
	var release int64
	switch {
	case cp.Work != nil && cp.Work.Kind != agent.KindRollback && cp.Work.PendingSlot == slot:
		release = cp.Work.Artifact.ReleaseCounter
	case cp.Previous != nil:
		release = cp.Previous.Release
	}
	return release == bad
}

// faultTransport injects network faults into a device's requests.
type faultTransport struct {
	next   http.RoundTripper
	device *device
}

func operation(path string) string {
	switch {
	case strings.HasSuffix(path, "/poll"):
		return "poll"
	case strings.HasSuffix(path, "/reports"):
		return "report"
	case strings.HasSuffix(path, "/manifest"):
		return "manifest"
	case strings.HasSuffix(path, "/content"):
		return "content"
	default:
		return "other"
	}
}

func (t *faultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	d := t.device
	f := d.env.faults
	op := operation(req.URL.Path)
	if d.offline(d.env.clock.Now()) {
		d.faults["offline_refusal"]++
		d.noteChunkFailure(op)
		return nil, errOffline
	}
	if d.env.live && f.Latency > 0 {
		delay := time.Duration(d.env.decide.value(d.id, op+"-latency", d.next(op+"-latency")) * float64(f.Latency))
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(delay):
		}
	}
	if d.hit(op+"-drop", f.Drop) {
		d.faults["drop"]++
		d.noteChunkFailure(op)
		return nil, errDropped
	}
	if op == "report" {
		d.noteFailure(req)
	}
	if op == "report" && d.hit("report-duplicate", f.DuplicateReport) {
		if duplicate, err := cloneRequest(req); err == nil {
			if resp, err := t.next.RoundTrip(duplicate); err == nil {
				_ = resp.Body.Close()
				d.faults["duplicate_report"]++
			}
		}
	}

	resp, err := t.next.RoundTrip(req)
	if err != nil {
		d.noteChunkFailure(op)
		return nil, err
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		d.faults["shed"]++
	}
	if d.hit(op+"-lost", f.LostResponse) {
		_ = resp.Body.Close()
		d.faults["lost_response"]++
		d.noteChunkFailure(op)
		return nil, errResponseLost
	}
	if op == "content" && resp.StatusCode < http.StatusMultipleChoices {
		switch {
		case d.hit("content-corrupt", f.Corrupt):
			d.faults["corrupt"]++
			d.chunkFailed = true
			resp.Body = &corruptingBody{body: resp.Body, at: int64(d.env.decide.value(d.id, "corrupt-at", d.faults["corrupt"]) * 1024)}
		case d.hit("content-truncate", f.Truncate):
			d.faults["truncate"]++
			d.chunkFailed = true
			resp.Body = &truncatedBody{body: resp.Body, remaining: int64(d.env.decide.value(d.id, "truncate-at", d.faults["truncate"]) * 1024)}
		case d.hit("content-break", f.Break):
			d.faults["break"]++
			d.chunkFailed = true
			at := d.env.decide.value(d.id, "break-at", d.faults["break"]) * float64(max(resp.ContentLength, 0))
			resp.Body = &truncatedBody{body: resp.Body, remaining: int64(at)}
		}
		resp.Body = &countingBody{body: resp.Body, device: d}
	}
	return resp, nil
}

// noteChunkFailure records that a request for content failed, or will.
func (d *device) noteChunkFailure(op string) {
	if op == "content" {
		d.chunkFailed = true
	}
}

// noteFailure records why the device says it failed, if it does.
func (d *device) noteFailure(req *http.Request) {
	if req.GetBody == nil {
		return
	}
	body, err := req.GetBody()
	if err != nil {
		return
	}
	defer func() { _ = body.Close() }()
	var report apiv1.ReportRequest
	if json.NewDecoder(body).Decode(&report) == nil && report.State == rollout.AssignmentFailed {
		d.failures[report.Sequence] = report.Reason
	}
}

func cloneRequest(req *http.Request) (*http.Request, error) {
	duplicate := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		duplicate.Body = body
	}
	return duplicate, nil
}

type corruptingBody struct {
	body io.ReadCloser
	at   int64
	read int64
}

func (b *corruptingBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if b.at >= b.read && b.at < b.read+int64(n) {
		p[b.at-b.read] ^= 0xff
	}
	b.read += int64(n)
	return n, err
}

func (b *corruptingBody) Close() error { return b.body.Close() }

type truncatedBody struct {
	body      io.ReadCloser
	remaining int64
}

func (b *truncatedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, errTruncated
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *truncatedBody) Close() error { return b.body.Close() }

type countingBody struct {
	body   io.ReadCloser
	device *device
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.device.bytes += int64(n)
	return n, err
}

func (b *countingBody) Close() error { return b.body.Close() }

// handlerTransport calls an HTTP handler directly, in the calling goroutine. That's what
// lets the deterministic runner keep everything on one goroutine.
type handlerTransport struct {
	handler http.Handler
}

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		// A handler can read the body after the client has moved on, so give it its own copy.
		data, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("read request body: %w", err)
		}
		_ = req.Body.Close()
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(data))
	}
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, req)
	return recorder.Result(), nil
}
