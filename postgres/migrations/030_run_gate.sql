-- The merge gate verdict a review's commit status reported: its state
-- ("success" or "failure") and the changed files on human review paths.
-- A failing verdict stays until a person re-reviews the commit, and the
-- human review note is posted only when those files change.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS gate jsonb NOT NULL DEFAULT '{}'::jsonb;
