-- 245_request_audit_reservations.sql
-- 强制审计在上游发送前同步写入的临时协议元数据；不保存模型正文或凭据明文。

CREATE TABLE IF NOT EXISTS request_audit_reservations (
    id BIGSERIAL PRIMARY KEY,
    logical_key TEXT NOT NULL UNIQUE,
    route_family TEXT NOT NULL,
    forced BOOLEAN NOT NULL DEFAULT FALSE,
    headers JSONB NOT NULL DEFAULT '{}'::jsonb,
    attempts JSONB NOT NULL DEFAULT '[]'::jsonb,
    usage_log_id BIGINT UNIQUE REFERENCES usage_logs(id) ON DELETE CASCADE,
    capture_completeness TEXT NOT NULL DEFAULT 'complete',
    capture_reason TEXT NOT NULL DEFAULT '',
    send_started_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_request_audit_reservations_expires_at
    ON request_audit_reservations(expires_at);

COMMENT ON TABLE request_audit_reservations IS
    'Temporary forced-audit metadata reserved before upstream HTTP sends; never stores model bodies';
