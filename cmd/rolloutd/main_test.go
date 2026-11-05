package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

// syncWriter lets the server's log output be collected without a data race.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func testEnv(dbURL string) func(string) string {
	vals := map[string]string{
		"ROLLOUTD_DATABASE_URL": dbURL,
		"ROLLOUTD_ADMIN_TOKEN":  strings.Repeat("x", 32),
		"ROLLOUTD_LISTEN_ADDR":  "127.0.0.1:0",
	}
	return func(k string) string { return vals[k] }
}

func httpStatus(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

func TestServeReportsUnreadyUntilMigratedThenShutsDownCleanly(t *testing.T) {
	dbURL := testdb.NewURL(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	listening := make(chan net.Addr, 1)
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() {
		done <- run(ctx, []string{"serve"}, env{
			getenv:      testEnv(dbURL),
			stdout:      io.Discard,
			stderr:      &syncWriter{w: &logs},
			onListening: func(addr net.Addr) { listening <- addr },
		})
	}()

	var base string
	select {
	case addr := <-listening:
		base = "http://" + addr.String()
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start listening")
	}

	if got := httpStatus(t, base+"/healthz"); got != http.StatusOK {
		t.Errorf("healthz = %d, want 200", got)
	}
	if got := httpStatus(t, base+"/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("readyz before migrating = %d, want 503", got)
	}

	migrateErr := run(ctx, []string{"migrate"}, env{getenv: testEnv(dbURL), stdout: io.Discard, stderr: io.Discard})
	if migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	if got := httpStatus(t, base+"/readyz"); got != http.StatusOK {
		t.Errorf("readyz after migrating = %d, want 200", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after shutdown, want nil", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestRunRejectsBadInvocations(t *testing.T) {
	cases := map[string][]string{
		"no command":      nil,
		"unknown command": {"explode"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			err := run(t.Context(), args, env{getenv: func(string) string { return "" }, stdout: io.Discard, stderr: &stderr})
			if err == nil {
				t.Fatal("run succeeded, want an error")
			}
			if !strings.Contains(err.Error(), "serve") {
				t.Fatalf("error %q should list the available commands", err)
			}
		})
	}
}

func TestRunExplainsMissingConfiguration(t *testing.T) {
	err := run(t.Context(), []string{"serve"}, env{getenv: func(string) string { return "" }, stdout: io.Discard, stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "ROLLOUTD_DATABASE_URL") {
		t.Fatalf("run error = %v, want it to name the missing setting", err)
	}
}
