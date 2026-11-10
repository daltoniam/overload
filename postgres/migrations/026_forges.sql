-- GitLab and Forgejo servers overload reviews pull requests on. GitHub
-- keeps its App tables; a repository or run without a forge is on GitHub.
CREATE TABLE IF NOT EXISTS forge_connections (
    id bigserial PRIMARY KEY,
    name text NOT NULL UNIQUE,
    kind text NOT NULL CHECK (kind IN ('gitlab', 'forgejo')),
    base_url text NOT NULL,
    token text NOT NULL,
    webhook_secret text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE repositories ADD COLUMN IF NOT EXISTS forge_id bigint REFERENCES forge_connections(id);
-- The host's numeric repository (GitLab project) ID, to follow renames.
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS forge_repo_id bigint;

-- Repository names are unique per host, not globally.
ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repositories_full_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS repositories_host_full_name ON repositories ((COALESCE(forge_id, 0)), full_name);
CREATE UNIQUE INDEX IF NOT EXISTS repositories_forge_repo ON repositories (forge_id, forge_repo_id) WHERE forge_repo_id IS NOT NULL;

ALTER TABLE runs ADD COLUMN IF NOT EXISTS forge_id bigint REFERENCES forge_connections(id);
