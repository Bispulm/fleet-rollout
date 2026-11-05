-- +goose Up
CREATE TABLE assignments (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    rollout_id     uuid NOT NULL,
    device_id      text NOT NULL,
    -- Raised each time the device gets new instructions for this rollout, so a
    -- delayed report from an older assignment can be recognized and set aside.
    generation     bigint NOT NULL DEFAULT 1 CHECK (generation >= 1),
    kind           text NOT NULL CHECK (kind IN ('forward', 'rollback')),
    stage_ordinal  integer NOT NULL,
    state          text NOT NULL DEFAULT 'assigned' CHECK (state IN (
                       'assigned', 'downloading', 'verifying', 'installing', 'health_check',
                       'reverting', 'stale', 'succeeded', 'failed', 'superseded')),
    issued_at      timestamptz NOT NULL,
    expires_at     timestamptz NOT NULL,
    last_report_at timestamptz,
    finished_at    timestamptz,
    UNIQUE (rollout_id, device_id, generation),
    FOREIGN KEY (rollout_id, device_id) REFERENCES rollout_members (rollout_id, device_id),
    -- A stale device may still report, so only these states are final.
    CONSTRAINT assignments_finished_matches_state
        CHECK ((state IN ('succeeded', 'failed', 'superseded')) = (finished_at IS NOT NULL))
);

-- A device has one set of instructions at a time, across every rollout.
CREATE UNIQUE INDEX assignments_one_open_per_device ON assignments (device_id)
    WHERE finished_at IS NULL;

CREATE INDEX assignments_rollout_stage_state_idx ON assignments (rollout_id, stage_ordinal, state);

-- Everything devices report, as received. Rollout decisions read from assignments;
-- this log exists for audit and for detecting replayed or conflicting reports.
CREATE TABLE device_events (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    device_id      text NOT NULL REFERENCES devices (id),
    -- Persisted by the agent and unique per device, which makes retries idempotent.
    sequence       bigint NOT NULL CHECK (sequence >= 1),
    assignment_id  uuid REFERENCES assignments (id),
    generation     bigint,
    type           text NOT NULL,
    payload        jsonb NOT NULL DEFAULT '{}',
    payload_digest bytea NOT NULL CHECK (octet_length(payload_digest) = 32),
    received_at    timestamptz NOT NULL,
    UNIQUE (device_id, sequence)
);

CREATE INDEX device_events_assignment_idx ON device_events (assignment_id);

CREATE TRIGGER device_events_append_only BEFORE UPDATE OR DELETE ON device_events
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
