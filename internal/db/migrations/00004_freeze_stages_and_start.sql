-- +goose Up

-- Starting a rollout is one-way, and what it records can't be rewritten afterward.
-- Without this, an update could return a rollout to draft and reopen its membership.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION guard_rollout_update() RETURNS trigger
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
    IF OLD.state <> 'draft' THEN
        IF NEW.state = 'draft' THEN
            RAISE EXCEPTION 'rollout % has started and cannot return to draft', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
        IF (NEW.member_count, NEW.started_at) IS DISTINCT FROM (OLD.member_count, OLD.started_at) THEN
            RAISE EXCEPTION 'member count and start time of rollout % are frozen', OLD.id
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION require_draft_rollout() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    current_state text;
BEGIN
    SELECT state INTO STRICT current_state FROM rollouts WHERE id = NEW.rollout_id FOR SHARE;
    IF current_state <> 'draft' THEN
        RAISE EXCEPTION 'membership and stage plan of rollout % are frozen', NEW.rollout_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- A stage's percentage is part of the immutable policy, and its size is fixed by the
-- start transaction. Only progress through the stage can change after that.
-- +goose StatementBegin
CREATE FUNCTION guard_rollout_stage_update() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    current_state text;
BEGIN
    IF (NEW.rollout_id, NEW.ordinal, NEW.percent) IS DISTINCT FROM (OLD.rollout_id, OLD.ordinal, OLD.percent) THEN
        RAISE EXCEPTION 'stage % of rollout % is immutable', OLD.ordinal, OLD.rollout_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF (NEW.target_count, NEW.cohort_size) IS DISTINCT FROM (OLD.target_count, OLD.cohort_size) THEN
        SELECT state INTO STRICT current_state FROM rollouts WHERE id = NEW.rollout_id FOR SHARE;
        IF current_state <> 'draft' THEN
            RAISE EXCEPTION 'size of stage % of rollout % is frozen', OLD.ordinal, OLD.rollout_id
                USING ERRCODE = 'restrict_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER rollout_stages_guard_update BEFORE UPDATE ON rollout_stages
    FOR EACH ROW EXECUTE FUNCTION guard_rollout_stage_update();
CREATE TRIGGER rollout_stages_frozen_insert BEFORE INSERT ON rollout_stages
    FOR EACH ROW EXECUTE FUNCTION require_draft_rollout();
CREATE TRIGGER rollout_stages_no_delete BEFORE DELETE ON rollout_stages
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
