package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本测试固定 usage-owned 请求审计值明细旁路的收窄契约：
// 它必须是**独立旁路表**（不把值塞回 request_audits），只以密文列保存值，
// 且有独立的到期时刻、状态与原因码。
func TestRequestAuditValueDetailsMigrationPinsNarrowContract(t *testing.T) {
	migration, err := FS.ReadFile("253_request_audit_value_details.sql")
	require.NoError(t, err)
	sql := string(migration)

	// 只新建自己的旁路表；不重建、不改写既有审计表与诊断表。
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS request_audit_value_details")
	for _, forbidden := range []string{
		"DROP TABLE usage_logs",
		"DROP TABLE request_audits",
		"DROP TABLE error_diagnostic_records",
		"ALTER TABLE request_audits",
		"ALTER TABLE error_diagnostic_records",
		"INSERT INTO request_audits",
	} {
		require.NotContains(t, sql, forbidden, "不得重建或改写既有表")
	}
	// 长期存活的 request_audits 不得出现值列。
	for _, forbidden := range []string{
		"request_audits ADD COLUMN",
		"request_audits.value",
		"header_values TEXT",
		"plaintext BYTEA",
		"model_plaintext",
	} {
		require.NotContains(t, sql, forbidden, "值明细不得改写或扩宽既有审计表")
	}
	// 明文列不含模型名。
	require.NotContains(t, sql, "    model TEXT")

	// 值只以密文列存在，并带有自己的到期时刻、状态、原因与密钥代。
	for _, fragment := range []string{
		"usage_log_id BIGINT NOT NULL UNIQUE REFERENCES usage_logs(id) ON DELETE CASCADE",
		"state TEXT NOT NULL DEFAULT 'not_observed'",
		"reason TEXT NOT NULL DEFAULT 'not_observed'",
		"ciphertext BYTEA",
		"key_version INTEGER NOT NULL DEFAULT 0",
		"payload_bytes INTEGER NOT NULL DEFAULT 0",
		"entry_count INTEGER NOT NULL DEFAULT 0",
		"attempt_count INTEGER NOT NULL DEFAULT 0",
		"expires_at TIMESTAMPTZ NOT NULL",
		"created_at TIMESTAMPTZ NOT NULL",
	} {
		require.Contains(t, sql, fragment, "迁移必须提供该受限列")
	}

	// 受限枚举：状态与原因都是闭集，且把「不在范围」「开关关闭」「缺密钥」「校验不合格」
	// 「尝试过多」分开，不允许合并成一个含糊取值。
	for _, fragment := range []string{
		"state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')",
		"'skipped_out_of_scope'",
		"'skipped_value_retention_disabled'",
		"'skipped_encryption_unavailable'",
		"'skipped_invalid_values'",
		"'skipped_too_many_attempts'",
	} {
		require.Contains(t, sql, fragment, "迁移必须固定该白名单取值")
	}
	require.NotContains(t, sql, "'unknown'")

	// 密文与「已留存」必须成对：自称 stored 必须有密文；没留存的行不得带密文。
	for _, fragment := range []string{
		"request_audit_value_details_stored_requires_ciphertext",
		"state <> 'stored' OR ciphertext IS NOT NULL",
		"request_audit_value_details_ciphertext_requires_storage",
		"ciphertext IS NULL OR state IN ('stored', 'expired', 'purged')",
	} {
		require.Contains(t, sql, fragment, "迁移必须保留密文配对约束")
	}

	// 计数、体量与状态的边界，以及到期晚于创建。
	for _, fragment := range []string{
		"request_audit_value_details_attempt_count_range",
		"attempt_count >= 0 AND attempt_count <= 16",
		"request_audit_value_details_entry_count_range",
		"entry_count >= 0 AND entry_count <= 256",
		"request_audit_value_details_payload_bytes_nonnegative",
		"request_audit_value_details_client_status_range",
		"request_audit_value_details_expires_after_creation",
		"expires_at > created_at",
	} {
		require.Contains(t, sql, fragment, "迁移必须保留该约束")
	}

	// 清理索引与幂等的约束添加方式（可重复执行）。
	require.Contains(t, sql, "request_audit_value_details_expires_at_idx")
	require.Contains(t, sql, "ON request_audit_value_details (expires_at)")
	require.Contains(t, sql, "WHERE ciphertext IS NOT NULL")
	require.Contains(t, sql, "ADD CONSTRAINT")
	require.Contains(t, sql, "SELECT 1 FROM pg_constraint")

	// 到期语义：API 在到期时刻拒绝读取；在线主库上的物理清除是周期任务
	// （约 10 分钟一轮、每轮至多 500 行），不承诺确切时刻，也不把副本／备份／PITR
	// 的到期不可恢复写成承诺。
	require.Contains(t, sql, "立即拒绝读取值")
	require.Contains(t, sql, "每轮至多 500 行")
	require.Contains(t, sql, "不承诺到期即已物理删除")
	require.Contains(t, sql, "也不承诺物理清除的确切时刻")
	require.Contains(t, sql, "本迁移不声称这些存储层的到期不可恢复")
	require.Contains(t, sql, "也不构成任何合规删除声明")

	// 明确声明不保存正文与凭据，并与 429 头值例外互不影响。
	require.Contains(t, sql, "明确**不**保存")
	require.Contains(t, sql, "模型正文")
	require.Contains(t, sql, "不存在明文回退")
	require.Contains(t, sql, "互不影响")
	require.Contains(t, sql, "error_diagnostic_records")

	// 写入必须先有 request_audits 行：迁移层把这条不变量写进注释，
	// 实现由写入语句的 EXISTS 守卫保证（见 repository 层）。
	require.Contains(t, sql, "request_audits 行**都已存在**")
}

// 240 必须保持原样：新能力不得靠改写既有迁移来放宽既有契约。
func TestRequestAuditMigrationUnchangedByValueDetails(t *testing.T) {
	migration, err := FS.ReadFile("240_request_audit.sql")
	require.NoError(t, err)
	sql := string(migration)

	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS request_audits")
	require.NotContains(t, sql, "request_audit_value_details")
	for _, forbidden := range []string{"ciphertext", "expires_at", "key_version", "payload_bytes"} {
		require.NotContains(t, sql, forbidden, "既有请求审计表不得出现值明细列")
		require.False(t, strings.Contains(sql, forbidden))
	}
}
