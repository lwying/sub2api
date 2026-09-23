package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestAuditMigrationCascadesWithUsageLogs(t *testing.T) {
	content, err := FS.ReadFile("240_request_audit.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS request_audits")
	require.Contains(t, sql, "usage_log_id")
	require.Contains(t, sql, "REFERENCES usage_logs(id)")
	require.Contains(t, sql, "ON DELETE CASCADE")
	require.Contains(t, sql, "UNIQUE")
}
