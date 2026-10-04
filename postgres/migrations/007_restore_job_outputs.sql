CREATE TABLE IF NOT EXISTS job_outputs (
    run_id bigint PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    kind text NOT NULL,
    content jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
