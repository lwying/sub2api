package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestAuditEventsMigrationAddsJSONColumn(t *testing.T) {
	content, err := FS.ReadFile("241_request_audit_events.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ALTER TABLE request_audits")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS events JSONB NOT NULL DEFAULT '[]'::jsonb")
	require.Contains(t, sql, "COMMENT ON COLUMN request_audits.events")
	require.Contains(t, sql, "never stores event text or deltas")
}
