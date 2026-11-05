package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestRetryClassification(t *testing.T) {
	api := func(status int) error { return &client.APIError{Problem: apiv1.Problem{Status: status}} }
	retryable := map[string]error{
		"transport":    errors.New("connection refused"),
		"deadline":     context.DeadlineExceeded,
		"server error": api(http.StatusInternalServerError),
		"unavailable":  api(http.StatusServiceUnavailable),
		"bad gateway":  api(http.StatusBadGateway),
		"rate limited": api(http.StatusTooManyRequests),
	}
	for name, err := range retryable {
		if !client.IsRetryable(err) {
			t.Errorf("%s should be retryable", name)
		}
	}
	permanent := map[string]error{
		"no error":     nil,
		"canceled":     context.Canceled,
		"unauthorized": api(http.StatusUnauthorized),
		"forbidden":    api(http.StatusForbidden),
		"not found":    api(http.StatusNotFound),
		"conflict":     api(http.StatusConflict),
		"invalid":      api(http.StatusUnprocessableEntity),
	}
	for name, err := range permanent {
		if client.IsRetryable(err) {
			t.Errorf("%s should not be retryable", name)
		}
	}
}

func TestRetryAfterIsParsedFromResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	_, err := client.New(srv.URL, "t", srv.Client()).GetFleet(t.Context(), "lab-a")
	if got := client.RetryAfter(err); got != 7*time.Second {
		t.Fatalf("RetryAfter = %v, want 7s", got)
	}
	if !client.IsStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("IsStatus = false for %v", err)
	}
}

func TestFetchRangeReportsWhatTheServerSaid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Range"); got != "bytes=1024-2047" {
			t.Errorf("Range = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer device-token" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Range", "bytes 1024-2047/9000")
		w.Header().Set("ETag", `"sha256:abc"`)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("chunk"))
	}))
	defer srv.Close()
	resp, err := client.NewDevice(srv.URL, "device-token", srv.Client()).FetchRange(t.Context(), "a1", 1024, 1024)
	if err != nil {
		t.Fatalf("FetchRange: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent || resp.ContentRange != "bytes 1024-2047/9000" || resp.ETag != `"sha256:abc"` {
		t.Fatalf("response = %+v", resp)
	}
}

func TestFetchRangeTurnsErrorsIntoTypedOnes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(apiv1.Problem{Status: http.StatusForbidden, Code: apiv1.CodeNoOpenAssignment})
	}))
	defer srv.Close()
	_, err := client.NewDevice(srv.URL, "t", srv.Client()).FetchRange(t.Context(), "a1", 0, 10)
	if !client.IsCode(err, apiv1.CodeNoOpenAssignment) {
		t.Fatalf("error = %v", err)
	}
}

// endless serves a JSON string that never ends, as a hostile server could.
type endless struct{ started bool }

func (e *endless) Read(p []byte) (int, error) {
	if !e.started {
		e.started = true
		return copy(p, `{"payload":"`), nil
	}
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestADeviceDoesntBufferAnEndlessResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, &endless{})
	}))
	defer srv.Close()
	device := client.NewDevice(srv.URL, "token", nil)

	if _, err := device.Manifest(t.Context(), "11111111-1111-4111-8111-111111111111"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("Manifest error = %v, want one about the size", err)
	}
	if _, err := device.Poll(t.Context()); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("Poll error = %v, want one about the size", err)
	}
}

func TestAManifestIsParsedStrictly(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":   `{"payloadType":"x","payload":"","signatures":[],"extra":1}`,
		"trailing data":   `{"payloadType":"x","payload":"","signatures":[]} {}`,
		"not an envelope": `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()
			if _, err := client.NewDevice(srv.URL, "token", nil).Manifest(t.Context(), "11111111-1111-4111-8111-111111111111"); err == nil {
				t.Fatalf("accepted %s", body)
			}
		})
	}
}
