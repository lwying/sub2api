package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupThinkingDisabledStrictMigration(t *testing.T) {
	content, err := FS.ReadFile("239_group_thinking_disabled_strict.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql,
		"ADD COLUMN IF NOT EXISTS thinking_disabled_strict BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "COMMENT ON COLUMN groups.thinking_disabled_strict")
}
