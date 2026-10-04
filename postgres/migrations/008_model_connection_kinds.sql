ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS connection_kind text NOT NULL DEFAULT 'local' CHECK (connection_kind IN ('local','hosted'));
