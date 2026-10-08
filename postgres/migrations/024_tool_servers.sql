-- MCP servers whose tools scheduled agents can call (for example a
-- self-hosted or hosted Switchboard). Bearer tokens stay in the
-- environment; token_env only names the variable.
CREATE TABLE IF NOT EXISTS tool_servers (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name text NOT NULL UNIQUE,
    url text NOT NULL,
    token_env text NOT NULL DEFAULT '',
    description text NOT NULL DEFAULT '',
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
-- The tool servers each agent may use, by name.
ALTER TABLE agent_definitions ADD COLUMN IF NOT EXISTS tool_servers jsonb NOT NULL DEFAULT '[]';
