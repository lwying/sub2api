-- Only request audits produced by the new forced reservation path are positively
-- identified as forced. NULL is the historical/unknown origin, never ordinary or
-- forced by inference. Existing rows and their retention remain unchanged.
ALTER TABLE request_audits
    ADD COLUMN IF NOT EXISTS forced_provenance TEXT;

COMMENT ON COLUMN request_audits.forced_provenance IS
    'forced for new verified forced-audit records; NULL for historical/unknown origin';

ALTER TABLE request_audits
    DROP CONSTRAINT IF EXISTS request_audits_forced_provenance_allowed;

ALTER TABLE request_audits
    ADD CONSTRAINT request_audits_forced_provenance_allowed CHECK (
        forced_provenance IS NULL OR forced_provenance = 'forced'
    ) NOT VALID;

ALTER TABLE request_audits
    VALIDATE CONSTRAINT request_audits_forced_provenance_allowed;
