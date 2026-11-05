// Command rolloutd runs the rollout control plane.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/config"
	"github.com/Bispulm/fleet-rollout/internal/db"
	"github.com/Bispulm/fleet-rollout/internal/store"
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
	server := api.New(api.Options{
		Logger:     logger,
		Ready:      func(ctx context.Context) error { return db.CheckReady(ctx, pool) },
		Store:      store.New(pool),
		AdminToken: cfg.AdminToken,
	})

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "listening", slog.String("addr", listener.Addr().String()))
	if e.onListening != nil {
		e.onListening(listener.Addr())
	}
	if err := server.Serve(ctx, listener, cfg.ShutdownTimeout); err != nil {
		return err
	}
	logger.LogAttrs(context.WithoutCancel(ctx), slog.LevelInfo, "stopped")
	return nil
}
