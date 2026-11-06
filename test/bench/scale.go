package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/sim"
)

// deviceRoutes are the routes devices call, which are the ones that grow with the fleet.
var deviceRoutes = []string{
	"POST /v1/device/poll",
	"POST /v1/device/reports",
	"GET /v1/device/artifacts/{id}/manifest",
	"GET /v1/device/artifacts/{id}/content",
}

type scaleResult struct {
	Devices        int                    `json:"devices"`
	State          rollout.State          `json:"state"`
	TimedOut       bool                   `json:"timed_out"`
	Elapsed        time.Duration          `json:"elapsed"`
	Routes         map[string]*routeStats `json:"routes"`
	DeviceRequests int                    `json:"device_requests"`
	RequestsPerSec float64                `json:"requests_per_second"`
	// Shed counts the 503 answers devices received: shed requests, and requests that timed out
	// inside rolloutd. Requests devices gave up on when the run ended aren't in it.
	Shed     int       `json:"shed"`
	Errors   int       `json:"errors"`
	Rolloutd procUsage `json:"rolloutd"`
	// SimulatorCPU is what the simulated devices used, on the same machine.
	SimulatorCPU time.Duration `json:"simulator_cpu"`
	Postgres     postgresLoad  `json:"postgres"`
	Pass         bool          `json:"pass"`
}

type postgresLoad struct {
	// CPU is in cores, from docker stats on the Postgres container.
	CPUMean           float64 `json:"cpu_mean"`
	CPUPeak           float64 `json:"cpu_peak"`
	CommitsPerSec     float64 `json:"commits_per_second"`
	RowsWrittenPerSec float64 `json:"rows_written_per_second"`
	RowsReadPerSec    float64 `json:"rows_read_per_second"`
	CacheHitRatio     float64 `json:"cache_hit_ratio"`
}

// runScale runs a full staged rollout on live fleets of growing size, against real rolloutd and
// Postgres on this machine, and reports latency inside rolloutd and the load on each part.
func runScale(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("bench scale", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := databaseFlag(flags)
	binary := flags.String("rolloutd", "bin/rolloutd", "rolloutd binary to run")
	sizes := flags.String("devices", "1000,2500,5000,10000,20000", "fleet sizes, comma-separated")
	pollInterval := flags.Duration("poll-interval", 10*time.Second, "ROLLOUTD_POLL_INTERVAL")
	soak := flags.Duration("soak", 30*time.Second, "soak time for each of the four stages")
	slo := flags.Duration("p99", 100*time.Millisecond, "p99 latency a device route must stay under for a size to pass")
	container := flags.String("postgres-container", "fleet-rollout-postgres-1", "Docker container running Postgres, for its CPU use")
	keepGoing := flags.Bool("keep-going", false, "run every size even after one fails")
	out := flags.String("json", "", "also write the results to this file as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	serverURL, err := databaseServer(*server, getenv)
	if err != nil {
		return err
	}
	fleetSizes, err := parseInts(*sizes)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "fleet-rollout-bench-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	m := describeMachine()
	env := map[string]string{"ROLLOUTD_POLL_INTERVAL": pollInterval.String()}
	var results []scaleResult
	for _, devices := range fleetSizes {
		r, err := scaleRun(ctx, *binary, serverURL, dir, env, *container, devices, *soak)
		if err != nil {
			return fmt.Errorf("%d devices: %w", devices, err)
		}
		r.Pass = r.State == rollout.StateCompleted && !r.TimedOut && r.Shed == 0 && r.Errors == 0
		for _, route := range deviceRoutes {
			if stats := r.Routes[route]; stats != nil && stats.P99 >= *slo {
				r.Pass = false
			}
		}
		results = append(results, r)
		_, _ = fmt.Fprintf(stderr, "%d devices: %s in %s, %.0f requests/s, pass %v\n",
			devices, r.State, r.Elapsed.Round(time.Second), r.RequestsPerSec, r.Pass)
		if !r.Pass && !*keepGoing {
			break
		}
	}
	m.LoadTo = loadAverage()

	_, _ = fmt.Fprintf(stdout, "Scale: a staged rollout (10%%, 25%%, 50%%, 100%%, %s soaks) on live simulated fleets, devices "+
		"polling every %s, rolloutd with default settings, everything on one machine.\n", *soak, *pollInterval)
	printMachine(stdout, m)
	_, _ = fmt.Fprintln(stdout)
	rows := make([][]string, 0, len(results))
	for _, r := range results {
		poll, report := r.Routes[deviceRoutes[0]], r.Routes[deviceRoutes[1]]
		rows = append(rows, []string{
			strconv.Itoa(r.Devices), strconv.FormatFloat(r.RequestsPerSec, 'f', 0, 64),
			latencies(poll), latencies(report), strconv.Itoa(r.Shed),
			fmt.Sprintf("%.2f", r.Rolloutd.CPU.Seconds()/r.Rolloutd.Elapsed.Seconds()), fmt.Sprintf("%.0f", r.Rolloutd.MaxRSSMB),
			fmt.Sprintf("%.2f / %.2f", r.Postgres.CPUMean, r.Postgres.CPUPeak), fmt.Sprintf("%.0f", r.Postgres.CommitsPerSec),
			fmt.Sprintf("%.2f", r.SimulatorCPU.Seconds()/r.Elapsed.Seconds()),
			fmt.Sprintf("%s in %s", r.State, r.Elapsed.Round(time.Second)),
		})
	}
	table(stdout, []string{
		"Devices", "Device requests/s", "Poll p50/p95/p99 (ms)", "Report p50/p95/p99 (ms)", "Shed",
		"rolloutd CPU (cores)", "rolloutd peak RSS (MB)", "Postgres CPU mean/peak (cores)", "Postgres commits/s",
		"Simulator CPU (cores)", "Rollout",
	}, rows)
	largest := 0
	for _, r := range results {
		if r.Pass {
			largest = max(largest, r.Devices)
		}
	}
	_, _ = fmt.Fprintf(stdout, "\nLargest fleet that completed with nothing shed, no errors, and every device route under %s at p99: %d devices.\n",
		*slo, largest)
	return writeJSON(*out, struct {
		Machine machine       `json:"machine"`
		Results []scaleResult `json:"results"`
	}{m, results})
}

func latencies(stats *routeStats) string {
	if stats == nil {
		return "-"
	}
	return formatMillis(stats.P50) + " / " + formatMillis(stats.P95) + " / " + formatMillis(stats.P99)
}

func scaleRun(ctx context.Context, binary, databaseServer, dir string, env map[string]string, container string, devices int, soak time.Duration) (scaleResult, error) {
	srv, err := startServer(ctx, binary, databaseServer, dir, env)
	if err != nil {
		return scaleResult{}, err
	}
	defer func() { _ = srv.cleanUp() }()

	before, err := readPostgresStats(ctx, srv.DBURL)
	if err != nil {
		return scaleResult{}, err
	}
	cpuBefore := selfCPU()
	sampler := startCPUSampler(ctx, container)
	stage := func(percent int) rollout.StageSpec {
		return rollout.StageSpec{Percent: percent, Soak: durationPtr(soak), Deadline: durationPtr(30 * time.Minute)}
	}
	report, err := sim.RunLive(ctx, sim.LiveConfig{
		Scenario: sim.Scenario{
			Seed: 1, Fleet: "scale", Devices: devices, ArtifactBytes: 8 << 10, ChunkSize: 4 << 10,
			Policy: rollout.PolicySpec{
				Stages: []rollout.StageSpec{stage(10), stage(25), stage(50), stage(100)},
				Safety: rollout.SafetySpec{MaxFailureBPS: new(500)},
			},
			MaxDuration: 30 * time.Minute,
		},
		APIURL: srv.URL, AdminToken: srv.Token, Signer: srv.Signer, Trust: srv.Trust,
	})
	cpuMean, cpuPeak := sampler.stop()
	simulatorCPU := selfCPU() - cpuBefore
	if err != nil {
		_, _ = srv.stop()
		return scaleResult{}, err
	}
	after, err := readPostgresStats(ctx, srv.DBURL)
	if err != nil {
		_, _ = srv.stop()
		return scaleResult{}, err
	}
	used, err := srv.stop()
	if err != nil {
		return scaleResult{}, err
	}
	routes, err := readRequestLog(srv.LogPath)
	if err != nil {
		return scaleResult{}, err
	}

	r := scaleResult{
		Devices: devices, State: report.State, TimedOut: report.TimedOut, Elapsed: report.Elapsed, Routes: map[string]*routeStats{},
		Rolloutd: used, SimulatorCPU: simulatorCPU, Shed: report.Faults["shed"],
	}
	for _, route := range deviceRoutes {
		if stats := routes[route]; stats != nil {
			r.Routes[route] = stats
			r.DeviceRequests += stats.Count
			r.Errors += stats.Errors
		}
	}
	seconds := report.Elapsed.Seconds()
	r.RequestsPerSec = float64(r.DeviceRequests) / seconds
	r.Postgres = postgresLoad{
		CPUMean: cpuMean, CPUPeak: cpuPeak,
		CommitsPerSec:     float64(after.commits-before.commits) / seconds,
		RowsWrittenPerSec: float64(after.written-before.written) / seconds,
		RowsReadPerSec:    float64(after.read-before.read) / seconds,
	}
	if hits, reads := after.hits-before.hits, after.reads-before.reads; hits+reads > 0 {
		r.Postgres.CacheHitRatio = float64(hits) / float64(hits+reads)
	}
	return r, nil
}

type postgresStats struct {
	commits, written, read, hits, reads int64
}

// readPostgresStats reads the scratch database's own counters, so nothing else on the server
// is counted.
func readPostgresStats(ctx context.Context, dbURL string) (postgresStats, error) {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return postgresStats{}, err
	}
	defer func() { _ = conn.Close(ctx) }()
	var s postgresStats
	// Counters are cached per transaction and refreshed on demand, so ask for fresh ones.
	if _, err := conn.Exec(ctx, "SELECT pg_stat_clear_snapshot()"); err != nil {
		return postgresStats{}, err
	}
	err = conn.QueryRow(ctx, `SELECT xact_commit, tup_inserted + tup_updated + tup_deleted, tup_returned, blks_hit, blks_read
		FROM pg_stat_database WHERE datname = current_database()`).Scan(&s.commits, &s.written, &s.read, &s.hits, &s.reads)
	return s, err
}

func selfCPU() time.Duration {
	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err != nil {
		return 0
	}
	return time.Duration(r.Utime.Nano() + r.Stime.Nano())
}

// cpuSampler reads a container's CPU use from docker stats until stopped.
type cpuSampler struct {
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	samples []float64
}

func startCPUSampler(ctx context.Context, container string) *cpuSampler {
	ctx, cancel := context.WithCancel(ctx)
	s := &cpuSampler{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		for ctx.Err() == nil {
			out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{.CPUPerc}}", container).Output() //nolint:gosec // The container to watch.
			if err == nil {
				if v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(string(out)), "%"), 64); err == nil {
					s.mu.Lock()
					s.samples = append(s.samples, v/100)
					s.mu.Unlock()
				}
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
	return s
}

// stop returns the mean and peak CPU use, in cores.
func (s *cpuSampler) stop() (mean, peak float64) {
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range s.samples {
		sum += v
		peak = max(peak, v)
	}
	return sum / float64(len(s.samples)), peak
}
