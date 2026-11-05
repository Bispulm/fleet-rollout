-- +goose Up

-- The signed envelope is kept exactly as received, because the signature covers exact bytes.
-- A device verifies it again itself, so the server doesn't have to be trusted with it.
ALTER TABLE artifacts
    ADD COLUMN envelope bytea,
    ADD COLUMN chunk_size bigint CHECK (chunk_size >= 1),
    ADD COLUMN content_ready_at timestamptz,
    ADD CONSTRAINT artifacts_content_needs_envelope CHECK (content_ready_at IS NULL OR envelope IS NOT NULL);

-- An artifact stays immutable, except for recording once that its content has arrived.
-- +goose StatementBegin
CREATE FUNCTION guard_artifact_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - 'content_ready_at') IS DISTINCT FROM (to_jsonb(OLD) - 'content_ready_at') THEN
        RAISE EXCEPTION 'artifact % is immutable', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.content_ready_at IS NOT NULL AND NEW.content_ready_at IS DISTINCT FROM OLD.content_ready_at THEN
        RAISE EXCEPTION 'content of artifact % was already recorded', OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

DROP TRIGGER artifacts_immutable ON artifacts;
CREATE TRIGGER artifacts_guard_update BEFORE UPDATE ON artifacts
    FOR EACH ROW EXECUTE FUNCTION guard_artifact_update();
CREATE TRIGGER artifacts_no_delete BEFORE DELETE ON artifacts
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();
