// Package testdb gives tests an isolated database on a real Postgres server.
//
// Set ROLLOUT_TEST_DATABASE_URL to a connection URL with permission to create
// databases. Tests skip when it is unset, unless ROLLOUT_TEST_REQUIRE_DB=1, which
// CI sets so that a misconfigured job fails instead of silently testing nothing.
package testdb

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/fleet-rollout/internal/db"
)

const setupTimeout = 30 * time.Second

// ServerURL returns the URL of the test Postgres server, or skips the test if there isn't one.
func ServerURL(t testing.TB) string {
	t.Helper()
	serverURL := os.Getenv("ROLLOUT_TEST_DATABASE_URL")
	if serverURL == "" {
		if os.Getenv("ROLLOUT_TEST_REQUIRE_DB") == "1" {
			t.Fatal("ROLLOUT_TEST_REQUIRE_DB=1 but ROLLOUT_TEST_DATABASE_URL is unset")
		}
		t.Skip("ROLLOUT_TEST_DATABASE_URL is unset; skipping database test")
	}
	return serverURL
}

// NewURL creates an empty database, without migrations, and returns its URL.
// The database is dropped when the test ends.
func NewURL(t testing.TB) string {
	t.Helper()
	server := ServerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	scratch, drop, err := db.CreateScratch(ctx, server, "rollout_test_")
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if err := drop(); err != nil {
			t.Error(err)
		}
	})
	return scratch
}

// New creates a migrated database and returns a pool connected to it. The pool
// closes and the database is dropped when the test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dbURL := NewURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	pool, err := db.Open(ctx, dbURL, 8)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return pool
}
