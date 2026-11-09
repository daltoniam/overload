-- Incremental re-reviews: the earlier run a re-review built on, and the
-- run that found an earlier finding fixed.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS since_run_id bigint REFERENCES runs(id);
ALTER TABLE findings ADD COLUMN IF NOT EXISTS resolved_by_run_id bigint REFERENCES runs(id);
CREATE INDEX IF NOT EXISTS runs_repository_pr ON runs (repository_id, pr_number, id);
-- github_comment_id is the review comment a posted finding became, so
-- replies and thread resolution follow the comment itself rather than
-- its path, line and title.
ALTER TABLE findings ADD COLUMN IF NOT EXISTS github_comment_id bigint;
