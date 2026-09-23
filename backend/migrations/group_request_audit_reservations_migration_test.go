package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestAuditReservationsMigrationCreatesTemporaryMetadataTable(t *testing.T) {
	content, err := FS.ReadFile("245_request_audit_reservations.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS request_audit_reservations")
	require.Contains(t, sql, "logical_key TEXT NOT NULL UNIQUE")
	require.Contains(t, sql, "headers JSONB NOT NULL DEFAULT '{}'::jsonb")
	require.Contains(t, sql, "attempts JSONB NOT NULL DEFAULT '[]'::jsonb")
	require.Contains(t, sql, "usage_log_id BIGINT UNIQUE REFERENCES usage_logs(id) ON DELETE CASCADE")
	require.Contains(t, sql, "capture_completeness TEXT NOT NULL DEFAULT 'complete'")
	require.Contains(t, sql, "CREATE INDEX IF NOT EXISTS idx_request_audit_reservations_expires_at")
}
