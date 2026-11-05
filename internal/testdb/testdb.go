// Package testdb gives tests an isolated database on a real Postgres server.
//
// Set ROLLOUT_TEST_DATABASE_URL to a connection URL with permission to create
// databases. Tests skip when it is unset, unless ROLLOUT_TEST_REQUIRE_DB=1, which
// CI sets so that a misconfigured job fails instead of silently testing nothing.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/fleet-rollout/internal/db"
)

const setupTimeout = 30 * time.Second

// NewURL creates an empty database, without migrations, and returns its URL.
// The database is dropped when the test ends.
func NewURL(t testing.TB) string {
	t.Helper()
	adminURL := os.Getenv("ROLLOUT_TEST_DATABASE_URL")
	if adminURL == "" {
		if os.Getenv("ROLLOUT_TEST_REQUIRE_DB") == "1" {
			t.Fatal("ROLLOUT_TEST_REQUIRE_DB=1 but ROLLOUT_TEST_DATABASE_URL is unset")
		}
		t.Skip("ROLLOUT_TEST_DATABASE_URL is unset; skipping database test")
	}

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generate database name: %v", err)
	}
	name := "rollout_test_" + hex.EncodeToString(suffix)

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect to test server: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		admin, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Errorf("connect to drop test database: %v", err)
			return
		}
		defer func() { _ = admin.Close(ctx) }()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
	})

	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	parsed.Path = "/" + name
	return parsed.String()
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
