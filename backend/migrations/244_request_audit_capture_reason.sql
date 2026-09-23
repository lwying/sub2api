-- 244_request_audit_capture_reason.sql
-- 为未覆盖入口保存稳定原因码；不回填历史记录。

ALTER TABLE request_audits
    ADD COLUMN IF NOT EXISTS capture_reason TEXT NOT NULL DEFAULT '';

COMMENT ON COLUMN request_audits.capture_reason IS
    'Capture reason code; phase1_uncovered means this usage path is outside first-phase audit coverage';
