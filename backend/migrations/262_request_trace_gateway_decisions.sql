-- Migration 261 closed request_trace_stages.metadata -- the single free-form JSONB
-- column of the Trace schema -- to the 14 keys produced by RequestTraceStageFacts and
-- to the two stages that legitimately carry transport facts (client_metadata and
-- wire_attempt). Every other stage had to keep '{}'.
--
-- The gateway decision stage described by service/request_trace_decision.go needs the
-- same column for a second, disjoint typed projection: the body-less gateway_decision
-- stage stores a RequestTraceDecisionFacts object (decision, outcome, source, sequence,
-- model/protocol from+to, account_id, decided_at). This migration widens the closed key
-- set and the stage allowlist to cover both projections, without touching 261 or 258:
--
--   1. metadata must still be a JSON object, not an array or a scalar;
--   2. the accepted keys are chosen per stage, so the two projections can never be
--      mixed inside one row: gateway_decision accepts only the decision keys, the two
--      transport stages accept only the transport keys, and every other stage must
--      keep '{}';
--   3. a gateway_decision row must actually carry a decision ('{}' is refused), so a
--      stage named "decision" cannot exist without the decision it claims to describe;
--   4. only the three stages that legitimately carry facts may store a non-empty
--      object, so an observed transport fact cannot be attached to a body stage and a
--      decision cannot be attached to a wire attempt.
--
-- The 4096 byte canonical-text bound added by 261 is intentionally left in place: the
-- decision projection is additionally bounded to 1024 bytes by the application
-- (requestTraceDecisionFactsLimit) and, once serialized, is far below this column bound.
--
-- 261 and 258 are not modified; these are additive replacements of the two allowlists.
-- Each constraint is dropped by name first because the migration runner skips
-- already-applied files by checksum, but a disaster-recovery replay of this file
-- re-executes the SQL and must not fail with "constraint ... already exists"
-- (migrations/README.md).
--
-- Both constraints stay validating rather than NOT VALID for the same reason as 261:
-- request_trace_stages belongs to the same not-yet-released, default-off Trace feature,
-- so the only rows that can exist are the ones the single writer produced -- either the
-- bounded transport projection on client_metadata/wire_attempt or '{}'. Scanning them
-- lets PostgreSQL prove the already-captured rows conform instead of trusting the
-- application guard forever.
--
-- The outer CASE keeps the non-object rejection of 261. The inner CASE is exhaustive so
-- the predicate is never NULL: a CHECK that evaluates to NULL is satisfied, which would
-- silently accept an unlisted stage carrying a non-empty object.
ALTER TABLE request_trace_stages
    DROP CONSTRAINT IF EXISTS request_trace_stages_metadata_shape_allowed;
ALTER TABLE request_trace_stages
    ADD CONSTRAINT request_trace_stages_metadata_shape_allowed CHECK (
        CASE jsonb_typeof(metadata)
            WHEN 'object' THEN
                CASE
                    WHEN stage = 'gateway_decision' THEN
                        metadata <> '{}'::jsonb
                        AND (metadata - ARRAY[
                            'decision', 'outcome', 'source', 'sequence',
                            'model_from', 'model_to',
                            'protocol_from', 'protocol_to',
                            'account_id', 'decided_at'
                        ]) = '{}'::jsonb
                    WHEN stage IN ('client_metadata', 'wire_attempt') THEN
                        (metadata - ARRAY[
                            'method', 'url', 'url_omitted',
                            'request_headers', 'request_headers_omitted',
                            'response_headers', 'response_headers_omitted',
                            'account_id', 'model', 'protocol', 'value_protocol',
                            'status', 'started_at', 'ended_at'
                        ]) = '{}'::jsonb
                    ELSE
                        metadata = '{}'::jsonb
                END
            ELSE FALSE
        END
    );

ALTER TABLE request_trace_stages
    DROP CONSTRAINT IF EXISTS request_trace_stages_metadata_stage_allowed;
ALTER TABLE request_trace_stages
    ADD CONSTRAINT request_trace_stages_metadata_stage_allowed CHECK (
        metadata = '{}'::jsonb OR stage IN ('client_metadata', 'wire_attempt', 'gateway_decision')
    );
