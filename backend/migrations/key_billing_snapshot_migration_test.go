package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeyBillingSnapshotMigrationUsesSQLBindingRevisionAndByteStablePayload(t *testing.T) {
	migration, err := FS.ReadFile("249_key_billing_snapshot.sql")
	require.NoError(t, err)
	sql := string(migration)
	require.Contains(t, sql, "billing_binding_revision BIGINT NOT NULL DEFAULT 1")
	require.Contains(t, sql, "NEW.user_id IS DISTINCT FROM OLD.user_id")
	require.Contains(t, sql, "NEW.group_id IS DISTINCT FROM OLD.group_id")
	require.Contains(t, sql, "payload TEXT")
	require.Contains(t, sql, "refresh_lease_until TIMESTAMPTZ")
	require.Contains(t, sql, "retry_after TIMESTAMPTZ")
}
