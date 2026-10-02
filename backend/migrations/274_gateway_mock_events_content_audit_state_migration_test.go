package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 274 只给 gateway_mock_events 增加一列"内容审计是否执行"的有界状态。
// 列必须 NOT NULL 且有默认值 'unknown'：旧记录获得稳定的 unknown，写入方不传该列时也由
// 默认值补齐，因此不会被误当成 skipped_local_mock（"未记录"不能被说成"已确认未执行审计"）。
func TestGatewayMockEventContentAuditStateMigrationAddsBoundedDefaultColumn(t *testing.T) {
	migration, err := FS.ReadFile("274_gateway_mock_events_content_audit_state.sql")
	require.NoError(t, err)
	sql := string(migration)

	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS content_audit_state TEXT NOT NULL DEFAULT 'unknown'")
	require.Contains(t, sql, "COMMENT ON COLUMN gateway_mock_events.content_audit_state")

	// 约束可重复执行：灾难恢复会重放这个文件。
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS gateway_mock_events_content_audit_state_allowed")
	require.Contains(t, sql, "ADD CONSTRAINT gateway_mock_events_content_audit_state_allowed")
	// 闭集恰好是两个允许值；迁移不得回填历史行为 skipped。
	require.Contains(t, sql, "content_audit_state IN ('unknown', 'skipped_local_mock')")
	require.NotContains(t, sql, "UPDATE gateway_mock_events")
	require.NotContains(t, sql, "skipped_local_mock' WHERE")
}
