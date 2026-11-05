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
