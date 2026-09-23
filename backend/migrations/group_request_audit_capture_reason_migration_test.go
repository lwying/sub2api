package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestAuditCaptureReasonMigrationAddsColumn(t *testing.T) {
	content, err := FS.ReadFile("244_request_audit_capture_reason.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ALTER TABLE request_audits")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS capture_reason TEXT NOT NULL DEFAULT ''")
	require.Contains(t, sql, "COMMENT ON COLUMN request_audits.capture_reason")
}
