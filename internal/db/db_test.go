package db_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Bispulm/fleet-rollout/internal/db"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestMigrateBringsEmptyDatabaseToCurrentSchema(t *testing.T) {
	ctx := testContext(t)
	pool, err := db.Open(ctx, testdb.NewURL(t), 4)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := db.CheckReady(ctx, pool); !errors.Is(err, db.ErrSchemaBehind) {
		t.Fatalf("CheckReady before migrating = %v, want ErrSchemaBehind", err)
	}

	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if applied == 0 {
		t.Fatal("Migrate applied nothing to an empty database")
	}

	if err := db.CheckReady(ctx, pool); err != nil {
		t.Fatalf("CheckReady after migrating: %v", err)
	}
}

func TestMigrateTwiceAppliesNothingNew(t *testing.T) {
	ctx := testContext(t)
	pool := testdb.New(t)

	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if applied != 0 {
		t.Fatalf("second Migrate applied %d migrations, want 0", applied)
	}
}

func TestConcurrentMigrationsDoNotConflict(t *testing.T) {
	ctx := testContext(t)
	dbURL := testdb.NewURL(t)

	const replicas = 4
	var wg sync.WaitGroup
	errs := make([]error, replicas)
	for i := range replicas {
		wg.Go(func() {
			pool, err := db.Open(ctx, dbURL, 4)
			if err != nil {
				errs[i] = err
				return
			}
			defer pool.Close()
			_, errs[i] = db.Migrate(ctx, pool)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d: %v", i, err)
		}
	}
}

func TestCheckReadyReportsUnreachableDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Port 1 on loopback refuses connections. Open must not fail on this, so the
	// server can start first and report itself unready until Postgres appears.
	pool, err := db.Open(ctx, "postgres://u:p@127.0.0.1:1/none?connect_timeout=1", 1)
	if err != nil {
		t.Fatalf("Open should not require a reachable server: %v", err)
	}
	defer pool.Close()

	err = db.CheckReady(ctx, pool)
	if err == nil {
		t.Fatal("CheckReady succeeded against an unreachable server")
	}
	if errors.Is(err, db.ErrSchemaBehind) {
		t.Fatalf("CheckReady = %v, want a connection error rather than ErrSchemaBehind", err)
	}
}

func TestSchemaRejectsInvalidFleetAndDeviceRows(t *testing.T) {
	ctx := testContext(t)
	pool := testdb.New(t)

	var fleetID string
	if err := pool.QueryRow(ctx, `INSERT INTO fleets (name) VALUES ('lab-a') RETURNING id`).Scan(&fleetID); err != nil {
		t.Fatalf("insert fleet: %v", err)
	}
	hash := make([]byte, 32)
	insertDevice := func(id, platform, status string, credential []byte) error {
		_, err := pool.Exec(ctx, `INSERT INTO devices (id, fleet_id, platform, status, credential_hash)
			VALUES ($1, $2, $3, $4, $5)`, id, fleetID, platform, status, credential)
		return err
	}
	if err := insertDevice("sim-001", "linux/arm64", "active", hash); err != nil {
		t.Fatalf("insert valid device: %v", err)
	}

	cases := []struct {
		name     string
		wantCode string
		exec     func() error
	}{
		{"duplicate fleet name", "23505", func() error {
			_, err := pool.Exec(ctx, `INSERT INTO fleets (name) VALUES ('lab-a')`)
			return err
		}},
		{"malformed fleet name", "23514", func() error {
			_, err := pool.Exec(ctx, `INSERT INTO fleets (name) VALUES ('Lab A')`)
			return err
		}},
		{"non-object fleet labels", "23514", func() error {
			_, err := pool.Exec(ctx, `INSERT INTO fleets (name, labels) VALUES ('lab-b', '[]')`)
			return err
		}},
		{"duplicate device id", "23505", func() error { return insertDevice("sim-001", "linux/arm64", "active", hash) }},
		{"malformed device id", "23514", func() error { return insertDevice("bad id", "linux/arm64", "active", hash) }},
		{"unknown device status", "23514", func() error { return insertDevice("sim-002", "linux/arm64", "gone", hash) }},
		{"short credential hash", "23514", func() error { return insertDevice("sim-003", "linux/arm64", "active", make([]byte, 8)) }},
		{"unknown fleet", "23503", func() error {
			_, err := pool.Exec(ctx, `INSERT INTO devices (id, fleet_id, platform, credential_hash)
				VALUES ('sim-004', gen_random_uuid(), 'linux/arm64', $1)`, hash)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.exec()
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("error = %v, want a Postgres error", err)
			}
			if pgErr.Code != tc.wantCode {
				t.Fatalf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.ConstraintName, tc.wantCode)
			}
		})
	}
}
