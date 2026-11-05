// Package client is a Go client for the control plane's admin API. The CLI uses it,
// and the Kubernetes operator will too, so they can't drift into different behavior.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/signing"
)

const (
	defaultTimeout   = 60 * time.Second
	maxErrorBodySize = 1 << 20
)

// APIError is an error response from the server, as opposed to a transport
// failure. Callers can use it to tell permanent failures from retryable ones.
type APIError struct {
	Problem apiv1.Problem
	// RetryAfter is how long the server asked the client to wait, if it did.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	var b strings.Builder
	switch {
	case e.Problem.Detail != "":
		b.WriteString(e.Problem.Detail)
	case e.Problem.Title != "":
		b.WriteString(e.Problem.Title)
	default:
		fmt.Fprintf(&b, "unexpected response status %d", e.Problem.Status)
	}
	for _, v := range e.Problem.Violations {
		fmt.Fprintf(&b, "\n  %s: %s", v.Field, v.Message)
	}
	return b.String()
}

// IsRetryable reports whether trying again could help. Transport failures and server-side
// failures could. Anything the server rejected on its merits, such as a bad credential or a
// failed integrity check, can't. A canceled context means the caller has stopped caring.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return true
	}
	status := apiErr.Problem.Status
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// RetryAfter returns how long the server asked for a client to wait, or zero.
func RetryAfter(err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	return 0
}

// IsStatus reports whether err is an API error with this HTTP status.
func IsStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Problem.Status == status
}

// IsCode reports whether err is an API error with the given problem code.
func IsCode(err error, code string) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Problem.Code == code
}

// Client calls the admin API. It is safe for concurrent use.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New returns a Client. Ordinary calls get a request deadline here, rather than on the HTTP
// client, because a large upload or download needs far longer than they do.
func New(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: httpClient}
}

// do sends a request and decodes a successful response into out, if out is set.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	var reader io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return decodeError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s %s: %w", method, path, err)
	}
	return nil
}

func decodeError(resp *http.Response) error {
	problem := apiv1.Problem{Status: resp.StatusCode, Title: http.StatusText(resp.StatusCode)}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodySize))
	if err == nil && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		var decoded apiv1.Problem
		if json.Unmarshal(raw, &decoded) == nil && decoded.Status != 0 {
			problem = decoded
		}
	}
	out := &APIError{Problem: problem}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
		out.RetryAfter = time.Duration(seconds) * time.Second
	}
	return out
}

func segment(s string) string { return url.PathEscape(s) }

// CreateFleet creates a fleet.
func (c *Client) CreateFleet(ctx context.Context, req apiv1.CreateFleetRequest) (apiv1.Fleet, error) {
	var out apiv1.Fleet
	err := c.do(ctx, http.MethodPost, "/v1/fleets", nil, req, &out)
	return out, err
}

// GetFleet returns a fleet by name.
func (c *Client) GetFleet(ctx context.Context, name string) (apiv1.Fleet, error) {
	var out apiv1.Fleet
	err := c.do(ctx, http.MethodGet, "/v1/fleets/"+segment(name), nil, nil, &out)
	return out, err
}

// RegisterDevices registers up to 1000 devices, all or nothing. The returned
// devices carry their tokens, which can't be retrieved again.
func (c *Client) RegisterDevices(ctx context.Context, fleet string, devices []apiv1.DeviceSpec) ([]apiv1.Device, error) {
	var out apiv1.DeviceList
	err := c.do(ctx, http.MethodPost, "/v1/fleets/"+segment(fleet)+"/devices", nil,
		apiv1.RegisterDevicesRequest{Devices: devices}, &out)
	return out.Devices, err
}

// ListDevices returns a page of a fleet's devices.
func (c *Client) ListDevices(ctx context.Context, fleet, after string, limit int) (apiv1.DeviceList, error) {
	query := url.Values{}
	if after != "" {
		query.Set("after", after)
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out apiv1.DeviceList
	err := c.do(ctx, http.MethodGet, "/v1/fleets/"+segment(fleet)+"/devices", query, nil, &out)
	return out, err
}

// UpdateDevice changes a device's administrative status.
func (c *Client) UpdateDevice(ctx context.Context, id string, req apiv1.UpdateDeviceRequest) (apiv1.Device, error) {
	var out apiv1.Device
	err := c.do(ctx, http.MethodPatch, "/v1/devices/"+segment(id), nil, req, &out)
	return out, err
}

// CreateArtifact registers a signed manifest. Repeating it for the same content returns the
// existing artifact.
func (c *Client) CreateArtifact(ctx context.Context, envelope signing.Envelope) (apiv1.Artifact, error) {
	var out apiv1.Artifact
	err := c.do(ctx, http.MethodPost, "/v1/artifacts", nil, apiv1.CreateArtifactRequest{Envelope: envelope}, &out)
	return out, err
}

// UploadArtifactContent sends an artifact's bytes. There's no deadline here beyond ctx, because
// how long this takes depends on the artifact's size.
func (c *Client) UploadArtifactContent(ctx context.Context, id string, content io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/v1/artifacts/"+segment(id)+"/content", content)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upload artifact: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		return decodeError(resp)
	}
	return nil
}

// GetArtifact returns an artifact by ID.
func (c *Client) GetArtifact(ctx context.Context, id string) (apiv1.Artifact, error) {
	var out apiv1.Artifact
	err := c.do(ctx, http.MethodGet, "/v1/artifacts/"+segment(id), nil, nil, &out)
	return out, err
}

// CreateRollout creates a draft rollout, or returns the existing one if an
// identical request already succeeded.
func (c *Client) CreateRollout(ctx context.Context, fleet string, req apiv1.CreateRolloutRequest) (apiv1.Rollout, error) {
	var out apiv1.Rollout
	err := c.do(ctx, http.MethodPost, "/v1/fleets/"+segment(fleet)+"/rollouts", nil, req, &out)
	return out, err
}

// GetRollout returns a rollout's status.
func (c *Client) GetRollout(ctx context.Context, id string) (apiv1.Rollout, error) {
	var out apiv1.Rollout
	err := c.do(ctx, http.MethodGet, "/v1/rollouts/"+segment(id), nil, nil, &out)
	return out, err
}

// The rollout actions accepted by Transition.
const (
	ActionStart  = "start"
	ActionPause  = "pause"
	ActionResume = "resume"
	ActionAbort  = "abort"
)

// Transition applies an action such as ActionPause to a rollout.
func (c *Client) Transition(ctx context.Context, id, action string, req apiv1.TransitionRequest) (apiv1.Rollout, error) {
	var out apiv1.Rollout
	err := c.do(ctx, http.MethodPost, "/v1/rollouts/"+segment(id)+"/"+action, nil, req, &out)
	return out, err
}

// ListCohort returns a page of a started rollout's members, in position order. A
// stage of zero means all stages.
func (c *Client) ListCohort(ctx context.Context, id string, stage, after, limit int) (apiv1.CohortPage, error) {
	query := url.Values{}
	if stage > 0 {
		query.Set("stage", strconv.Itoa(stage))
	}
	if after > 0 {
		query.Set("after", strconv.Itoa(after))
	}
	if limit > 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	var out apiv1.CohortPage
	err := c.do(ctx, http.MethodGet, "/v1/rollouts/"+segment(id)+"/cohort", query, nil, &out)
	return out, err
}

// ListTransitions returns a rollout's audit log, oldest first.
func (c *Client) ListTransitions(ctx context.Context, id string) ([]apiv1.Transition, error) {
	var out apiv1.TransitionLog
	err := c.do(ctx, http.MethodGet, "/v1/rollouts/"+segment(id)+"/transitions", nil, nil, &out)
	return out.Transitions, err
}

// DeviceClient calls the API that devices use. It carries one device's token, so it
// can only act as that device.
type DeviceClient struct {
	client *Client
}

// NewDevice returns a client for one device.
func NewDevice(baseURL, token string, httpClient *http.Client) *DeviceClient {
	return &DeviceClient{client: New(baseURL, token, httpClient)}
}

// Poll asks for the device's current assignment, and records that it was heard from.
func (c *DeviceClient) Poll(ctx context.Context) (apiv1.PollResponse, error) {
	var out apiv1.PollResponse
	err := c.client.do(ctx, http.MethodPost, "/v1/device/poll", nil, nil, &out)
	return out, err
}

// Report sends a status update. A retry must repeat the same request exactly.
func (c *DeviceClient) Report(ctx context.Context, req apiv1.ReportRequest) (apiv1.ReportResponse, error) {
	var out apiv1.ReportResponse
	err := c.client.do(ctx, http.MethodPost, "/v1/device/reports", nil, req, &out)
	return out, err
}

// Manifest fetches the signed manifest for an artifact the device is assigned.
func (c *DeviceClient) Manifest(ctx context.Context, artifactID string) (signing.Envelope, error) {
	var out signing.Envelope
	err := c.client.do(ctx, http.MethodGet, "/v1/device/artifacts/"+segment(artifactID)+"/manifest", nil, nil, &out)
	return out, err
}

// RangeResponse is what came back for a Range request, unchecked. The body is the caller's
// to close. Deciding whether to believe any of it is up to the caller, which holds the
// signed manifest.
type RangeResponse struct {
	StatusCode   int
	ContentRange string
	ETag         string
	Body         io.ReadCloser
}

// FetchRange requests length bytes of an artifact, starting at offset.
func (c *DeviceClient) FetchRange(ctx context.Context, artifactID string, offset, length int64) (RangeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.client.baseURL+"/v1/device/artifacts/"+segment(artifactID)+"/content", http.NoBody)
	if err != nil {
		return RangeResponse{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.client.token)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp, err := c.client.http.Do(req)
	if err != nil {
		return RangeResponse{}, fmt.Errorf("fetch artifact range: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		defer func() { _ = resp.Body.Close() }()
		return RangeResponse{}, decodeError(resp)
	}
	return RangeResponse{
		StatusCode:   resp.StatusCode,
		ContentRange: resp.Header.Get("Content-Range"),
		ETag:         resp.Header.Get("ETag"),
		Body:         resp.Body,
	}, nil
}
