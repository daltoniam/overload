-- Rules that trust OIDC tokens from automation (GitHub Actions, Kubernetes
-- service accounts) whose issuer, audience and claims match.
CREATE TABLE IF NOT EXISTS workload_rules (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE,
    issuer text NOT NULL,
    audience text NOT NULL,
    conditions jsonb NOT NULL,
    scopes text[] NOT NULL,
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
