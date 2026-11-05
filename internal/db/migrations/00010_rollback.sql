-- +goose Up

-- What each member ran when membership froze. It's what a rollback restores, and what the
-- device's installed release returns to afterward.
ALTER TABLE rollout_members ADD COLUMN prior_release bigint NOT NULL DEFAULT 0 CHECK (prior_release >= 0);

-- A rollback assignment names the release to restore, and a forward one doesn't. Forward
-- assignments are generation 1, and each rollback attempt uses the next generation up.
ALTER TABLE assignments ADD COLUMN restore_release bigint CHECK (restore_release >= 0);
ALTER TABLE assignments ADD CONSTRAINT assignments_restore_matches_kind
    CHECK ((kind = 'rollback') = (restore_release IS NOT NULL));
ALTER TABLE assignments ADD CONSTRAINT assignments_generation_matches_kind
    CHECK ((kind = 'forward' AND generation = 1) OR (kind = 'rollback' AND generation >= 2));

-- The rollback in progress. Attempt 0 means none has started.
ALTER TABLE rollouts ADD COLUMN rollback_attempt integer NOT NULL DEFAULT 0 CHECK (rollback_attempt >= 0);
ALTER TABLE rollouts ADD COLUMN rollback_deadline_at timestamptz;
