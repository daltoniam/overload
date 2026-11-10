-- Model prices in US dollars per million tokens (NULL: unpriced), and the
-- cost of each run.
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS price_input double precision;
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS price_cached_input double precision;
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS price_cache_write double precision;
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS price_output double precision;
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS long_context_tokens bigint NOT NULL DEFAULT 0;
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS long_context_input_multiplier double precision NOT NULL DEFAULT 0;
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS long_context_output_multiplier double precision NOT NULL DEFAULT 0;

-- cost_usd is the priced spend of a run's model calls; cost_unpriced_calls
-- counts calls to models without prices. NULL cost means not recorded.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS cost_usd double precision;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS cost_unpriced_calls bigint NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS runs_cost_created ON runs (created_at) WHERE cost_usd IS NOT NULL;

-- run_costs records each addition to a run's cost when it happened, so
-- budgets and the dashboard count spend in the period it was incurred,
-- even for a run that spans midnight or the end of a month.
CREATE TABLE IF NOT EXISTS run_costs (
    id bigserial PRIMARY KEY,
    run_id bigint NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    usd double precision NOT NULL,
    unpriced_calls bigint NOT NULL DEFAULT 0,
    at timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS run_costs_at ON run_costs (at);
CREATE INDEX IF NOT EXISTS run_costs_run ON run_costs (run_id);
