-- New request-audit value details are plaintext and usage-owned. Existing encrypted
-- rows retain their original seven-day expiry; migration 253 is immutable.
-- Physical usage deletion cascades this row. A partition DROP does not invoke
-- row-level foreign key actions; deployments using partition DROP must first
-- synchronously remove linked plaintext rows or disable value capture there.
-- The new CHECK predicates are supersets of migration 253's constraints for
-- existing encrypted rows. NOT VALID avoids validating a large history under
-- ACCESS EXCLUSIVE while enforcing the new predicates on all future writes.
--
-- 重放安全：253／255／256 都按「先 DROP IF EXISTS、再 ADD」重建约束，本迁移同样如此。
-- 运行器按 checksum 跳过已应用的迁移，但灾备重放与手工重放会重新执行同一份 SQL；
-- 裸 ADD CONSTRAINT 会在第二次执行时报 constraint already exists 而失败。
-- ADD COLUMN ... NOT NULL DEFAULT 走 fast default（不重写堆表），代价与既有行数无关。
ALTER TABLE request_audit_value_details
    ADD COLUMN IF NOT EXISTS storage_format TEXT NOT NULL DEFAULT 'encrypted_v1',
    ADD COLUMN IF NOT EXISTS plaintext_payload BYTEA;

ALTER TABLE request_audit_value_details
    ALTER COLUMN expires_at DROP NOT NULL,
    ALTER COLUMN expires_at DROP DEFAULT;

ALTER TABLE request_audit_value_details
    DROP CONSTRAINT IF EXISTS request_audit_value_details_stored_requires_ciphertext,
    DROP CONSTRAINT IF EXISTS request_audit_value_details_expires_after_creation,
    DROP CONSTRAINT IF EXISTS request_audit_value_details_reason_allowed,
    -- 这两条由本迁移新建；一并 DROP 才能重放（它们只存在于本迁移之后的库上）。
    DROP CONSTRAINT IF EXISTS request_audit_value_details_storage_format_allowed,
    DROP CONSTRAINT IF EXISTS request_audit_value_details_storage_pairing;

ALTER TABLE request_audit_value_details
    ADD CONSTRAINT request_audit_value_details_reason_allowed CHECK (
        reason IN ('not_observed', 'retained', 'skipped_out_of_scope',
            'skipped_value_retention_disabled', 'skipped_encryption_unavailable',
            'skipped_invalid_values', 'skipped_too_many_attempts', 'skipped_unsupported_protocol')
    ) NOT VALID,
    ADD CONSTRAINT request_audit_value_details_storage_format_allowed CHECK (
        storage_format IN ('encrypted_v1', 'plaintext_usage_bound')
    ) NOT VALID,
    ADD CONSTRAINT request_audit_value_details_storage_pairing CHECK (
        (storage_format = 'encrypted_v1' AND plaintext_payload IS NULL AND expires_at IS NOT NULL AND expires_at > created_at
            AND (state <> 'stored' OR ciphertext IS NOT NULL))
        OR
        (storage_format = 'plaintext_usage_bound' AND ciphertext IS NULL AND key_version = 0 AND expires_at IS NULL
            AND (state <> 'stored' OR plaintext_payload IS NOT NULL)
            AND (plaintext_payload IS NULL OR state = 'stored'))
    ) NOT VALID;

COMMENT ON COLUMN request_audit_value_details.storage_format IS
    'encrypted_v1 retains its original seven-day expiry; plaintext_usage_bound has no independent deadline and cascades with usage_logs';
COMMENT ON COLUMN request_audit_value_details.plaintext_payload IS
    'Bounded, revalidated allowlisted value details; never an HTTP body, credential header or cookie';
COMMENT ON COLUMN request_audit_value_details.expires_at IS
    'Seven-day expiry for encrypted_v1 rows only; plaintext_usage_bound rows have NULL and live until usage deletion';
