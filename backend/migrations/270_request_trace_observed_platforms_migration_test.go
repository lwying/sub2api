package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 270 只给 request_traces 加一列"实际选中平台历史"，并给它数据库侧的形状护栏。
// 列必须可空：NULL 表示"从未选到任何账号"（平台未知），不能被回填或被强制要求有值。
func TestRequestTraceObservedPlatformsMigrationAddsBoundedNullableColumn(t *testing.T) {
	migration, err := FS.ReadFile("270_request_trace_observed_platforms.sql")
	require.NoError(t, err)
	sql := string(migration)

	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS observed_platforms JSONB")
	require.Contains(t, sql, "COMMENT ON COLUMN request_traces.observed_platforms")
	// 约束与函数都要可重复执行：灾难恢复会重放这个文件。
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS request_traces_observed_platforms_shape_allowed")
	require.Contains(t, sql, "ADD CONSTRAINT request_traces_observed_platforms_shape_allowed")
	require.Contains(t, sql, "CREATE OR REPLACE FUNCTION public.request_trace_observed_platforms_valid")

	// 形状：NULL 合法；否则必须是非空、有界、元素类型合法、每个元素都合法且互不重复的数组。
	require.Contains(t, sql, "WHEN platforms IS NULL THEN TRUE")
	require.Contains(t, sql, "jsonb_typeof(platforms) <> 'array' THEN FALSE")
	require.Contains(t, sql, "jsonb_array_length(platforms) NOT BETWEEN 1 AND 16")
	require.Contains(t, sql, "jsonb_typeof(element) <> 'string'")
	require.Contains(t, sql, "'^[A-Za-z0-9][A-Za-z0-9_./:+-]{0,127}$'")
	// 去重与字节上限：单条 CHECK 表达式表达不了数组去重，因此放进 IMMUTABLE 函数。
	require.Contains(t, sql, "count(DISTINCT element)")
	require.Contains(t, sql, "octet_length(platforms::text) > 2048")
	require.Contains(t, sql, "IMMUTABLE")

	// 历史行保持 NULL，不回填、不设默认值、不改既有列。
	require.NotContains(t, sql, "NOT NULL")
	require.NotContains(t, sql, "DEFAULT")
	require.NotContains(t, sql, "UPDATE request_traces")
}
