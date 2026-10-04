ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS agents jsonb NOT NULL DEFAULT '[]'::jsonb;
