ALTER TABLE request_audits
    ADD COLUMN IF NOT EXISTS events JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN request_audits.events IS
    'SSE event skeletons: type, index, bytes; never stores event text or deltas';
