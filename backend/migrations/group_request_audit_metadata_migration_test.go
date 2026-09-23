package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestAuditMetadataMigrationAddsBodyFreeMetadata(t *testing.T) {
	content, err := FS.ReadFile("247_request_audit_metadata.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb")
	require.NotContains(t, strings.ToLower(sql), "prompt")
	require.NotContains(t, strings.ToLower(sql), "completion")
}
