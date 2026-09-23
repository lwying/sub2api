CREATE TABLE IF NOT EXISTS request_audits (
    id BIGSERIAL PRIMARY KEY,
    usage_log_id BIGINT NOT NULL UNIQUE REFERENCES usage_logs(id) ON DELETE CASCADE,
    headers JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE request_audits IS
    'Request audit protocol metadata attached 1:1 to usage_logs; never stores model body';

COMMENT ON COLUMN request_audits.usage_log_id IS
    'Owning usage log; deleted with the usage log';

COMMENT ON COLUMN request_audits.headers IS
    'Allowed protocol headers only; credentials stored as presence, never plaintext';
