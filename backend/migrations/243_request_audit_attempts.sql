ALTER TABLE request_audits
    ADD COLUMN IF NOT EXISTS attempts JSONB NOT NULL DEFAULT '[]'::jsonb;

COMMENT ON COLUMN request_audits.attempts IS
    'Upstream attempt timeline: account, model, protocol, stage; never stores model body';
