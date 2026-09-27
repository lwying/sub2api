package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlaintextValueDetailMigrationKeepsLegacyRowsAndUsageOwnership(t *testing.T) {
	migration, err := FS.ReadFile("254_request_audit_value_details_plaintext.sql")
	require.NoError(t, err)
	text := string(migration)

	for _, required := range []string{
		"ADD COLUMN IF NOT EXISTS storage_format TEXT NOT NULL DEFAULT 'encrypted_v1'",
		"ADD COLUMN IF NOT EXISTS plaintext_payload BYTEA",
		"ALTER COLUMN expires_at DROP NOT NULL",
		"ALTER COLUMN expires_at DROP DEFAULT",
		"DROP CONSTRAINT IF EXISTS request_audit_value_details_stored_requires_ciphertext",
		"DROP CONSTRAINT IF EXISTS request_audit_value_details_expires_after_creation",
		"storage_format IN ('encrypted_v1', 'plaintext_usage_bound')",
		"storage_format = 'encrypted_v1' AND plaintext_payload IS NULL AND expires_at IS NOT NULL AND expires_at > created_at",
		"storage_format = 'plaintext_usage_bound' AND ciphertext IS NULL AND key_version = 0 AND expires_at IS NULL",
		"state <> 'stored' OR plaintext_payload IS NOT NULL",
		"plaintext_payload IS NULL OR state = 'stored'",
		"'skipped_unsupported_protocol'",
		"ADD CONSTRAINT request_audit_value_details_reason_allowed CHECK (",
		"ADD CONSTRAINT request_audit_value_details_storage_format_allowed CHECK (",
		"ADD CONSTRAINT request_audit_value_details_storage_pairing CHECK (",
	} {
		require.Contains(t, text, required)
	}
	// These are all broadened/superset constraints over migration 253's legacy rows.
	// The runner holds the ALTER TABLE lock until commit; validation here would scan
	// every historical envelope row while reads and writes wait behind that lock.
	for _, constraint := range []string{
		"request_audit_value_details_reason_allowed",
		"request_audit_value_details_storage_format_allowed",
		"request_audit_value_details_storage_pairing",
	} {
		marker := "ADD CONSTRAINT " + constraint
		start := strings.Index(text, marker)
		require.GreaterOrEqual(t, start, 0)
		end := strings.Index(text[start:], ";")
		require.GreaterOrEqual(t, end, 0)
		require.Contains(t, text[start:start+end], "NOT VALID")
	}
	for _, forbidden := range []string{
		"ALTER TABLE request_audits", "ALTER TABLE error_diagnostic_records", "DROP TABLE usage_logs",
		"UPDATE request_audit_value_details", "SET plaintext_payload", "SET ciphertext",
	} {
		require.NotContains(t, text, forbidden)
	}
}

// TestPlaintextValueDetailMigrationRebuildsConstraintsReplaySafely 把「可重放」写成可检查的规则：
// 迁移运行器按 checksum 跳过已应用的迁移，但灾备重放／手工重放会重新执行同一份 SQL。
// 因此（1）每条 DROP CONSTRAINT 都必须带 IF EXISTS，（2）每条 ADD CONSTRAINT 之前都必须有
// 同名 DROP CONSTRAINT IF EXISTS，（3）每条 ADD CONSTRAINT 都必须带 NOT VALID，
// 否则 ADD 会在重放时报 already exists 或按行数扫描整表。
// 现状由真实 PostgreSQL 上的集成测试验证（internal/repository 的 254 迁移用例）；
// 这条单测只是防止后续编辑悄悄退回裸 ADD CONSTRAINT。
func TestPlaintextValueDetailMigrationRebuildsConstraintsReplaySafely(t *testing.T) {
	migration, err := FS.ReadFile("254_request_audit_value_details_plaintext.sql")
	require.NoError(t, err)
	text := string(migration)

	// 每条 DROP CONSTRAINT 都必须带 IF EXISTS，避免重放时因对象已不存在而失败。
	for _, match := range regexp.MustCompile(`DROP CONSTRAINT\s+([A-Za-z]+)`).FindAllStringSubmatch(text, -1) {
		require.Equalf(t, "IF", match[1],
			"DROP CONSTRAINT must use IF EXISTS for replay safety, found %q", match[0])
	}

	addPattern := regexp.MustCompile(`ADD CONSTRAINT\s+([a-z0-9_]+)`)
	added := addPattern.FindAllStringSubmatchIndex(text, -1)
	require.NotEmpty(t, added, "expected the migration to (re)build its constraints")

	for _, match := range added {
		name := text[match[2]:match[3]]

		statementEnd := strings.Index(text[match[0]:], ";")
		require.GreaterOrEqual(t, statementEnd, 0, "unterminated ADD CONSTRAINT %s", name)
		statement := text[match[0] : match[0]+statementEnd]
		require.Containsf(t, statement, "NOT VALID",
			"ADD CONSTRAINT %s must stay NOT VALID to keep the ACCESS EXCLUSIVE window O(1)", name)

		drop := "DROP CONSTRAINT IF EXISTS " + name
		dropAt := strings.Index(text, drop)
		require.GreaterOrEqualf(t, dropAt, 0,
			"ADD CONSTRAINT %s has no matching %q; replaying the migration would fail with already exists", name, drop)
		require.Lessf(t, dropAt, match[0],
			"%q must appear before the ADD CONSTRAINT %s", drop, name)
	}
}
