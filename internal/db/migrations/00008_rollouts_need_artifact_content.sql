-- +goose Up

-- A rollout can only use an artifact whose bytes are stored and match its signed manifest.
-- Otherwise devices would be assigned something they could never fetch.
-- +goose StatementBegin
CREATE FUNCTION require_artifact_content() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    ready timestamptz;
BEGIN
    SELECT content_ready_at INTO ready FROM artifacts WHERE id = NEW.artifact_id;
    IF ready IS NULL THEN
        RAISE EXCEPTION 'artifact % has no verified content', NEW.artifact_id
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER rollouts_require_artifact_content BEFORE INSERT ON rollouts
    FOR EACH ROW EXECUTE FUNCTION require_artifact_content();
