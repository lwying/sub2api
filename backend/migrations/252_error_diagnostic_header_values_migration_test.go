package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 本测试固定 429 头值留存列的收窄契约文本：
// 它是既有错误诊断表的**增量**列，不得改动 251 已发布的表结构与枚举，
// 也不得放宽成「保存原始请求头」「保存凭据头的值」或把物理清除写成有时刻承诺的删除。
func TestErrorDiagnosticHeaderValuesMigrationPinsNarrowContract(t *testing.T) {
	migration, err := FS.ReadFile("252_error_diagnostic_header_values.sql")
	require.NoError(t, err)
	sql := string(migration)

	// 增量迁移：只 ALTER 既有诊断表，不重建表、不动 usage-owned 请求审计。
	require.Contains(t, sql, "ALTER TABLE error_diagnostic_records")
	require.NotContains(t, sql, "CREATE TABLE")
	require.NotContains(t, sql, "DROP TABLE")
	require.NotContains(t, sql, "ALTER TABLE request_audits")
	require.NotContains(t, sql, "ADD COLUMN IF NOT EXISTS body_")

	// 头值只以密文列存在，且有自己的到期时刻与状态／原因。
	for _, fragment := range []string{
		"header_state TEXT NOT NULL DEFAULT 'not_observed'",
		"header_reason TEXT NOT NULL DEFAULT 'not_observed'",
		"header_ciphertext BYTEA",
		"header_key_version INTEGER NOT NULL DEFAULT 0",
		"header_bytes INTEGER NOT NULL DEFAULT 0",
		"header_entry_count INTEGER NOT NULL DEFAULT 0",
		"header_expires_at TIMESTAMPTZ",
	} {
		require.Contains(t, sql, fragment, "迁移必须提供该受限列")
	}
	require.NotContains(t, sql, "header_plaintext")
	require.NotContains(t, sql, "header_values TEXT")
	require.NotContains(t, sql, "request_headers")

	// 受限枚举：状态与原因都必须是闭集，且把「开关关闭」「缺密钥」「校验不合格」分开。
	for _, fragment := range []string{
		"header_state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')",
		"'skipped_header_retention_disabled'",
		"'skipped_encryption_unavailable'",
		"'skipped_invalid_values'",
	} {
		require.Contains(t, sql, fragment, "迁移必须固定该白名单取值")
	}
	// 「不在范围」按领域词汇是未采集（not_observed），不得在这里多出一个 skipped_out_of_scope：
	// 那会把「未采集」写成一种跳过，与「开关关闭／缺密钥」的语义混在一起。
	require.NotContains(t, sql, "'skipped_out_of_scope'")
	// 「未采集」「没要求」「要求了但做不到」不能合并成同一个含糊取值。
	require.NotContains(t, sql, "'unknown'")

	// 计数与体量的边界，以及密文／到期成对约束。
	for _, fragment := range []string{
		"error_diagnostic_records_header_ciphertext_pairing",
		"error_diagnostic_records_header_entry_count_range",
		"header_entry_count <= 64",
		"error_diagnostic_records_header_bytes_nonnegative",
		"error_diagnostic_records_header_metadata_outlives_headers",
		"metadata_expires_at > header_expires_at",
	} {
		require.Contains(t, sql, fragment, "迁移必须保留该约束")
	}

	// 清理索引与幂等的约束添加方式（可重复执行）。
	require.Contains(t, sql, "error_diagnostic_records_header_expires_at_idx")
	require.Contains(t, sql, "ON error_diagnostic_records (header_expires_at)")
	require.Contains(t, sql, "WHERE header_ciphertext IS NOT NULL")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS")
	require.Contains(t, sql, "SELECT 1 FROM pg_constraint")

	// 到期语义：API 在到期时刻拒绝读取；在线主库上的物理清除是周期任务，不承诺确切时刻；
	// 也不把副本／备份／PITR 的到期不可恢复写成承诺。
	require.Contains(t, sql, "立即拒绝读取头值")
	require.Contains(t, sql, "每轮有批量上限")
	require.Contains(t, sql, "不承诺到期即已物理删除")
	require.Contains(t, sql, "也不承诺物理清除的确切时刻")
	require.Contains(t, sql, "本迁移不声称这些存储层的到期不可恢复")
	require.Contains(t, sql, "也不构成任何合规删除声明")

	// 头值例外必须显式写成「与正文留存互不影响」，避免把两套开关耦合成一套。
	require.Contains(t, sql, "互不影响")
	require.Contains(t, sql, "恰好收到 HTTP 429")
}

// 251 必须保持原样：新能力不得靠改写既有迁移来放宽既有契约。
func TestErrorDiagnosticRecordsMigrationUnchangedByHeaderValues(t *testing.T) {
	migration, err := FS.ReadFile("251_error_diagnostic_records.sql")
	require.NoError(t, err)
	sql := string(migration)

	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS error_diagnostic_records")
	require.NotContains(t, sql, "header_state")
	require.NotContains(t, sql, "header_ciphertext")
	require.NotContains(t, sql, "ALTER TABLE error_diagnostic_records")
}
