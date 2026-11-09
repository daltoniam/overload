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
-- cost_at is when the cost was recorded: budgets count spend in the
-- period it happened, not the one the run was queued in.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS cost_usd double precision;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS cost_unpriced_calls bigint NOT NULL DEFAULT 0;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS cost_at timestamptz;
CREATE INDEX IF NOT EXISTS runs_cost_at ON runs ((COALESCE(cost_at, created_at))) WHERE cost_usd IS NOT NULL;
