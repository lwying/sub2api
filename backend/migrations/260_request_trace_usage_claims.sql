-- The usage writer may finish before the HTTP Trace envelope is stored. A claim
-- contains only a server-generated identity and a usage ID produced by an actual
-- INSERT. It never infers linkage from caller-supplied request IDs or timestamps.
-- Claims disappear with usage deletion and are periodically removed after day 30
-- when no Trace was persisted. Historical migrations remain unchanged.
CREATE TABLE IF NOT EXISTS request_trace_usage_claims (
    trace_id TEXT PRIMARY KEY,
    usage_log_id BIGINT NOT NULL UNIQUE REFERENCES usage_logs(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL DEFAULT (now() + interval '30 days'),
    CONSTRAINT request_trace_usage_claim_id_shape CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    CONSTRAINT request_trace_usage_claim_expiry CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS request_trace_usage_claim_expiry_idx
    ON request_trace_usage_claims(expires_at, trace_id);
