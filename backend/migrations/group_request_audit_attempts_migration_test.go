package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestAuditAttemptsMigrationAddsJSONColumn(t *testing.T) {
	content, err := FS.ReadFile("243_request_audit_attempts.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ALTER TABLE request_audits")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS attempts JSONB NOT NULL DEFAULT '[]'::jsonb")
	require.Contains(t, sql, "COMMENT ON COLUMN request_audits.attempts")
	require.Contains(t, sql, "never stores model body")
}
