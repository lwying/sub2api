-- Local plaintext Trace exports are independently owned temporary files, never
-- usage-owned. DB rows store task state and an opaque basename, not body bytes,
-- filesystem path, filters containing body text, or the administrator session ID.
CREATE TABLE IF NOT EXISTS request_trace_exports (
    export_id TEXT PRIMARY KEY,
    status TEXT NOT NULL,
    filters JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by BIGINT NOT NULL,
    session_digest TEXT NOT NULL,
    instance_id TEXT NOT NULL,
    filename TEXT,
    rows_exported BIGINT NOT NULL DEFAULT 0,
    rows_skipped BIGINT NOT NULL DEFAULT 0,
    bytes_exported BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    download_until TIMESTAMPTZ,
    CONSTRAINT request_trace_exports_id_shape CHECK (export_id ~ '^[0-9a-f]{32}$'),
    CONSTRAINT request_trace_exports_status_allowed CHECK (status IN ('pending', 'running', 'completed', 'failed')),
    CONSTRAINT request_trace_exports_digest_shape CHECK (session_digest ~ '^[0-9a-f]{64}$'),
    CONSTRAINT request_trace_exports_instance_shape CHECK (length(instance_id) BETWEEN 1 AND 128),
    CONSTRAINT request_trace_exports_filename_shape CHECK (
        filename IS NULL OR (length(filename) <= 255 AND filename !~ '[/\\]')
    ),
    CONSTRAINT request_trace_exports_counts_nonnegative CHECK (
        rows_exported >= 0 AND rows_skipped >= 0 AND bytes_exported >= 0
    ),
    CONSTRAINT request_trace_exports_complete_pair CHECK (
        (status = 'completed') = (completed_at IS NOT NULL AND download_until IS NOT NULL AND filename IS NOT NULL)
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS request_trace_exports_one_active_instance_idx
    ON request_trace_exports (instance_id) WHERE status IN ('pending', 'running');
CREATE INDEX IF NOT EXISTS request_trace_exports_instance_pending_idx
    ON request_trace_exports (instance_id, created_at, export_id) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS request_trace_exports_expiry_idx
    ON request_trace_exports (download_until, export_id) WHERE download_until IS NOT NULL;
CREATE INDEX IF NOT EXISTS request_trace_exports_actor_idx
    ON request_trace_exports (created_by, created_at DESC);
