package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

func TestQuantilesAreValuesThatWereObserved(t *testing.T) {
	values := sortedCopy([]time.Duration{5, 1, 4, 2, 3, 10, 9, 8, 7, 6})
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{{0, 1}, {0.5, 5}, {0.95, 10}, {0.99, 10}, {1, 10}, {0.11, 2}} {
		if got := quantile(values, c.q); got != c.want {
			t.Errorf("quantile(%g) = %d, want %d", c.q, got, c.want)
		}
	}
	if got := quantile(nil, 0.5); got != 0 {
		t.Errorf("quantile of nothing = %d", got)
	}
}

func TestTheRequestLogIsReadByRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rolloutd.log")
	lines := []string{
		`{"level":"INFO","msg":"starting"}`,
		`{"msg":"request","route":"POST /v1/device/poll","status":200,"duration":1000000}`,
		`{"msg":"request","route":"POST /v1/device/poll","status":200,"duration":3000000}`,
		`{"msg":"request","route":"POST /v1/device/poll","status":503,"duration":50000}`,
		`{"msg":"request","route":"POST /v1/device/reports","status":500,"duration":2000000}`,
		`not json`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	routes, err := readRequestLog(path)
	if err != nil {
		t.Fatal(err)
	}
	poll := routes["POST /v1/device/poll"]
	// The 503 is counted, and kept out of the latencies.
	if poll == nil || poll.Count != 3 || poll.Unavailable != 1 || poll.Errors != 0 || poll.P50 != time.Millisecond ||
		poll.Max != 3*time.Millisecond {
		t.Fatalf("poll = %+v", poll)
	}
	if reports := routes["POST /v1/device/reports"]; reports == nil || reports.Errors != 1 || reports.Unavailable != 0 {
		t.Fatalf("reports = %+v", reports)
	}
	if len(routes) != 2 {
		t.Fatalf("routes = %v, want only the two that were requested", routes)
	}
}

func TestResumeAndStagingRunEndToEnd(t *testing.T) {
	server := testdb.ServerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	getenv := func(string) string { return "" }

	var out bytes.Buffer
	if err := run(ctx, []string{"resume", "--database-url", server, "--devices", "5", "--artifact-bytes", "65536",
		"--chunk-size", "8192", "--break-rates", "0,0.2"}, getenv, &out, &bytes.Buffer{}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	// With nothing broken, each device downloads the artifact exactly once either way.
	if !strings.Contains(out.String(), "| 0.0%") || !strings.Contains(out.String(), "| 1.00×") {
		t.Fatalf("resume output:\n%s", out.String())
	}

	out.Reset()
	if err := run(ctx, []string{"staging", "--database-url", server, "--devices", "200", "--min-observations", "2", "--bad-shares", "1"},
		getenv, &out, &bytes.Buffer{}); err != nil {
		t.Fatalf("staging: %v", err)
	}
	if strings.Count(out.String(), "| halted") != 2 {
		t.Fatalf("both arms should halt a release that fails everywhere:\n%s", out.String())
	}
}

func TestAnUnknownCommandIsRefused(t *testing.T) {
	err := run(context.Background(), []string{"nope"}, func(string) string { return "" }, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), `unknown command "nope"`) {
		t.Fatalf("err = %v", err)
	}
}
