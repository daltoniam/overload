ALTER TABLE runs ADD COLUMN IF NOT EXISTS installation_id bigint;
ALTER TABLE trigger_bindings DROP COLUMN IF EXISTS dry_run;
