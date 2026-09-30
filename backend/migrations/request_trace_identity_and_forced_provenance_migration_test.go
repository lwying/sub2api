package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestForcedAuditProvenanceMigrationIsAdditiveAndHistoricalUnknown(t *testing.T) {
	migration, err := FS.ReadFile("271_request_audit_forced_provenance.sql")
	require.NoError(t, err)
	sql := string(migration)
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS forced_provenance TEXT")
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS request_audits_forced_provenance_allowed")
	require.Contains(t, sql, "forced_provenance IS NULL OR forced_provenance = 'forced'")
	require.NotContains(t, sql, "UPDATE request_audits")
	require.NotContains(t, sql, "DELETE FROM request_audits")
}

func TestTraceIdentityMigrationPreservesHistoricalNullsAndCreatesOnlineIndexes(t *testing.T) {
	migration, err := FS.ReadFile("272_request_trace_identity.sql")
	require.NoError(t, err)
	sql := string(migration)
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS user_id BIGINT")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS api_key_id BIGINT")
	require.NotContains(t, sql, "NOT NULL")
	require.NotContains(t, sql, "UPDATE request_traces")
	indexes, err := FS.ReadFile("273_request_trace_identity_indexes_notx.sql")
	require.NoError(t, err)
	require.Contains(t, string(indexes), "CREATE INDEX CONCURRENTLY IF NOT EXISTS request_traces_user_created_idx")
	require.Contains(t, string(indexes), "CREATE INDEX CONCURRENTLY IF NOT EXISTS request_traces_api_key_created_idx")
}
