-- request_trace_stages.metadata is the only free-form JSONB in the Trace schema.
-- Migration 258 created it as JSONB NOT NULL DEFAULT '{}'::jsonb with no shape, key
-- or size bound: the typed RequestTraceStageFacts projection and the closed key set
-- are enforced in the application only. A future writer, a manual psql write or a
-- deployment that skips the service guard could therefore store an array, a scalar,
-- an unbounded blob or arbitrary keys in the stage row that the admin UI and the
-- plaintext export read back. This migration adds the database-side backstop:
--
--   1. metadata must be a JSON object, not an array or a scalar;
--   2. only the closed key set produced by RequestTraceStageFacts is accepted
--      (method, url, url_omitted, request_headers, request_headers_omitted,
--       response_headers, response_headers_omitted, account_id, model, protocol,
--       value_protocol, status, started_at, ended_at);
--   3. the canonical metadata text stays within 4096 bytes, so a single stage row
--      cannot carry a body-sized blob through the facts channel;
--   4. only the two stages that legitimately carry facts -- client_metadata and
--      wire_attempt -- may store a non-empty object; every other stage must keep
--      '{}', so an observed transport fact cannot be attached to a body stage.
--
-- 258 is not modified; these are additive constraints. Each one is dropped by name
-- first because the migration runner skips already-applied files by checksum, but a
-- disaster-recovery replay of this file re-executes the SQL and must not fail with
-- "constraint ... already exists" (migrations/README.md).
--
-- The constraints are validating rather than NOT VALID. request_trace_stages is
-- created by 258 as part of this same not-yet-released, default-off Trace feature,
-- so the table holds at most the rows a single opt-in deployment captured, and the
-- only writer stores the bounded 14-key projection or '{}'. Scanning that bounded
-- set lets PostgreSQL prove the already-captured rows conform instead of trusting
-- the application guard forever.
--
-- The object shape and the key allowlist are one constraint because jsonb minus a
-- key array raises "cannot delete from scalar" for non-objects; the CASE makes the
-- key subtraction unreachable unless jsonb_typeof already proved an object.
ALTER TABLE request_trace_stages
    DROP CONSTRAINT IF EXISTS request_trace_stages_metadata_shape_allowed;
ALTER TABLE request_trace_stages
    ADD CONSTRAINT request_trace_stages_metadata_shape_allowed CHECK (
        CASE
            WHEN jsonb_typeof(metadata) = 'object' THEN
                (metadata - ARRAY[
                    'method', 'url', 'url_omitted',
                    'request_headers', 'request_headers_omitted',
                    'response_headers', 'response_headers_omitted',
                    'account_id', 'model', 'protocol', 'value_protocol',
                    'status', 'started_at', 'ended_at'
                ]) = '{}'::jsonb
            ELSE FALSE
        END
    );

-- Canonical jsonb text (jsonb_out) is what the readers and the exporter observe, so
-- the bound is measured on that exact representation rather than on the caller's
-- compact encoding. The writer's own budget is 3072 bytes, leaving headroom for the
-- canonical ": "/", " separators.
ALTER TABLE request_trace_stages
    DROP CONSTRAINT IF EXISTS request_trace_stages_metadata_size_bound;
ALTER TABLE request_trace_stages
    ADD CONSTRAINT request_trace_stages_metadata_size_bound CHECK (
        octet_length(metadata::text) <= 4096
    );

ALTER TABLE request_trace_stages
    DROP CONSTRAINT IF EXISTS request_trace_stages_metadata_stage_allowed;
ALTER TABLE request_trace_stages
    ADD CONSTRAINT request_trace_stages_metadata_stage_allowed CHECK (
        metadata = '{}'::jsonb OR stage IN ('client_metadata', 'wire_attempt')
    );
