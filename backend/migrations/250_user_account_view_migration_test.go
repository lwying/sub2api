package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 票据 05 的迁移必须在结构层保持「默认关闭」：逐用户查看开关默认 false，
// 逐用户分配集默认零行；两份结构都可重复应用（IF NOT EXISTS），
// 且不得顺手改动 accounts 等既有表的形状。
func TestUserAccountViewMigrationAddsClosedByDefaultStructures(t *testing.T) {
	sql := readExecutableMigrationSQL(t, "250_user_account_view.sql")

	// 1. 逐用户查看能力：NOT NULL + DEFAULT false，新用户与注册路径都不会隐式获得。
	require.Contains(
		t,
		sql,
		"ALTER TABLE users ADD COLUMN IF NOT EXISTS can_view_assigned_accounts BOOLEAN NOT NULL DEFAULT false",
	)
	require.NotContains(t, strings.ToUpper(sql), "DEFAULT TRUE", "查看能力不得存在任何默认开启的写法")

	// 2. 分配表：默认零行，靠主键 (user_id, account_id) 保证同一对关系只有一行。
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS user_visible_accounts")
	require.Contains(t, sql, "PRIMARY KEY (user_id, account_id)")

	// 3. 物理删除由数据库级联清理；软删除（deleted_at）留给查询条件，不依赖级联。
	require.Contains(t, sql, "user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE")
	require.Contains(t, sql, "account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE")

	// 4. granted_by 只用于追责：可为空，且刻意不建外键（管理员删除不清理分配行）。
	grantedByColumn := userAccountViewColumnDefinition(t, sql, "granted_by")
	require.Contains(t, grantedByColumn, "granted_by BIGINT")
	require.NotContains(t, grantedByColumn, "NOT NULL", "追责字段允许为空（运维脚本写入无授权管理员）")
	require.NotContains(t, grantedByColumn, "REFERENCES", "granted_by 不得引用 users，否则管理员删除会连带清理分配行")

	// 5. 账号侧反查索引（账号被禁用／删除时的可见性校验）。
	require.Contains(
		t,
		sql,
		"CREATE INDEX IF NOT EXISTS user_visible_accounts_account_id_idx ON user_visible_accounts (account_id)",
	)
}

// 迁移只允许新增本票的两份结构，不得改动既有账号表或删除任何数据。
func TestUserAccountViewMigrationDoesNotTouchExistingAccountSchema(t *testing.T) {
	sql := readExecutableMigrationSQL(t, "250_user_account_view.sql")

	upper := strings.ToUpper(sql)
	require.NotContains(t, upper, "ALTER TABLE ACCOUNTS", "本票不修改 accounts 表")
	require.NotContains(t, upper, "DROP TABLE", "增量迁移不得删除表")
	require.NotContains(t, upper, "DROP COLUMN", "增量迁移不得删除列")
	require.NotContains(t, upper, "DELETE FROM", "迁移不得清理既有数据")
}

// 回滚路径必须写清楚，且只作为注释存在（上面已断言可执行 SQL 里没有 DROP）。
func TestUserAccountViewMigrationDocumentsRollback(t *testing.T) {
	content, err := FS.ReadFile("250_user_account_view.sql")
	require.NoError(t, err)

	raw := string(content)
	require.Contains(t, raw, "DROP TABLE user_visible_accounts")
	require.Contains(t, raw, "DROP COLUMN can_view_assigned_accounts")
}

// readExecutableMigrationSQL 读取迁移并去掉 `--` 注释行，再归一化空白，
// 让断言只针对真正会被执行的语句（回滚说明写在注释里，不应被当成语句）。
func readExecutableMigrationSQL(t *testing.T, name string) string {
	t.Helper()

	content, err := FS.ReadFile(name)
	require.NoError(t, err)

	var builder strings.Builder
	for _, line := range strings.Split(string(content), "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		_, _ = builder.WriteString(line)
		_, _ = builder.WriteString("\n")
	}

	return strings.Join(strings.Fields(builder.String()), " ")
}

// userAccountViewColumnDefinition 取出某一列从列名到下一个逗号为止的定义片段。
func userAccountViewColumnDefinition(t *testing.T, sql, column string) string {
	t.Helper()

	start := strings.Index(sql, column+" BIGINT")
	require.NotEqual(t, -1, start, "expected column %s in migration", column)

	rest := sql[start:]
	if end := strings.Index(rest, ","); end >= 0 {
		return rest[:end]
	}
	return rest
}
