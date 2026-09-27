-- New diagnostic values have a separate, opt-in plaintext format. Previously written
-- ciphertext and its seven-day deadline remain untouched; the legacy usage_log_id
-- relationship keeps its ON DELETE SET NULL behavior.
-- New records start unlinked and become usage-owned only after the exact logical
-- request and real wire attempt have been verified. Unlinked records stop being
-- readable at metadata_expires_at (created_at + 30 days), even if cleanup lags.
-- A linked record is deleted with its owning usage; if usage cleanup is disabled,
-- its plaintext has no fixed maximum lifetime. Backups and replicas can retain it.
ALTER TABLE error_diagnostic_records
    ADD COLUMN IF NOT EXISTS plain_record BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS plain_owner_usage_log_id BIGINT REFERENCES usage_logs(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS plain_link_digest TEXT,
    ADD COLUMN IF NOT EXISTS plain_link_attempt_index INTEGER,
    ADD COLUMN IF NOT EXISTS plain_link_wire_status INTEGER,
    ADD COLUMN IF NOT EXISTS plain_body_state TEXT NOT NULL DEFAULT 'not_observed',
    ADD COLUMN IF NOT EXISTS plain_body_reason TEXT NOT NULL DEFAULT 'not_observed',
    ADD COLUMN IF NOT EXISTS plain_body_payload BYTEA,
    ADD COLUMN IF NOT EXISTS plain_body_bytes INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS plain_header_state TEXT NOT NULL DEFAULT 'not_observed',
    ADD COLUMN IF NOT EXISTS plain_header_reason TEXT NOT NULL DEFAULT 'not_observed',
    ADD COLUMN IF NOT EXISTS plain_header_payload BYTEA,
    ADD COLUMN IF NOT EXISTS plain_header_bytes INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS plain_header_entry_count INTEGER NOT NULL DEFAULT 0;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'error_diagnostic_plain_owner_exclusive') THEN
        ALTER TABLE error_diagnostic_records ADD CONSTRAINT error_diagnostic_plain_owner_exclusive CHECK (
            plain_owner_usage_log_id IS NULL OR (plain_record AND usage_log_id IS NULL)
        );
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'error_diagnostic_plain_link_shape') THEN
        ALTER TABLE error_diagnostic_records ADD CONSTRAINT error_diagnostic_plain_link_shape CHECK (
            (plain_link_digest IS NULL OR (plain_record AND plain_link_digest ~ '^[0-9a-f]{64}$'))
            AND (plain_link_attempt_index IS NULL OR (plain_record AND plain_link_attempt_index BETWEEN 1 AND 1000))
            AND (plain_link_wire_status IS NULL OR (plain_record AND plain_link_wire_status BETWEEN 400 AND 599))
            AND (plain_link_attempt_index IS NULL OR plain_link_wire_status IS NOT NULL)
        );
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'error_diagnostic_plain_body_allowed') THEN
        ALTER TABLE error_diagnostic_records ADD CONSTRAINT error_diagnostic_plain_body_allowed CHECK (
            plain_body_state IN ('not_observed', 'stored', 'skipped')
            AND plain_body_reason IN ('not_observed', 'plain_body_retained',
                'skipped_not_text_json', 'skipped_too_large', 'skipped_attachment',
                'skipped_known_credential', 'skipped_incomplete_read', 'skipped_body_retention_disabled')
        );
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'error_diagnostic_plain_header_allowed') THEN
        ALTER TABLE error_diagnostic_records ADD CONSTRAINT error_diagnostic_plain_header_allowed CHECK (
            plain_header_state IN ('not_observed', 'stored', 'skipped')
            AND plain_header_reason IN ('not_observed', 'plain_header_retained',
                'skipped_header_retention_disabled', 'skipped_invalid_values')
        );
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'error_diagnostic_plain_body_pair') THEN
        ALTER TABLE error_diagnostic_records ADD CONSTRAINT error_diagnostic_plain_body_pair CHECK (
            (plain_body_state = 'stored') = (plain_body_payload IS NOT NULL)
            AND (plain_body_payload IS NULL OR (plain_record AND body_ciphertext IS NULL))
            AND plain_body_bytes >= 0 AND plain_body_bytes <= 1048576
        );
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'error_diagnostic_plain_header_pair') THEN
        ALTER TABLE error_diagnostic_records ADD CONSTRAINT error_diagnostic_plain_header_pair CHECK (
            (plain_header_state = 'stored') = (plain_header_payload IS NOT NULL)
            AND (plain_header_payload IS NULL OR (plain_record AND header_ciphertext IS NULL))
            AND plain_header_bytes >= 0 AND plain_header_bytes <= 4096
            AND plain_header_entry_count BETWEEN 0 AND 64
        );
    END IF;
END
$$;

CREATE INDEX IF NOT EXISTS error_diagnostic_plain_pending_digest_idx
    ON error_diagnostic_records (plain_link_digest)
    WHERE plain_record AND plain_owner_usage_log_id IS NULL AND plain_link_digest IS NOT NULL;
CREATE INDEX IF NOT EXISTS error_diagnostic_plain_owner_idx
    ON error_diagnostic_records (plain_owner_usage_log_id)
    WHERE plain_owner_usage_log_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS error_diagnostic_plain_unlinked_expiry_idx
    ON error_diagnostic_records (metadata_expires_at)
    WHERE plain_record AND plain_owner_usage_log_id IS NULL;
