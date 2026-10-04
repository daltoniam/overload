ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS prompt_profile text NOT NULL DEFAULT 'context';
