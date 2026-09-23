ALTER TABLE request_audits
    ADD COLUMN IF NOT EXISTS fingerprint_salt BYTEA;

COMMENT ON COLUMN request_audits.fingerprint_salt IS
    'Internal per-record random salt for user-isolated keyed request and event fingerprints; never expose in audit DTOs';
