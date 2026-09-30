-- Online indexes for request-time owner lookups on potentially hot Trace rows.
CREATE INDEX CONCURRENTLY IF NOT EXISTS request_traces_user_created_idx
    ON request_traces (user_id, created_at DESC) WHERE user_id IS NOT NULL;
CREATE INDEX CONCURRENTLY IF NOT EXISTS request_traces_api_key_created_idx
    ON request_traces (api_key_id, created_at DESC) WHERE api_key_id IS NOT NULL;
