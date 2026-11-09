-- The catalog entry a model connection names ('' for none); the model
-- policy is checked against it.
ALTER TABLE model_profiles ADD COLUMN IF NOT EXISTS catalog_id text NOT NULL DEFAULT '';
-- The administrator's model policy (package catalog). One row.
CREATE TABLE IF NOT EXISTS model_policy (
    id integer PRIMARY KEY CHECK (id = 1),
    policy jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
