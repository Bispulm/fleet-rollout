-- +goose Up

-- A stage can end because its rollout stopped for reasons outside the stage: an abort,
-- or a failure elsewhere. Calling that "failed" would blame devices that did nothing wrong.
ALTER TABLE rollout_stages DROP CONSTRAINT rollout_stages_state_check;
ALTER TABLE rollout_stages ADD CONSTRAINT rollout_stages_state_check CHECK (state IN (
    'pending', 'active', 'soaking', 'passed', 'failed', 'skipped', 'stopped'));
