-- A signed webhook body may only be ingested once. GitHub redeliveries keep
-- their delivery ID, and two real events never share a body, so a captured
-- payload replayed under a new delivery ID is ignored instead of cancelling
-- the review of a newer head.
ALTER TABLE webhook_deliveries ADD COLUMN IF NOT EXISTS payload_sha256 text;
CREATE UNIQUE INDEX IF NOT EXISTS webhook_deliveries_source_payload ON webhook_deliveries (source, payload_sha256) WHERE payload_sha256 IS NOT NULL;
