-- +goose Up

-- Devices authenticate by the hash of their token, so it has to find exactly one.
CREATE UNIQUE INDEX devices_credential_hash_key ON devices (credential_hash);

-- A device's outcome counts once per rollout. Rollback instructions are separate.
CREATE UNIQUE INDEX assignments_one_forward_per_member ON assignments (rollout_id, device_id)
    WHERE kind = 'forward';

-- Tracks the newest report applied, so a delayed older one can't undo it.
ALTER TABLE assignments ADD COLUMN last_sequence bigint NOT NULL DEFAULT 0 CHECK (last_sequence >= 0);

-- Unknown is computed from when a device was last heard from, never stored.
ALTER TABLE assignments DROP CONSTRAINT assignments_state_check;
ALTER TABLE assignments ADD CONSTRAINT assignments_state_check CHECK (state IN (
    'assigned', 'downloading', 'verifying', 'installing', 'health_check',
    'reverting', 'succeeded', 'failed', 'superseded'));

-- Every report is kept, including those that couldn't be applied, along with why.
ALTER TABLE device_events
    ADD COLUMN rollout_id uuid REFERENCES rollouts (id),
    ADD COLUMN applied boolean NOT NULL,
    ADD COLUMN ignored_reason text;

-- Why a rollout halted, and which engine leadership generation made a decision.
ALTER TABLE rollouts ADD COLUMN halt_reason text;
ALTER TABLE rollouts ADD COLUMN halt_detail text;
ALTER TABLE rollout_transitions ADD COLUMN engine_generation bigint;

-- Engine leadership. Taking over needs an exclusive lock on this row, and every
-- engine transaction holds a shared lock on it after checking its generation, so a
-- stale leader can't commit a decision once another instance has taken over.
CREATE TABLE engine_lease (
    name       text PRIMARY KEY,
    holder     text NOT NULL,
    generation bigint NOT NULL CHECK (generation >= 1),
    expires_at timestamptz NOT NULL
);
