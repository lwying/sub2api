package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 票据 08／09：新明文列的清理状态。
//
// 迁移必须只放宽状态集合，不得动原因码集合、不得新增列、不得触碰被冻结的旧迁移
// （251／252／253／255 的其它约束与索引）。
func TestErrorDiagnosticPlaintextStatesMigration(t *testing.T) {
	migration, err := FS.ReadFile("256_error_diagnostic_plaintext_states.sql")
	require.NoError(t, err)
	text := string(migration)
	for _, required := range []string{
		"plain_body_state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')",
		"plain_header_state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')",
		"'plain_body_retained'",
		"'plain_header_retained'",
		"DROP CONSTRAINT error_diagnostic_plain_body_allowed",
		"DROP CONSTRAINT error_diagnostic_plain_header_allowed",
	} {
		require.Contains(t, text, required)
	}
	for _, forbidden := range []string{
		"ADD COLUMN",
		"DROP TABLE",
		"ALTER TABLE usage_logs",
		"ALTER TABLE request_audits",
		"DROP INDEX",
	} {
		require.NotContains(t, text, forbidden)
	}
}

// TestErrorDiagnosticPlaintextStatesMigrationRelaxesWithoutTableScan 固定 256 的锁姿态：
// 这是一次**放宽**（新状态集合是 255 三值集合的超集），被替换的旧约束已保证所有已存行
// 满足更窄的旧集合，因此已存行必然满足新集合。
//
// 迁移运行器把整个文件放在一个事务里跑，DROP／ADD 拿到的 ACCESS EXCLUSIVE 会一直持有到提交；
// 如果 ADD CONSTRAINT 走默认的**带校验**路径，就会在持锁期间全表扫描，把加锁窗口从 O(1) 放大到
// O(行数)，阻塞诊断表的写入与读取。因此两条放宽后的约束都必须用 NOT VALID 重建：
// 既不扫描已存行，又继续对之后的 INSERT／UPDATE 生效。
func TestErrorDiagnosticPlaintextStatesMigrationRelaxesWithoutTableScan(t *testing.T) {
	migration, err := FS.ReadFile("256_error_diagnostic_plaintext_states.sql")
	require.NoError(t, err)
	sql := string(migration)

	cases := []struct {
		constraint string
		states     string
	}{
		{"error_diagnostic_plain_body_allowed", "plain_body_state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')"},
		{"error_diagnostic_plain_header_allowed", "plain_header_state IN ('not_observed', 'stored', 'skipped', 'expired', 'purged')"},
	}
	for _, tc := range cases {
		statement := addedConstraintStatement(t, sql, tc.constraint)
		require.Contains(t, statement, tc.states, "%s must keep the relaxed state set", tc.constraint)
		require.Contains(t, statement, "NOT VALID",
			"%s must be re-added NOT VALID so the migration does not scan the whole table while holding ACCESS EXCLUSIVE",
			tc.constraint)
	}

	// 恰好一条 ADD：不得既留一条带校验的旧写法又补一条 NOT VALID。
	require.Equal(t, 1, strings.Count(sql, "ADD CONSTRAINT error_diagnostic_plain_body_allowed"))
	require.Equal(t, 1, strings.Count(sql, "ADD CONSTRAINT error_diagnostic_plain_header_allowed"))

	// 放宽不需要事后校验：已存行必然满足超集。若有人加上 VALIDATE CONSTRAINT，
	// 就会把一次纯目录变更重新变成持锁全表扫描。
	require.NotContains(t, sql, "VALIDATE CONSTRAINT")
}

// addedConstraintStatement 返回 "ADD CONSTRAINT <name>" 起到该语句分号为止的片段。
func addedConstraintStatement(t *testing.T, sql, constraint string) string {
	t.Helper()
	marker := "ADD CONSTRAINT " + constraint
	start := strings.Index(sql, marker)
	require.GreaterOrEqual(t, start, 0, "migration must add constraint %s", constraint)
	rest := sql[start:]
	end := strings.Index(rest, ";")
	require.GreaterOrEqual(t, end, 0, "constraint %s must be terminated by a semicolon", constraint)
	return rest[:end]
}
