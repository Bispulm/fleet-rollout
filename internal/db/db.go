// Package db opens the Postgres connection pool and applies the embedded schema
// migrations. Postgres is the only durable authority in the system.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

const sqlStateUndefinedTable = "42P01"

//go:embed migrations/*.sql
var embedded embed.FS

// ErrSchemaBehind reports that the database is reachable but has not been
// migrated to the version this binary requires.
var ErrSchemaBehind = errors.New("database schema is behind this binary")

// Open creates a connection pool. It connects lazily, so a server can start
// before Postgres is available and report itself unready until then.
func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		// The parser's error is deliberately dropped: it can contain the URL.
		return nil, errors.New("parse database URL: invalid connection string")
	}
	cfg.MaxConns = maxConns
	cfg.MaxConnIdleTime = 5 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}
	return pool, nil
}

// Migrate applies pending migrations and returns how many ran. A session-level
// advisory lock serializes concurrent callers, so replicas can start together.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	migrations, err := migrationsFS()
	if err != nil {
		return 0, err
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return 0, fmt.Errorf("create migration lock: %w", err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations, goose.WithSessionLocker(locker))
	if err != nil {
		return 0, fmt.Errorf("create migration provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return 0, fmt.Errorf("apply migrations: %w", err)
	}
	return len(results), nil
}

// CheckReady verifies that Postgres is reachable and that its schema is at least
// the version embedded in this binary. A newer schema is accepted so that an
// older replica keeps serving during a rolling upgrade.
func CheckReady(ctx context.Context, pool *pgxpool.Pool) error {
	want, err := embeddedVersion()
	if err != nil {
		return err
	}
	var got int64
	err = pool.QueryRow(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&got)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == sqlStateUndefinedTable {
			return fmt.Errorf("%w: no migrations applied, want version %d", ErrSchemaBehind, want)
		}
		return fmt.Errorf("query schema version: %w", err)
	}
	if got < want {
		return fmt.Errorf("%w: at version %d, want %d", ErrSchemaBehind, got, want)
	}
	return nil
}

func migrationsFS() (fs.FS, error) {
	migrations, err := fs.Sub(embedded, "migrations")
	if err != nil {
		return nil, fmt.Errorf("open embedded migrations: %w", err)
	}
	return migrations, nil
}

// embeddedVersion returns the highest migration number compiled into the binary.
func embeddedVersion() (int64, error) {
	names, err := fs.Glob(embedded, "migrations/*.sql")
	if err != nil {
		return 0, fmt.Errorf("list embedded migrations: %w", err)
	}
	var latest int64
	for _, name := range names {
		prefix, _, ok := strings.Cut(path.Base(name), "_")
		if !ok {
			return 0, fmt.Errorf("migration %q has no version prefix", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("migration %q has an invalid version prefix: %w", name, err)
		}
		latest = max(latest, version)
	}
	return latest, nil
}
