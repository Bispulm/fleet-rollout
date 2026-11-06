package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/db"
	"github.com/Bispulm/fleet-rollout/internal/signing"
)

// benchAddr is where the benchmarks run rolloutd, inside the project's reserved ports.
const benchAddr = "127.0.0.1:18106"

// server is a rolloutd process the benchmark started, on a scratch database of its own.
type server struct {
	URL     string
	Token   string
	Signer  ed25519.PrivateKey
	Trust   signing.TrustSet
	DBURL   string
	LogPath string

	cmd     *exec.Cmd
	logFile *os.File
	drop    func() error
	started time.Time
}

// procUsage is what a stopped rolloutd used.
type procUsage struct {
	CPU      time.Duration `json:"cpu"`
	Elapsed  time.Duration `json:"elapsed"`
	MaxRSSMB float64       `json:"max_rss_mb"`
}

// startServer migrates a scratch database and runs rolloutd serve against it, with env added
// to its configuration, logging to a file in dir.
func startServer(ctx context.Context, binary, databaseServer, dir string, env map[string]string) (*server, error) {
	dbURL, drop, err := db.CreateScratch(ctx, databaseServer, "rollout_bench_")
	if err != nil {
		return nil, err
	}
	s := &server{URL: "http://" + benchAddr, DBURL: dbURL, drop: drop}
	if err := s.start(ctx, binary, dir, env); err != nil {
		_ = s.cleanUp()
		return nil, err
	}
	return s, nil
}

func (s *server) start(ctx context.Context, binary, dir string, env map[string]string) error {
	pool, err := db.Open(ctx, s.DBURL, 2)
	if err != nil {
		return err
	}
	_, err = db.Migrate(ctx, pool)
	pool.Close()
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	if s.Signer, err = signing.GenerateKey(); err != nil {
		return err
	}
	public, err := signing.MarshalPublicKey(s.Signer.Public().(ed25519.PublicKey)) //nolint:forcetypeassert // Ed25519 keys have Ed25519 public keys.
	if err != nil {
		return err
	}
	if s.Trust, err = signing.ParseTrustSet(public); err != nil {
		return err
	}
	trustFile := filepath.Join(dir, "trust.pem")
	if err := os.WriteFile(trustFile, public, 0o600); err != nil {
		return err
	}
	token := make([]byte, 24)
	_, _ = rand.Read(token)
	s.Token = hex.EncodeToString(token)
	artifacts := filepath.Join(dir, "artifacts")
	if err := os.RemoveAll(artifacts); err != nil {
		return err
	}

	settings := map[string]string{
		"ROLLOUTD_DATABASE_URL":    s.DBURL,
		"ROLLOUTD_ADMIN_TOKEN":     s.Token,
		"ROLLOUTD_TRUST_KEYS_FILE": trustFile,
		"ROLLOUTD_ARTIFACT_DIR":    artifacts,
		"ROLLOUTD_LISTEN_ADDR":     benchAddr,
		"ROLLOUTD_METRICS_ADDR":    "off",
	}
	maps.Copy(settings, env)
	s.LogPath = filepath.Join(dir, "rolloutd.log")
	if s.logFile, err = os.Create(s.LogPath); err != nil {
		return err
	}
	s.cmd = exec.CommandContext(ctx, binary, "serve") //nolint:gosec // The benchmark runs the binary it was told to.
	// Only what rolloutd needs, so that settings in the caller's environment can't leak in.
	s.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for _, key := range slices.Sorted(maps.Keys(settings)) {
		s.cmd.Env = append(s.cmd.Env, key+"="+settings[key])
	}
	s.cmd.Stdout, s.cmd.Stderr = s.logFile, s.logFile
	if err := s.cmd.Start(); err != nil {
		return fmt.Errorf("start rolloutd: %w", err)
	}
	s.started = time.Now()
	return s.waitReady(ctx)
}

func (s *server) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/readyz", nil)
		if err != nil {
			return err
		}
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("rolloutd wasn't ready within 30s; see %s", s.LogPath)
}

// stop shuts rolloutd down and reports what it used. The database stays, for analysis, until
// cleanUp.
func (s *server) stop() (procUsage, error) {
	if s.cmd == nil || s.cmd.Process == nil {
		return procUsage{}, errors.New("rolloutd isn't running")
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return procUsage{}, err
	}
	err := s.cmd.Wait()
	elapsed := time.Since(s.started)
	_ = s.logFile.Close()
	state := s.cmd.ProcessState
	if state == nil {
		return procUsage{}, fmt.Errorf("rolloutd: %w", err)
	}
	u := procUsage{CPU: state.UserTime() + state.SystemTime(), Elapsed: elapsed}
	if rusage, ok := state.SysUsage().(*syscall.Rusage); ok {
		u.MaxRSSMB = maxRSSMegabytes(rusage)
	}
	if !state.Success() {
		return u, fmt.Errorf("rolloutd exited with %s; see %s", state, s.LogPath)
	}
	return u, nil
}

func (s *server) cleanUp() error {
	if s.logFile != nil {
		_ = s.logFile.Close()
	}
	return s.drop()
}

// requestLog is one request rolloutd logged.
type requestLog struct {
	Msg      string `json:"msg"`
	Route    string `json:"route"`
	Status   int    `json:"status"`
	Duration int64  `json:"duration"`
}

// routeStats is what rolloutd's log says about the requests to each route: how long each took
// inside rolloutd, from the request arriving to the response being written. Answers of 503 are
// counted apart and left out of the latencies. They're shed requests, timeouts, or requests the
// client gave up on, which rolloutd also answers with 503, so the log can't tell them apart.
type routeStats struct {
	Count       int           `json:"count"`
	Unavailable int           `json:"unavailable"`
	Errors      int           `json:"errors"`
	P50         time.Duration `json:"p50"`
	P95         time.Duration `json:"p95"`
	P99         time.Duration `json:"p99"`
	Max         time.Duration `json:"max"`
	samples     []time.Duration
}

func readRequestLog(path string) (map[string]*routeStats, error) {
	f, err := os.Open(path) //nolint:gosec // The benchmark's own log.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	routes := map[string]*routeStats{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		var entry requestLog
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Msg != "request" {
			continue
		}
		stats := routes[entry.Route]
		if stats == nil {
			stats = &routeStats{}
			routes[entry.Route] = stats
		}
		stats.Count++
		switch {
		case entry.Status == http.StatusServiceUnavailable:
			stats.Unavailable++
			continue
		case entry.Status >= http.StatusInternalServerError:
			stats.Errors++
		}
		stats.samples = append(stats.samples, time.Duration(entry.Duration))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for _, stats := range routes {
		sorted := sortedCopy(stats.samples)
		stats.P50, stats.P95, stats.P99 = quantile(sorted, 0.5), quantile(sorted, 0.95), quantile(sorted, 0.99)
		stats.Max = quantile(sorted, 1)
	}
	return routes, nil
}
