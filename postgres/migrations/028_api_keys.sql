-- Per-person API keys for agents and CLIs. secret_hash is SHA-256 of the
-- whole key; prefix is shown in lists. user_id is NULL for keys created on
-- installs without OIDC sign-in.
CREATE TABLE IF NOT EXISTS api_keys (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id bigint REFERENCES users(id) ON DELETE CASCADE,
    name text NOT NULL,
    prefix text NOT NULL UNIQUE,
    secret_hash bytea NOT NULL UNIQUE,
    scopes text[] NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz
);
CREATE INDEX IF NOT EXISTS api_keys_user ON api_keys (user_id);
-- Model calls through the gateway, in tokens and time rather than money.
CREATE TABLE IF NOT EXISTS model_usage (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    api_key_id bigint REFERENCES api_keys(id) ON DELETE SET NULL,
    user_id bigint REFERENCES users(id) ON DELETE SET NULL,
    model text NOT NULL,
    endpoint text NOT NULL,
    status integer NOT NULL,
    input_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    queue_ms bigint NOT NULL DEFAULT 0,
    duration_ms bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS model_usage_recent ON model_usage (created_at DESC);
CREATE INDEX IF NOT EXISTS model_usage_user ON model_usage (user_id, created_at);
