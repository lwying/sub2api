package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestAuditCaptureCompletenessMigrationAddsColumn(t *testing.T) {
	content, err := FS.ReadFile("242_request_audit_capture_completeness.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ALTER TABLE request_audits")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS capture_completeness TEXT NOT NULL DEFAULT 'complete'")
	require.Contains(t, sql, "COMMENT ON COLUMN request_audits.capture_completeness")
}
