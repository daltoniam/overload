ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS concurrency integer NOT NULL DEFAULT 1;
DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'model_profiles_concurrency_range') THEN
        ALTER TABLE model_profiles ADD CONSTRAINT model_profiles_concurrency_range CHECK (concurrency BETWEEN 1 AND 32);
    END IF;
END $$;
