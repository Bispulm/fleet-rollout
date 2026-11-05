package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/auth"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

const (
	adminRequestTimeout = 30 * time.Second
	// maxBodyBytes must fit a rollout that lists the maximum number of exclusions.
	maxBodyBytes     = 4 << 20
	defaultPageLimit = 100
	maxPageLimit     = 1000
	maxDeviceBatch   = 1000
)

// handlerFunc is a handler that reports failure by returning an error.
type handlerFunc func(http.ResponseWriter, *http.Request) error

// admin wraps a handler with the admin token check and a request deadline. Every
// route under /v1 for operators goes through here.
func (s *Server) admin(next handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.adminAuth.Verify(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
			writeProblemDocument(w, apiv1.Problem{Status: http.StatusUnauthorized, Code: apiv1.CodeUnauthorized})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), adminRequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		if err := next(w, r); err != nil {
			s.writeError(w, r, err)
		}
	})
}

func (s *Server) registerAdminRoutes() {
	s.mux.Handle("POST /v1/fleets", s.admin(s.handleCreateFleet))
	s.mux.Handle("GET /v1/fleets/{fleet}", s.admin(s.handleGetFleet))
	s.mux.Handle("POST /v1/fleets/{fleet}/devices", s.admin(s.handleRegisterDevices))
	s.mux.Handle("GET /v1/fleets/{fleet}/devices", s.admin(s.handleListDevices))
	s.mux.Handle("PATCH /v1/devices/{id}", s.admin(s.handleUpdateDevice))
	s.mux.Handle("POST /v1/artifacts", s.admin(s.handleCreateArtifact))
	s.mux.Handle("GET /v1/artifacts/{id}", s.admin(s.handleGetArtifact))
	s.mux.Handle("POST /v1/fleets/{fleet}/rollouts", s.admin(s.handleCreateRollout))
	s.mux.Handle("GET /v1/rollouts/{id}", s.admin(s.handleGetRollout))
	s.mux.Handle("GET /v1/rollouts/{id}/cohort", s.admin(s.handleListCohort))
	s.mux.Handle("GET /v1/rollouts/{id}/transitions", s.admin(s.handleListTransitions))
	s.mux.Handle("POST /v1/rollouts/{id}/start", s.admin(s.handleTransition(s.store.StartRollout)))
	s.mux.Handle("POST /v1/rollouts/{id}/pause", s.admin(s.handleTransition(s.store.PauseRollout)))
	s.mux.Handle("POST /v1/rollouts/{id}/resume", s.admin(s.handleTransition(s.store.ResumeRollout)))
	s.mux.Handle("POST /v1/rollouts/{id}/abort", s.admin(s.handleTransition(s.store.AbortRollout)))
}

// decodeJSON reads exactly one JSON document, rejecting unknown fields so that a
// misspelled safety setting fails loudly rather than silently using a default.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return &requestError{status: http.StatusRequestEntityTooLarge, detail: "the request body is too large"}
		}
		return &requestError{status: http.StatusBadRequest, detail: "the request body is not valid: " + err.Error(), cause: err}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return badRequest("the request body must contain a single JSON document")
	}
	return nil
}

func pageLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultPageLimit, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maxPageLimit {
		return 0, badRequest("limit must be an integer between 1 and " + strconv.Itoa(maxPageLimit))
	}
	return limit, nil
}

func (s *Server) handleCreateFleet(w http.ResponseWriter, r *http.Request) error {
	var req apiv1.CreateFleetRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if !rollout.ValidName(req.Name) {
		return invalid("name", "must be lowercase letters, digits, and hyphens, at most 63 characters")
	}
	if !rollout.ValidLabels(req.Labels) {
		return invalid("labels", "has too many entries, or a malformed key or value")
	}
	fleet, err := s.store.CreateFleet(r.Context(), req.Name, req.Labels)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, fleetDocument(fleet))
	return nil
}

func (s *Server) handleGetFleet(w http.ResponseWriter, r *http.Request) error {
	fleet, err := s.store.GetFleet(r.Context(), r.PathValue("fleet"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, fleetDocument(fleet))
	return nil
}

func (s *Server) handleRegisterDevices(w http.ResponseWriter, r *http.Request) error {
	var req apiv1.RegisterDevicesRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if len(req.Devices) == 0 || len(req.Devices) > maxDeviceBatch {
		return invalid("devices", "must contain between 1 and "+strconv.Itoa(maxDeviceBatch)+" devices")
	}

	devices := make([]store.NewDevice, len(req.Devices))
	tokens := make(map[string]string, len(req.Devices))
	for i, spec := range req.Devices {
		field := "devices[" + strconv.Itoa(i) + "]"
		switch {
		case !rollout.ValidDeviceID(spec.ID):
			return invalid(field+".id", "must be letters, digits, '.', '_', and '-', at most 128 characters")
		case !rollout.ValidPlatform(spec.Platform):
			return invalid(field+".platform", "must have the form os/arch")
		case !rollout.ValidLabels(spec.Labels):
			return invalid(field+".labels", "has too many entries, or a malformed key or value")
		case spec.InstalledRelease < 0:
			return invalid(field+".installed_release", "must not be negative")
		}
		if _, repeated := tokens[spec.ID]; repeated {
			return invalid(field+".id", "appears more than once in this request")
		}
		token, hash, err := auth.NewDeviceToken()
		if err != nil {
			return err
		}
		tokens[spec.ID] = token
		devices[i] = store.NewDevice{
			ID:               spec.ID,
			Platform:         spec.Platform,
			Labels:           spec.Labels,
			InstalledRelease: spec.InstalledRelease,
			CredentialHash:   hash,
		}
	}

	registered, err := s.store.RegisterDevices(r.Context(), r.PathValue("fleet"), devices)
	if err != nil {
		return err
	}
	out := apiv1.DeviceList{Devices: make([]apiv1.Device, len(registered))}
	for i, device := range registered {
		out.Devices[i] = deviceDocument(device)
		out.Devices[i].Token = tokens[device.ID]
	}
	writeJSON(w, http.StatusCreated, out)
	return nil
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) error {
	limit, err := pageLimit(r)
	if err != nil {
		return err
	}
	devices, err := s.store.ListDevices(r.Context(), r.PathValue("fleet"), r.URL.Query().Get("after"), limit)
	if err != nil {
		return err
	}
	out := apiv1.DeviceList{Devices: make([]apiv1.Device, len(devices))}
	for i, device := range devices {
		out.Devices[i] = deviceDocument(device)
	}
	if len(devices) == limit {
		out.NextAfter = devices[len(devices)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) handleUpdateDevice(w http.ResponseWriter, r *http.Request) error {
	var req apiv1.UpdateDeviceRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	switch req.Status {
	case rollout.DeviceActive, rollout.DeviceExcluded, rollout.DeviceDecommissioned:
	default:
		return invalid("status", "must be active, excluded, or decommissioned")
	}
	device, err := s.store.SetDeviceStatus(r.Context(), r.PathValue("id"), req.Status)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, deviceDocument(device))
	return nil
}

func fleetDocument(f store.Fleet) apiv1.Fleet {
	return apiv1.Fleet{ID: f.ID, Name: f.Name, Labels: f.Labels, CreatedAt: f.CreatedAt}
}

func deviceDocument(d store.Device) apiv1.Device {
	return apiv1.Device{
		ID:               d.ID,
		FleetID:          d.FleetID,
		Platform:         d.Platform,
		Labels:           d.Labels,
		Status:           d.Status,
		InstalledRelease: d.InstalledRelease,
		LastSeenAt:       d.LastSeenAt,
		CreatedAt:        d.CreatedAt,
	}
}
