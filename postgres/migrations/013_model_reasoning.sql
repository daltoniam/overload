ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS reasoning_param text NOT NULL DEFAULT '';
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS reasoning_effort text NOT NULL DEFAULT '';
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS max_output_tokens integer NOT NULL DEFAULT 0;
