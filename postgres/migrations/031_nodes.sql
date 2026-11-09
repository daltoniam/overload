-- Machines that report their inventory: hardware, runtime versions and the
-- models they serve.
CREATE TABLE IF NOT EXISTS nodes (
    name text PRIMARY KEY,
    report jsonb NOT NULL,
    last_seen timestamptz NOT NULL DEFAULT now()
);
-- Models sharded across nodes with MLX over Thunderbolt RDMA.
CREATE TABLE IF NOT EXISTS placements (
    name text PRIMARY KEY,
    spec jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
