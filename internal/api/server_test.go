package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/api"
)

func newTestServer(t *testing.T, check api.ReadinessCheck, logs *bytes.Buffer) *api.Server {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	if logs != nil {
		logger = slog.New(slog.NewJSONHandler(logs, nil))
	}
	return api.New(api.Options{Logger: logger, Ready: check})
}

func alwaysReady(context.Context) error { return nil }

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthzDoesNotDependOnReadiness(t *testing.T) {
	srv := newTestServer(t, func(context.Context) error { return errors.New("database down") }, nil)
	rec := get(t, srv, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReadyzReflectsCheck(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		rec := get(t, newTestServer(t, alwaysReady, nil), "/readyz")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("unready", func(t *testing.T) {
		secret := "postgres://user:hunter2@db/rollout"
		srv := newTestServer(t, func(context.Context) error { return errors.New(secret) }, nil)
		rec := get(t, srv, "/readyz")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "hunter2") {
			t.Fatalf("readiness body leaks check error: %s", rec.Body)
		}
	})
}

func TestReadyzFailsOnceDraining(t *testing.T) {
	srv := newTestServer(t, alwaysReady, nil)
	srv.Drain()
	rec := get(t, srv, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 so load balancers stop routing here", rec.Code)
	}
	if rec := get(t, srv, "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("healthz while draining = %d, want 200", rec.Code)
	}
}

func TestUnknownRouteReturnsProblemJSON(t *testing.T) {
	rec := get(t, newTestServer(t, alwaysReady, nil), "/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json", ct)
	}
	var problem struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &problem); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body, err)
	}
	if problem.Status != http.StatusNotFound || problem.Title == "" {
		t.Fatalf("problem = %+v", problem)
	}
}

func TestWrongMethodReturnsProblemJSONWithAllow(t *testing.T) {
	srv := newTestServer(t, alwaysReady, nil)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/healthz", http.NoBody)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Fatalf("Allow = %q, want it to include GET", allow)
	}
}

func TestPanicBecomesInternalErrorWithoutDetails(t *testing.T) {
	var logs bytes.Buffer
	srv := newTestServer(t, alwaysReady, &logs)
	srv.Handle("GET /boom", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret detail")
	}))
	rec := get(t, srv, "/boom")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret detail") {
		t.Fatalf("response leaks panic value: %s", rec.Body)
	}
	if !strings.Contains(logs.String(), "secret detail") {
		t.Fatalf("panic value missing from logs: %s", logs.String())
	}
}

func TestResponsesCarryRequestIDThatAppearsInLogs(t *testing.T) {
	var logs bytes.Buffer
	srv := newTestServer(t, alwaysReady, &logs)
	rec := get(t, srv, "/healthz")
	id := rec.Header().Get("X-Request-Id")
	if id == "" {
		t.Fatal("response has no X-Request-Id")
	}
	if !strings.Contains(logs.String(), id) {
		t.Fatalf("request id %q missing from logs: %s", id, logs.String())
	}
}

func TestInboundRequestIDIsReplacedWhenMalformed(t *testing.T) {
	srv := newTestServer(t, alwaysReady, nil)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", http.NoBody)
	req.Header.Set("X-Request-Id", "bad id\nwith newline")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Request-Id"); got == "" || strings.ContainsAny(got, " \n") {
		t.Fatalf("X-Request-Id = %q, want a generated clean value", got)
	}
}

func TestServeFinishesInFlightRequestsOnShutdown(t *testing.T) {
	srv := newTestServer(t, alwaysReady, nil)
	started := make(chan struct{})
	release := make(chan struct{})
	srv.Handle("GET /slow", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx, listener, 10*time.Second) }()

	url := "http://" + listener.Addr().String() + "/slow"
	statusCh := make(chan int, 1)
	errCh := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
		if err != nil {
			errCh <- err
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = resp.Body.Close() }()
		statusCh <- resp.StatusCode
	}()

	<-started
	cancel()
	select {
	case err := <-serveDone:
		t.Fatalf("Serve returned before the in-flight request finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	select {
	case status := <-statusCh:
		if status != http.StatusNoContent {
			t.Fatalf("in-flight status = %d, want 204", status)
		}
	case err := <-errCh:
		t.Fatalf("in-flight request failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not complete")
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}
