ALTER TABLE request_audits
    ADD COLUMN IF NOT EXISTS request_fingerprint TEXT,
    ADD COLUMN IF NOT EXISTS fingerprint_key_version INTEGER NOT NULL DEFAULT 0;

COMMENT ON COLUMN request_audits.request_fingerprint IS
    'Keyed audit fingerprint only; never contains model body or the HMAC key';

COMMENT ON COLUMN request_audits.fingerprint_key_version IS
    'Version of the keyed audit fingerprint secret; zero means legacy row';
