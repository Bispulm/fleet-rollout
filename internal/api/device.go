package api

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/auth"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

const (
	deviceRequestTimeout = 10 * time.Second
	maxDeviceBodyBytes   = 64 << 10
	defaultPollInterval  = 60 * time.Second
	// defaultMaxDeviceRequests is comfortably more than the connection pool, so database
	// connections are what actually queue, and only briefly.
	defaultMaxDeviceRequests = 256
	// defaultMaxDeviceDownloads allows plenty of downloads at once, each one chunk long.
	defaultMaxDeviceDownloads = 128
	// Shed requests are told to come back after a random 1 to 5 seconds, so they don't all
	// come back at the same moment.
	minRetryAfterSeconds    = 1
	retryAfterSpreadSeconds = 5
)

// deviceHandlerFunc handles a request from an authenticated device. Its identity comes
// from the token, never from anything in the request, so one device can't act as another.
type deviceHandlerFunc func(http.ResponseWriter, *http.Request, store.Device) error

// device wraps a handler with device authentication and a request deadline. It's
// separate from admin authentication, and neither credential works on the other's routes.
func (s *Server) device(next deviceHandlerFunc) http.Handler {
	return s.deviceWithSlots(s.deviceSlots, next)
}

// deviceDownload is device for artifact downloads, which take slots from their own pool.
func (s *Server) deviceDownload(next deviceHandlerFunc) http.Handler {
	return s.deviceWithSlots(s.downloadSlots, next)
}

func (s *Server) deviceWithSlots(slots chan struct{}, next deviceHandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Taking a slot comes before anything that costs a database query, so that a
		// reconnecting fleet can't queue more work than the server can take on.
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.Header().Set("Retry-After", strconv.Itoa(minRetryAfterSeconds+rand.IntN(retryAfterSpreadSeconds))) //nolint:gosec // Jitter, not security.
			writeProblemDocument(w, apiv1.Problem{
				Status: http.StatusServiceUnavailable, Detail: "too many device requests at once", Code: apiv1.CodeOverloaded,
			})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), deviceRequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)

		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeDeviceUnauthorized(w)
			return
		}
		device, err := s.store.AuthenticateDevice(ctx, auth.HashToken(token))
		if errors.Is(err, store.ErrNotFound) {
			writeDeviceUnauthorized(w)
			return
		}
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		if err := next(w, r, device); err != nil {
			s.writeError(w, r, err)
		}
	})
}

func writeDeviceUnauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="device"`)
	writeProblemDocument(w, apiv1.Problem{Status: http.StatusUnauthorized, Code: apiv1.CodeUnauthorized})
}

func (s *Server) handleDevicePoll(w http.ResponseWriter, r *http.Request, device store.Device) error {
	assignment, err := s.store.PollAssignment(r.Context(), device)
	if err != nil {
		return err
	}
	out := apiv1.PollResponse{PollAfterSeconds: int(s.pollInterval / time.Second)}
	if assignment != nil {
		document := deviceAssignmentDocument(*assignment)
		out.Assignment = &document
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) handleDeviceReport(w http.ResponseWriter, r *http.Request, device store.Device) error {
	var req apiv1.ReportRequest
	if err := decodeJSONLimit(w, r, &req, maxDeviceBodyBytes); err != nil {
		return err
	}
	switch {
	case req.Sequence < 1:
		return invalid("sequence", "must be at least 1")
	case req.Generation < 1:
		return invalid("generation", "must be at least 1")
	case !rollout.ValidUUID(req.RolloutID):
		return invalid("rollout_id", "must be a canonical UUID")
	case !rollout.ValidUUID(req.AssignmentID):
		return invalid("assignment_id", "must be a canonical UUID")
	case !req.State.Reportable():
		return invalid("state", "isn't a state a device can report")
	case len(req.Reason) > maxReasonLength:
		return invalid("reason", "must be at most "+strconv.Itoa(maxReasonLength)+" bytes")
	case strings.ContainsRune(req.Reason, 0):
		return invalid("reason", "must not contain NUL characters")
	}

	result, err := s.store.RecordReport(r.Context(), store.Report{
		DeviceID:     device.ID,
		Sequence:     req.Sequence,
		RolloutID:    req.RolloutID,
		AssignmentID: req.AssignmentID,
		Generation:   req.Generation,
		State:        req.State,
		Reason:       req.Reason,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, apiv1.ReportResponse{
		Applied:       result.Applied,
		Duplicate:     result.Duplicate,
		IgnoredReason: result.IgnoredReason,
	})
	return nil
}

func deviceAssignmentDocument(a store.Assignment) apiv1.DeviceAssignment {
	return apiv1.DeviceAssignment{
		ID:             a.ID,
		RolloutID:      a.RolloutID,
		Generation:     a.Generation,
		Stage:          a.Stage,
		State:          a.State,
		Directive:      a.Directive,
		Kind:           a.Kind,
		Artifact:       artifactDocument(a.Artifact),
		RestoreRelease: a.RestoreRelease,
	}
}
