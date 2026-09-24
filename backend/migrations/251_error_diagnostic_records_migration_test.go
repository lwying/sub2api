package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本测试固定错误诊断记录表的收窄契约文本，
// 防止后续改动误删约束、放宽枚举、重新引入可枚举主键或身份字段，
// 或把留存声明写成「副本／备份也到期不可恢复」。
func TestErrorDiagnosticRecordsMigrationPinsNarrowContract(t *testing.T) {
	migration, err := FS.ReadFile("251_error_diagnostic_records.sql")
	require.NoError(t, err)
	sql := string(migration)

	// 独立表，不是往 request_audits 里塞正文列。
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS error_diagnostic_records")
	require.NotContains(t, sql, "ALTER TABLE request_audits")

	// 主键是应用生成的不可猜 opaque 标识，并强制其形状；没有可枚举序列号。
	require.Contains(t, sql, "diagnostic_id       TEXT PRIMARY KEY")
	require.Contains(t, sql, "diagnostic_id ~ '^[0-9a-f]{32}$'")
	require.NotContains(t, sql, "BIGSERIAL")

	// 无 usage 也可成立，且 usage 删除不级联带走诊断。
	require.Contains(t, sql, "usage_log_id        BIGINT REFERENCES usage_logs(id) ON DELETE SET NULL")
	require.NotContains(t, sql, "ON DELETE CASCADE")

	// 受限枚举与边界。
	for _, fragment := range []string{
		"protocol IN ('messages', 'chat_completions', 'responses')",
		"stage = 'wire'",
		"upstream_status BETWEEN 400 AND 599",
		"attempt_index >= 0 AND attempt_index <= 1000",
		"body_state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')",
		"'skipped_not_text_json'",
		"'skipped_too_large'",
		"'skipped_attachment'",
		"'skipped_known_credential'",
		"'skipped_incomplete_read'",
		"'skipped_encryption_unavailable'",
		"'skipped_body_retention_disabled'",
	} {
		require.Contains(t, sql, fragment, "迁移必须固定该白名单取值")
	}

	// 正文只能以密文列存在，且在应用层不做明文回退。
	require.Contains(t, sql, "body_ciphertext     BYTEA")
	require.NotContains(t, sql, "body_plaintext")
	require.NotContains(t, sql, "body TEXT")

	// 不保存身份／自由字段。
	for _, forbidden := range []string{
		"account_id",
		"request_fingerprint",
		"error_message",
		"raw_url",
		"request_headers",
	} {
		require.NotContains(t, sql, forbidden, "管理端契约不含该字段")
	}

	// 清理所需索引：第 7 天正文、第 30 天整行、usage 关联、无 usage 入口。
	for _, index := range []string{
		"error_diagnostic_records_body_expires_at_idx",
		"error_diagnostic_records_metadata_expires_at_idx",
		"error_diagnostic_records_usage_log_id_idx",
		"error_diagnostic_records_protocol_created_at_idx",
	} {
		require.Contains(t, sql, index)
	}

	// 到期语义必须区分「API 立即拒绝读取」与「在线主库上的周期性物理清除」，
	// 且后者不得被写成有确切时刻的删除承诺。
	require.Contains(t, sql, "API 立即拒绝读取正文")
	require.Contains(t, sql, "API 立即拒绝列表／详情")
	require.Contains(t, sql, "在线主库上的物理清除是周期任务")
	require.Contains(t, sql, "每轮有批量上限")
	require.Contains(t, sql, "不承诺")
	require.Contains(t, sql, "也不承诺物理清除的确切时刻")
	require.Contains(t, sql, "积压量与最老超期时长由清理服务上报")

	// 显式排除副本／备份／PITR 的留存承诺与合规删除声明。
	require.Contains(t, sql, "本迁移不声称这些存储层的到期不可恢复")
	require.Contains(t, sql, "也不构成任何合规删除声明")

	// 不能出现「已满足／保证」这类完成式声明，也不能复制旧规格的备份不可恢复措辞。
	for _, forbidden := range []string{
		"已满足", "保证", "合规删除已", "backup erasure", "第 7 天删除", "第 30 天删除",
	} {
		require.NotContains(t, sql, forbidden)
	}
	// 更强的检查：文件里每一次「承诺」都必须是被否定的（前面紧跟「不」）。
	// 这样即使措辞被改写，也无法悄悄变成对物理删除时刻的肯定承诺；
	// 同时允许「不承诺…」这种免责表述继续存在。
	require.NotRegexp(t, `[^不]承诺`, sql, "任何「承诺」都必须是否定形式")
	require.False(t, strings.Contains(sql, "副本和备份中均不可恢复"),
		"不得复制旧规格里「副本和备份均不可恢复」的声明")
}
