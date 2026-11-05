// Package agent is the device side of a rollout: it polls for assignments, downloads and
// verifies artifacts, installs into an inactive slot, and reports what happened.
//
// The agent is a state machine advanced by Step. A step does a bounded amount of work and
// returns how long to wait, so nothing in here sleeps. Everything it touches outside itself
// is behind an interface: the control plane, durable state, partial downloads, the installer,
// and the clock. The standalone agent and the simulator run exactly this code, and differ
// only in what they plug in.
//
// Every state change is saved before it's acted on. docs/rollout-semantics.md describes what
// that guarantees after a crash.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/retry"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
)

// Why an assignment failed. A device reports these to the server, so they're stable.
const (
	ReasonInvalidSignature = "invalid_signature"
	ReasonInvalidManifest  = "invalid_manifest"
	ReasonManifestMismatch = "manifest_mismatch"
	ReasonPlatformMismatch = "platform_mismatch"
	ReasonTooLarge         = "too_large"
	ReasonDowngrade        = "downgrade"
	ReasonIntegrity        = "integrity"
	ReasonInstallError     = "install_error"
	ReasonUnhealthy        = "unhealthy"
	ReasonInterrupted      = "interrupted"
	ReasonLostState        = "lost_state"
	ReasonUnsupportedKind  = "unsupported_kind"
	ReasonNothingToRestore = "nothing_to_restore"
	ReasonRestoreUnhealthy = "restore_unhealthy"
)

const (
	defaultMaxArtifactBytes = 1 << 30
	defaultPollInterval     = time.Minute
	pollJitterFraction      = 0.25
	backoffBase             = time.Second
	backoffMax              = 5 * time.Minute
	// maxIntegrityStrikes is how many bad responses in a row a device tolerates. A few can
	// be an unreliable network. More than that, and it stops trusting the source.
	maxIntegrityStrikes = 5
	fetchTimeout        = 2 * time.Minute
	// A device installs into one of two slots, and the other keeps running.
	slotA, slotB = "a", "b"
)

// Transport is the control plane, as a device sees it. *client.DeviceClient implements it.
type Transport interface {
	Poll(ctx context.Context) (apiv1.PollResponse, error)
	Report(ctx context.Context, report apiv1.ReportRequest) (apiv1.ReportResponse, error)
	Manifest(ctx context.Context, artifactID string) (signing.Envelope, error)
	FetchRange(ctx context.Context, artifactID string, offset, length int64) (client.RangeResponse, error)
}

// Checkpoints stores the agent's state. Save must be atomic: after a crash, Load returns the
// last state saved in full, and never a partial one.
type Checkpoints interface {
	Load() (Checkpoint, bool, error)
	Save(Checkpoint) error
}

// Partials stores artifacts while they download. Append must be durable when it returns.
// ErrPartialShort reports a partial download shorter than asked for, as one can be when
// power is lost before a new file's directory entry is on disk.
var ErrPartialShort = errors.New("partial download is shorter than expected")

type Partials interface {
	// Truncate discards everything past size, creating the partial if needed. It never
	// extends a partial, and returns ErrPartialShort if it's shorter than size.
	Truncate(artifactID string, size int64) error
	Append(artifactID string, data []byte) error
	Open(artifactID string) (io.ReadCloser, error)
	Remove(artifactID string) error
	// RemoveExcept deletes every partial but the one named, which may be empty.
	RemoveExcept(artifactID string) error
}

// Installer writes artifacts into slots. Installation is simulated: artifacts are opaque
// bytes, and nothing unpacks or runs them.
type Installer interface {
	Install(ctx context.Context, slot string, content io.Reader, size int64) error
	// HealthCheck returns an error if the release in a slot is unhealthy.
	HealthCheck(ctx context.Context, slot string) error
	// Discard removes a slot's content. Discarding an empty slot is fine.
	Discard(ctx context.Context, slot string) error
}

// Clock tells the time.
type Clock interface {
	Now() time.Time
}

// Deps are the agent's boundaries with everything outside it.
type Deps struct {
	Transport   Transport
	Checkpoints Checkpoints
	Partials    Partials
	Installer   Installer
	Clock       Clock
	// Rand returns a value in [0, 1). It decides jitter.
	Rand   func() float64
	Logger *slog.Logger
}

// Config describes the device.
type Config struct {
	DeviceID string
	Platform string
	// Trust is who the device accepts signatures from. It's independent of the server.
	Trust signing.TrustSet
	// MaxArtifactBytes is the largest artifact this device will accept.
	MaxArtifactBytes int64
	// InitialRelease is what the device is running before its first update.
	InitialRelease int64
}

// Phase is how far along an assignment is.
type Phase string

// The phases. PhaseReady means fully verified, and waiting for permission to install.
const (
	PhaseManifest    Phase = "manifest"
	PhaseDownloading Phase = "downloading"
	PhaseVerifying   Phase = "verifying"
	PhaseReady       Phase = "ready"
	PhaseInstalling  Phase = "installing"
	PhaseHealthCheck Phase = "health_check"
	PhaseReverting   Phase = "reverting"
	// PhaseRestoring is a rollback switching back to the previous slot, once the server agreed.
	PhaseRestoring Phase = "restoring"
)

// The kinds of work.
const (
	KindForward  = "forward"
	KindRollback = "rollback"
)

// Installed describes the release in a slot.
type Installed struct {
	ArtifactID string `json:"artifact_id,omitempty"`
	Version    string `json:"version,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Release    int64  `json:"release"`
}

// Work is an assignment in progress.
type Work struct {
	// Kind is forward or rollback. Empty means forward, from before rollback existed.
	Kind string `json:"kind,omitempty"`
	// RestoreRelease is the release a rollback restores.
	RestoreRelease int64          `json:"restore_release,omitempty"`
	AssignmentID   string         `json:"assignment_id"`
	RolloutID      string         `json:"rollout_id"`
	Generation     int64          `json:"generation"`
	Artifact       apiv1.Artifact `json:"artifact"`
	Phase          Phase          `json:"phase"`
	// Envelope is the signed manifest. It's verified again whenever it's loaded.
	Envelope *signing.Envelope `json:"envelope,omitempty"`
	// VerifiedChunks is how many chunks of the partial download passed their digest.
	VerifiedChunks int `json:"verified_chunks"`
	// Strikes counts bad responses in a row.
	Strikes int `json:"strikes"`
	// Held means the server said to wait.
	Held bool `json:"held"`
	// Claim is the report of installing that asks the server for permission to start. It's
	// saved before it's sent, so that a retry is identical and counts as a duplicate.
	Claim       *apiv1.ReportRequest `json:"claim,omitempty"`
	PendingSlot string               `json:"pending_slot,omitempty"`
	// FailReason says why a revert is happening.
	FailReason string `json:"fail_reason,omitempty"`
}

// Checkpoint is everything needed to recover after a crash.
type Checkpoint struct {
	// Sequence is the last report sequence used. It only ever goes up.
	Sequence int64 `json:"sequence"`
	// Outbox holds reports not yet acknowledged, oldest first.
	Outbox     []apiv1.ReportRequest `json:"outbox"`
	ActiveSlot string                `json:"active_slot"`
	Active     Installed             `json:"active"`
	// Previous is the last known-healthy release before the active one.
	Previous *Installed `json:"previous,omitempty"`
	// Work is the assignment in progress, or nil when idle.
	Work *Work `json:"work,omitempty"`
}

func (c Checkpoint) clone() Checkpoint {
	out := c
	out.Outbox = slices.Clone(c.Outbox)
	if c.Previous != nil {
		previous := *c.Previous
		out.Previous = &previous
	}
	if c.Work != nil {
		work := *c.Work
		if c.Work.Claim != nil {
			claim := *c.Work.Claim
			work.Claim = &claim
		}
		out.Work = &work
	}
	return out
}

// awaitingDelivery reports whether the outbox holds a report about an assignment.
func (c Checkpoint) awaitingDelivery(assignmentID string) bool {
	for _, report := range c.Outbox {
		if report.AssignmentID == assignmentID {
			return true
		}
	}
	return false
}

// report allocates the next sequence for a report.
func (c *Checkpoint) report(rolloutID, assignmentID string, generation int64, state rollout.AssignmentState, reason string) apiv1.ReportRequest {
	c.Sequence++
	return apiv1.ReportRequest{
		Sequence:     c.Sequence,
		RolloutID:    rolloutID,
		AssignmentID: assignmentID,
		Generation:   generation,
		State:        state,
		Reason:       reason,
	}
}

// enqueue adds a report to the outbox. The caller saves the result before acting on it, so
// a report always exists for anything that happened.
func (c *Checkpoint) enqueue(rolloutID, assignmentID string, generation int64, state rollout.AssignmentState, reason string) {
	c.Outbox = append(c.Outbox, c.report(rolloutID, assignmentID, generation, state, reason))
}

// Agent runs one device. It isn't safe for concurrent use.
type Agent struct {
	config Config
	deps   Deps
	cp     Checkpoint

	// manifest is the verified form of the current work's envelope.
	manifest *signing.Manifest
	// partialDirty means a failed write may have left bytes past what's verified.
	partialDirty bool
	backoff      retry.Backoff
	failures     int

	nextPoll     time.Time
	nextFlush    time.Time
	pollInterval time.Duration
}

// New loads the device's state and recovers from however the last run ended.
func New(config Config, deps Deps) (*Agent, error) {
	if config.MaxArtifactBytes == 0 {
		config.MaxArtifactBytes = defaultMaxArtifactBytes
	}
	a := &Agent{
		config:       config,
		deps:         deps,
		backoff:      retry.Backoff{Base: backoffBase, Max: backoffMax, Rand: deps.Rand},
		nextPoll:     deps.Clock.Now(),
		pollInterval: defaultPollInterval,
	}
	cp, found, err := deps.Checkpoints.Load()
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	if !found {
		cp = Checkpoint{ActiveSlot: slotA, Active: Installed{Release: config.InitialRelease}}
		if err := deps.Checkpoints.Save(cp); err != nil {
			return nil, fmt.Errorf("save first checkpoint: %w", err)
		}
	}
	a.cp = cp
	if err := a.recover(); err != nil {
		return nil, err
	}
	return a, nil
}

// Checkpoint returns a copy of the current state.
func (a *Agent) Checkpoint() Checkpoint { return a.cp.clone() }

// commit saves a new state, and only then adopts it, so what's in memory is never ahead of
// what would survive a crash.
func (a *Agent) commit(next Checkpoint) error {
	if err := a.deps.Checkpoints.Save(next); err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}
	a.cp = next
	return nil
}

// recover makes durable state consistent again after a restart.
func (a *Agent) recover() error {
	work := a.cp.Work
	keep := ""
	if work != nil {
		keep = work.Artifact.ID
	}
	if err := a.deps.Partials.RemoveExcept(keep); err != nil {
		return fmt.Errorf("clean up partial downloads: %w", err)
	}
	if work == nil {
		return nil
	}

	if work.Kind == KindRollback {
		// Nothing was downloaded, and nothing changes until the one save that switches slots,
		// so a restore that was cut short just starts again.
		return nil
	}
	switch work.Phase {
	case PhaseInstalling, PhaseHealthCheck:
		// There's no way to prove the install or health check finished, so the new slot
		// can't be trusted. The active slot hasn't changed, because it changes only in the
		// same save that records success.
		next := a.cp.clone()
		next.Work.Phase = PhaseReverting
		next.Work.FailReason = ReasonInterrupted
		next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentReverting, ReasonInterrupted)
		return a.commit(next)
	case PhaseReverting, PhaseManifest:
		return nil
	}

	// A signature check here means a change of trust takes effect across a restart.
	manifest, err := a.verifyEnvelope(*work.Envelope)
	if err != nil {
		return a.failWork(reasonForVerifyError(err))
	}
	a.manifest = &manifest
	// Keep exactly what's verified, and nothing a crash may have left after it.
	err = a.truncateToVerified(manifest, work.VerifiedChunks)
	if !errors.Is(err, ErrPartialShort) {
		return err
	}
	// Some of what was verified is gone, and nothing says which, so start again.
	next := a.cp.clone()
	next.Work.VerifiedChunks = 0
	if err := a.commit(next); err != nil {
		return err
	}
	return a.truncateToVerified(manifest, 0)
}

func (a *Agent) truncateToVerified(m signing.Manifest, chunks int) error {
	size := m.ChunkOffset(chunks)
	if chunks == len(m.Chunks) {
		size = m.SizeBytes
	}
	if err := a.deps.Partials.Truncate(a.cp.Work.Artifact.ID, size); err != nil {
		return fmt.Errorf("truncate partial download: %w", err)
	}
	return nil
}

// Run steps until ctx ends. Only a failure to read or write durable state is logged here,
// and everything else is handled by the steps themselves.
func (a *Agent) Run(ctx context.Context) error {
	for {
		wait, err := a.Step(ctx)
		if err != nil {
			a.deps.Logger.LogAttrs(ctx, slog.LevelError, "agent step failed",
				slog.String("device_id", a.config.DeviceID), slog.Any("error", err))
			wait = a.backoff.Delay(a.failures)
			a.failures++
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// Step does the next piece of work and returns how long to wait before the next step. Work
// past the point of no return comes first, and needs no network, because a device that has
// started changing itself has to finish whatever else is going on. Reports go out in order,
// with their own backoff, and an undeliverable one doesn't hold up anything else. It returns
// an error only if durable state couldn't be updated.
func (a *Agent) Step(ctx context.Context) (time.Duration, error) {
	if work := a.cp.Work; work != nil {
		switch work.Phase {
		case PhaseInstalling:
			return 0, a.stepInstall(ctx)
		case PhaseHealthCheck:
			return 0, a.stepHealthCheck(ctx)
		case PhaseReverting:
			return 0, a.stepRevert(ctx)
		case PhaseRestoring:
			return 0, a.stepRestore(ctx)
		}
	}

	now := a.deps.Clock.Now()
	if len(a.cp.Outbox) > 0 && !now.Before(a.nextFlush) {
		if err := a.flush(ctx, now); err != nil {
			return 0, err
		}
	}

	if !now.Before(a.nextPoll) {
		wait, err := a.poll(ctx, now)
		if err != nil {
			return 0, err
		}
		if wait > 0 {
			return a.soonest(now, wait), nil
		}
	}

	work := a.cp.Work
	if work == nil || work.Held {
		return a.soonest(now, a.untilNextPoll(now)), nil
	}
	switch work.Phase {
	case PhaseManifest:
		return a.stepManifest(ctx, now)
	case PhaseDownloading:
		return a.stepDownload(ctx, now)
	case PhaseVerifying:
		return 0, a.stepVerify()
	case PhaseReady:
		return a.stepClaim(ctx, now)
	default:
		return 0, fmt.Errorf("unexpected phase %q", work.Phase)
	}
}

// soonest shortens a wait if reports are due to be retried before it ends.
func (a *Agent) soonest(now time.Time, wait time.Duration) time.Duration {
	if len(a.cp.Outbox) == 0 {
		return wait
	}
	return min(wait, max(a.nextFlush.Sub(now), 0))
}

func (a *Agent) untilNextPoll(now time.Time) time.Duration {
	return max(a.nextPoll.Sub(now), 0)
}

// networkFailure records a failed call and returns how long to wait before another. A server
// that asks for more time gets it, plus some jitter so that clients don't all come back at
// once. A rejected credential means the device can't do anything useful until someone fixes
// it, so it slows right down, but it keeps its state and keeps trying.
func (a *Agent) networkFailure(err error) time.Duration {
	if client.IsStatus(err, http.StatusUnauthorized) {
		return backoffMax
	}
	wait := a.backoff.Delay(a.failures)
	a.failures++
	if after := client.RetryAfter(err); after > 0 {
		wait = max(wait, retry.Jitter(after, pollJitterFraction, a.deps.Rand)+after/4)
	}
	return wait
}

// flush sends reports in order, and stops at the first one that can't be delivered, which is
// retried after a backoff.
func (a *Agent) flush(ctx context.Context, now time.Time) error {
	for len(a.cp.Outbox) > 0 {
		report := a.cp.Outbox[0]
		_, err := a.deps.Transport.Report(ctx, report)
		switch {
		case err == nil:
			a.failures = 0
		case ctx.Err() != nil:
			// The agent is stopping, which says nothing about the report. It goes next time.
			return nil //nolint:nilerr // Stopping isn't a failure of the report.
		case client.IsRetryable(err) || client.IsStatus(err, http.StatusUnauthorized):
			a.nextFlush = now.Add(a.networkFailure(err))
			return nil
		default:
			// Retrying can't help, and holding on to it would block every later report.
			a.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "report rejected, dropping it",
				slog.String("device_id", a.config.DeviceID),
				slog.Int64("sequence", report.Sequence),
				slog.Any("error", err))
		}
		next := a.cp.clone()
		next.Outbox = next.Outbox[1:]
		if err := a.commit(next); err != nil {
			return err
		}
	}
	return nil
}

// poll asks the server what to do. A nonzero wait means it couldn't be reached.
func (a *Agent) poll(ctx context.Context, now time.Time) (time.Duration, error) {
	resp, err := a.deps.Transport.Poll(ctx)
	if err != nil {
		wait := a.networkFailure(err)
		a.nextPoll = now.Add(wait)
		return wait, nil
	}
	a.failures = 0
	if resp.PollAfterSeconds > 0 {
		a.pollInterval = time.Duration(resp.PollAfterSeconds) * time.Second
	}
	a.nextPoll = now.Add(retry.Jitter(a.pollInterval, pollJitterFraction, a.deps.Rand))
	return 0, a.applyPoll(resp.Assignment)
}

// applyPoll reconciles what the server said with the work in progress.
func (a *Agent) applyPoll(assignment *apiv1.DeviceAssignment) error {
	work := a.cp.Work
	if work != nil && (assignment == nil || assignment.ID != work.AssignmentID) {
		// The server has cancelled it. This can only be work that hasn't reached
		// installation, since Step finishes anything past that before polling.
		if err := a.abandonWork(); err != nil {
			return err
		}
		work = nil
	}
	if assignment == nil {
		return nil
	}
	if work == nil && a.cp.awaitingDelivery(assignment.ID) {
		// The device has finished this one, and the server will know once the outbox drains.
		// Until then, what the server says about it is out of date.
		return nil
	}
	if work == nil {
		return a.startWork(*assignment)
	}

	held := assignment.Directive == rollout.DirectiveHold
	if held == work.Held {
		return nil
	}
	next := a.cp.clone()
	next.Work.Held = held
	return a.commit(next)
}

// startWork takes on a new assignment, unless the device can already tell it's unacceptable.
func (a *Agent) startWork(assignment apiv1.DeviceAssignment) error {
	artifact := assignment.Artifact
	next := a.cp.clone()
	reject := func(reason string) error {
		next.enqueue(assignment.RolloutID, assignment.ID, assignment.Generation, rollout.AssignmentFailed, reason)
		return a.commit(next)
	}
	switch {
	case assignment.State.InFlight():
		// The server thinks this device is partway through installing, but the device has
		// no record of it. It must have lost its state.
		return reject(ReasonLostState)
	case assignment.Kind == KindRollback:
		return a.startRollback(assignment, next, reject)
	case assignment.Kind != KindForward:
		return reject(ReasonUnsupportedKind)
	case artifact.Platform != a.config.Platform:
		return reject(ReasonPlatformMismatch)
	case artifact.SizeBytes > a.config.MaxArtifactBytes:
		return reject(ReasonTooLarge)
	case artifact.ReleaseCounter == a.cp.Active.Release && artifact.SHA256 == a.cp.Active.SHA256:
		// It already runs this exact release, which can happen when an install from an earlier
		// rollout finishes after a follow-up froze its membership. Confirming that is a step
		// into flight, which a hold refuses, so it waits to be released first.
		if assignment.Directive == rollout.DirectiveHold {
			return nil
		}
		next.enqueue(assignment.RolloutID, assignment.ID, assignment.Generation, rollout.AssignmentHealthCheck, "")
		next.enqueue(assignment.RolloutID, assignment.ID, assignment.Generation, rollout.AssignmentSucceeded, "")
		return a.commit(next)
	case artifact.ReleaseCounter <= a.cp.Active.Release:
		return reject(ReasonDowngrade)
	}
	next.Work = &Work{
		AssignmentID: assignment.ID,
		RolloutID:    assignment.RolloutID,
		Generation:   assignment.Generation,
		Artifact:     artifact,
		Phase:        PhaseManifest,
		Held:         assignment.Directive == rollout.DirectiveHold,
	}
	return a.commit(next)
}

func (a *Agent) abandonWork() error {
	artifactID := a.cp.Work.Artifact.ID
	next := a.cp.clone()
	next.Work = nil
	if err := a.commit(next); err != nil {
		return err
	}
	a.manifest = nil
	return a.removePartial(artifactID)
}

// removePartial is best effort: a leftover file is cleaned up at the next start.
func (a *Agent) removePartial(artifactID string) error {
	if err := a.deps.Partials.Remove(artifactID); err != nil {
		a.deps.Logger.LogAttrs(context.Background(), slog.LevelWarn, "remove partial download",
			slog.String("device_id", a.config.DeviceID), slog.Any("error", err))
	}
	return nil
}

// failWork ends an assignment that hasn't reached installation, and reports why.
func (a *Agent) failWork(reason string) error {
	work := a.cp.Work
	next := a.cp.clone()
	next.Work = nil
	next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentFailed, reason)
	if err := a.commit(next); err != nil {
		return err
	}
	a.manifest = nil
	return a.removePartial(work.Artifact.ID)
}

func (a *Agent) verifyEnvelope(envelope signing.Envelope) (signing.Manifest, error) {
	manifest, err := a.config.Trust.Verify(envelope)
	if err != nil {
		return signing.Manifest{}, fmt.Errorf("verify manifest: %w", err)
	}
	return manifest, nil
}

func reasonForVerifyError(err error) string {
	if errors.Is(err, signing.ErrInvalidSignature) {
		return ReasonInvalidSignature
	}
	return ReasonInvalidManifest
}

// stepManifest fetches and verifies the signed manifest, which everything after this is
// checked against. Nothing is downloaded until it's trusted.
func (a *Agent) stepManifest(ctx context.Context, now time.Time) (time.Duration, error) {
	work := a.cp.Work
	envelope, err := a.deps.Transport.Manifest(ctx, work.Artifact.ID)
	if err != nil {
		return a.fetchFailure(err, now), nil
	}
	a.failures = 0

	manifest, err := a.verifyEnvelope(envelope)
	if err != nil {
		return 0, a.failWork(reasonForVerifyError(err))
	}
	// The assignment is only what the server claims, and the manifest is what the signer
	// said. They have to agree, and the device has to be what the manifest is for.
	if manifest.Name != work.Artifact.Name || manifest.Version != work.Artifact.Version ||
		manifest.Platform != work.Artifact.Platform || manifest.Platform != a.config.Platform ||
		manifest.ReleaseCounter != work.Artifact.ReleaseCounter ||
		manifest.SizeBytes != work.Artifact.SizeBytes || manifest.SHA256 != work.Artifact.SHA256 {
		return 0, a.failWork(ReasonManifestMismatch)
	}
	if err := a.deps.Partials.Truncate(work.Artifact.ID, 0); err != nil {
		return 0, fmt.Errorf("start partial download: %w", err)
	}

	next := a.cp.clone()
	next.Work.Envelope = &envelope
	next.Work.Phase = PhaseDownloading
	next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentDownloading, "")
	if err := a.commit(next); err != nil {
		return 0, err
	}
	a.manifest = &manifest
	return 0, nil
}

// fetchFailure handles a failed fetch. Being refused may mean the assignment was cancelled,
// which the next poll will confirm, so that's scheduled right away.
func (a *Agent) fetchFailure(err error, now time.Time) time.Duration {
	if client.IsRetryable(err) || client.IsStatus(err, http.StatusUnauthorized) {
		return a.networkFailure(err)
	}
	a.nextPoll = now
	return a.networkFailure(err)
}

// stepDownload fetches and verifies the next chunk. A chunk is checked before it's kept,
// so a partial download only ever holds verified bytes.
func (a *Agent) stepDownload(ctx context.Context, now time.Time) (time.Duration, error) {
	work := a.cp.Work
	manifest := *a.manifest
	if work.VerifiedChunks == len(manifest.Chunks) {
		next := a.cp.clone()
		next.Work.Phase = PhaseVerifying
		next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentVerifying, "")
		return 0, a.commit(next)
	}

	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	index := work.VerifiedChunks
	offset, length := manifest.ChunkOffset(index), manifest.ChunkLength(index)
	resp, err := a.deps.Transport.FetchRange(ctx, work.Artifact.ID, offset, length)
	if err != nil {
		return a.fetchFailure(err, now), nil
	}
	defer func() { _ = resp.Body.Close() }()

	etag := `"sha256:` + manifest.SHA256 + `"`
	if resp.ETag != etag {
		return a.strike("the response is for a different artifact")
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		return a.restartFromWholeResponse(resp.Body, manifest)
	default:
		return a.strike(fmt.Sprintf("unexpected status %d", resp.StatusCode))
	}
	if !validContentRange(resp.ContentRange, offset, length, manifest.SizeBytes) {
		return a.strike("the response covers the wrong bytes: " + resp.ContentRange)
	}

	data := make([]byte, length)
	if _, err := io.ReadFull(resp.Body, data); err != nil {
		return a.networkFailure(err), nil
	}
	if !manifest.ChunkMatches(index, data) {
		return a.strike(fmt.Sprintf("chunk %d failed its digest", index))
	}
	return 0, a.keepChunk(data)
}

// keepChunk appends a verified chunk, and then records that it's verified. A crash between
// the two leaves bytes that the restart will discard.
func (a *Agent) keepChunk(data []byte) error {
	work := a.cp.Work
	if a.partialDirty {
		if err := a.truncateToVerified(*a.manifest, work.VerifiedChunks); err != nil {
			return err
		}
		a.partialDirty = false
	}
	if err := a.deps.Partials.Append(work.Artifact.ID, data); err != nil {
		// Part of the chunk may have been written, and the next one mustn't land after it.
		if truncErr := a.truncateToVerified(*a.manifest, work.VerifiedChunks); truncErr != nil {
			a.partialDirty = true
		}
		return fmt.Errorf("write partial download: %w", err)
	}
	a.failures = 0
	next := a.cp.clone()
	next.Work.VerifiedChunks++
	next.Work.Strikes = 0
	return a.commit(next)
}

// restartFromWholeResponse handles a server that ignored the Range request. It sent the
// artifact from the first byte, so the only safe way to use it is from the first byte. Each
// chunk is still checked, and nothing unverified is kept.
func (a *Agent) restartFromWholeResponse(body io.Reader, manifest signing.Manifest) (time.Duration, error) {
	next := a.cp.clone()
	next.Work.VerifiedChunks = 0
	if err := a.commit(next); err != nil {
		return 0, err
	}
	if err := a.deps.Partials.Truncate(a.cp.Work.Artifact.ID, 0); err != nil {
		return 0, fmt.Errorf("restart partial download: %w", err)
	}
	for index := range manifest.Chunks {
		data := make([]byte, manifest.ChunkLength(index))
		if _, err := io.ReadFull(body, data); err != nil {
			return a.networkFailure(err), nil
		}
		if !manifest.ChunkMatches(index, data) {
			return a.strike(fmt.Sprintf("chunk %d of a whole response failed its digest", index))
		}
		if err := a.keepChunk(data); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

// strike counts a response that can't be believed. It's discarded either way. Enough of them
// in a row, and the device gives up on this artifact.
func (a *Agent) strike(reason string) (time.Duration, error) {
	a.deps.Logger.LogAttrs(context.Background(), slog.LevelWarn, "discarded a bad download response",
		slog.String("device_id", a.config.DeviceID), slog.String("reason", reason))
	if a.cp.Work.Strikes+1 >= maxIntegrityStrikes {
		return 0, a.failWork(ReasonIntegrity)
	}
	next := a.cp.clone()
	next.Work.Strikes++
	if err := a.commit(next); err != nil {
		return 0, err
	}
	wait := a.backoff.Delay(a.failures)
	a.failures++
	return wait, nil
}

// validContentRange checks that a response covers exactly the bytes that were asked for, of
// an artifact of the expected total size.
func validContentRange(header string, offset, length, total int64) bool {
	rangePart, ok := strings.CutPrefix(header, "bytes ")
	if !ok {
		return false
	}
	span, totalPart, ok := strings.Cut(rangePart, "/")
	if !ok {
		return false
	}
	startPart, endPart, ok := strings.Cut(span, "-")
	if !ok {
		return false
	}
	start, err := strconv.ParseInt(startPart, 10, 64)
	if err != nil {
		return false
	}
	end, err := strconv.ParseInt(endPart, 10, 64)
	if err != nil {
		return false
	}
	got, err := strconv.ParseInt(totalPart, 10, 64)
	if err != nil {
		return false
	}
	return start == offset && end == offset+length-1 && got == total
}

// stepVerify checks the whole artifact against the signed manifest, and the release against
// what's running. This is the last check before asking for permission to install.
func (a *Agent) stepVerify() error {
	work := a.cp.Work
	manifest := *a.manifest
	content, err := a.deps.Partials.Open(work.Artifact.ID)
	if err != nil {
		return fmt.Errorf("open partial download: %w", err)
	}
	defer func() { _ = content.Close() }()

	if err := manifest.CheckContent(content); err != nil {
		if errors.Is(err, signing.ErrContentMismatch) {
			return a.failWork(ReasonIntegrity)
		}
		return fmt.Errorf("verify partial download: %w", err)
	}
	if manifest.ReleaseCounter <= a.cp.Active.Release {
		return a.failWork(ReasonDowngrade)
	}
	next := a.cp.clone()
	next.Work.Phase = PhaseReady
	return a.commit(next)
}

// stepClaim asks the server for permission to start installing, by reporting installing.
// The server decides that against the rollout as it stands, so nothing starts once a halt,
// an abort, or a pause has committed, and the server always knows who is in flight. Without
// an answer the device waits, however long that takes. Earlier reports go first, so that the
// server hears them in order.
func (a *Agent) stepClaim(ctx context.Context, now time.Time) (time.Duration, error) {
	if len(a.cp.Outbox) > 0 {
		return a.soonest(now, a.untilNextPoll(now)), nil
	}
	work := a.cp.Work
	if work.Claim == nil {
		next := a.cp.clone()
		claim := next.report(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentInstalling, "")
		next.Work.Claim = &claim
		if err := a.commit(next); err != nil {
			return 0, err
		}
		work = a.cp.Work
	}

	resp, err := a.deps.Transport.Report(ctx, *work.Claim)
	switch {
	case err == nil && resp.Applied:
		// A duplicate says what became of the original, so this holds after a lost response.
		a.failures = 0
		return 0, a.beginInstall()
	case err == nil && resp.IgnoredReason == rollout.IgnoredOnHold:
		// The server has this request on record as refused, so the next one has to be new.
		a.failures = 0
		next := a.cp.clone()
		next.Work.Claim = nil
		next.Work.Held = true
		return a.untilNextPoll(now), a.commit(next)
	case err != nil && ctx.Err() != nil:
		// The agent is stopping. The same request goes again when it starts.
		return 0, nil //nolint:nilerr // Stopping isn't a refusal.
	case err == nil:
		// The assignment is over, as far as the server is concerned.
		a.failures = 0
		a.deps.Logger.LogAttrs(ctx, slog.LevelInfo, "install refused, abandoning the assignment",
			slog.String("device_id", a.config.DeviceID), slog.String("reason", resp.IgnoredReason))
		return 0, a.abandonWork()
	case client.IsRetryable(err) || client.IsStatus(err, http.StatusUnauthorized):
		return a.networkFailure(err), nil
	default:
		// Retrying can't help, and without the server's agreement there's no going ahead.
		a.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "install refused for good, abandoning the assignment",
			slog.String("device_id", a.config.DeviceID), slog.Any("error", err))
		return 0, a.abandonWork()
	}
}

// beginInstall crosses the point of no return, once the server has agreed. It's saved first,
// so that if the agent doesn't survive what comes next, the restart knows the new slot can't
// be trusted.
func (a *Agent) beginInstall() error {
	next := a.cp.clone()
	next.Work.Phase = PhaseInstalling
	next.Work.PendingSlot = otherSlot(a.cp.ActiveSlot)
	if next.Work.Kind == KindRollback {
		next.Work.Phase = PhaseRestoring
	}
	next.Work.Claim = nil
	return a.commit(next)
}

// startRollback takes on a rollback assignment, which restores the release the device ran before
// this rollout from its previous slot. The server's permission covers only this one step back,
// and only to the release the device itself recorded (ADR 0011).
func (a *Agent) startRollback(assignment apiv1.DeviceAssignment, next Checkpoint, reject func(string) error) error {
	artifact := assignment.Artifact
	switch {
	case assignment.RestoreRelease == nil:
		return reject(ReasonNothingToRestore)
	case a.cp.Active.Release == *assignment.RestoreRelease:
		// It's already running what it would restore, so there's nothing to undo.
		if assignment.Directive == rollout.DirectiveHold {
			return nil
		}
		next.enqueue(assignment.RolloutID, assignment.ID, assignment.Generation, rollout.AssignmentHealthCheck, "")
		next.enqueue(assignment.RolloutID, assignment.ID, assignment.Generation, rollout.AssignmentSucceeded, "")
		return a.commit(next)
	case a.cp.Active.Release != artifact.ReleaseCounter || a.cp.Active.SHA256 != artifact.SHA256,
		a.cp.Previous == nil, a.cp.Previous.Release != *assignment.RestoreRelease:
		return reject(ReasonNothingToRestore)
	}
	next.Work = &Work{
		Kind:           KindRollback,
		RestoreRelease: *assignment.RestoreRelease,
		AssignmentID:   assignment.ID,
		RolloutID:      assignment.RolloutID,
		Generation:     assignment.Generation,
		Artifact:       artifact,
		Phase:          PhaseReady,
		Held:           assignment.Directive == rollout.DirectiveHold,
	}
	return a.commit(next)
}

// stepRestore switches back to the previous slot, in one save, after checking the release there.
// The release a device shipped with was never installed by the agent, so there's nothing in its
// slot to check, and it's known to have been running before this rollout.
func (a *Agent) stepRestore(ctx context.Context) error {
	work := a.cp.Work
	previousSlot := otherSlot(a.cp.ActiveSlot)
	next := a.cp.clone()
	next.Work = nil
	if a.cp.Previous.ArtifactID != "" {
		if err := a.deps.Installer.HealthCheck(ctx, previousSlot); err != nil {
			a.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "the release to restore is unhealthy",
				slog.String("device_id", a.config.DeviceID), slog.Any("error", err))
			next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentFailed, ReasonRestoreUnhealthy)
			return a.commit(next)
		}
	}
	current := a.cp.Active
	next.Active = *a.cp.Previous
	next.Previous = &current
	next.ActiveSlot = previousSlot
	next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentHealthCheck, "")
	next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentSucceeded, "")
	return a.commit(next)
}

func otherSlot(slot string) string {
	if slot == slotA {
		return slotB
	}
	return slotA
}

func (a *Agent) stepInstall(ctx context.Context) error {
	work := a.cp.Work
	content, err := a.deps.Partials.Open(work.Artifact.ID)
	if err != nil {
		return fmt.Errorf("open partial download: %w", err)
	}
	defer func() { _ = content.Close() }()

	next := a.cp.clone()
	if err := a.deps.Installer.Install(ctx, work.PendingSlot, content, work.Artifact.SizeBytes); err != nil {
		a.deps.Logger.LogAttrs(ctx, slog.LevelError, "install failed",
			slog.String("device_id", a.config.DeviceID), slog.Any("error", err))
		next.Work.Phase = PhaseReverting
		next.Work.FailReason = ReasonInstallError
		next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentReverting, ReasonInstallError)
		return a.commit(next)
	}
	next.Work.Phase = PhaseHealthCheck
	next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentHealthCheck, "")
	return a.commit(next)
}

// stepHealthCheck decides the release's fate. The active slot only changes in the same save
// that records success, so there's no point at which a crash could leave it half-switched.
func (a *Agent) stepHealthCheck(ctx context.Context) error {
	work := a.cp.Work
	next := a.cp.clone()
	if err := a.deps.Installer.HealthCheck(ctx, work.PendingSlot); err != nil {
		a.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "release is unhealthy",
			slog.String("device_id", a.config.DeviceID), slog.Any("error", err))
		next.Work.Phase = PhaseReverting
		next.Work.FailReason = ReasonUnhealthy
		next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentReverting, ReasonUnhealthy)
		return a.commit(next)
	}

	previous := a.cp.Active
	next.Previous = &previous
	next.ActiveSlot = work.PendingSlot
	next.Active = Installed{
		ArtifactID: work.Artifact.ID,
		Version:    work.Artifact.Version,
		SHA256:     work.Artifact.SHA256,
		Release:    work.Artifact.ReleaseCounter,
	}
	next.Work = nil
	next.enqueue(work.RolloutID, work.AssignmentID, work.Generation, rollout.AssignmentSucceeded, "")
	if err := a.commit(next); err != nil {
		return err
	}
	a.manifest = nil
	return a.removePartial(work.Artifact.ID)
}

// stepRevert discards the pending slot, and finishes the assignment. The active slot was
// never touched, so there's nothing to restore.
func (a *Agent) stepRevert(ctx context.Context) error {
	work := a.cp.Work
	if work.PendingSlot != "" {
		if err := a.deps.Installer.Discard(ctx, work.PendingSlot); err != nil {
			return fmt.Errorf("discard pending slot: %w", err)
		}
	}
	return a.failWork(work.FailReason)
}
