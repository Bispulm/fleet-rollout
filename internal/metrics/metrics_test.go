package metrics_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/metrics"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

func scrape(t *testing.T, m *metrics.Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func requireLine(t *testing.T, body, line string) {
	t.Helper()
	if !strings.Contains(body, line+"\n") {
		t.Fatalf("missing %q in:\n%s", line, body)
	}
}

func TestEventsAreCounted(t *testing.T) {
	m := metrics.New()
	m.Request("POST /v1/device/poll", "POST", 200, 3*time.Millisecond)
	m.Request("POST /v1/device/poll", "POST", 200, 4*time.Millisecond)
	m.Request("", "GET", 404, time.Millisecond)
	m.Shed("downloads")
	m.Report("succeeded", "applied")
	m.Report("installing", "on_hold")
	m.Decision("breach", "stage_breach")
	m.Decision("coverage_met", "")
	m.Leading(true)

	body := scrape(t, m)
	requireLine(t, body, `fleet_rollout_http_requests_total{code="200",method="POST",route="POST /v1/device/poll"} 2`)
	requireLine(t, body, `fleet_rollout_http_requests_total{code="404",method="GET",route="unmatched"} 1`)
	requireLine(t, body, `fleet_rollout_http_request_duration_seconds_count{route="POST /v1/device/poll"} 2`)
	requireLine(t, body, `fleet_rollout_device_requests_shed_total{pool="downloads"} 1`)
	requireLine(t, body, `fleet_rollout_device_reports_total{result="on_hold",state="installing"} 1`)
	requireLine(t, body, `fleet_rollout_engine_decisions_total{event="breach"} 1`)
	requireLine(t, body, `fleet_rollout_rollout_halts_total{reason="stage_breach"} 1`)
	requireLine(t, body, `fleet_rollout_engine_leader 1`)
	if strings.Contains(body, `reason=""`) {
		t.Fatal("a decision that isn't a halt was counted as one")
	}
}

func TestKnownCountersStartAtZero(t *testing.T) {
	body := scrape(t, metrics.New())
	for _, line := range []string{
		`fleet_rollout_rollout_halts_total{reason="certain_breach"} 0`,
		`fleet_rollout_engine_decisions_total{event="rollback_done"} 0`,
		`fleet_rollout_device_requests_shed_total{pool="downloads"} 0`,
	} {
		requireLine(t, body, line)
	}
}

func TestANilMetricsRecordsNothing(t *testing.T) {
	var m *metrics.Metrics
	m.Request("x", "GET", 200, time.Second)
	m.Shed("requests")
	m.Report("failed", "applied")
	m.Decision("breach", "stage_breach")
	m.Leading(true)
}

func TestRolloutStateIsReadAtEachScrape(t *testing.T) {
	ctx := context.Background()
	s := store.New(testdb.New(t))
	if _, err := s.CreateFleet(ctx, "lab-a", nil); err != nil {
		t.Fatal(err)
	}
	devices := make([]store.NewDevice, 10)
	for i := range devices {
		id := fmt.Sprintf("sim-%05d", i)
		devices[i] = store.NewDevice{ID: id, Platform: "linux/arm64", CredentialHash: sha256.Sum256([]byte(id))}
	}
	if _, err := s.RegisterDevices(ctx, "lab-a", devices); err != nil {
		t.Fatal(err)
	}
	artifact, _, err := s.RegisterArtifact(ctx, store.NewArtifact{
		Name: "demo-app", Version: "1.0.0", Platform: "linux/arm64", ReleaseCounter: 1, SizeBytes: 10,
		SHA256: sha256.Sum256([]byte("x")), ChunkSize: 10, Envelope: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArtifactContentReady(ctx, artifact.ID); err != nil {
		t.Fatal(err)
	}
	policy, err := rollout.PolicySpec{
		Stages: []rollout.StageSpec{{Percent: 50}, {Percent: 100}},
		Safety: rollout.SafetySpec{MaxFailureBPS: new(200)},
	}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := s.CreateRollout(ctx, "lab-a", store.NewRollout{Name: "demo", ArtifactID: artifact.ID, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}

	m := metrics.New()
	m.ObserveRollouts(s, slog.New(slog.DiscardHandler))
	if body := scrape(t, m); strings.Contains(body, "fleet_rollout_rollout_info{") {
		t.Fatalf("a draft was reported:\n%s", body)
	}
	if _, err := s.StartRollout(ctx, created.ID, store.TransitionOptions{Actor: "test"}); err != nil {
		t.Fatal(err)
	}
	body := scrape(t, m)
	labels := fmt.Sprintf(`fleet="lab-a",rollout="demo",rollout_id="%s"`, created.ID)
	requireLine(t, body, `fleet_rollout_rollout_info{`+labels+`,state="running"} 1`)
	requireLine(t, body, `fleet_rollout_rollout_current_stage{`+labels+`} 1`)
	requireLine(t, body, fmt.Sprintf(`fleet_rollout_stage_devices{fleet="lab-a",outcome="pending",rollout="demo",rollout_id="%s",stage="1"} 5`, created.ID))
	requireLine(t, body, `fleet_rollout_rollout_scrape_errors 0`)
}
