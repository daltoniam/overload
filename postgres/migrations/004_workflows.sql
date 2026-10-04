CREATE TABLE IF NOT EXISTS prompt_templates (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('entry', 'review', 'final')),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, kind)
);
CREATE TABLE IF NOT EXISTS prompt_revisions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    template_id bigint NOT NULL REFERENCES prompt_templates(id) ON DELETE RESTRICT,
    revision integer NOT NULL CHECK (revision > 0),
    body text NOT NULL,
    content_sha256 text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (template_id, revision)
);
CREATE TABLE IF NOT EXISTS agent_definitions (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE,
    model_profile_id bigint NOT NULL REFERENCES model_profiles(id) ON DELETE RESTRICT,
    entry_prompt_revision_id bigint NOT NULL REFERENCES prompt_revisions(id) ON DELETE RESTRICT,
    review_prompt_revision_id bigint REFERENCES prompt_revisions(id) ON DELETE RESTRICT,
    final_prompt_revision_id bigint REFERENCES prompt_revisions(id) ON DELETE RESTRICT,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS workflows (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE,
    kind text NOT NULL CHECK (kind IN ('pr_review', 'scheduled_prompt', 'incident')),
    revision integer NOT NULL DEFAULT 1,
    agent_names jsonb NOT NULL DEFAULT '[]'::jsonb,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS trigger_bindings (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source text NOT NULL,
    event text NOT NULL,
    action text NOT NULL,
    repository_full_name text NOT NULL DEFAULT '',
    workflow_id bigint NOT NULL REFERENCES workflows(id) ON DELETE RESTRICT,
    enabled boolean NOT NULL DEFAULT true,
    dry_run boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, event, action, repository_full_name)
);
CREATE TABLE IF NOT EXISTS schedules (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE,
    workflow_id bigint NOT NULL REFERENCES workflows(id) ON DELETE RESTRICT,
    cron text NOT NULL,
    timezone text NOT NULL,
    input_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    next_run_at timestamptz NOT NULL,
    enabled boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS schedule_occurrences (
    schedule_id bigint NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
    scheduled_for timestamptz NOT NULL,
    run_id bigint UNIQUE REFERENCES runs(id),
    PRIMARY KEY (schedule_id, scheduled_for)
);
ALTER TABLE runs ADD COLUMN IF NOT EXISTS config_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS binding_id bigint REFERENCES trigger_bindings(id);
ALTER TABLE runs ADD COLUMN IF NOT EXISTS schedule_id bigint REFERENCES schedules(id);
ALTER TABLE runs ADD COLUMN IF NOT EXISTS scheduled_for timestamptz;
ALTER TABLE runs ALTER COLUMN repository_id DROP NOT NULL;
ALTER TABLE runs ALTER COLUMN pr_number DROP NOT NULL;
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS source text NOT NULL DEFAULT 'github';
CREATE UNIQUE INDEX IF NOT EXISTS webhook_deliveries_source_delivery ON webhook_deliveries (source, delivery_id);
ALTER TABLE webhook_deliveries DROP CONSTRAINT IF EXISTS webhook_deliveries_delivery_id_key;
CREATE TABLE IF NOT EXISTS job_outputs (
    run_id bigint PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    kind text NOT NULL,
    content jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
