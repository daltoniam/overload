ALTER TABLE runs ADD COLUMN IF NOT EXISTS post_status text NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN IF NOT EXISTS posted_at timestamptz;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS github_review_id bigint;
CREATE INDEX IF NOT EXISTS findings_posted_fingerprint ON findings (fingerprint) WHERE status = 'posted';
