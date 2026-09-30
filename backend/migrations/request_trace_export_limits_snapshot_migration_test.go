package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 269 只加一列：任务创建时固定的资源上限快照，并给它数据库侧的形状与取值
// 护栏。列必须可空——升级前创建的任务没有快照，执行时退回当时的生效配置，
// 不能被回填或被强制要求有值。
func TestRequestTraceExportLimitsSnapshotMigrationAddsBoundedNullableColumn(t *testing.T) {
	migration, err := FS.ReadFile("269_request_trace_export_limits_snapshot.sql")
	require.NoError(t, err)
	sql := string(migration)

	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS limits_snapshot JSONB")
	// 约束与 COMMENT 都要可重复执行：灾难恢复会重放这个文件。
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS request_trace_exports_limits_snapshot_shape_allowed")
	require.Contains(t, sql, "ADD CONSTRAINT request_trace_exports_limits_snapshot_shape_allowed")
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS request_trace_exports_limits_snapshot_bounds_allowed")
	require.Contains(t, sql, "ADD CONSTRAINT request_trace_exports_limits_snapshot_bounds_allowed")

	// 形状：只接受对象、封闭键集、有界文本。
	require.Contains(t, sql, "jsonb_typeof(limits_snapshot) = 'object'")
	require.Contains(t, sql, "= '{}'::jsonb")
	for _, key := range []string{
		"'max_rows'", "'max_bytes'", "'max_runtime_seconds'",
		"'max_shard_rows'", "'max_shard_bytes'", "'configured'",
	} {
		require.Contains(t, sql, key, "the closed key set must list %s", key)
	}
	require.Contains(t, sql, "jsonb_typeof(limits_snapshot -> 'configured') = 'boolean'")
	require.Contains(t, sql, "octet_length(limits_snapshot::text) <= 512")

	// 取值：有限、落在服务端允许区间内，且单片上限不超过整任务上限。
	require.Contains(t, sql, "BETWEEN 100 AND 5000000")
	require.Contains(t, sql, "BETWEEN 1048576 AND 68719476736")
	require.Contains(t, sql, "BETWEEN 30 AND 21600")
	require.Contains(t, sql, "BETWEEN 10 AND 5000000")
	require.Contains(t, sql, "<= (limits_snapshot ->> 'max_rows')::numeric")
	require.Contains(t, sql, "<= (limits_snapshot ->> 'max_bytes')::numeric")

	// 历史行保持 NULL，不回填、不设默认值。
	require.NotContains(t, sql, "NOT NULL")
	require.NotContains(t, sql, "DEFAULT")
	require.NotContains(t, sql, "UPDATE request_trace_exports")
}
