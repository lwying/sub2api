-- Independently owned request traces may exist without usage (for example an auth
-- rejection). A verified usage owner deletes the trace and every stage atomically
-- through ordinary row-level cascading FKs. Partition DROP is not row-level DELETE:
-- the Trace gate rejects that deployment shape before capturing plaintext.
-- Unlinked rows become cleanup candidates after 30 days, but remain readable until
-- physically deleted. Backups, replicas and exports have independent lifetimes.
CREATE TABLE IF NOT EXISTS request_traces (
    id BIGSERIAL PRIMARY KEY,
    trace_id TEXT NOT NULL UNIQUE,
    route_family TEXT NOT NULL,
    inbound_endpoint TEXT NOT NULL,
    capture_state TEXT NOT NULL DEFAULT 'not_observed',
    client_status INTEGER NOT NULL DEFAULT 0,
    usage_log_id BIGINT UNIQUE REFERENCES usage_logs(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    completed_at TIMESTAMPTZ,
    cleanup_after TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '30 days'),
    CONSTRAINT request_traces_trace_id_shape CHECK (trace_id ~ '^[0-9a-f]{32}$'),
    CONSTRAINT request_traces_route_family_allowed CHECK (
        route_family IN ('messages', 'chat_completions', 'responses')
    ),
    CONSTRAINT request_traces_capture_state_allowed CHECK (
        capture_state IN ('not_observed', 'stored', 'partial', 'write_failed')
    ),
    CONSTRAINT request_traces_client_status_range CHECK (client_status BETWEEN 0 AND 599)
);

CREATE TABLE IF NOT EXISTS request_trace_stages (
    id BIGSERIAL PRIMARY KEY,
    trace_id BIGINT NOT NULL REFERENCES request_traces(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    stage TEXT NOT NULL,
    attempt_index INTEGER NOT NULL DEFAULT 0,
    view_name TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    reason TEXT NOT NULL,
    observed_bytes BIGINT NOT NULL DEFAULT 0,
    retained_bytes INTEGER NOT NULL DEFAULT 0,
    dropped_events INTEGER NOT NULL DEFAULT 0,
    redaction_unverified BOOLEAN NOT NULL DEFAULT FALSE,
    payload BYTEA,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT request_trace_stages_ordinal_positive CHECK (ordinal > 0),
    CONSTRAINT request_trace_stages_attempt_range CHECK (attempt_index BETWEEN 0 AND 1000),
    CONSTRAINT request_trace_stages_view_allowed CHECK (
        view_name IN ('', 'transmitted', 'decoded', 'wire', 'received', 'downstream')
    ),
    CONSTRAINT request_trace_stages_state_allowed CHECK (
        state IN ('not_observed', 'stored', 'truncated', 'unsupported', 'redaction_unverified', 'write_failed')
    ),
    CONSTRAINT request_trace_stages_payload_size CHECK (
        observed_bytes >= 0 AND retained_bytes BETWEEN 0 AND 1048576
        AND dropped_events >= 0
        AND octet_length(payload) <= 1048576
        AND (payload IS NULL OR (state IN ('stored', 'truncated', 'redaction_unverified') AND retained_bytes = octet_length(payload)))
    ),
    UNIQUE (trace_id, ordinal)
);

CREATE INDEX IF NOT EXISTS request_traces_created_at_id_idx ON request_traces (created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS request_traces_unlinked_cleanup_idx ON request_traces (cleanup_after, id)
    WHERE usage_log_id IS NULL;
CREATE INDEX IF NOT EXISTS request_trace_stages_trace_order_idx ON request_trace_stages (trace_id, ordinal);
