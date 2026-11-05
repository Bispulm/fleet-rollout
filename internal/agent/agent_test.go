package agent_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/agent"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/signing/signingtest"
)

const (
	testChunk    = 1024
	testRelease  = 14
	testPoll     = 60 * time.Second
	testLease    = 5 * time.Minute
	testPlatform = "linux/arm64"
)

// fakeClock is advanced by the test, or by the wait a step returned.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// fakeServer is a scriptable control plane holding one assignment.
type fakeServer struct {
	t        *testing.T
	envelope signing.Envelope
	manifest signing.Manifest
	content  []byte

	assignment *apiv1.DeviceAssignment

	reports      []apiv1.ReportRequest
	fetched      []int64 // Offsets asked for.
	bytesServed  int64
	polls        int // Successful polls only.
	pollErr      error
	reportErr    error
	manifestErr  error
	loseResponse int // Accept this many reports, then act as though they failed.

	// fetch, if set, replaces normal serving of a Range request.
	fetch func(offset, length int64) (client.RangeResponse, error)
}

func (s *fakeServer) Poll(context.Context) (apiv1.PollResponse, error) {
	if s.pollErr != nil {
		return apiv1.PollResponse{}, s.pollErr
	}
	s.polls++
	resp := apiv1.PollResponse{PollAfterSeconds: int(testPoll / time.Second)}
	if s.assignment != nil {
		copied := *s.assignment
		resp.Assignment = &copied
	}
	return resp, nil
}

func (s *fakeServer) Report(_ context.Context, r apiv1.ReportRequest) (apiv1.ReportResponse, error) {
	if s.reportErr != nil {
		return apiv1.ReportResponse{}, s.reportErr
	}
	s.reports = append(s.reports, r)
	if s.loseResponse > 0 {
		s.loseResponse--
		return apiv1.ReportResponse{}, errors.New("connection lost before the response")
	}
	// A terminal report finishes the assignment, as it does on a real server.
	if r.State.IsTerminal() && s.assignment != nil && s.assignment.ID == r.AssignmentID {
		s.assignment = nil
	}
	return apiv1.ReportResponse{Applied: true}, nil
}

func (s *fakeServer) Manifest(context.Context, string) (signing.Envelope, error) {
	if s.manifestErr != nil {
		return signing.Envelope{}, s.manifestErr
	}
	return s.envelope, nil
}

func (s *fakeServer) serve(offset, length int64) client.RangeResponse {
	s.bytesServed += length
	return client.RangeResponse{
		StatusCode:   http.StatusPartialContent,
		ContentRange: fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, len(s.content)),
		ETag:         `"sha256:` + s.manifest.SHA256 + `"`,
		Body:         io.NopCloser(bytes.NewReader(s.content[offset : offset+length])),
	}
}

func (s *fakeServer) FetchRange(_ context.Context, _ string, offset, length int64) (client.RangeResponse, error) {
	s.fetched = append(s.fetched, offset)
	if s.fetch != nil {
		return s.fetch(offset, length)
	}
	return s.serve(offset, length), nil
}

func (s *fakeServer) states() []rollout.AssignmentState {
	out := make([]rollout.AssignmentState, len(s.reports))
	for i, r := range s.reports {
		out[i] = r.State
	}
	return out
}

func (s *fakeServer) last() apiv1.ReportRequest {
	s.t.Helper()
	if len(s.reports) == 0 {
		s.t.Fatal("no reports were sent")
	}
	return s.reports[len(s.reports)-1]
}

// fixture wires one agent to a fake server and in-memory boundaries.
type fixture struct {
	t           *testing.T
	ctx         context.Context
	clock       *fakeClock
	server      *fakeServer
	checkpoints *agent.MemoryCheckpoints
	partials    *agent.MemoryPartials
	installer   *agent.MemoryInstaller
	signer      ed25519.PrivateKey
	config      agent.Config
	agent       *agent.Agent
}

func testContent() []byte { return bytes.Repeat([]byte("artifact-bytes!!"), 400) } // 6,400 bytes.

func assignmentFor(m signing.Manifest) *apiv1.DeviceAssignment {
	return &apiv1.DeviceAssignment{
		ID: "assignment-1", RolloutID: "rollout-1", Generation: 1, Stage: 1, Kind: "forward",
		State: rollout.AssignmentAssigned, Directive: rollout.DirectiveProceed,
		LeaseSeconds: int(testLease / time.Second),
		Artifact: apiv1.Artifact{
			ID: "artifact-1", Name: m.Name, Version: m.Version, Platform: m.Platform,
			ReleaseCounter: m.ReleaseCounter, SizeBytes: m.SizeBytes, SHA256: m.SHA256, ChunkSize: m.ChunkSize,
		},
	}
}

func testMeta() signing.Meta {
	return signing.Meta{Name: "demo-app", Version: "1.4.0", Platform: testPlatform, ReleaseCounter: testRelease, ChunkSize: testChunk}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	signer := signingtest.NewKey(t)
	content := testContent()
	manifest, envelope := signingtest.Signed(t, signer, testMeta(), content)
	f := &fixture{
		t:     t,
		ctx:   t.Context(),
		clock: &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		server: &fakeServer{
			t: t, envelope: envelope, manifest: manifest, content: content,
			assignment: assignmentFor(manifest),
		},
		checkpoints: agent.NewMemoryCheckpoints(),
		partials:    agent.NewMemoryPartials(),
		installer:   agent.NewMemoryInstaller(),
		signer:      signer,
		config: agent.Config{
			DeviceID: "sim-00001",
			Platform: testPlatform,
			Trust:    signingtest.Trust(t, signer),
		},
	}
	f.restart()
	return f
}

func (f *fixture) deps() agent.Deps {
	return agent.Deps{
		Transport:   f.server,
		Checkpoints: f.checkpoints,
		Partials:    f.partials,
		Installer:   f.installer,
		Clock:       f.clock,
		Rand:        func() float64 { return 0.5 },
		Logger:      slog.New(slog.DiscardHandler),
	}
}

// restart builds a new agent from what's durable, as a real restart would.
func (f *fixture) restart() {
	f.t.Helper()
	a, err := agent.New(f.config, f.deps())
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	f.agent = a
}

// step runs one step and lets its wait elapse.
func (f *fixture) step() time.Duration {
	f.t.Helper()
	wait, err := f.agent.Step(f.ctx)
	if err != nil {
		f.t.Fatalf("Step: %v", err)
	}
	f.clock.now = f.clock.now.Add(wait)
	return wait
}

// runUntil steps until cond holds, failing the test if that takes too long.
func (f *fixture) runUntil(what string, cond func() bool) {
	f.t.Helper()
	for range 2000 {
		if cond() {
			return
		}
		f.step()
	}
	f.t.Fatalf("never reached: %s; reports = %v; checkpoint = %+v", what, f.server.states(), f.agent.Checkpoint())
}

// runToIdle steps until the assignment is over, and everything has been reported.
func (f *fixture) runToIdle() {
	f.t.Helper()
	f.runUntil("idle", f.isIdle)
}

// isIdle reports whether the server's assignment is finished, and the device has nothing
// left to do or say.
func (f *fixture) isIdle() bool {
	cp := f.agent.Checkpoint()
	return cp.Work == nil && len(cp.Outbox) == 0 && f.server.polls > 0 && f.server.assignment == nil
}

// reconfigure starts the device over from nothing, with a different configuration.
func (f *fixture) reconfigure(edit func(*agent.Config)) {
	f.t.Helper()
	edit(&f.config)
	f.checkpoints = agent.NewMemoryCheckpoints()
	f.restart()
}

var healthyPath = []rollout.AssignmentState{
	rollout.AssignmentDownloading, rollout.AssignmentVerifying, rollout.AssignmentInstalling,
	rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded,
}

func TestAHealthyUpdateRunsAllTheWayThrough(t *testing.T) {
	f := newFixture(t)
	before := f.agent.Checkpoint()
	f.runToIdle()

	if got := f.server.states(); !slices.Equal(got, healthyPath) {
		t.Fatalf("reports = %v, want %v", got, healthyPath)
	}
	for i, report := range f.server.reports {
		if report.Sequence != int64(i+1) || report.AssignmentID != "assignment-1" ||
			report.RolloutID != "rollout-1" || report.Generation != 1 {
			t.Fatalf("report %d = %+v", i, report)
		}
	}
	cp := f.agent.Checkpoint()
	if cp.ActiveSlot == before.ActiveSlot {
		t.Fatalf("the active slot didn't change from %q", before.ActiveSlot)
	}
	if cp.Active.Release != testRelease || cp.Active.SHA256 != f.server.manifest.SHA256 {
		t.Fatalf("active = %+v", cp.Active)
	}
	if cp.Previous == nil || cp.Previous.Release != 0 {
		t.Fatalf("previous = %+v, want what was running before", cp.Previous)
	}
	if got := f.installer.Content(cp.ActiveSlot); !bytes.Equal(got, f.server.content) {
		t.Fatalf("the active slot holds %d bytes, not the artifact", len(got))
	}
	if f.partials.Exists("artifact-1") {
		t.Fatal("the partial download wasn't cleaned up")
	}
}

func TestIdleDevicePollsOnAJitteredSchedule(t *testing.T) {
	f := newFixture(t)
	f.server.assignment = nil
	f.step()
	if f.server.polls != 1 {
		t.Fatalf("polls = %d", f.server.polls)
	}
	// Our fixed random value of 0.5 lands exactly on the interval. Other values spread it.
	wait := f.step()
	if wait <= 0 || wait > testPoll*5/4 {
		t.Fatalf("wait = %v, want it around the poll interval", wait)
	}
}

func TestPollIntervalIsSpreadOut(t *testing.T) {
	f := newFixture(t)
	f.server.assignment = nil
	deps := f.deps()
	deps.Rand = func() float64 { return 0 }
	low, err := agent.New(f.config, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wait, err := low.Step(f.ctx)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if wait >= testPoll || wait < testPoll/2 {
		t.Fatalf("wait = %v, want it spread below the interval of %v", wait, testPoll)
	}
}

func TestAnInterruptedDownloadResumesFromTheLastVerifiedChunk(t *testing.T) {
	f := newFixture(t)
	failed := false
	f.server.fetch = func(offset, length int64) (client.RangeResponse, error) {
		if offset == 3*testChunk && !failed {
			failed = true
			return client.RangeResponse{}, errors.New("connection reset")
		}
		return f.server.serve(offset, length), nil
	}
	f.runToIdle()

	if got := f.server.states(); !slices.Equal(got, healthyPath) {
		t.Fatalf("reports = %v", got)
	}
	// Each chunk comes down once, apart from the one that failed.
	want := []int64{0, testChunk, 2 * testChunk, 3 * testChunk, 3 * testChunk, 4 * testChunk, 5 * testChunk, 6 * testChunk}
	if !slices.Equal(f.server.fetched, want) {
		t.Fatalf("fetched = %v, want %v", f.server.fetched, want)
	}
}

func TestATruncatedChunkIsRetriedAndNotKept(t *testing.T) {
	f := newFixture(t)
	failed := false
	f.server.fetch = func(offset, length int64) (client.RangeResponse, error) {
		resp := f.server.serve(offset, length)
		if offset == testChunk && !failed {
			failed = true
			resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(f.server.content[offset:offset+10]), failingReader{}))
		}
		return resp, nil
	}
	f.runToIdle()
	if got := f.server.states(); !slices.Equal(got, healthyPath) {
		t.Fatalf("reports = %v", got)
	}
	if got := f.installer.Content(f.agent.Checkpoint().ActiveSlot); !bytes.Equal(got, f.server.content) {
		t.Fatal("the installed content is wrong")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset mid-body") }

func TestACorruptedChunkIsNeverKept(t *testing.T) {
	f := newFixture(t)
	corrupted := false
	f.server.fetch = func(offset, length int64) (client.RangeResponse, error) {
		resp := f.server.serve(offset, length)
		if offset == 2*testChunk && !corrupted {
			corrupted = true
			data := bytes.Clone(f.server.content[offset : offset+length])
			data[100] ^= 0xff
			resp.Body = io.NopCloser(bytes.NewReader(data))
		}
		return resp, nil
	}
	f.runToIdle()
	if got := f.server.states(); !slices.Equal(got, healthyPath) {
		t.Fatalf("reports = %v", got)
	}
	if got := f.installer.Content(f.agent.Checkpoint().ActiveSlot); !bytes.Equal(got, f.server.content) {
		t.Fatal("corruption reached the installed slot")
	}
}

func TestPersistentCorruptionFailsWithoutInstalling(t *testing.T) {
	f := newFixture(t)
	f.server.fetch = func(offset, length int64) (client.RangeResponse, error) {
		resp := f.server.serve(offset, length)
		resp.Body = io.NopCloser(bytes.NewReader(make([]byte, length)))
		return resp, nil
	}
	f.runToIdle()
	got := f.server.last()
	if got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonIntegrity {
		t.Fatalf("last report = %+v", got)
	}
	if f.installer.Installs() != 0 {
		t.Fatal("something was installed")
	}
	if f.partials.Exists("artifact-1") {
		t.Fatal("the partial download wasn't cleaned up")
	}
}

func TestResponsesThatDontProveWhatTheyAreAreDiscarded(t *testing.T) {
	cases := map[string]func(*client.RangeResponse){
		"wrong start":    func(r *client.RangeResponse) { r.ContentRange = "bytes 0-1023/6400" },
		"wrong end":      func(r *client.RangeResponse) { r.ContentRange = "bytes 1024-1500/6400" },
		"wrong total":    func(r *client.RangeResponse) { r.ContentRange = "bytes 1024-2047/9999" },
		"unknown total":  func(r *client.RangeResponse) { r.ContentRange = "bytes 1024-2047/*" },
		"missing range":  func(r *client.RangeResponse) { r.ContentRange = "" },
		"malformed":      func(r *client.RangeResponse) { r.ContentRange = "1024-2047" },
		"other artifact": func(r *client.RangeResponse) { r.ETag = `"sha256:other"` },
		"missing ETag":   func(r *client.RangeResponse) { r.ETag = "" },
		"odd status":     func(r *client.RangeResponse) { r.StatusCode = http.StatusNoContent },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			bad := true
			f.server.fetch = func(offset, length int64) (client.RangeResponse, error) {
				resp := f.server.serve(offset, length)
				if offset == testChunk && bad {
					bad = false
					edit(&resp)
				}
				return resp, nil
			}
			f.runToIdle()
			if got := f.server.states(); !slices.Equal(got, healthyPath) {
				t.Fatalf("reports = %v", got)
			}
			// The bad response was asked for again, rather than being kept.
			if count := countOf(f.server.fetched, testChunk); count != 2 {
				t.Fatalf("chunk 1 was fetched %d times, want 2", count)
			}
		})
	}
}

func countOf(values []int64, want int64) int {
	count := 0
	for _, v := range values {
		if v == want {
			count++
		}
	}
	return count
}

func TestAServerThatIgnoresRangeCausesASafeRestart(t *testing.T) {
	f := newFixture(t)
	f.server.fetch = func(offset, length int64) (client.RangeResponse, error) {
		if offset == 3*testChunk {
			// It ignores the request, and sends the whole artifact from the beginning.
			return client.RangeResponse{
				StatusCode: http.StatusOK,
				ETag:       `"sha256:` + f.server.manifest.SHA256 + `"`,
				Body:       io.NopCloser(bytes.NewReader(f.server.content)),
			}, nil
		}
		return f.server.serve(offset, length), nil
	}
	f.runToIdle()
	if got := f.server.states(); !slices.Equal(got, healthyPath) {
		t.Fatalf("reports = %v", got)
	}
	if got := f.installer.Content(f.agent.Checkpoint().ActiveSlot); !bytes.Equal(got, f.server.content) {
		t.Fatal("the installed content is wrong: incompatible bytes were appended")
	}
}

func TestAWholeResponseForADifferentArtifactIsRefused(t *testing.T) {
	f := newFixture(t)
	other := bytes.Repeat([]byte("something else!!"), 400)
	f.server.fetch = func(int64, int64) (client.RangeResponse, error) {
		return client.RangeResponse{
			StatusCode: http.StatusOK,
			ETag:       `"sha256:not-the-artifact"`,
			Body:       io.NopCloser(bytes.NewReader(other)),
		}, nil
	}
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonIntegrity {
		t.Fatalf("last report = %+v", got)
	}
	if f.installer.Installs() != 0 {
		t.Fatal("something was installed")
	}
}

func TestSignatureFailuresInstallNothing(t *testing.T) {
	stranger := signingtest.NewKey(t)
	cases := map[string]struct {
		envelope func(*fixture) signing.Envelope
		reason   string
	}{
		"unsigned": {
			envelope: func(f *fixture) signing.Envelope {
				e := f.server.envelope
				e.Signatures = nil
				return e
			},
			reason: agent.ReasonInvalidSignature,
		},
		"another key": {
			envelope: func(f *fixture) signing.Envelope {
				_, e := signingtest.Signed(t, stranger, testMeta(), f.server.content)
				return e
			},
			reason: agent.ReasonInvalidSignature,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.server.envelope = tc.envelope(f)
			f.runToIdle()
			got := f.server.last()
			if got.State != rollout.AssignmentFailed || got.Reason != tc.reason {
				t.Fatalf("last report = %+v, want failed with %q", got, tc.reason)
			}
			if len(f.server.fetched) != 0 {
				t.Fatal("it downloaded something before trusting the manifest")
			}
			if f.installer.Installs() != 0 {
				t.Fatal("something was installed")
			}
		})
	}
}

func TestAManifestThatDisagreesWithTheAssignmentIsRefused(t *testing.T) {
	cases := map[string]func(*apiv1.Artifact){
		"name":    func(a *apiv1.Artifact) { a.Name = "other-app" },
		"version": func(a *apiv1.Artifact) { a.Version = "9.9.9" },
		"release": func(a *apiv1.Artifact) { a.ReleaseCounter = testRelease + 1 },
		"size":    func(a *apiv1.Artifact) { a.SizeBytes++ },
		"digest":  func(a *apiv1.Artifact) { a.SHA256 = strings.Repeat("0", 64) },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			edit(&f.server.assignment.Artifact)
			f.runToIdle()
			if got := f.server.last(); got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonManifestMismatch {
				t.Fatalf("last report = %+v", got)
			}
			if f.installer.Installs() != 0 {
				t.Fatal("something was installed")
			}
		})
	}
}

func TestAnAssignmentForTheWrongPlatformIsRefused(t *testing.T) {
	f := newFixture(t)
	f.server.assignment.Artifact.Platform = "linux/amd64"
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonPlatformMismatch {
		t.Fatalf("last report = %+v", got)
	}
}

func TestASignedManifestForTheWrongPlatformIsRefused(t *testing.T) {
	f := newFixture(t)
	meta := testMeta()
	meta.Platform = "linux/amd64"
	m, envelope := signingtest.Signed(t, f.signer, meta, f.server.content)
	f.server.manifest, f.server.envelope = m, envelope
	// The assignment claims the device's own platform, but the signed manifest doesn't.
	f.runToIdle()
	got := f.server.last()
	if got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonManifestMismatch {
		t.Fatalf("last report = %+v", got)
	}
}

func TestAnArtifactOverTheDevicesLimitIsRefused(t *testing.T) {
	f := newFixture(t)
	f.reconfigure(func(c *agent.Config) { c.MaxArtifactBytes = 1024 })
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonTooLarge {
		t.Fatalf("last report = %+v", got)
	}
	if len(f.server.fetched) != 0 {
		t.Fatal("it started downloading something too large")
	}
}

func TestDowngradesAreRefused(t *testing.T) {
	f := newFixture(t)
	f.reconfigure(func(c *agent.Config) { c.InitialRelease = testRelease + 1 })
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonDowngrade {
		t.Fatalf("last report = %+v", got)
	}
	if f.installer.Installs() != 0 || len(f.server.fetched) != 0 {
		t.Fatal("a downgrade got as far as downloading or installing")
	}
}

func TestASameReleaseWithDifferentContentIsRefused(t *testing.T) {
	f := newFixture(t)
	f.reconfigure(func(c *agent.Config) { c.InitialRelease = testRelease })
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonDowngrade {
		t.Fatalf("last report = %+v", got)
	}
}

func TestAReleaseAlreadyRunningSucceedsWithoutReinstalling(t *testing.T) {
	f := newFixture(t)
	f.runToIdle()
	installs := f.installer.Installs()
	f.server.reports = nil

	// A follow-up rollout assigns the release this device already has.
	second := assignmentFor(f.server.manifest)
	second.ID = "assignment-2"
	second.RolloutID = "rollout-2"
	f.server.assignment = second
	f.clock.now = f.clock.now.Add(2 * testPoll)
	f.runUntil("the follow-up is reported", func() bool { return len(f.server.reports) == 2 && f.isIdle() })

	want := []rollout.AssignmentState{rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded}
	if got := f.server.states(); !slices.Equal(got, want) {
		t.Fatalf("reports = %v, want %v", got, want)
	}
	if f.installer.Installs() != installs {
		t.Fatal("it reinstalled a release it was already running")
	}
}

func TestAnUnhealthyReleaseIsRevertedAndReported(t *testing.T) {
	f := newFixture(t)
	f.installer.FailHealth(errors.New("service won't start"))
	before := f.agent.Checkpoint()
	f.runToIdle()

	want := []rollout.AssignmentState{
		rollout.AssignmentDownloading, rollout.AssignmentVerifying, rollout.AssignmentInstalling,
		rollout.AssignmentHealthCheck, rollout.AssignmentReverting, rollout.AssignmentFailed,
	}
	if got := f.server.states(); !slices.Equal(got, want) {
		t.Fatalf("reports = %v, want %v", got, want)
	}
	if got := f.server.last().Reason; got != agent.ReasonUnhealthy {
		t.Fatalf("reason = %q", got)
	}
	cp := f.agent.Checkpoint()
	if cp.ActiveSlot != before.ActiveSlot || cp.Active != before.Active {
		t.Fatalf("the active slot changed: %+v", cp)
	}
	if cp.Previous != nil {
		t.Fatalf("previous = %+v, want nothing recorded by a failed update", cp.Previous)
	}
	if got := f.installer.Content("b"); got != nil {
		t.Fatalf("the failed slot wasn't discarded: %d bytes", len(got))
	}
}

func TestAnInstallErrorIsRevertedAndReported(t *testing.T) {
	f := newFixture(t)
	f.installer.FailInstall(errors.New("disk full"))
	before := f.agent.Checkpoint()
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonInstallError {
		t.Fatalf("last report = %+v", got)
	}
	if got := f.agent.Checkpoint(); got.ActiveSlot != before.ActiveSlot {
		t.Fatalf("the active slot changed: %+v", got)
	}
}

func TestHoldStopsProgressUntilReleased(t *testing.T) {
	f := newFixture(t)
	f.runUntil("downloading", func() bool { return len(f.server.fetched) > 0 })
	f.server.assignment.Directive = rollout.DirectiveHold

	// A poll has to happen for the device to hear about it.
	f.clock.now = f.clock.now.Add(2 * testPoll)
	f.step()
	fetched := len(f.server.fetched)
	for range 20 {
		f.step()
	}
	if len(f.server.fetched) != fetched {
		t.Fatalf("kept downloading during a hold: %d, then %d", fetched, len(f.server.fetched))
	}
	if f.installer.Installs() != 0 {
		t.Fatal("installed during a hold")
	}

	f.server.assignment.Directive = rollout.DirectiveProceed
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentSucceeded {
		t.Fatalf("last report = %+v", got)
	}
}

func TestAnAssignmentThatDisappearsIsAbandoned(t *testing.T) {
	f := newFixture(t)
	f.runUntil("downloading", func() bool { return len(f.server.fetched) > 1 })
	if !f.partials.Exists("artifact-1") {
		t.Fatal("expected a partial download")
	}
	f.server.assignment = nil
	f.clock.now = f.clock.now.Add(2 * testPoll)
	f.runUntil("abandoned", func() bool { return f.agent.Checkpoint().Work == nil })

	if f.partials.Exists("artifact-1") {
		t.Fatal("the partial download wasn't deleted")
	}
	if f.installer.Installs() != 0 {
		t.Fatal("something was installed")
	}
	for _, state := range f.server.states() {
		if state.IsTerminal() {
			t.Fatalf("reported %s for work the server cancelled", state)
		}
	}
}

func TestLosingAccessMidDownloadIsCheckedByPolling(t *testing.T) {
	f := newFixture(t)
	f.server.fetch = func(offset, length int64) (client.RangeResponse, error) {
		if offset == testChunk {
			f.server.assignment = nil
			return client.RangeResponse{}, &client.APIError{Problem: apiv1.Problem{
				Status: http.StatusForbidden, Code: apiv1.CodeNoOpenAssignment,
			}}
		}
		return f.server.serve(offset, length), nil
	}
	f.runUntil("abandoned", func() bool { return f.agent.Checkpoint().Work == nil && f.server.polls > 1 })
	if f.installer.Installs() != 0 {
		t.Fatal("something was installed")
	}
}

func TestANewAssignmentReplacesOneThatDisappeared(t *testing.T) {
	f := newFixture(t)
	f.runUntil("downloading", func() bool { return len(f.server.fetched) > 1 })
	replacement := assignmentFor(f.server.manifest)
	replacement.ID = "assignment-2"
	f.server.assignment = replacement
	f.clock.now = f.clock.now.Add(2 * testPoll)
	f.runToIdle()
	if got := f.server.last(); got.AssignmentID != "assignment-2" || got.State != rollout.AssignmentSucceeded {
		t.Fatalf("last report = %+v", got)
	}
}

func TestNoInstallStartsAfterTheLeaseExpiresWithoutContact(t *testing.T) {
	f := newFixture(t)
	f.runUntil("ready to install", func() bool {
		work := f.agent.Checkpoint().Work
		return work != nil && work.Phase == agent.PhaseVerifying
	})
	// Contact is lost, and enough time passes for the lease to run out.
	f.server.pollErr = errors.New("network unreachable")
	f.server.reportErr = errors.New("network unreachable")
	f.clock.now = f.clock.now.Add(testLease + time.Second)
	for range 50 {
		f.step()
	}
	if f.installer.Installs() != 0 {
		t.Fatal("it started installing with an expired lease")
	}
	if phase := f.agent.Checkpoint().Work.Phase; phase != agent.PhaseReady {
		t.Fatalf("phase = %s, want it waiting at ready", phase)
	}

	// Contact comes back, and a fresh poll renews the lease.
	f.server.pollErr, f.server.reportErr = nil, nil
	f.runToIdle()
	if f.installer.Installs() != 1 {
		t.Fatalf("installs = %d", f.installer.Installs())
	}
	if got := f.server.last(); got.State != rollout.AssignmentSucceeded {
		t.Fatalf("last report = %+v", got)
	}
}

func TestAHaltDuringTheLeaseStopsTheInstallAtTheLastPoll(t *testing.T) {
	f := newFixture(t)
	f.runUntil("verifying", func() bool {
		work := f.agent.Checkpoint().Work
		return work != nil && work.Phase == agent.PhaseVerifying
	})
	// The lease is still valid, but the assignment has been cancelled in the meantime.
	f.server.assignment = nil
	f.runUntil("abandoned", func() bool { return f.agent.Checkpoint().Work == nil })
	if f.installer.Installs() != 0 {
		t.Fatal("it installed something the server had cancelled")
	}
}

func TestAnInstallCanProceedOnAValidLeaseWithoutContact(t *testing.T) {
	f := newFixture(t)
	f.runUntil("verifying", func() bool {
		work := f.agent.Checkpoint().Work
		return work != nil && work.Phase == agent.PhaseVerifying
	})
	f.server.pollErr = errors.New("network unreachable")
	f.server.reportErr = errors.New("network unreachable")
	f.runUntil("installed", func() bool { return f.installer.Installs() == 1 })
}

func TestInFlightWorkFinishesWithoutTheNetwork(t *testing.T) {
	f := newFixture(t)
	f.runUntil("installing", func() bool {
		work := f.agent.Checkpoint().Work
		return work != nil && work.Phase == agent.PhaseInstalling
	})
	// A device that has started changing itself has to finish, whatever else happens.
	f.server.pollErr = errors.New("network unreachable")
	f.server.reportErr = errors.New("network unreachable")
	f.runUntil("committed", func() bool { return f.agent.Checkpoint().Active.Release == testRelease })

	// The reports wait until they can be delivered.
	f.server.pollErr, f.server.reportErr = nil, nil
	f.runToIdle()
	if got := f.server.states(); !slices.Equal(got, healthyPath) {
		t.Fatalf("reports = %v", got)
	}
}

func TestAnInFlightAssignmentFromBeforeLostStateIsReportedFailed(t *testing.T) {
	f := newFixture(t)
	f.server.assignment.State = rollout.AssignmentInstalling
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonLostState {
		t.Fatalf("last report = %+v", got)
	}
}

func TestReportsThatLoseTheirResponseAreSentAgainIdentically(t *testing.T) {
	f := newFixture(t)
	f.server.loseResponse = 3
	f.runToIdle()

	byHead := map[int64][]apiv1.ReportRequest{}
	for _, report := range f.server.reports {
		byHead[report.Sequence] = append(byHead[report.Sequence], report)
	}
	for sequence, copies := range byHead {
		for _, copied := range copies {
			if !reflect.DeepEqual(copied, copies[0]) {
				t.Fatalf("sequence %d changed between attempts: %+v and %+v", sequence, copied, copies[0])
			}
		}
	}
	if len(f.server.reports) != len(healthyPath)+3 {
		t.Fatalf("reports sent = %d, want every state plus 3 retries", len(f.server.reports))
	}
}

func TestReportsStayInOrderAcrossFailures(t *testing.T) {
	f := newFixture(t)
	f.server.reportErr = errors.New("network unreachable")
	f.runUntil("work done and waiting", func() bool {
		cp := f.agent.Checkpoint()
		return cp.Work == nil && len(cp.Outbox) == len(healthyPath)
	})
	f.server.reportErr = nil
	f.runToIdle()
	if got := f.server.states(); !slices.Equal(got, healthyPath) {
		t.Fatalf("reports = %v, want them in order", got)
	}
}

func TestPermanentReportFailuresAreDroppedSoTheyCantBlockEverything(t *testing.T) {
	f := newFixture(t)
	f.server.reportErr = &client.APIError{Problem: apiv1.Problem{Status: http.StatusConflict, Code: apiv1.CodeReportConflict}}
	f.runUntil("installed, with nothing left to send", func() bool {
		cp := f.agent.Checkpoint()
		return f.installer.Installs() == 1 && cp.Work == nil && len(cp.Outbox) == 0
	})
}

func TestTransportFailuresBackOffAndRecover(t *testing.T) {
	f := newFixture(t)
	f.server.pollErr = errors.New("network unreachable")
	first := f.step()
	var waits []time.Duration
	for range 12 {
		waits = append(waits, f.step())
	}
	if !slices.IsSorted(waits) {
		t.Fatalf("waits = %v, want them growing", waits)
	}
	if waits[0] < first {
		t.Fatalf("waits = %v after %v", waits, first)
	}
	if biggest := waits[len(waits)-1]; biggest > 10*time.Minute {
		t.Fatalf("the wait grew without a bound: %v", biggest)
	}

	f.server.pollErr = nil
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentSucceeded {
		t.Fatalf("last report = %+v", got)
	}
}

func TestRetryAfterIsRespected(t *testing.T) {
	f := newFixture(t)
	f.server.pollErr = &client.APIError{
		Problem:    apiv1.Problem{Status: http.StatusServiceUnavailable},
		RetryAfter: 90 * time.Second,
	}
	if wait := f.step(); wait < 90*time.Second {
		t.Fatalf("wait = %v, want at least the 90s the server asked for", wait)
	}
}

func TestARejectedCredentialSlowsDownAndKeepsState(t *testing.T) {
	f := newFixture(t)
	f.runUntil("downloading", func() bool { return len(f.server.fetched) > 1 })
	before := f.agent.Checkpoint()
	f.server.pollErr = &client.APIError{Problem: apiv1.Problem{Status: http.StatusUnauthorized}}
	f.clock.now = f.clock.now.Add(2 * testPoll)
	if wait := f.step(); wait < 2*time.Minute {
		t.Fatalf("wait = %v, want it slowed right down", wait)
	}
	if got := f.agent.Checkpoint(); got.Work == nil || got.Work.AssignmentID != before.Work.AssignmentID {
		t.Fatalf("a bad credential lost the device's work: %+v", got)
	}
}

func TestIntegrityFailuresReachTheServerAsPermanentFailures(t *testing.T) {
	f := newFixture(t)
	f.server.manifestErr = &client.APIError{Problem: apiv1.Problem{Status: http.StatusForbidden, Code: apiv1.CodeNoOpenAssignment}}
	f.server.assignment = nil
	f.step()
	if f.server.polls == 0 {
		t.Fatal("no poll happened")
	}
}

func TestSequenceNumbersSurviveARestart(t *testing.T) {
	f := newFixture(t)
	f.runUntil("some reports sent", func() bool { return len(f.server.reports) >= 2 })
	highest := f.server.last().Sequence
	f.restart()
	f.runToIdle()
	for _, report := range f.server.reports {
		if report.Sequence > highest {
			return
		}
	}
	t.Fatal("no new report used a higher sequence after the restart")
}

func TestACheckpointThatCantBeSavedStopsTheStep(t *testing.T) {
	f := newFixture(t)
	f.checkpoints.FailSaves(errors.New("disk full"))
	_, err := f.agent.Step(f.ctx)
	if err == nil {
		t.Fatal("Step ignored a failed save")
	}
	// What's in memory never gets ahead of what's on disk.
	if got := f.agent.Checkpoint(); got.Work != nil {
		t.Fatalf("checkpoint = %+v", got)
	}
	f.checkpoints.FailSaves(nil)
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentSucceeded {
		t.Fatalf("last report = %+v", got)
	}
}

// crash is what fault wrappers panic with, to stop an agent exactly where it is.
type crash struct{ boundary string }

// crashingCheckpoints dies around the nth save.
type crashingCheckpoints struct {
	*agent.MemoryCheckpoints
	counter *boundaryCounter
}

func (c crashingCheckpoints) Save(cp agent.Checkpoint) error {
	c.counter.hit("before save")
	if err := c.MemoryCheckpoints.Save(cp); err != nil {
		return err
	}
	c.counter.hit("after save")
	return nil
}

// crashingInstaller dies around each installer operation.
type crashingInstaller struct {
	*agent.MemoryInstaller
	counter *boundaryCounter
}

func (c crashingInstaller) Install(ctx context.Context, slot string, content io.Reader, size int64) error {
	c.counter.hit("before install")
	if err := c.MemoryInstaller.Install(ctx, slot, content, size); err != nil {
		return err
	}
	c.counter.hit("after install")
	return nil
}

func (c crashingInstaller) HealthCheck(ctx context.Context, slot string) error {
	c.counter.hit("before health check")
	err := c.MemoryInstaller.HealthCheck(ctx, slot)
	c.counter.hit("after health check")
	return err
}

// crashingPartials dies around each append.
type crashingPartials struct {
	*agent.MemoryPartials
	counter *boundaryCounter
}

func (c crashingPartials) Append(id string, data []byte) error {
	c.counter.hit("before append")
	if err := c.MemoryPartials.Append(id, data); err != nil {
		return err
	}
	c.counter.hit("after append")
	return nil
}

type boundaryCounter struct {
	target  int
	current int
	fired   string
}

func (b *boundaryCounter) hit(name string) {
	if b.current == b.target {
		b.current++
		b.fired = name
		panic(crash{boundary: name})
	}
	b.current++
}

// TestRecoveryFromACrashAtEveryBoundary kills the agent at each point where it touches
// durable state, one point per run, until a whole run gets through without reaching the
// target. Whatever the point, the outcome has to be consistent, and has to be reported.
func TestRecoveryFromACrashAtEveryBoundary(t *testing.T) {
	boundaries := map[string]int{}
	for target := 0; ; target++ {
		f := newFixture(t)
		counter := &boundaryCounter{target: target}
		deps := f.deps()
		deps.Checkpoints = crashingCheckpoints{f.checkpoints, counter}
		deps.Installer = crashingInstaller{f.installer, counter}
		deps.Partials = crashingPartials{f.partials, counter}

		a, err := agent.New(f.config, deps)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		initial := a.Checkpoint()

		crashed := func() (crashed bool) {
			defer func() {
				if v := recover(); v != nil {
					if _, ok := v.(crash); !ok {
						panic(v)
					}
					crashed = true
				}
			}()
			for range 500 {
				wait, err := a.Step(f.ctx)
				if err != nil {
					t.Fatalf("target %d: Step: %v", target, err)
				}
				f.clock.now = f.clock.now.Add(wait)
				cp := a.Checkpoint()
				if cp.Work == nil && len(cp.Outbox) == 0 && f.server.polls > 0 {
					return false
				}
			}
			t.Fatalf("target %d: the first run never finished", target)
			return false
		}()
		if !crashed {
			if target < 10 {
				t.Fatalf("only %d boundaries were reached", target)
			}
			t.Logf("crashed at each of %d boundaries: %v", target, boundaries)
			return
		}
		boundaries[counter.fired]++

		// What was durable when it died decides what recovery has to do.
		durable, _, err := f.checkpoints.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		mustRevert := durable.Work != nil &&
			(durable.Work.Phase == agent.PhaseInstalling || durable.Work.Phase == agent.PhaseHealthCheck)

		// Restart from what's durable, with nothing failing this time.
		f.restart()
		f.runToIdle()
		assertConsistentOutcome(t, f, target, counter.fired, initial)
		if mustRevert {
			// An install or health check that may not have finished can't be trusted, even
			// though running it again might well work.
			label := fmt.Sprintf("crash %d (%s)", target, counter.fired)
			if last := f.server.last(); last.State != rollout.AssignmentFailed || last.Reason != agent.ReasonInterrupted {
				t.Fatalf("%s: died with an install in progress, then reported %+v", label, last)
			}
			if !slices.Contains(f.server.states(), rollout.AssignmentReverting) {
				t.Fatalf("%s: no revert was reported: %v", label, f.server.states())
			}
		}
	}
}

func assertConsistentOutcome(t *testing.T, f *fixture, target int, boundary string, initial agent.Checkpoint) {
	t.Helper()
	label := fmt.Sprintf("crash %d (%s)", target, boundary)
	cp := f.agent.Checkpoint()
	if cp.Work != nil || len(cp.Outbox) != 0 {
		t.Fatalf("%s: not finished: %+v", label, cp)
	}

	// Each sequence was used for one report, however many times it was sent.
	seen := map[int64]apiv1.ReportRequest{}
	for _, report := range f.server.reports {
		if earlier, ok := seen[report.Sequence]; ok && !reflect.DeepEqual(earlier, report) {
			t.Fatalf("%s: sequence %d was used for two different reports: %+v, %+v", label, report.Sequence, earlier, report)
		}
		seen[report.Sequence] = report
	}

	last := f.server.last()
	switch last.State {
	case rollout.AssignmentSucceeded:
		// If it claims success, then the new release must really be running.
		if cp.Active.Release != testRelease || cp.Active.SHA256 != f.server.manifest.SHA256 {
			t.Fatalf("%s: reported success, but running %+v", label, cp.Active)
		}
		if got := f.installer.Content(cp.ActiveSlot); !bytes.Equal(got, f.server.content) {
			t.Fatalf("%s: reported success, but the active slot has the wrong bytes", label)
		}
		if !slices.Contains(f.server.states(), rollout.AssignmentHealthCheck) {
			t.Fatalf("%s: reported success without a health check: %v", label, f.server.states())
		}
	case rollout.AssignmentFailed:
		// If it failed, then it must still be running what it had before.
		if cp.ActiveSlot != initial.ActiveSlot || cp.Active != initial.Active {
			t.Fatalf("%s: reported failure, but running %+v in %s", label, cp.Active, cp.ActiveSlot)
		}
		if last.Reason != agent.ReasonInterrupted {
			t.Fatalf("%s: failure reason = %q", label, last.Reason)
		}
	default:
		t.Fatalf("%s: last report = %+v, want a final one", label, last)
	}
}

func TestACrashDuringDownloadDiscardsAnythingUnverified(t *testing.T) {
	f := newFixture(t)
	f.runUntil("two chunks", func() bool {
		work := f.agent.Checkpoint().Work
		return work != nil && work.VerifiedChunks == 2
	})
	// Bytes the agent wrote but hadn't recorded as verified, as a crash could leave.
	if err := f.partials.Append("artifact-1", []byte("garbage that never got verified")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	f.restart()
	if got := f.partials.Size("artifact-1"); got != 2*testChunk {
		t.Fatalf("partial = %d bytes after restart, want exactly the 2 verified chunks", got)
	}
	f.runToIdle()
	if got := f.installer.Content(f.agent.Checkpoint().ActiveSlot); !bytes.Equal(got, f.server.content) {
		t.Fatal("unverified bytes reached the installed slot")
	}
	// It carried on from where it was, rather than starting over.
	if got := countOf(f.server.fetched, 0); got != 1 {
		t.Fatalf("the first chunk was fetched %d times", got)
	}
}

func TestStoredBytesThatGoBadAreCaughtBeforeInstalling(t *testing.T) {
	f := newFixture(t)
	f.runUntil("downloaded", func() bool {
		work := f.agent.Checkpoint().Work
		return work != nil && work.Phase == agent.PhaseVerifying
	})
	// Every chunk passed when it arrived, but storage corrupted one afterward.
	f.partials.Corrupt("artifact-1", 3*testChunk+7)
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonIntegrity {
		t.Fatalf("last report = %+v", got)
	}
	if f.installer.Installs() != 0 {
		t.Fatal("corrupted bytes were installed")
	}
}

func TestACheckpointThatNoLongerVerifiesIsNotTrusted(t *testing.T) {
	f := newFixture(t)
	f.runUntil("two chunks", func() bool {
		work := f.agent.Checkpoint().Work
		return work != nil && work.VerifiedChunks == 2
	})
	// The saved manifest fails verification, perhaps because trust changed.
	f.config.Trust = signingtest.Trust(t, signingtest.NewKey(t))
	f.restart()
	f.runToIdle()
	if got := f.server.last(); got.State != rollout.AssignmentFailed || got.Reason != agent.ReasonInvalidSignature {
		t.Fatalf("last report = %+v", got)
	}
	if f.installer.Installs() != 0 {
		t.Fatal("something was installed")
	}
}

func TestRunStopsWhenTheContextEnds(t *testing.T) {
	f := newFixture(t)
	f.server.assignment = nil
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.agent.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't stop")
	}
}
