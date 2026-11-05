-- +goose Up

-- Rejects any change to a row. Used for records that must never be rewritten.
-- +goose StatementBegin
CREATE FUNCTION reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TABLE artifacts (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name            text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    version         text NOT NULL CHECK (version ~ '^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$'),
    platform        text NOT NULL CHECK (platform ~ '^[a-z0-9]+/[a-z0-9]+$'),
    -- Monotonic counter used for downgrade protection, independent of version text.
    release_counter bigint NOT NULL CHECK (release_counter >= 1),
    size_bytes      bigint NOT NULL CHECK (size_bytes >= 1),
    sha256          bytea NOT NULL CHECK (octet_length(sha256) = 32),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, platform, version),
    UNIQUE (name, platform, release_counter)
);

CREATE TRIGGER artifacts_immutable BEFORE UPDATE OR DELETE ON artifacts
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TABLE rollouts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    fleet_id      uuid NOT NULL REFERENCES fleets (id),
    artifact_id   uuid NOT NULL REFERENCES artifacts (id),
    name          text NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
    state         text NOT NULL DEFAULT 'draft' CHECK (state IN (
                      'draft', 'running', 'soaking', 'paused', 'halted', 'rolling_back',
                      'rollback_incomplete', 'rolled_back', 'completed', 'aborted')),
    revision      bigint NOT NULL DEFAULT 1 CHECK (revision >= 1),
    policy        jsonb NOT NULL,
    policy_digest bytea NOT NULL CHECK (octet_length(policy_digest) = 32),
    current_stage integer,
    member_count  integer CHECK (member_count >= 1),
    paused_from   text CHECK (paused_from IN ('running', 'soaking')),
    paused_at     timestamptz,
    created_at    timestamptz NOT NULL,
    started_at    timestamptz,
    finished_at   timestamptz,
    UNIQUE (fleet_id, name),
    CONSTRAINT rollouts_paused_from_matches_state CHECK ((state = 'paused') = (paused_from IS NOT NULL)),
    CONSTRAINT rollouts_paused_at_matches_state CHECK ((state = 'paused') = (paused_at IS NOT NULL)),
    CONSTRAINT rollouts_draft_not_started CHECK (state <> 'draft' OR started_at IS NULL)
);

-- A device must never receive competing instructions, so a fleet has one started,
-- unfinished rollout at a time. A halted rollout still holds the fleet: its
-- exposed devices need a decision before anything else moves.
CREATE UNIQUE INDEX rollouts_one_active_per_fleet ON rollouts (fleet_id)
    WHERE state IN ('running', 'soaking', 'paused', 'halted', 'rolling_back', 'rollback_incomplete');

-- +goose StatementBegin
CREATE FUNCTION guard_rollout_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.state IN ('completed', 'aborted', 'rolled_back') THEN
        RAISE EXCEPTION 'rollout % is finished in state %', OLD.id, OLD.state
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF (NEW.id, NEW.fleet_id, NEW.artifact_id, NEW.name, NEW.policy, NEW.policy_digest, NEW.created_at)
        IS DISTINCT FROM
       (OLD.id, OLD.fleet_id, OLD.artifact_id, OLD.name, OLD.policy, OLD.policy_digest, OLD.created_at) THEN
        RAISE EXCEPTION 'identity and policy of rollout % are immutable', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.revision <> OLD.revision + 1 THEN
        RAISE EXCEPTION 'update of rollout % must advance its revision by one', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER rollouts_guard_update BEFORE UPDATE ON rollouts
    FOR EACH ROW EXECUTE FUNCTION guard_rollout_update();

CREATE TRIGGER rollouts_no_delete BEFORE DELETE ON rollouts
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TABLE rollout_stages (
    rollout_id          uuid NOT NULL REFERENCES rollouts (id),
    ordinal             integer NOT NULL CHECK (ordinal >= 1),
    percent             integer NOT NULL CHECK (percent BETWEEN 1 AND 100),
    target_count        integer CHECK (target_count >= 1),
    cohort_size         integer CHECK (cohort_size >= 0),
    state               text NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('pending', 'active', 'soaking', 'passed', 'failed', 'skipped')),
    started_at          timestamptz,
    deadline_at         timestamptz,
    soak_started_at     timestamptz,
    soak_accumulated_ms bigint NOT NULL DEFAULT 0 CHECK (soak_accumulated_ms >= 0),
    finished_at         timestamptz,
    PRIMARY KEY (rollout_id, ordinal)
);

CREATE TABLE rollout_members (
    rollout_id    uuid NOT NULL REFERENCES rollouts (id),
    device_id     text NOT NULL REFERENCES devices (id),
    position      integer NOT NULL CHECK (position >= 1),
    stage_ordinal integer NOT NULL,
    rank_key      bytea NOT NULL CHECK (octet_length(rank_key) = 32),
    PRIMARY KEY (rollout_id, device_id),
    UNIQUE (rollout_id, position),
    FOREIGN KEY (rollout_id, stage_ordinal) REFERENCES rollout_stages (rollout_id, ordinal)
);

CREATE TABLE rollout_exclusions (
    rollout_id uuid NOT NULL REFERENCES rollouts (id),
    device_id  text NOT NULL REFERENCES devices (id),
    reason     text NOT NULL CHECK (reason IN (
                   'device_status', 'platform_mismatch', 'selector_mismatch',
                   'rollout_excluded', 'already_current')),
    PRIMARY KEY (rollout_id, device_id)
);

-- Membership can only be written while the rollout is a draft, which is how the
-- start transaction writes it. Locking the rollout row makes a concurrent writer
-- wait for that transaction and then see the new state.
-- +goose StatementBegin
CREATE FUNCTION require_draft_rollout() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    current_state text;
BEGIN
    SELECT state INTO STRICT current_state FROM rollouts WHERE id = NEW.rollout_id FOR SHARE;
    IF current_state <> 'draft' THEN
        RAISE EXCEPTION 'membership of rollout % is frozen', NEW.rollout_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER rollout_members_frozen_insert BEFORE INSERT ON rollout_members
    FOR EACH ROW EXECUTE FUNCTION require_draft_rollout();
CREATE TRIGGER rollout_members_immutable BEFORE UPDATE OR DELETE ON rollout_members
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
CREATE TRIGGER rollout_exclusions_frozen_insert BEFORE INSERT ON rollout_exclusions
    FOR EACH ROW EXECUTE FUNCTION require_draft_rollout();
CREATE TRIGGER rollout_exclusions_immutable BEFORE UPDATE OR DELETE ON rollout_exclusions
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- The audit trail of every state change.
CREATE TABLE rollout_transitions (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    rollout_id  uuid NOT NULL REFERENCES rollouts (id),
    -- The rollout's revision after this transition.
    revision    bigint NOT NULL,
    from_state  text NOT NULL,
    to_state    text NOT NULL,
    event       text NOT NULL,
    actor       text NOT NULL,
    reason      text NOT NULL DEFAULT '',
    occurred_at timestamptz NOT NULL,
    UNIQUE (rollout_id, revision)
);

CREATE TRIGGER rollout_transitions_append_only BEFORE UPDATE OR DELETE ON rollout_transitions
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
