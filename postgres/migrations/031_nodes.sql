-- Machines that report their inventory: hardware, runtime versions and the
-- models they serve. The first API key to report a name claims it, so a key
-- cannot overwrite another key's nodes; forgetting a node releases it.
CREATE TABLE IF NOT EXISTS nodes (
    name text PRIMARY KEY,
    report jsonb NOT NULL,
    api_key_id bigint REFERENCES api_keys(id) ON DELETE SET NULL,
    last_seen timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS nodes_api_key ON nodes (api_key_id);
-- Models sharded across nodes with MLX over Thunderbolt RDMA.
CREATE TABLE IF NOT EXISTS placements (
    name text PRIMARY KEY,
    spec jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
