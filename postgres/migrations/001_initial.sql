CREATE TABLE IF NOT EXISTS github_installations (
    id bigint PRIMARY KEY,
    account_login text NOT NULL,
    account_type text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    suspended_at timestamptz
);
CREATE TABLE IF NOT EXISTS model_profiles (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE,
    provider text NOT NULL DEFAULT 'openaicompat',
    base_url text NOT NULL,
    model text NOT NULL,
    api_key_env text NOT NULL DEFAULT '',
    mode text NOT NULL DEFAULT 'context',
    params jsonb NOT NULL DEFAULT '{}',
    is_default boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS model_profiles_one_default ON model_profiles (is_default) WHERE is_default;
CREATE TABLE IF NOT EXISTS repositories (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    installation_id bigint REFERENCES github_installations(id),
    github_id bigint UNIQUE,
    full_name text NOT NULL UNIQUE,
    enabled boolean NOT NULL DEFAULT false,
    dry_run boolean NOT NULL DEFAULT true,
    model_profile_id bigint REFERENCES model_profiles(id),
    config jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS eval_sets (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS eval_cases (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    eval_set_id bigint NOT NULL REFERENCES eval_sets(id),
    repository_full_name text NOT NULL,
    pr_number integer NOT NULL,
    head_sha text NOT NULL,
    base_sha text NOT NULL,
    expected jsonb NOT NULL DEFAULT '[]',
    notes text NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS runs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    kind text NOT NULL DEFAULT 'pr_review',
    repository_id bigint NOT NULL REFERENCES repositories(id),
    pr_number integer NOT NULL,
    head_sha text NOT NULL DEFAULT '',
    base_sha text NOT NULL DEFAULT '',
    trigger text NOT NULL,
    status text NOT NULL DEFAULT 'queued',
    mode text NOT NULL DEFAULT 'context',
    model_profile jsonb NOT NULL DEFAULT '{}',
    prompt_version text NOT NULL DEFAULT '',
    sandbox_name text NOT NULL DEFAULT '',
    river_job_id bigint,
    dry_run boolean NOT NULL DEFAULT true,
    error_code text NOT NULL DEFAULT '',
    error_message text NOT NULL DEFAULT '',
    metrics jsonb NOT NULL DEFAULT '{}',
    eval_case_id bigint REFERENCES eval_cases(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz
);
CREATE INDEX IF NOT EXISTS runs_recent ON runs (created_at DESC);
CREATE TABLE IF NOT EXISTS run_events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id bigint NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    at timestamptz NOT NULL DEFAULT now(),
    level text NOT NULL,
    step text NOT NULL,
    message text NOT NULL,
    data jsonb NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS run_artifacts (
    run_id bigint NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    name text NOT NULL,
    content bytea NOT NULL,
    PRIMARY KEY (run_id, name)
);
CREATE TABLE IF NOT EXISTS findings (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    run_id bigint NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    path text NOT NULL,
    line integer NOT NULL,
    start_line integer,
    side text NOT NULL DEFAULT 'RIGHT',
    severity text NOT NULL,
    category text NOT NULL,
    title text NOT NULL,
    body text NOT NULL,
    confidence double precision NOT NULL,
    evidence text NOT NULL DEFAULT '',
    fingerprint text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'pending',
    suppressed_reason text NOT NULL DEFAULT '',
    github_comment_id bigint,
    human_label text,
    labeled_by text,
    labeled_at timestamptz
);
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    delivery_id text NOT NULL UNIQUE,
    event text NOT NULL,
    action text NOT NULL DEFAULT '',
    repository_full_name text NOT NULL DEFAULT '',
    payload jsonb NOT NULL,
    outcome text NOT NULL,
    skip_reason text NOT NULL DEFAULT '',
    run_id bigint REFERENCES runs(id),
    received_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS eval_runs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    eval_set_id bigint NOT NULL REFERENCES eval_sets(id),
    model_profile_id bigint NOT NULL REFERENCES model_profiles(id),
    mode text NOT NULL,
    prompt_version text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'queued',
    summary jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
