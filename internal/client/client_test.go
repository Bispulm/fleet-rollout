package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
)

func TestSendsTokenAndDecodesSuccess(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	var gotBody apiv1.CreateFleetRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(apiv1.Fleet{ID: "f1", Name: gotBody.Name})
	}))
	defer srv.Close()

	c := client.New(srv.URL, "secret", srv.Client())
	fleet, err := c.CreateFleet(t.Context(), apiv1.CreateFleetRequest{Name: "lab-a"})
	if err != nil {
		t.Fatalf("CreateFleet: %v", err)
	}
	if fleet.ID != "f1" || gotAuth != "Bearer secret" || gotMethod != http.MethodPost || gotPath != "/v1/fleets" {
		t.Fatalf("fleet = %+v, auth = %q, %s %s", fleet, gotAuth, gotMethod, gotPath)
	}
}

func TestReturnsProblemsAsTypedErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(apiv1.Problem{
			Status: http.StatusUnprocessableEntity,
			Title:  "Unprocessable Entity",
			Detail: "the rollout policy can't be satisfied",
			Code:   apiv1.CodeInvalidPolicy,
			Violations: []rollout.Violation{{
				Field: "stages[0].percent", Message: "must be between 1 and 100, got 0",
			}},
		})
	}))
	defer srv.Close()

	_, err := client.New(srv.URL, "t", srv.Client()).GetRollout(t.Context(), "r1")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if !client.IsCode(err, apiv1.CodeInvalidPolicy) || len(apiErr.Problem.Violations) != 1 {
		t.Fatalf("problem = %+v", apiErr.Problem)
	}
	want := "the rollout policy can't be satisfied\n  stages[0].percent: must be between 1 and 100, got 0"
	if apiErr.Error() != want {
		t.Fatalf("Error() = %q, want %q", apiErr.Error(), want)
	}
}

func TestDescribesNonProblemErrorResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<html>bad gateway</html>", http.StatusBadGateway)
	}))
	defer srv.Close()
	_, err := client.New(srv.URL, "t", srv.Client()).GetFleet(t.Context(), "lab-a")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Problem.Status != http.StatusBadGateway {
		t.Fatalf("error = %#v", err)
	}
}

func TestEscapesPathSegments(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_ = json.NewEncoder(w).Encode(apiv1.Fleet{})
	}))
	defer srv.Close()
	if _, err := client.New(srv.URL, "t", srv.Client()).GetFleet(t.Context(), "a/b c"); err != nil {
		t.Fatalf("GetFleet: %v", err)
	}
	if gotPath != "/v1/fleets/a%2Fb%20c" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestContextCancelsRequests(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := client.New(srv.URL, "t", srv.Client()).GetFleet(ctx, "lab-a")
	if err == nil {
		t.Fatal("request succeeded after its context ended")
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("a transport failure was reported as an API error: %v", err)
	}
}
