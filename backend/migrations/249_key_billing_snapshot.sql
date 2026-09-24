ALTER TABLE api_keys
    ADD COLUMN IF NOT EXISTS billing_binding_revision BIGINT NOT NULL DEFAULT 1;

CREATE OR REPLACE FUNCTION bump_api_key_billing_binding_revision()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.user_id IS DISTINCT FROM OLD.user_id
       OR NEW.group_id IS DISTINCT FROM OLD.group_id THEN
        NEW.billing_binding_revision := OLD.billing_binding_revision + 1;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS api_keys_billing_binding_revision ON api_keys;
CREATE TRIGGER api_keys_billing_binding_revision
    BEFORE UPDATE OF user_id, group_id ON api_keys
    FOR EACH ROW
    EXECUTE FUNCTION bump_api_key_billing_binding_revision();

CREATE TABLE IF NOT EXISTS key_billing_snapshots (
    api_key_id BIGINT PRIMARY KEY REFERENCES api_keys(id) ON DELETE CASCADE,
    owner_user_id BIGINT NOT NULL,
    group_id BIGINT NOT NULL,
    binding_revision BIGINT NOT NULL,
    generation TEXT NOT NULL,
    payload TEXT,
    observed_at TIMESTAMPTZ,
    refresh_owner TEXT,
    refresh_lease_until TIMESTAMPTZ,
    retry_after TIMESTAMPTZ,
    failure_code TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT key_billing_snapshots_payload_time_pair CHECK (
        (payload IS NULL AND observed_at IS NULL) OR
        (payload IS NOT NULL AND observed_at IS NOT NULL)
    )
);

CREATE INDEX IF NOT EXISTS key_billing_snapshots_observed_at_idx
    ON key_billing_snapshots (observed_at);
