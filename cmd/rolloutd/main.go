// Command rolloutd runs the rollout control plane.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"

	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
	"github.com/Bispulm/fleet-rollout/internal/config"
	"github.com/Bispulm/fleet-rollout/internal/db"
	"github.com/Bispulm/fleet-rollout/internal/engine"
	"github.com/Bispulm/fleet-rollout/internal/metrics"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/tracing"
)

const (
	usage          = "usage: rolloutd <serve|migrate>"
	migrateTimeout = 5 * time.Minute
)

// env carries the process boundaries that tests replace.
type env struct {
	getenv      func(string) string
	stdout      io.Writer
	stderr      io.Writer
	onListening func(net.Addr)
	// onMetrics, if set, is told where metrics are served.
	onMetrics func(net.Addr)
}

func main() {
	if err := runProcess(); err != nil {
		fmt.Fprintln(os.Stderr, "rolloutd:", err)
		os.Exit(1)
	}
}

func runProcess() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], env{getenv: os.Getenv, stdout: os.Stdout, stderr: os.Stderr})
}

func run(ctx context.Context, args []string, e env) error {
	if len(args) != 1 {
		return errors.New(usage)
	}
	switch args[0] {
	case "serve", "migrate":
	default:
		return fmt.Errorf("unknown command %q; %s", args[0], usage)
	}

	cfg, err := config.Load(e.getenv)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(e.stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	pool, err := db.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	if args[0] == "migrate" {
		return runMigrate(ctx, pool, e.stdout)
	}
	return runServe(ctx, cfg, pool, logger, e)
}

func runMigrate(ctx context.Context, pool *pgxpool.Pool, stdout io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, migrateTimeout)
	defer cancel()
	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "applied %d migrations\n", applied)
	return err
}

func runServe(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger, e env) error {
	trustKeys, err := os.ReadFile(cfg.TrustKeysFile)
	if err != nil {
		return fmt.Errorf("read trust list: %w", err)
	}
	trust, err := signing.ParseTrustSet(trustKeys)
	if err != nil {
		return fmt.Errorf("read trust list: %w", err)
	}
	artifacts, err := artifactstore.NewFilesystem(cfg.ArtifactDir)
	if err != nil {
		return err
	}

	tracer, stopTracing, err := tracing.Setup(ctx, "rolloutd", e.getenv)
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = stopTracing(ctx)
	}()

	st := store.New(pool)
	m := metrics.New()
	m.ObserveRollouts(st, logger)
	server := api.New(api.Options{
		Logger:             logger,
		Ready:              func(ctx context.Context) error { return db.CheckReady(ctx, pool) },
		Store:              st,
		AdminToken:         cfg.AdminToken,
		PollInterval:       cfg.PollInterval,
		Trust:              trust,
		Artifacts:          artifacts,
		MaxArtifactBytes:   cfg.MaxArtifactBytes,
		MaxDeviceRequests:  cfg.MaxDeviceRequests,
		MaxDeviceDownloads: cfg.MaxDeviceDownloads,
		Metrics:            m,
		Tracer:             tracer,
	})

	stopMetrics, err := serveMetrics(ctx, cfg.MetricsAddr, m, logger, e.onMetrics)
	if err != nil {
		return err
	}
	defer stopMetrics()

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "listening", slog.String("addr", listener.Addr().String()))
	if e.onListening != nil {
		e.onListening(listener.Addr())
	}

	stopEngine, err := startEngine(ctx, cfg, st, logger, m, tracer)
	if err != nil {
		_ = listener.Close()
		return err
	}
	serveErr := server.Serve(ctx, listener, cfg.ShutdownTimeout)
	// Both stop when ctx ends. This waits for the engine, before the pool closes.
	stopEngine()
	if serveErr != nil {
		return serveErr
	}
	logger.LogAttrs(context.WithoutCancel(ctx), slog.LevelInfo, "stopped")
	return nil
}

// startEngine runs the rollout engine in the background, and returns a function that
// stops it and waits for it to give up its lease.
func startEngine(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger, m *metrics.Metrics, tracer trace.TracerProvider) (func(), error) {
	if !cfg.EngineEnabled {
		logger.LogAttrs(ctx, slog.LevelInfo, "rollout engine disabled")
		return func() {}, nil
	}
	holder, err := engineHolder()
	if err != nil {
		return nil, err
	}
	engineCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	rolloutEngine := engine.New(engine.Options{
		Store:    st,
		Holder:   holder,
		Interval: cfg.EngineInterval,
		LeaseTTL: cfg.EngineLeaseTTL,
		Logger:   logger,
		Metrics:  m,
		Tracer:   tracer,
	})
	go func() {
		defer close(done)
		if err := rolloutEngine.Run(engineCtx); err != nil {
			logger.LogAttrs(engineCtx, slog.LevelError, "rollout engine stopped", slog.Any("error", err))
		}
	}()
	return func() {
		cancel()
		<-done
	}, nil
}

// engineHolder names this process in the engine lease. The random part keeps a
// restarted process from being mistaken for its previous self.
func engineHolder() (string, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generate engine holder ID: %w", err)
	}
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	return host + "-" + hex.EncodeToString(suffix), nil
}

// serveMetrics serves Prometheus metrics on their own listener, until ctx ends or the returned
// function is called. An empty address serves nothing.
func serveMetrics(ctx context.Context, addr string, m *metrics.Metrics, logger *slog.Logger, onListening func(net.Addr)) (func(), error) {
	if addr == "" {
		return func() {}, nil
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen for metrics: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "serving metrics", slog.String("addr", listener.Addr().String()))
	if onListening != nil {
		onListening(listener.Addr())
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.LogAttrs(ctx, slog.LevelError, "metrics server stopped", slog.Any("error", err))
		}
	}()
	return func() { _ = server.Close() }, nil
}
