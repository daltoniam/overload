-- Skip globs, sub-agent scopes, the main agent's review mode and the
-- file-review limit of a PR review workflow. An empty object keeps the
-- previous behaviour: every agent reviews every changed file.
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS routing jsonb NOT NULL DEFAULT '{}'::jsonb;
