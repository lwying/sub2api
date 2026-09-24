//go:build integration

package repository

// 票据 05「普通用户查看已分配账号的脱敏只读视图」的迁移与 SQL 层验收。
//
// 本文件只覆盖票据 05 验收标准中落在真实 PostgreSQL 上的部分：
//   - users.can_view_assigned_accounts 默认 false（新用户／注册路径都不得隐式获得）；
//   - user_visible_accounts 默认零行，(user_id, account_id) 唯一；
//   - 物理删除 user／account 由数据库 ON DELETE CASCADE 清理分配行；
//   - 软删除（users.deleted_at／accounts.deleted_at）不依赖级联，必须由查询条件处理；
//   - 可见范围：已分配 且 用户具备查看能力且未软删除 且 账号未软删除 且 账号未被手动禁用。
//
// 迁移 250 是本票的结构事实来源：Ent schema 已带 entsql.Cascade，ent/migrate/schema.go
// 与迁移 250 对 user_visible_accounts 的两条外键都声明 ON DELETE CASCADE，
// 复合主键与账号侧索引名以编号迁移为准。
//
// 关于可见范围：范围断言直接走真实仓储 userVisibleAccountRepository.ListVisibleAccounts／
// GetVisibleAccount（不再用测试里手写的范围 SQL），预期集合由 fixture 给定。
// naiveAccountSchedulerStatusFilterSQL 只作为负面对照，故意写成「直接沿用管理员调度口径」的
// 错误做法，证明可见范围不能由此推导；它不复制仓储实现。

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// naiveAccountSchedulerStatusFilterSQL 是负面对照，不是实现：它模拟「直接沿用管理员调度口径」
// 的错误做法，只保留 status='active' AND schedulable 的账号。它会把 error／expired 账号错误地隐藏，
// 用于证明可见范围不能这样推导（仓储实现走的是状态口径，与调度口径无关）。
const naiveAccountSchedulerStatusFilterSQL = `
SELECT COALESCE(array_agg(uva.account_id ORDER BY uva.account_id), '{}'::bigint[])
FROM user_visible_accounts uva
JOIN accounts a ON a.id = uva.account_id
WHERE uva.user_id = $1
  AND a.deleted_at IS NULL
  AND a.status = 'active'
  AND a.schedulable IS TRUE
`

func TestUserAccountViewMigrationSchemaShape(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)

	// 1. 逐用户查看能力默认关闭且不可为空。
	requireColumn(t, tx, "users", "can_view_assigned_accounts", "boolean", 0, false)
	requireColumnDefaultContains(t, tx, "users", "can_view_assigned_accounts", "false")

	// 2. 分配表存在，列形状与迁移 250 一致。
	var regclass sql.NullString
	require.NoError(t, scanSingleRow(ctx, tx,
		"SELECT to_regclass('public.user_visible_accounts')", nil, &regclass))
	require.True(t, regclass.Valid, "expected user_visible_accounts table to exist")

	requireColumn(t, tx, "user_visible_accounts", "user_id", "bigint", 0, false)
	requireColumn(t, tx, "user_visible_accounts", "account_id", "bigint", 0, false)
	requireColumn(t, tx, "user_visible_accounts", "granted_by", "bigint", 0, true)
	requireColumn(t, tx, "user_visible_accounts", "created_at", "timestamp with time zone", 0, false)

	// 3. 物理删除 user／account 必须由数据库级联清理分配行。
	requireForeignKeyOnDelete(t, tx, "user_visible_accounts", "user_id", "users", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "user_visible_accounts", "account_id", "accounts", "CASCADE")

	// granted_by 只用于追责，刻意不建外键：管理员被删除不清理分配行。
	requireUserVisibleNoForeignKeyOnColumn(t, tx, "user_visible_accounts", "granted_by")

	// 4. (user_id, account_id) 唯一：复合主键或同列集合的唯一索引均可。
	requireUniqueKeyOnColumns(t, tx, "user_visible_accounts", "user_id", "account_id")

	// 5. 账号侧反查索引（账号被禁用／删除时的可见性校验）。
	requireIndex(t, tx, "user_visible_accounts", "user_visible_accounts_account_id_idx")
}

// 新用户（注册路径与管理员创建路径）默认既无查看能力也无任何分配。
func TestUserAccountViewDefaultsClosedForFreshUser(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	// 直接用 SQL 插入，证明默认值来自数据库列默认，而非某个应用分支的赋值。
	var (
		userID                  int64
		canViewAssignedAccounts bool
	)
	require.NoError(t, scanSingleRow(ctx, tx, `
INSERT INTO users (email, password_hash, role, status)
VALUES ($1, 'test-password-hash', 'user', 'active')
RETURNING id, can_view_assigned_accounts
`, []any{"scope-default-" + uuid.NewString() + "@example.com"}, &userID, &canViewAssignedAccounts))
	require.False(t, canViewAssignedAccounts, "新用户默认不得拥有账号查看能力")
	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", userID, 0)

	// ent 创建路径（真实注册／管理员创建用户所用）必须得到同一默认值。
	user := mustCreateUser(t, client, &service.User{Email: "scope-default-ent-" + uuid.NewString() + "@example.com"})
	canViewAssignedAccounts = true
	require.NoError(t, scanSingleRow(ctx, tx,
		`SELECT can_view_assigned_accounts FROM users WHERE id = $1`, []any{user.ID}, &canViewAssignedAccounts))
	require.False(t, canViewAssignedAccounts, "ent 创建路径同样默认关闭")
	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", user.ID, 0)
}

// 同一 (user_id, account_id) 只允许一行；不同组合互不影响。
func TestUserVisibleAccountsRejectDuplicateAssignmentPair(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	userA := mustCreateUser(t, client, &service.User{Email: "scope-dup-a-" + uuid.NewString() + "@example.com"})
	userB := mustCreateUser(t, client, &service.User{Email: "scope-dup-b-" + uuid.NewString() + "@example.com"})
	accountA := mustCreateAccount(t, client, &service.Account{Name: "scope-dup-acc-a-" + uuid.NewString()})
	accountB := mustCreateAccount(t, client, &service.Account{Name: "scope-dup-acc-b-" + uuid.NewString()})

	insert := func(userID, accountID int64) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO user_visible_accounts (user_id, account_id) VALUES ($1, $2)`, userID, accountID)
		return err
	}

	require.NoError(t, insert(userA.ID, accountA.ID), "首次分配应成功")

	// 重复分配必须被唯一约束拒绝；用 SAVEPOINT 隔离失败语句，
	// 否则 PostgreSQL 会把整个事务置为 aborted，后面的插入无法继续。
	_, err := tx.ExecContext(ctx, "SAVEPOINT user_visible_dup_attempt")
	require.NoError(t, err)
	err = insert(userA.ID, accountA.ID)
	require.Error(t, err, "同一 (user_id, account_id) 不得重复分配")

	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "重复分配应由唯一约束拒绝，实际错误：%v", err)
	require.Equal(t, pq.ErrorCode("23505"), pqErr.Code, "唯一约束冲突码")

	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT user_visible_dup_attempt")
	require.NoError(t, err, "回滚失败语句后事务应继续可用")

	// 同一用户的其他账号、其他用户的同一账号都必须仍可建立。
	require.NoError(t, insert(userA.ID, accountB.ID), "同一用户分配第二个账号应成功")
	require.NoError(t, insert(userB.ID, accountA.ID), "同一账号分配给第二个用户应成功")

	var total int
	require.NoError(t, scanSingleRow(ctx, tx, `
SELECT COUNT(*) FROM user_visible_accounts WHERE user_id IN ($1, $2)
`, []any{userA.ID, userB.ID}, &total))
	require.Equal(t, 3, total, "唯一约束不得误伤其他组合")
}

// 物理删除账号／用户时分配行必须由数据库级联消失，且只影响被删除的那一侧。
func TestUserVisibleAccountsHardDeleteCascades(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	targetUser := mustCreateUser(t, client, &service.User{Email: "scope-cascade-user-" + uuid.NewString() + "@example.com"})
	siblingUser := mustCreateUser(t, client, &service.User{Email: "scope-cascade-user-sibling-" + uuid.NewString() + "@example.com"})
	targetAccount := mustCreateAccount(t, client, &service.Account{Name: "scope-cascade-acc-" + uuid.NewString()})
	siblingAccount := mustCreateAccount(t, client, &service.Account{Name: "scope-cascade-acc-sibling-" + uuid.NewString()})

	mustInsertUserVisibleAccount(t, ctx, tx, targetUser.ID, targetAccount.ID)
	mustInsertUserVisibleAccount(t, ctx, tx, targetUser.ID, siblingAccount.ID)
	mustInsertUserVisibleAccount(t, ctx, tx, siblingUser.ID, siblingAccount.ID)

	requireUserVisibleAssignmentCount(t, ctx, tx, "account_id", targetAccount.ID, 1)
	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", targetUser.ID, 2)

	// 账号用 SQL DELETE 覆盖 FK 级联真正生效的路径（ent 的 Delete 是软删除，见下一个测试）。
	_, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, targetAccount.ID)
	require.NoError(t, err, "物理删除账号")

	requireUserVisibleAssignmentCount(t, ctx, tx, "account_id", targetAccount.ID, 0)
	requireUserVisibleAssignmentCount(t, ctx, tx, "account_id", siblingAccount.ID, 2, "兄弟账号的分配行不得被牵连")
	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", targetUser.ID, 1)

	_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, targetUser.ID)
	require.NoError(t, err, "物理删除用户")

	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", targetUser.ID, 0)
	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", siblingUser.ID, 1, "兄弟用户的分配行不得被牵连")
	requireUserVisibleAssignmentCount(t, ctx, tx, "account_id", siblingAccount.ID, 1)
}

// 软删除是查询侧条件，不是级联：users／accounts 置 deleted_at 后分配行必须保留，
// 但在可见范围内必须立即消失。
func TestUserVisibleAccountsSoftDeleteKeepsRowsButHidesFromScope(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	user := mustCreateUser(t, client, &service.User{Email: "scope-soft-user-" + uuid.NewString() + "@example.com"})
	account := mustCreateAccount(t, client, &service.Account{Name: "scope-soft-acc-" + uuid.NewString()})
	repo := NewUserVisibleAccountRepository(client)
	mustInsertUserVisibleAccount(t, ctx, tx, user.ID, account.ID)
	userVisibleSetCanView(t, ctx, tx, user.ID)

	require.Equal(t, []int64{account.ID}, visibleAccountIDsViaRepository(t, ctx, repo, user.ID), "软删除前可见")

	// 账号软删除（ent SoftDeleteMixin 写入的就是这个 UPDATE）：级联不触发，分配行保留。
	_, err := tx.ExecContext(ctx, `UPDATE accounts SET deleted_at = NOW() WHERE id = $1`, account.ID)
	require.NoError(t, err)
	requireUserVisibleAssignmentCount(t, ctx, tx, "account_id", account.ID, 1, "软删除账号不得清理分配行")
	require.Empty(t, visibleAccountIDsViaRepository(t, ctx, repo, user.ID), "软删除账号必须立即从可见范围消失")

	// 恢复账号后再软删除用户：分配行同样保留，可见范围为空。
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET deleted_at = NULL WHERE id = $1`, account.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{account.ID}, visibleAccountIDsViaRepository(t, ctx, repo, user.ID))

	_, err = tx.ExecContext(ctx, `UPDATE users SET deleted_at = NOW() WHERE id = $1`, user.ID)
	require.NoError(t, err)
	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", user.ID, 1, "软删除用户不得清理分配行")
	require.Empty(t, visibleAccountIDsViaRepository(t, ctx, repo, user.ID), "软删除用户的分配不得再可见")

	// 真删除用户时才由级联清理（与上一个测试相衔接）。
	_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, user.ID)
	require.NoError(t, err)
	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", user.ID, 0)
}

// granted_by 只用于追责：既不要求指向真实用户，也不因授权管理员被删除而清理分配行。
func TestUserVisibleAccountsGrantedByIsAccountabilityOnly(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()

	admin := mustCreateUser(t, client, &service.User{
		Email: "scope-grant-admin-" + uuid.NewString() + "@example.com",
		Role:  service.RoleAdmin,
	})
	user := mustCreateUser(t, client, &service.User{Email: "scope-grant-user-" + uuid.NewString() + "@example.com"})
	account := mustCreateAccount(t, client, &service.Account{Name: "scope-grant-acc-" + uuid.NewString()})
	otherAccount := mustCreateAccount(t, client, &service.Account{Name: "scope-grant-acc-2-" + uuid.NewString()})

	mustInsertUserVisibleAccount(t, ctx, tx, user.ID, account.ID, admin.ID)

	// 不存在的追责 id（脚本写入）同样允许，证明 granted_by 没有外键约束。
	mustInsertUserVisibleAccount(t, ctx, tx, user.ID, otherAccount.ID, int64(999_999_999))

	_, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, admin.ID)
	require.NoError(t, err, "物理删除授权管理员")

	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", user.ID, 2, "管理员删除不得清理其授权过的分配行")
}

// 可见范围的 fixture 验收（走真实仓储 ListVisibleAccounts／GetVisibleAccount）：
// 手动禁用（inactive／disabled，含大小写与首尾空白等兼容写法）与软删除不可见，
// 非手动的 error／expired／限流／临时不可调度必须保持可见；列表计数与按 ID 详情同口径；
// 直接沿用管理员调度口径会错误隐藏 error／expired。
func TestUserVisibleAccountScopeFixtures(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := NewUserVisibleAccountRepository(client)

	viewer := mustCreateUser(t, client, &service.User{Email: "scope-viewer-" + uuid.NewString() + "@example.com"})
	setCanView := func(userID int64, canView bool) {
		t.Helper()
		_, err := tx.ExecContext(ctx,
			`UPDATE users SET can_view_assigned_accounts = $2 WHERE id = $1`, userID, canView)
		require.NoError(t, err)
	}
	setCanView(viewer.ID, true)

	newAccount := func(label, status string) *service.Account {
		t.Helper()
		return mustCreateAccount(t, client, &service.Account{
			Name:   "scope-" + label + "-" + uuid.NewString(),
			Status: status,
		})
	}

	active := newAccount("active", service.StatusActive)
	inactive := newAccount("inactive", "inactive") // 当前账号编辑器写入的手动禁用值
	disabled := newAccount("disabled", service.StatusDisabled)
	mixedCaseInactive := newAccount("mixed-case-inactive", " Inactive ") // 兼容写法
	upperDisabled := newAccount("upper-disabled", "DISABLED")            // 兼容写法
	failed := newAccount("error", service.StatusError)
	expired := newAccount("expired", service.StatusExpired)
	rateLimited := newAccount("rate-limited", service.StatusActive)
	tempUnschedulable := newAccount("temp-unschedulable", service.StatusActive)
	softDeleted := newAccount("soft-deleted", service.StatusActive)
	unassigned := newAccount("unassigned", service.StatusActive)

	for _, account := range []*service.Account{
		active, inactive, disabled, mixedCaseInactive, upperDisabled,
		failed, expired, rateLimited, tempUnschedulable, softDeleted,
	} {
		mustInsertUserVisibleAccount(t, ctx, tx, viewer.ID, account.ID)
	}

	// 限流与临时不可调度：非手动状态，必须保持可见。
	_, err := tx.ExecContext(ctx, `
UPDATE accounts
SET rate_limited_at = NOW(), rate_limit_reset_at = NOW() + INTERVAL '1 hour'
WHERE id = $1
`, rateLimited.ID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
UPDATE accounts
SET temp_unschedulable_until = NOW() + INTERVAL '1 hour',
    temp_unschedulable_reason = 'scope fixture'
WHERE id = $1
`, tempUnschedulable.ID)
	require.NoError(t, err)

	// 账号软删除：分配行保留，但必须从可见范围消失。
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET deleted_at = NOW() WHERE id = $1`, softDeleted.ID)
	require.NoError(t, err)

	// 前提：隐藏不是靠缺行实现的——所有 fixture 都真的存在分配行。
	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", viewer.ID, 10)

	enabled, err := repo.GetAccountViewEnabled(ctx, viewer.ID)
	require.NoError(t, err, "GetAccountViewEnabled")
	require.True(t, enabled)

	visible := visibleAccountIDsViaRepository(t, ctx, repo, viewer.ID)
	require.Equal(t,
		sortedIDs(active.ID, failed.ID, expired.ID, rateLimited.ID, tempUnschedulable.ID),
		visible,
		"可见范围应排除手动禁用与软删除账号，但保留 error／expired／限流／临时不可调度")

	// 列表与详情同一可见范围：隐藏账号连按 ID 查询也不可见（猜 ID 不泄露存在性）。
	for _, hidden := range []*service.Account{inactive, disabled, mixedCaseInactive, upperDisabled, softDeleted, unassigned} {
		account, err := repo.GetVisibleAccount(ctx, viewer.ID, hidden.ID)
		require.NoError(t, err, "GetVisibleAccount(hidden)")
		require.Nil(t, account, "不可见账号 %d (%s) 不得按 ID 读取", hidden.ID, hidden.Status)
	}

	// 非手动状态账号可以通过详情读取。
	for _, expected := range []*service.Account{active, failed, expired, rateLimited, tempUnschedulable} {
		account, err := repo.GetVisibleAccount(ctx, viewer.ID, expected.ID)
		require.NoError(t, err, "GetVisibleAccount(visible)")
		require.NotNil(t, account, "可见账号 %d 必须能按 ID 读取", expected.ID)
		require.Equal(t, expected.ID, account.ID)
	}

	// 反例：直接沿用管理员调度口径（status='active' AND schedulable）会错误隐藏 error／expired。
	naive := userVisibleNaiveSchedulerIDs(t, ctx, tx, viewer.ID)
	require.Equal(t, sortedIDs(active.ID, rateLimited.ID, tempUnschedulable.ID), naive)
	require.NotContains(t, naive, failed.ID, "调度口径会把故障账号错误隐藏")
	require.NotContains(t, naive, expired.ID, "调度口径会把过期账号错误隐藏")
	require.NotEqual(t, visible, naive, "可见范围不得由调度口径推导")

	// 管理员撤销口径包含手动禁用账号，但不含软删除账号。
	summaries, err := repo.ListAssignedAccountSummaries(ctx, viewer.ID)
	require.NoError(t, err, "ListAssignedAccountSummaries")
	summaryIDs := make([]int64, 0, len(summaries))
	for _, summary := range summaries {
		summaryIDs = append(summaryIDs, summary.ID)
	}
	require.Equal(t,
		sortedIDs(active.ID, inactive.ID, disabled.ID, mixedCaseInactive.ID, upperDisabled.ID,
			failed.ID, expired.ID, rateLimited.ID, tempUnschedulable.ID),
		sortedIDs(summaryIDs...),
		"管理员撤销口径含手动禁用账号，不含软删除账号")

	// 没有查看能力的用户即使有分配也看不到任何账号（授权默认关闭、撤销立即生效）。
	unprivileged := mustCreateUser(t, client, &service.User{Email: "scope-unprivileged-" + uuid.NewString() + "@example.com"})
	mustInsertUserVisibleAccount(t, ctx, tx, unprivileged.ID, active.ID)
	enabled, err = repo.GetAccountViewEnabled(ctx, unprivileged.ID)
	require.NoError(t, err)
	require.False(t, enabled, "能力默认关闭")
	require.Empty(t, visibleAccountIDsViaRepository(t, ctx, repo, unprivileged.ID), "未获查看能力的用户不得看到已分配账号")
	account, err := repo.GetVisibleAccount(ctx, unprivileged.ID, active.ID)
	require.NoError(t, err)
	require.Nil(t, account, "未获查看能力的用户不得按 ID 读取账号")

	// 被禁用的用户同样不可见（仓储要求用户状态为 active）。
	disabledUser := mustCreateUser(t, client, &service.User{
		Email:  "scope-disabled-user-" + uuid.NewString() + "@example.com",
		Status: service.StatusDisabled,
	})
	setCanView(disabledUser.ID, true)
	mustInsertUserVisibleAccount(t, ctx, tx, disabledUser.ID, active.ID)
	require.Empty(t, visibleAccountIDsViaRepository(t, ctx, repo, disabledUser.ID), "被禁用的用户不得看到账号")

	// 软删除用户即使仍有查看能力与分配也不可见。
	deletedViewer := mustCreateUser(t, client, &service.User{Email: "scope-deleted-viewer-" + uuid.NewString() + "@example.com"})
	setCanView(deletedViewer.ID, true)
	mustInsertUserVisibleAccount(t, ctx, tx, deletedViewer.ID, active.ID)
	require.Equal(t, []int64{active.ID}, visibleAccountIDsViaRepository(t, ctx, repo, deletedViewer.ID))
	_, err = tx.ExecContext(ctx, `UPDATE users SET deleted_at = NOW() WHERE id = $1`, deletedViewer.ID)
	require.NoError(t, err)
	require.Empty(t, visibleAccountIDsViaRepository(t, ctx, repo, deletedViewer.ID), "软删除用户不得再看到账号")
	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", deletedViewer.ID, 1, "软删除保留分配行，仅查询侧隐藏")
}

// 直接修改 users.can_view_assigned_accounts 必须立即影响可见范围（撤销立即生效）。
func TestUserVisibleAccountScopeFollowsViewCapabilityFlag(t *testing.T) {
	ctx := context.Background()
	tx := testEntTx(t)
	client := tx.Client()
	repo := NewUserVisibleAccountRepository(client)

	user := mustCreateUser(t, client, &service.User{Email: "scope-toggle-" + uuid.NewString() + "@example.com"})
	account := mustCreateAccount(t, client, &service.Account{Name: "scope-toggle-acc-" + uuid.NewString()})
	mustInsertUserVisibleAccount(t, ctx, tx, user.ID, account.ID)

	require.Empty(t, visibleAccountIDsViaRepository(t, ctx, repo, user.ID), "能力默认关闭时不可见")

	_, err := tx.ExecContext(ctx, `UPDATE users SET can_view_assigned_accounts = TRUE WHERE id = $1`, user.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{account.ID}, visibleAccountIDsViaRepository(t, ctx, repo, user.ID), "开启能力后可见")

	_, err = tx.ExecContext(ctx, `UPDATE users SET can_view_assigned_accounts = FALSE WHERE id = $1`, user.ID)
	require.NoError(t, err)
	require.Empty(t, visibleAccountIDsViaRepository(t, ctx, repo, user.ID), "撤销能力后立即不可见，且旧权限不影响分配行")
	requireUserVisibleAssignmentCount(t, ctx, tx, "user_id", user.ID, 1)
}

// --- helpers -------------------------------------------------------------

func mustInsertUserVisibleAccount(t *testing.T, ctx context.Context, tx *dbent.Tx, userID, accountID int64, grantedBy ...int64) {
	t.Helper()

	query := `INSERT INTO user_visible_accounts (user_id, account_id) VALUES ($1, $2)`
	args := []any{userID, accountID}
	if len(grantedBy) > 0 {
		query = `INSERT INTO user_visible_accounts (user_id, account_id, granted_by) VALUES ($1, $2, $3)`
		args = append(args, grantedBy[0])
	}

	_, err := tx.ExecContext(ctx, query, args...)
	require.NoError(t, err, "插入 user_visible_accounts 行")
}

// userVisibleSetCanView 打开某个用户的查看能力。
func userVisibleSetCanView(t *testing.T, ctx context.Context, tx *dbent.Tx, userID int64) {
	t.Helper()

	_, err := tx.ExecContext(ctx,
		`UPDATE users SET can_view_assigned_accounts = TRUE WHERE id = $1`, userID)
	require.NoError(t, err, "开启 can_view_assigned_accounts")
}

// visibleAccountIDsViaRepository 走真实仓储读取该用户当前可见账号 id（升序），
// 并顺带断言分页计数与返回内容同口径。
func visibleAccountIDsViaRepository(t *testing.T, ctx context.Context, repo service.VisibleAccountRepository, userID int64) []int64 {
	t.Helper()

	// PageSize 必须显式给足：仓储不做默认值归一化，Limit(0) 会返回空页。
	accounts, total, err := repo.ListVisibleAccounts(ctx, userID, service.VisibleAccountFilter{Page: 1, PageSize: 100})
	require.NoError(t, err, "ListVisibleAccounts")

	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	slices.Sort(ids)
	require.Equal(t, int(total), len(ids), "分页计数必须与返回内容同一可见范围")
	return ids
}

// userVisibleNaiveSchedulerIDs 返回错误地沿用管理员调度口径时能看到的账号 id（升序）。
func userVisibleNaiveSchedulerIDs(t *testing.T, ctx context.Context, tx *dbent.Tx, userID int64) []int64 {
	t.Helper()

	var ids pq.Int64Array
	require.NoError(t, scanSingleRow(ctx, tx, naiveAccountSchedulerStatusFilterSQL, []any{userID}, &ids))
	return []int64(ids)
}

func sortedIDs(ids ...int64) []int64 {
	slices.Sort(ids)
	return ids
}

// requireUserVisibleAssignmentCount 断言某个用户／账号维度上的分配行数量。
func requireUserVisibleAssignmentCount(t *testing.T, ctx context.Context, tx *dbent.Tx, column string, value int64, expected int, msgAndArgs ...any) {
	t.Helper()

	// column 只允许本文件内的两个字面量，避免拼接注入。
	require.Contains(t, []string{"user_id", "account_id"}, column, "不支持的过滤列")

	var count int
	require.NoError(t, scanSingleRow(ctx, tx,
		`SELECT COUNT(*) FROM user_visible_accounts WHERE `+column+` = $1`, []any{value}, &count))
	require.Equal(t, expected, count, msgAndArgs...)
}

// requireUserVisibleNoForeignKeyOnColumn 断言某列上没有外键约束。
func requireUserVisibleNoForeignKeyOnColumn(t *testing.T, tx *sql.Tx, table, column string) {
	t.Helper()

	var count int
	require.NoError(t, scanSingleRow(context.Background(), tx, `
SELECT COUNT(*)
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
JOIN pg_attribute attr ON attr.attrelid = tbl.oid AND attr.attnum = ANY(c.conkey)
WHERE ns.nspname = 'public'
  AND c.contype = 'f'
  AND tbl.relname = $1
  AND attr.attname = $2
`, []any{table, column}, &count))
	require.Zero(t, count, "expected no foreign key on %s.%s", table, column)
}

// requireUniqueKeyOnColumns 断言存在一个唯一约束或唯一索引，其索引列恰好是给定集合
// （顺序无关），因此对 (user_id, account_id) 这样的关系行天然去重。
func requireUniqueKeyOnColumns(t *testing.T, tx *sql.Tx, table string, columns ...string) {
	t.Helper()

	var count int
	require.NoError(t, scanSingleRow(context.Background(), tx, `
SELECT COUNT(*)
FROM pg_index i
JOIN pg_class tbl ON tbl.oid = i.indrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'public'
  AND tbl.relname = $1
  AND i.indisunique
  AND i.indnatts = $2
  -- 索引里不得有集合之外的列
  AND NOT EXISTS (
    SELECT 1
    FROM pg_attribute attr
    WHERE attr.attrelid = tbl.oid
      AND attr.attnum = ANY(i.indkey)
      AND attr.attname <> ALL($3::text[])
  )
  -- 集合里的每一列都必须被索引
  AND NOT EXISTS (
    SELECT 1
    FROM unnest($3::text[]) AS wanted(name)
    WHERE NOT EXISTS (
      SELECT 1
      FROM pg_attribute attr
      WHERE attr.attrelid = tbl.oid
        AND attr.attnum = ANY(i.indkey)
        AND attr.attname = wanted.name
    )
  )
`, []any{table, len(columns), pq.Array(columns)}, &count))
	require.NotZero(t, count, "expected a unique constraint or index covering %s(%v)", table, columns)
}
