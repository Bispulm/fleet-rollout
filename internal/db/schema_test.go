package db_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

const (
	sqlUniqueViolation   = "23505"
	sqlCheckViolation    = "23514"
	sqlRestrictViolation = "23001"
)

func requireSQLState(t *testing.T, err error, want string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want SQLSTATE %s", err, want)
	}
	if pgErr.Code != want {
		t.Fatalf("SQLSTATE = %s (%s: %s), want %s", pgErr.Code, pgErr.ConstraintName, pgErr.Message, want)
	}
}

// fixture holds the rows most schema tests need: a fleet, a device, and an
// artifact, plus a draft rollout with two stages.
type fixture struct {
	pool      *pgxpool.Pool
	fleetID   string
	artifact  string
	deviceID  string
	rolloutID string
}

func newFixture(t *testing.T, ctx context.Context) fixture {
	t.Helper()
	f := fixture{pool: testdb.New(t), deviceID: "sim-001"}
	mustScan(t, f.pool.QueryRow(ctx, `INSERT INTO fleets (name) VALUES ('lab-a') RETURNING id`), &f.fleetID)
	mustExec(t, ctx, f.pool, `INSERT INTO devices (id, fleet_id, platform, credential_hash)
		VALUES ($1, $2, 'linux/arm64', $3)`, f.deviceID, f.fleetID, make([]byte, 32))
	mustScan(t, f.pool.QueryRow(ctx, `INSERT INTO artifacts (name, version, platform, release_counter, size_bytes, sha256)
		VALUES ('demo', '1.0.0', 'linux/arm64', 10, 1024, $1) RETURNING id`, make([]byte, 32)), &f.artifact)
	f.rolloutID = f.insertRollout(t, ctx, "first")
	for ordinal, percent := range map[int]int{1: 50, 2: 100} {
		mustExec(t, ctx, f.pool, `INSERT INTO rollout_stages (rollout_id, ordinal, percent) VALUES ($1, $2, $3)`,
			f.rolloutID, ordinal, percent)
	}
	return f
}

func (f fixture) insertRollout(t *testing.T, ctx context.Context, name string) string {
	t.Helper()
	var id string
	mustScan(t, f.pool.QueryRow(ctx, `INSERT INTO rollouts (fleet_id, artifact_id, name, policy, policy_digest, created_at)
		VALUES ($1, $2, $3, '{}', $4, now()) RETURNING id`, f.fleetID, f.artifact, name, make([]byte, 32)), &id)
	return id
}

func (f fixture) addMember(ctx context.Context, rolloutID, deviceID string) error {
	_, err := f.pool.Exec(ctx, `INSERT INTO rollout_members (rollout_id, device_id, position, stage_ordinal, rank_key)
		VALUES ($1, $2, 1, 1, $3)`, rolloutID, deviceID, make([]byte, 32))
	return err
}

// start moves a rollout out of draft the way the store does: one revision bump.
func (f fixture) start(ctx context.Context, rolloutID string) error {
	_, err := f.pool.Exec(ctx, `UPDATE rollouts SET state = 'running', revision = revision + 1, started_at = now()
		WHERE id = $1`, rolloutID)
	return err
}

type scanner interface{ Scan(dest ...any) error }

func mustScan(t *testing.T, row scanner, dest ...any) {
	t.Helper()
	if err := row.Scan(dest...); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func TestArtifactsAreUniqueAndImmutable(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	digest := make([]byte, 32)

	_, err := f.pool.Exec(ctx, `INSERT INTO artifacts (name, version, platform, release_counter, size_bytes, sha256)
		VALUES ('demo', '1.0.0', 'linux/arm64', 11, 1, $1)`, digest)
	requireSQLState(t, err, sqlUniqueViolation)

	_, err = f.pool.Exec(ctx, `INSERT INTO artifacts (name, version, platform, release_counter, size_bytes, sha256)
		VALUES ('demo', '1.0.1', 'linux/arm64', 10, 1, $1)`, digest)
	requireSQLState(t, err, sqlUniqueViolation)

	_, err = f.pool.Exec(ctx, `UPDATE artifacts SET size_bytes = 2 WHERE id = $1`, f.artifact)
	requireSQLState(t, err, sqlRestrictViolation)
}

func TestRolloutPolicyAndIdentityAreImmutable(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	cases := map[string]string{
		"policy":   `UPDATE rollouts SET policy = '{"changed":true}', revision = revision + 1 WHERE id = $1`,
		"digest":   `UPDATE rollouts SET policy_digest = decode(repeat('ff', 32), 'hex'), revision = revision + 1 WHERE id = $1`,
		"name":     `UPDATE rollouts SET name = 'other', revision = revision + 1 WHERE id = $1`,
		"artifact": `UPDATE rollouts SET artifact_id = gen_random_uuid(), revision = revision + 1 WHERE id = $1`,
		"fleet":    `UPDATE rollouts SET fleet_id = gen_random_uuid(), revision = revision + 1 WHERE id = $1`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.pool.Exec(ctx, sql, f.rolloutID)
			requireSQLState(t, err, sqlRestrictViolation)
		})
	}
}

func TestRolloutUpdatesMustBumpRevisionByOne(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	for name, sql := range map[string]string{
		"unchanged": `UPDATE rollouts SET current_stage = 1 WHERE id = $1`,
		"skipped":   `UPDATE rollouts SET current_stage = 1, revision = revision + 2 WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.pool.Exec(ctx, sql, f.rolloutID)
			requireSQLState(t, err, sqlRestrictViolation)
		})
	}
	if err := f.start(ctx, f.rolloutID); err != nil {
		t.Fatalf("update with a single bump: %v", err)
	}
}

func TestTerminalRolloutsCannotChange(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	mustExec(t, ctx, f.pool, `UPDATE rollouts SET state = 'aborted', revision = revision + 1 WHERE id = $1`, f.rolloutID)
	_, err := f.pool.Exec(ctx, `UPDATE rollouts SET state = 'running', revision = revision + 1, started_at = now() WHERE id = $1`, f.rolloutID)
	requireSQLState(t, err, sqlRestrictViolation)
}

func TestRolloutsCannotBeDeleted(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	_, err := f.pool.Exec(ctx, `DELETE FROM rollouts WHERE id = $1`, f.rolloutID)
	requireSQLState(t, err, sqlRestrictViolation)
}

func TestPausedColumnsMatchState(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	if err := f.start(ctx, f.rolloutID); err != nil {
		t.Fatalf("start: %v", err)
	}
	_, err := f.pool.Exec(ctx, `UPDATE rollouts SET state = 'paused', revision = revision + 1 WHERE id = $1`, f.rolloutID)
	requireSQLState(t, err, sqlCheckViolation)
}

func TestMembershipFreezesWhenRolloutLeavesDraft(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	if err := f.addMember(ctx, f.rolloutID, f.deviceID); err != nil {
		t.Fatalf("add member to a draft: %v", err)
	}
	if err := f.start(ctx, f.rolloutID); err != nil {
		t.Fatalf("start: %v", err)
	}

	mustExec(t, ctx, f.pool, `INSERT INTO devices (id, fleet_id, platform, credential_hash)
		VALUES ('sim-002', $1, 'linux/arm64', $2)`, f.fleetID, make([]byte, 32))
	requireSQLState(t, f.addMember(ctx, f.rolloutID, "sim-002"), sqlRestrictViolation)

	_, err := f.pool.Exec(ctx, `UPDATE rollout_members SET position = 9 WHERE rollout_id = $1`, f.rolloutID)
	requireSQLState(t, err, sqlRestrictViolation)
	_, err = f.pool.Exec(ctx, `DELETE FROM rollout_members WHERE rollout_id = $1`, f.rolloutID)
	requireSQLState(t, err, sqlRestrictViolation)

	_, err = f.pool.Exec(ctx, `INSERT INTO rollout_exclusions (rollout_id, device_id, reason)
		VALUES ($1, 'sim-002', 'already_current')`, f.rolloutID)
	requireSQLState(t, err, sqlRestrictViolation)
}

func TestMemberPositionsAreUnique(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	if err := f.addMember(ctx, f.rolloutID, f.deviceID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	mustExec(t, ctx, f.pool, `INSERT INTO devices (id, fleet_id, platform, credential_hash)
		VALUES ('sim-002', $1, 'linux/arm64', $2)`, f.fleetID, make([]byte, 32))
	requireSQLState(t, f.addMember(ctx, f.rolloutID, "sim-002"), sqlUniqueViolation)
}

func TestOneActiveRolloutPerFleet(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	second := f.insertRollout(t, ctx, "second") // Another draft is fine.

	if err := f.start(ctx, f.rolloutID); err != nil {
		t.Fatalf("start first: %v", err)
	}
	requireSQLState(t, f.start(ctx, second), sqlUniqueViolation)

	// A finished rollout releases the fleet.
	mustExec(t, ctx, f.pool, `UPDATE rollouts SET state = 'aborted', revision = revision + 1 WHERE id = $1`, f.rolloutID)
	if err := f.start(ctx, second); err != nil {
		t.Fatalf("start second after the first finished: %v", err)
	}
}

func TestRolloutNamesAreUniquePerFleet(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	_, err := f.pool.Exec(ctx, `INSERT INTO rollouts (fleet_id, artifact_id, name, policy, policy_digest, created_at)
		VALUES ($1, $2, 'first', '{}', $3, now())`, f.fleetID, f.artifact, make([]byte, 32))
	requireSQLState(t, err, sqlUniqueViolation)
}

func TestTransitionLogIsAppendOnly(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	insert := `INSERT INTO rollout_transitions (rollout_id, revision, from_state, to_state, event, actor, occurred_at)
		VALUES ($1, 2, 'draft', 'running', 'start', 'test', now())`
	mustExec(t, ctx, f.pool, insert, f.rolloutID)

	_, err := f.pool.Exec(ctx, insert, f.rolloutID)
	requireSQLState(t, err, sqlUniqueViolation)

	_, err = f.pool.Exec(ctx, `UPDATE rollout_transitions SET actor = 'someone-else' WHERE rollout_id = $1`, f.rolloutID)
	requireSQLState(t, err, sqlRestrictViolation)
	_, err = f.pool.Exec(ctx, `DELETE FROM rollout_transitions WHERE rollout_id = $1`, f.rolloutID)
	requireSQLState(t, err, sqlRestrictViolation)
}
