ALTER TABLE request_audits
    ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

COMMENT ON COLUMN request_audits.metadata IS
    'Sanitized request audit routes, identifiers, statuses and byte counts; never stores model body';
