-- Stable request-time identity facts for a captured logical request.
-- NULL means not observed (including every pre-upgrade Trace and auth rejection).
-- Names are intentionally not snapshotted; the admin list resolves current names
-- when present, falling back to the stable ID after deletion.
ALTER TABLE request_traces
    ADD COLUMN IF NOT EXISTS user_id BIGINT,
    ADD COLUMN IF NOT EXISTS api_key_id BIGINT;

COMMENT ON COLUMN request_traces.user_id IS
    'Authenticated downstream user ID at request time; NULL when not observed';
COMMENT ON COLUMN request_traces.api_key_id IS
    'Authenticated downstream API Key ID at request time; NULL when not observed';
