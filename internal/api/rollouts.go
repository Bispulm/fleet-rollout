package api

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

const (
	maxReasonLength = 1024
	// adminActor attributes changes in the audit log. There's one shared admin
	// credential for now, so this can't name an individual.
	adminActor = "admin"
)

func (s *Server) handleCreateRollout(w http.ResponseWriter, r *http.Request) error {
	var req apiv1.CreateRolloutRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return err
	}
	if !rollout.ValidName(req.Name) {
		return invalid("name", "must be lowercase letters, digits, and hyphens, at most 63 characters")
	}
	policy, err := req.Policy.Normalize()
	if err != nil {
		return err
	}
	created, isNew, err := s.store.CreateRollout(r.Context(), r.PathValue("fleet"), store.NewRollout{
		ID:         req.ID,
		Name:       req.Name,
		ArtifactID: req.ArtifactID,
		Policy:     policy,
	})
	if err != nil {
		return err
	}
	status := http.StatusOK
	if isNew {
		status = http.StatusCreated
	}
	writeJSON(w, status, rolloutDocument(created))
	return nil
}

func (s *Server) handleGetRollout(w http.ResponseWriter, r *http.Request) error {
	found, err := s.store.GetRollout(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rolloutDocument(found))
	return nil
}

// transitionFunc is a store method that applies one state change.
type transitionFunc func(context.Context, string, store.TransitionOptions) (store.Rollout, error)

// handleTransition adapts a store transition to a handler. The request body is
// optional, so `POST .../pause` with nothing else is enough during an incident.
func (s *Server) handleTransition(apply transitionFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		var req apiv1.TransitionRequest
		if err := decodeJSON(w, r, &req); err != nil && !isEmptyBody(err) {
			return err
		}
		if len(req.Reason) > maxReasonLength {
			return invalid("reason", "must be at most "+strconv.Itoa(maxReasonLength)+" bytes")
		}
		updated, err := apply(r.Context(), r.PathValue("id"), store.TransitionOptions{
			Actor:            adminActor,
			Reason:           req.Reason,
			ExpectedRevision: req.ExpectedRevision,
		})
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, rolloutDocument(updated))
		return nil
	}
}

func isEmptyBody(err error) bool {
	var reqErr *requestError
	return errors.As(err, &reqErr) && errors.Is(reqErr.cause, io.EOF)
}

func (s *Server) handleListCohort(w http.ResponseWriter, r *http.Request) error {
	limit, err := pageLimit(r)
	if err != nil {
		return err
	}
	stage, err := nonNegativeQuery(r, "stage")
	if err != nil {
		return err
	}
	after, err := nonNegativeQuery(r, "after")
	if err != nil {
		return err
	}
	entries, err := s.store.ListCohort(r.Context(), r.PathValue("id"), store.CohortQuery{
		Stage: stage, AfterPosition: after, Limit: limit,
	})
	if err != nil {
		return err
	}
	out := apiv1.CohortPage{Entries: make([]apiv1.CohortEntry, len(entries))}
	for i, entry := range entries {
		out.Entries[i] = apiv1.CohortEntry{
			DeviceID: entry.DeviceID,
			Position: entry.Position,
			Stage:    entry.Stage,
			RankKey:  hex.EncodeToString(entry.RankKey[:]),
		}
	}
	if len(entries) == limit {
		out.NextAfter = entries[len(entries)-1].Position
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) handleListTransitions(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	// An empty log is normal for a draft, so check the rollout itself exists.
	if _, err := s.store.GetRollout(r.Context(), id); err != nil {
		return err
	}
	log, err := s.store.ListTransitions(r.Context(), id)
	if err != nil {
		return err
	}
	out := apiv1.TransitionLog{Transitions: make([]apiv1.Transition, len(log))}
	for i, entry := range log {
		out.Transitions[i] = apiv1.Transition{
			Revision:   entry.Revision,
			From:       entry.From,
			To:         entry.To,
			Event:      entry.Event,
			Actor:      entry.Actor,
			Reason:     entry.Reason,
			OccurredAt: entry.OccurredAt,
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func nonNegativeQuery(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, badRequest(name + " must be a non-negative integer")
	}
	return value, nil
}

func rolloutDocument(r store.Rollout) apiv1.Rollout {
	stages := make([]apiv1.Stage, len(r.Stages))
	for i, st := range r.Stages {
		stages[i] = apiv1.Stage{
			Ordinal:         st.Ordinal,
			Percent:         st.Percent,
			TargetCount:     st.TargetCount,
			CohortSize:      st.CohortSize,
			State:           string(st.State),
			StartedAt:       st.StartedAt,
			DeadlineAt:      st.DeadlineAt,
			SoakStartedAt:   st.SoakStartedAt,
			SoakAccumulated: st.SoakAccumulated.String(),
			FinishedAt:      st.FinishedAt,
		}
	}
	return apiv1.Rollout{
		ID:           r.ID,
		FleetID:      r.FleetID,
		Fleet:        r.FleetName,
		ArtifactID:   r.ArtifactID,
		Name:         r.Name,
		State:        r.State,
		Revision:     r.Revision,
		Policy:       r.Policy,
		PolicyDigest: hex.EncodeToString(r.PolicyDigest[:]),
		CurrentStage: r.CurrentStage,
		MemberCount:  r.MemberCount,
		PausedFrom:   r.PausedFrom,
		PausedAt:     r.PausedAt,
		CreatedAt:    r.CreatedAt,
		StartedAt:    r.StartedAt,
		FinishedAt:   r.FinishedAt,
		Stages:       stages,
		Exclusions:   r.Exclusions,
	}
}
