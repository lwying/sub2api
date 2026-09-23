ALTER TABLE request_audits
    ADD COLUMN IF NOT EXISTS capture_completeness TEXT NOT NULL DEFAULT 'complete';

COMMENT ON COLUMN request_audits.capture_completeness IS
    '采集完整性: complete, truncated, incomplete, write_failed, not_captured; never stores model body';
