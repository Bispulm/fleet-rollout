package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
)

func writeProblem(w http.ResponseWriter, status int, detail string) {
	writeProblemDocument(w, apiv1.Problem{Status: status, Detail: detail})
}

func writeProblemDocument(w http.ResponseWriter, p apiv1.Problem) {
	p.Type = "about:blank"
	p.Title = http.StatusText(p.Status)
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(p.Status)
	// The status line is already sent, so an encoding failure can only surface as
	// a truncated body. The access log records the status either way.
	_ = json.NewEncoder(w).Encode(p)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// requestError is a problem with the request itself, as opposed to one caused by
// the current state of a resource.
type requestError struct {
	status     int
	detail     string
	violations []rollout.Violation
	cause      error
}

func (e *requestError) Error() string { return e.detail }

func badRequest(detail string) error {
	return &requestError{status: http.StatusBadRequest, detail: detail}
}

func invalid(field, message string) error {
	return &requestError{
		status:     http.StatusUnprocessableEntity,
		detail:     "the request is invalid",
		violations: []rollout.Violation{{Field: field, Message: message}},
	}
}

// writeError turns an error from a handler into a problem document. Anything not
// recognized here is unexpected, so it's logged in full and hidden from the client.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		reqErr    *requestError
		policy    *rollout.PolicyError
		invalidTr *rollout.InvalidTransitionError
		mismatch  *store.RevisionMismatchError
		conflict  *store.ConflictError
	)
	switch {
	case errors.As(err, &reqErr):
		writeProblemDocument(w, apiv1.Problem{
			Status: reqErr.status, Detail: reqErr.detail, Code: apiv1.CodeInvalidRequest, Violations: reqErr.violations,
		})
	case errors.As(err, &policy):
		writeProblemDocument(w, apiv1.Problem{
			Status: http.StatusUnprocessableEntity, Detail: "the rollout policy can't be satisfied",
			Code: apiv1.CodeInvalidPolicy, Violations: policy.Violations,
		})
	case errors.As(err, &invalidTr):
		writeProblemDocument(w, apiv1.Problem{Status: http.StatusConflict, Detail: err.Error(), Code: apiv1.CodeInvalidTransition})
	case errors.As(err, &mismatch):
		writeProblemDocument(w, apiv1.Problem{Status: http.StatusConflict, Detail: err.Error(), Code: apiv1.CodeRevisionMismatch})
	case errors.As(err, &conflict):
		writeProblemDocument(w, apiv1.Problem{Status: http.StatusConflict, Detail: err.Error(), Code: apiv1.CodeConflict})
	case errors.Is(err, store.ErrReportConflict):
		writeProblemDocument(w, apiv1.Problem{Status: http.StatusConflict, Detail: err.Error(), Code: apiv1.CodeReportConflict})
	case errors.Is(err, store.ErrReportMismatch):
		writeProblemDocument(w, apiv1.Problem{Status: http.StatusConflict, Detail: err.Error(), Code: apiv1.CodeReportMismatch})
	case errors.Is(err, store.ErrMembershipNotFrozen):
		writeProblemDocument(w, apiv1.Problem{Status: http.StatusConflict, Detail: err.Error(), Code: apiv1.CodeMembershipNotReady})
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, http.StatusNotFound, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeProblem(w, http.StatusServiceUnavailable, "the request timed out")
	case errors.Is(err, context.Canceled):
		// The client went away, so this response is for the access log only.
		writeProblem(w, http.StatusServiceUnavailable, "the request was canceled")
	default:
		s.logger.LogAttrs(r.Context(), slog.LevelError, "request failed",
			slog.String("request_id", requestID(r.Context())), slog.Any("error", err))
		writeProblem(w, http.StatusInternalServerError, "")
	}
}
