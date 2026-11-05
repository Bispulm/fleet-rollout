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

func isSQLState(err error, want string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == want
}

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
		VALUES ($1, $2, 'linux/arm64', sha256($1::text::bytea))`, f.deviceID, f.fleetID)
	mustScan(t, f.pool.QueryRow(ctx, `INSERT INTO artifacts
		(name, version, platform, release_counter, size_bytes, sha256, envelope, chunk_size, content_ready_at)
		VALUES ('demo', '1.0.0', 'linux/arm64', 10, 1024, $1, '{}', 1024, now()) RETURNING id`, make([]byte, 32)), &f.artifact)
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
	_, err := f.pool.Exec(ctx, `UPDATE rollouts SET state = 'running', revision = revision + 1, started_at = now(), member_count = 1
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
	_, err = f.pool.Exec(ctx, `DELETE FROM artifacts WHERE id = $1`, f.artifact)
	requireSQLState(t, err, sqlRestrictViolation)
}

func TestArtifactContentCanBeRecordedOnlyOnce(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	var id string
	mustScan(t, f.pool.QueryRow(ctx, `INSERT INTO artifacts (name, version, platform, release_counter, size_bytes, sha256)
		VALUES ('other', '1.0.0', 'linux/arm64', 1, 1, $1) RETURNING id`, make([]byte, 32)), &id)

	// Content needs an envelope, so nothing can be marked ready without a signed manifest.
	_, err := f.pool.Exec(ctx, `UPDATE artifacts SET content_ready_at = now() WHERE id = $1`, id)
	requireSQLState(t, err, sqlCheckViolation)

	var signed string
	mustScan(t, f.pool.QueryRow(ctx, `INSERT INTO artifacts (name, version, platform, release_counter, size_bytes, sha256, envelope, chunk_size)
		VALUES ('other', '2.0.0', 'linux/arm64', 2, 1, $1, '{}', 1024) RETURNING id`, make([]byte, 32)), &signed)
	mustExec(t, ctx, f.pool, `UPDATE artifacts SET content_ready_at = now() WHERE id = $1`, signed)
	_, err = f.pool.Exec(ctx, `UPDATE artifacts SET content_ready_at = now() - interval '1 day' WHERE id = $1`, signed)
	requireSQLState(t, err, sqlRestrictViolation)
	_, err = f.pool.Exec(ctx, `UPDATE artifacts SET envelope = 'x' WHERE id = $1`, signed)
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
		VALUES ('sim-002', $1, 'linux/arm64', sha256('sim-002'))`, f.fleetID)
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
		VALUES ('sim-002', $1, 'linux/arm64', sha256('sim-002'))`, f.fleetID)
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

// exposedFixture is a started rollout with one member, ready for assignments.
func exposedFixture(t *testing.T, ctx context.Context) fixture {
	t.Helper()
	f := newFixture(t, ctx)
	if err := f.addMember(ctx, f.rolloutID, f.deviceID); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := f.start(ctx, f.rolloutID); err != nil {
		t.Fatalf("start: %v", err)
	}
	return f
}

func (f fixture) assign(ctx context.Context, generation int) (string, error) {
	var id string
	err := f.pool.QueryRow(ctx, `INSERT INTO assignments
		(rollout_id, device_id, generation, kind, stage_ordinal, issued_at)
		VALUES ($1, $2, $3, 'forward', 1, now()) RETURNING id`,
		f.rolloutID, f.deviceID, generation).Scan(&id)
	return id, err
}

func TestDeviceHasAtMostOneOpenAssignment(t *testing.T) {
	ctx := testContext(t)
	f := exposedFixture(t, ctx)
	assignRollback := func() error {
		_, err := f.pool.Exec(ctx, `INSERT INTO assignments
			(rollout_id, device_id, generation, kind, stage_ordinal, issued_at, restore_release)
			VALUES ($1, $2, 2, 'rollback', 1, now(), 0)`, f.rolloutID, f.deviceID)
		return err
	}

	first, err := f.assign(ctx, 1)
	if err != nil {
		t.Fatalf("first assignment: %v", err)
	}
	err = assignRollback()
	requireSQLState(t, err, sqlUniqueViolation)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.ConstraintName != "assignments_one_open_per_device" {
		t.Fatalf("error = %v, want a violation of the one-open-assignment index", err)
	}

	mustExec(t, ctx, f.pool, `UPDATE assignments SET state = 'superseded', finished_at = now() WHERE id = $1`, first)
	if err := assignRollback(); err != nil {
		t.Fatalf("assignment after the first finished: %v", err)
	}
}

func TestAssignmentGenerationIsUniquePerRolloutAndDevice(t *testing.T) {
	ctx := testContext(t)
	f := exposedFixture(t, ctx)
	first, err := f.assign(ctx, 1)
	if err != nil {
		t.Fatalf("first assignment: %v", err)
	}
	mustExec(t, ctx, f.pool, `UPDATE assignments SET state = 'failed', finished_at = now() WHERE id = $1`, first)
	_, err = f.assign(ctx, 1)
	requireSQLState(t, err, sqlUniqueViolation)
}

func TestAssignmentRequiresRolloutMembership(t *testing.T) {
	ctx := testContext(t)
	f := exposedFixture(t, ctx)
	mustExec(t, ctx, f.pool, `INSERT INTO devices (id, fleet_id, platform, credential_hash)
		VALUES ('outsider', $1, 'linux/arm64', sha256('outsider'))`, f.fleetID)
	_, err := f.pool.Exec(ctx, `INSERT INTO assignments
		(rollout_id, device_id, kind, stage_ordinal, issued_at)
		VALUES ($1, 'outsider', 'forward', 1, now())`, f.rolloutID)
	requireSQLState(t, err, "23503")
}

func TestFinishedAtMatchesTerminalAssignmentState(t *testing.T) {
	ctx := testContext(t)
	f := exposedFixture(t, ctx)
	id, err := f.assign(ctx, 1)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	_, err = f.pool.Exec(ctx, `UPDATE assignments SET state = 'succeeded' WHERE id = $1`, id)
	requireSQLState(t, err, sqlCheckViolation)
	_, err = f.pool.Exec(ctx, `UPDATE assignments SET finished_at = now() WHERE id = $1`, id)
	requireSQLState(t, err, sqlCheckViolation)
	// Unknown is derived from when a device was last heard from, not stored.
	_, err = f.pool.Exec(ctx, `UPDATE assignments SET state = 'stale' WHERE id = $1`, id)
	requireSQLState(t, err, sqlCheckViolation)
}

func TestMemberHasAtMostOneForwardAssignmentPerRollout(t *testing.T) {
	ctx := testContext(t)
	f := exposedFixture(t, ctx)
	first, err := f.assign(ctx, 1)
	if err != nil {
		t.Fatalf("first assignment: %v", err)
	}
	mustExec(t, ctx, f.pool, `UPDATE assignments SET state = 'failed', finished_at = now() WHERE id = $1`, first)

	// A second one would count this device's outcome twice.
	_, err = f.assign(ctx, 1)
	requireSQLState(t, err, sqlUniqueViolation)
	// Forward assignments are always generation 1, so a later one can't slip past that.
	_, err = f.assign(ctx, 2)
	requireSQLState(t, err, sqlCheckViolation)

	// A rollback is a different kind of instruction, so it's allowed, with what to restore.
	mustExec(t, ctx, f.pool, `INSERT INTO assignments
		(rollout_id, device_id, generation, kind, stage_ordinal, issued_at, restore_release)
		VALUES ($1, $2, 2, 'rollback', 1, now(), 0)`, f.rolloutID, f.deviceID)
}

func TestRollbackAssignmentsSayWhatToRestore(t *testing.T) {
	ctx := testContext(t)
	f := exposedFixture(t, ctx)
	for name, insert := range map[string]string{
		"a rollback without a release to restore": `INSERT INTO assignments
			(rollout_id, device_id, generation, kind, stage_ordinal, issued_at)
			VALUES ($1, $2, 2, 'rollback', 1, now())`,
		"a forward assignment with one": `INSERT INTO assignments
			(rollout_id, device_id, generation, kind, stage_ordinal, issued_at, restore_release)
			VALUES ($1, $2, 1, 'forward', 1, now(), 3)`,
		"a rollback at the forward generation": `INSERT INTO assignments
			(rollout_id, device_id, generation, kind, stage_ordinal, issued_at, restore_release)
			VALUES ($1, $2, 1, 'rollback', 1, now(), 0)`,
	} {
		_, err := f.pool.Exec(ctx, insert, f.rolloutID, f.deviceID)
		if !isSQLState(err, sqlCheckViolation) {
			t.Errorf("%s: error = %v, want a check violation", name, err)
		}
	}
}

func TestDeviceCredentialsAreUnique(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	_, err := f.pool.Exec(ctx, `INSERT INTO devices (id, fleet_id, platform, credential_hash)
		VALUES ('sim-002', $1, 'linux/arm64', sha256($2::text::bytea))`, f.fleetID, f.deviceID)
	requireSQLState(t, err, sqlUniqueViolation)
}

func TestEngineLeaseHasOneRowPerName(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	insert := `INSERT INTO engine_lease (name, holder, generation, expires_at) VALUES ('rollout-engine', 'a', 1, now())`
	mustExec(t, ctx, f.pool, insert)
	_, err := f.pool.Exec(ctx, insert)
	requireSQLState(t, err, sqlUniqueViolation)
}

func TestDeviceEventsAreAppendOnlyAndSequenced(t *testing.T) {
	ctx := testContext(t)
	f := exposedFixture(t, ctx)
	insert := `INSERT INTO device_events (device_id, sequence, type, payload_digest, received_at, applied)
		VALUES ($1, $2, 'progress', $3, now(), true)`
	digest := make([]byte, 32)
	mustExec(t, ctx, f.pool, insert, f.deviceID, 1, digest)

	_, err := f.pool.Exec(ctx, insert, f.deviceID, 1, digest)
	requireSQLState(t, err, sqlUniqueViolation)

	_, err = f.pool.Exec(ctx, `UPDATE device_events SET type = 'changed' WHERE device_id = $1`, f.deviceID)
	requireSQLState(t, err, sqlRestrictViolation)
	_, err = f.pool.Exec(ctx, `DELETE FROM device_events WHERE device_id = $1`, f.deviceID)
	requireSQLState(t, err, sqlRestrictViolation)
}

func TestStartedRolloutCannotReturnToDraft(t *testing.T) {
	ctx := testContext(t)
	f := exposedFixture(t, ctx)
	_, err := f.pool.Exec(ctx, `UPDATE rollouts SET state = 'draft', started_at = NULL, revision = revision + 1 WHERE id = $1`, f.rolloutID)
	requireSQLState(t, err, sqlRestrictViolation)
}

func TestFrozenRolloutFactsCannotBeRewritten(t *testing.T) {
	ctx := testContext(t)
	f := exposedFixture(t, ctx)
	for name, sql := range map[string]string{
		"member count": `UPDATE rollouts SET member_count = 99, revision = revision + 1 WHERE id = $1`,
		"clear count":  `UPDATE rollouts SET member_count = NULL, revision = revision + 1 WHERE id = $1`,
		"start time":   `UPDATE rollouts SET started_at = now() - interval '1 day', revision = revision + 1 WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.pool.Exec(ctx, sql, f.rolloutID)
			requireSQLState(t, err, sqlRestrictViolation)
		})
	}
}

func TestStagePlanFreezesWhenRolloutLeavesDraft(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)

	// Sizing a stage is how the start transaction works, while still a draft.
	mustExec(t, ctx, f.pool, `UPDATE rollout_stages SET target_count = 1, cohort_size = 1 WHERE rollout_id = $1 AND ordinal = 1`, f.rolloutID)
	if err := f.start(ctx, f.rolloutID); err != nil {
		t.Fatalf("start: %v", err)
	}

	for name, sql := range map[string]string{
		"target":  `UPDATE rollout_stages SET target_count = 50 WHERE rollout_id = $1 AND ordinal = 1`,
		"cohort":  `UPDATE rollout_stages SET cohort_size = 50 WHERE rollout_id = $1 AND ordinal = 1`,
		"percent": `UPDATE rollout_stages SET percent = 99 WHERE rollout_id = $1 AND ordinal = 1`,
		"ordinal": `UPDATE rollout_stages SET ordinal = 7 WHERE rollout_id = $1 AND ordinal = 1`,
		"insert":  `INSERT INTO rollout_stages (rollout_id, ordinal, percent) VALUES ($1, 3, 100)`,
		"delete":  `DELETE FROM rollout_stages WHERE rollout_id = $1 AND ordinal = 2`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.pool.Exec(ctx, sql, f.rolloutID)
			requireSQLState(t, err, sqlRestrictViolation)
		})
	}

	// Progress through a stage is what the engine updates while a rollout runs.
	mustExec(t, ctx, f.pool, `UPDATE rollout_stages SET state = 'soaking', soak_started_at = now() WHERE rollout_id = $1 AND ordinal = 1`, f.rolloutID)
}

func TestStagePercentCannotChangeEvenInDraft(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	// The policy is immutable from creation, and percent is part of it.
	_, err := f.pool.Exec(ctx, `UPDATE rollout_stages SET percent = 99 WHERE rollout_id = $1 AND ordinal = 1`, f.rolloutID)
	requireSQLState(t, err, sqlRestrictViolation)
}

func TestRolloutsCannotUseAnArtifactWithoutItsContent(t *testing.T) {
	ctx := testContext(t)
	f := newFixture(t, ctx)
	var unready string
	mustScan(t, f.pool.QueryRow(ctx, `INSERT INTO artifacts (name, version, platform, release_counter, size_bytes, sha256, envelope, chunk_size)
		VALUES ('demo', '2.0.0', 'linux/arm64', 20, 1024, $1, '{}', 1024) RETURNING id`, make([]byte, 32)), &unready)
	_, err := f.pool.Exec(ctx, `INSERT INTO rollouts (fleet_id, artifact_id, name, policy, policy_digest, created_at)
		VALUES ($1, $2, 'unready', '{}', $3, now())`, f.fleetID, unready, make([]byte, 32))
	requireSQLState(t, err, sqlRestrictViolation)
}
