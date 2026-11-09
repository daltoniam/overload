-- Spending caps in US dollars: one global row (repository_id NULL) and
-- optional per-repository rows. NULL caps mean no cap. skip_status is the
-- commit status a review skipped for budget gets.
CREATE TABLE IF NOT EXISTS budgets (
    id bigserial PRIMARY KEY,
    repository_id bigint UNIQUE REFERENCES repositories(id) ON DELETE CASCADE,
    monthly_usd double precision,
    daily_usd double precision,
    skip_status text NOT NULL DEFAULT 'success' CHECK (skip_status IN ('success', 'error')),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS budgets_global ON budgets ((repository_id IS NULL)) WHERE repository_id IS NULL;
CREATE INDEX IF NOT EXISTS runs_active_status ON runs (status) WHERE status IN ('queued', 'running');
