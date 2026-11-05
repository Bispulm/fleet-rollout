// Package apiv1 defines the JSON documents of the control plane's HTTP API. The
// server, the Go client, and the Kubernetes operator all share these types.
package apiv1

import (
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

// CreateFleetRequest is the body of POST /v1/fleets.
type CreateFleetRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
}

// Fleet is a named group of devices.
type Fleet struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	CreatedAt time.Time         `json:"created_at"`
}

// DeviceSpec describes a device being registered.
type DeviceSpec struct {
	ID               string            `json:"id"`
	Platform         string            `json:"platform"`
	Labels           map[string]string `json:"labels,omitempty"`
	InstalledRelease int64             `json:"installed_release,omitempty"`
}

// RegisterDevicesRequest is the body of POST /v1/fleets/{fleet}/devices.
type RegisterDevicesRequest struct {
	Devices []DeviceSpec `json:"devices"`
}

// Device is a registered device. Token is only present in the response that
// registers the device, because the server keeps only its hash.
type Device struct {
	ID               string               `json:"id"`
	FleetID          string               `json:"fleet_id"`
	Platform         string               `json:"platform"`
	Labels           map[string]string    `json:"labels"`
	Status           rollout.DeviceStatus `json:"status"`
	InstalledRelease int64                `json:"installed_release"`
	LastSeenAt       *time.Time           `json:"last_seen_at,omitempty"`
	CreatedAt        time.Time            `json:"created_at"`
	Token            string               `json:"token,omitempty"`
}

// DeviceList is a page of devices. Pass NextAfter as the after parameter to get
// the next page. It's empty on the last page.
type DeviceList struct {
	Devices   []Device `json:"devices"`
	NextAfter string   `json:"next_after,omitempty"`
}

// UpdateDeviceRequest is the body of PATCH /v1/devices/{id}.
type UpdateDeviceRequest struct {
	Status rollout.DeviceStatus `json:"status"`
}

// Problem is an RFC 9457 error document. Code is a stable identifier for errors
// that clients act on.
type Problem struct {
	Type       string              `json:"type"`
	Title      string              `json:"title"`
	Status     int                 `json:"status"`
	Detail     string              `json:"detail,omitempty"`
	Code       string              `json:"code,omitempty"`
	Violations []rollout.Violation `json:"violations,omitempty"`
}

// The stable problem codes.
const (
	CodeInvalidTransition  = "invalid_transition"
	CodeRevisionMismatch   = "revision_mismatch"
	CodeMembershipNotReady = "membership_not_frozen"
	CodeInvalidPolicy      = "invalid_policy"
	CodeInvalidRequest     = "invalid_request"
	CodeConflict           = "conflict"
	CodeUnauthorized       = "unauthorized"
)

// CreateArtifactRequest is the body of POST /v1/artifacts. SHA256 is lowercase hex.
type CreateArtifactRequest struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	Platform       string `json:"platform"`
	ReleaseCounter int64  `json:"release_counter"`
	SizeBytes      int64  `json:"size_bytes"`
	SHA256         string `json:"sha256"`
}

// Artifact is immutable metadata for a release.
type Artifact struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Version        string    `json:"version"`
	Platform       string    `json:"platform"`
	ReleaseCounter int64     `json:"release_counter"`
	SizeBytes      int64     `json:"size_bytes"`
	SHA256         string    `json:"sha256"`
	CreatedAt      time.Time `json:"created_at"`
}

// CreateRolloutRequest is the body of POST /v1/fleets/{fleet}/rollouts. ID is
// optional. Supplying it makes the cohort reproducible across databases.
type CreateRolloutRequest struct {
	ID         string             `json:"id,omitempty"`
	Name       string             `json:"name"`
	ArtifactID string             `json:"artifact_id"`
	Policy     rollout.PolicySpec `json:"policy"`
}

// Stage is a stage's size and progress.
type Stage struct {
	Ordinal         int        `json:"ordinal"`
	Percent         int        `json:"percent"`
	TargetCount     *int       `json:"target_count,omitempty"`
	CohortSize      *int       `json:"cohort_size,omitempty"`
	State           string     `json:"state"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	DeadlineAt      *time.Time `json:"deadline_at,omitempty"`
	SoakStartedAt   *time.Time `json:"soak_started_at,omitempty"`
	SoakAccumulated string     `json:"soak_accumulated"`
	FinishedAt      *time.Time `json:"finished_at,omitempty"`
	Succeeded       int        `json:"succeeded"`
	Failed          int        `json:"failed"`
	Superseded      int        `json:"superseded"`
	// InFlight counts devices that have started installing and can't be recalled.
	// They're also counted as pending or unknown.
	InFlight int `json:"in_flight"`
	Unknown  int `json:"unknown"`
	Pending  int `json:"pending"`
}

// Rollout is a rollout's full status.
type Rollout struct {
	ID           string         `json:"id"`
	FleetID      string         `json:"fleet_id"`
	Fleet        string         `json:"fleet"`
	ArtifactID   string         `json:"artifact_id"`
	Name         string         `json:"name"`
	State        rollout.State  `json:"state"`
	Revision     int64          `json:"revision"`
	Policy       rollout.Policy `json:"policy"`
	PolicyDigest string         `json:"policy_digest"`
	CurrentStage *int           `json:"current_stage,omitempty"`
	MemberCount  *int           `json:"member_count,omitempty"`
	PausedFrom   *rollout.State `json:"paused_from,omitempty"`
	PausedAt     *time.Time     `json:"paused_at,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	StartedAt    *time.Time     `json:"started_at,omitempty"`
	FinishedAt   *time.Time     `json:"finished_at,omitempty"`
	Stages       []Stage        `json:"stages"`
	// Exclusions counts fleet devices left out of the membership, by reason.
	Exclusions map[rollout.ExclusionReason]int `json:"exclusions"`
	// HaltReason and HaltDetail say why a rollout halted, if it did.
	HaltReason string `json:"halt_reason,omitempty"`
	HaltDetail string `json:"halt_detail,omitempty"`
	// Waiting explains what an active rollout needs before it can move on.
	Waiting string `json:"waiting,omitempty"`
}

// TransitionRequest is the optional body of a rollout state change. Set
// ExpectedRevision to make the change conditional on the revision you last read.
type TransitionRequest struct {
	ExpectedRevision *int64 `json:"expected_revision,omitempty"`
	Reason           string `json:"reason,omitempty"`
}

// CohortEntry is one member of a rollout.
type CohortEntry struct {
	DeviceID string `json:"device_id"`
	Position int    `json:"position"`
	Stage    int    `json:"stage"`
	RankKey  string `json:"rank_key"`
}

// CohortPage is a page of members. Pass NextAfter as the after parameter to get
// the next page. It's zero on the last page.
type CohortPage struct {
	Entries   []CohortEntry `json:"entries"`
	NextAfter int           `json:"next_after,omitempty"`
}

// Transition is one entry in a rollout's audit log.
type Transition struct {
	Revision   int64         `json:"revision"`
	From       rollout.State `json:"from"`
	To         rollout.State `json:"to"`
	Event      rollout.Event `json:"event"`
	Actor      string        `json:"actor"`
	Reason     string        `json:"reason,omitempty"`
	OccurredAt time.Time     `json:"occurred_at"`
	// EngineGeneration is the leadership generation behind an engine's decision.
	EngineGeneration *int64 `json:"engine_generation,omitempty"`
}

// TransitionLog is a rollout's audit log, oldest first.
type TransitionLog struct {
	Transitions []Transition `json:"transitions"`
}

// DeviceAssignment is what a device is told to do. A device must not begin installing
// after ExpiresAt without polling again.
type DeviceAssignment struct {
	ID         string                  `json:"id"`
	RolloutID  string                  `json:"rollout_id"`
	Generation int64                   `json:"generation"`
	Stage      int                     `json:"stage"`
	State      rollout.AssignmentState `json:"state"`
	Directive  rollout.Directive       `json:"directive"`
	ExpiresAt  time.Time               `json:"expires_at"`
	Artifact   Artifact                `json:"artifact"`
}

// PollResponse is the response to POST /v1/device/poll. Assignment is null when there's
// nothing to do. A device holding an assignment that's no longer returned must abandon
// it, unless it has already started installing.
type PollResponse struct {
	PollAfterSeconds int               `json:"poll_after_seconds"`
	Assignment       *DeviceAssignment `json:"assignment"`
}

// ReportRequest is the body of POST /v1/device/reports. A retry must repeat it exactly,
// with the same Sequence, which the device persists before sending.
type ReportRequest struct {
	Sequence     int64                   `json:"sequence"`
	RolloutID    string                  `json:"rollout_id"`
	AssignmentID string                  `json:"assignment_id"`
	Generation   int64                   `json:"generation"`
	State        rollout.AssignmentState `json:"state"`
	Reason       string                  `json:"reason,omitempty"`
}

// ReportResponse says what became of a report. All of these are final, and none of
// them should be retried.
type ReportResponse struct {
	Applied       bool   `json:"applied"`
	Duplicate     bool   `json:"duplicate,omitempty"`
	IgnoredReason string `json:"ignored_reason,omitempty"`
}

// The problem codes for device reports. Both mean a retry can't succeed.
const (
	CodeReportConflict = "report_conflict"
	CodeReportMismatch = "report_mismatch"
)
