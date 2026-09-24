//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// TestUserVisibleAccountRepository_RealQuery 在真实 PostgreSQL 上验证普通用户
// 只读账号视图的可见范围：能力开关、显式分配、账号状态（只有手动禁用才隐藏）、
// 软删除与分页计数必须在同一 SQL 范围内。
func TestUserVisibleAccountRepository_RealQuery(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUserVisibleAccountRepository(client)

	userA := mustCreateUser(t, client, &service.User{})
	userB := mustCreateUser(t, client, &service.User{})
	disabledUser := mustCreateUser(t, client, &service.User{Status: service.StatusDisabled})

	active := mustCreateAccount(t, client, &service.Account{Name: "vis-active"})
	reserved := mustCreateAccount(t, client, &service.Account{Name: "vis-reserved"})
	failed := mustCreateAccount(t, client, &service.Account{Name: "vis-failed", Status: service.StatusError})
	expired := mustCreateAccount(t, client, &service.Account{Name: "vis-expired", Status: service.StatusExpired})
	manualDisabled := mustCreateAccount(t, client, &service.Account{Name: "vis-disabled", Status: service.StatusDisabled})
	inactive := mustCreateAccount(t, client, &service.Account{Name: "vis-inactive", Status: service.StatusInactive})
	// 历史脏数据：大小写/空白变体的手动停用必须与 service 层同口径隐藏。
	oddCasing := mustCreateAccount(t, client, &service.Account{Name: "vis-odd-casing"})
	softDeleted := mustCreateAccount(t, client, &service.Account{Name: "vis-soft-deleted"})

	_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET status = $1 WHERE id = $2`, "  Disabled ", oddCasing.ID)
	require.NoError(t, err)

	allIDs := []int64{active.ID, reserved.ID, failed.ID, expired.ID, manualDisabled.ID, inactive.ID, oddCasing.ID, softDeleted.ID}
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM user_visible_accounts WHERE user_id IN ($1, $2, $3)`, userA.ID, userB.ID, disabledUser.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = ANY($1)`, pq.Array(allIDs))
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id IN ($1, $2, $3)`, userA.ID, userB.ID, disabledUser.ID)
	})

	enabled := true
	require.NoError(t, repo.UpdateAccountView(ctx, userA.ID, &enabled, &allIDs, nil))

	// 分配之后账号才被软删除：分配行仍在，但必须立即从列表/详情/计数消失。
	_, err = integrationDB.ExecContext(ctx, `UPDATE accounts SET deleted_at = NOW() WHERE id = $1`, softDeleted.ID)
	require.NoError(t, err)

	// 1) 只有「非手动禁用且未软删除」的账号可见。
	accounts, total, err := repo.ListVisibleAccounts(ctx, userA.ID, service.VisibleAccountFilter{Page: 1, PageSize: 50})
	require.NoError(t, err)
	visibleIDs := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		visibleIDs = append(visibleIDs, account.ID)
	}
	require.ElementsMatch(t, []int64{active.ID, reserved.ID, failed.ID, expired.ID}, visibleIDs,
		"手动禁用/软删除账号必须隐藏，error/expired 必须保留")
	require.EqualValues(t, len(visibleIDs), total, "计数必须与过滤后的内容同范围")

	// 2) 详情与列表同范围；不可见账号返回 (nil, nil)。
	for _, id := range []int64{manualDisabled.ID, inactive.ID, oddCasing.ID, softDeleted.ID} {
		account, err := repo.GetVisibleAccount(ctx, userA.ID, id)
		require.NoError(t, err)
		require.Nil(t, account, "account %d must be invisible", id)
	}
	detail, err := repo.GetVisibleAccount(ctx, userA.ID, failed.ID)
	require.NoError(t, err)
	require.NotNil(t, detail)
	require.Equal(t, service.StatusError, detail.Status)

	// 3) 分页计数：page_size=1 时 total 仍为完整可见数量。
	page, total, err := repo.ListVisibleAccounts(ctx, userA.ID, service.VisibleAccountFilter{Page: 2, PageSize: 2})
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.EqualValues(t, 4, total)

	// 4) 跨用户隔离：能力开启但未分配的用户什么都看不到。
	require.NoError(t, repo.UpdateAccountView(ctx, userB.ID, &enabled, nil, nil))
	accounts, total, err = repo.ListVisibleAccounts(ctx, userB.ID, service.VisibleAccountFilter{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.EqualValues(t, 0, total)
	account, err := repo.GetVisibleAccount(ctx, userB.ID, active.ID)
	require.NoError(t, err)
	require.Nil(t, account)

	// 5) 已禁用用户即使持有能力与分配也不可见；能力读取也必须与列表门禁同口径。
	liveIDs := []int64{active.ID, reserved.ID, failed.ID, expired.ID, manualDisabled.ID, inactive.ID, oddCasing.ID}
	require.NoError(t, repo.UpdateAccountView(ctx, disabledUser.ID, &enabled, &liveIDs, nil))
	accounts, total, err = repo.ListVisibleAccounts(ctx, disabledUser.ID, service.VisibleAccountFilter{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.EqualValues(t, 0, total)

	disabledFlag, err := repo.GetAccountViewEnabled(ctx, disabledUser.ID)
	require.NoError(t, err)
	require.False(t, disabledFlag, "disabled user must not report account view capability")

	var rawFlag bool
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT can_view_assigned_accounts FROM users WHERE id = $1`, disabledUser.ID).Scan(&rawFlag))
	require.True(t, rawFlag, "fixture: the stored flag is true, the read must still fail closed")

	// 恢复为 active 后能力恢复可见。
	_, err = integrationDB.ExecContext(ctx, `UPDATE users SET status = $1 WHERE id = $2`, service.StatusActive, disabledUser.ID)
	require.NoError(t, err)
	disabledFlag, err = repo.GetAccountViewEnabled(ctx, disabledUser.ID)
	require.NoError(t, err)
	require.True(t, disabledFlag)

	// 6) 能力关闭但已分配：不可见；重新打开后立即恢复。
	off := false
	require.NoError(t, repo.UpdateAccountView(ctx, userA.ID, &off, nil, nil))
	accounts, _, err = repo.ListVisibleAccounts(ctx, userA.ID, service.VisibleAccountFilter{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.NoError(t, repo.UpdateAccountView(ctx, userA.ID, &enabled, nil, nil))
	accounts, _, err = repo.ListVisibleAccounts(ctx, userA.ID, service.VisibleAccountFilter{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Len(t, accounts, 4)

	// 7) 撤销（清空分配）后立即不可见，能力开关不受影响。
	empty := []int64{}
	require.NoError(t, repo.UpdateAccountView(ctx, userA.ID, nil, &empty, nil))
	accounts, _, err = repo.ListVisibleAccounts(ctx, userA.ID, service.VisibleAccountFilter{Page: 1, PageSize: 50})
	require.NoError(t, err)
	require.Empty(t, accounts)
	account, err = repo.GetVisibleAccount(ctx, userA.ID, active.ID)
	require.NoError(t, err)
	require.Nil(t, account)
	stillEnabled, err := repo.GetAccountViewEnabled(ctx, userA.ID)
	require.NoError(t, err)
	require.True(t, stillEnabled, "清空分配不应关闭能力开关")
}

// TestUserVisibleAccountRepository_UpdateIsAtomicAndValidated 验证更新接口的
// 事务性：未知账号 id 整单失败，不留下「开关已打开但分配未落库」的中间状态；
// 不存在的用户不会写入任何数据。
func TestUserVisibleAccountRepository_UpdateIsAtomicAndValidated(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUserVisibleAccountRepository(client)

	user := mustCreateUser(t, client, &service.User{})
	account := mustCreateAccount(t, client, &service.Account{Name: "atomic-account"})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM user_visible_accounts WHERE user_id = $1`, user.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = $1`, account.ID)
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})

	enabled := true
	withUnknown := []int64{account.ID, account.ID + 999999}
	err := repo.UpdateAccountView(ctx, user.ID, &enabled, &withUnknown, nil)
	require.ErrorIs(t, err, service.ErrUnknownVisibleAccount)

	flag, err := repo.GetAccountViewEnabled(ctx, user.ID)
	require.NoError(t, err)
	require.False(t, flag, "failed update must not flip the capability flag")
	assigned, err := repo.ListAssignedAccountSummaries(ctx, user.ID)
	require.NoError(t, err)
	require.Empty(t, assigned)

	// 不存在的用户：明确报错，不产生任何行。
	err = repo.UpdateAccountView(ctx, account.ID+999999, &enabled, &[]int64{account.ID}, nil)
	require.ErrorIs(t, err, service.ErrAccountViewUserNotFound)

	// 非法 id（0）必须 400，不能被静默丢弃成「清空分配」。
	err = repo.UpdateAccountView(ctx, user.ID, nil, &[]int64{0}, nil)
	require.ErrorIs(t, err, service.ErrUnknownVisibleAccount)
	assigned, err = repo.ListAssignedAccountSummaries(ctx, user.ID)
	require.NoError(t, err)
	require.Empty(t, assigned)

	// 正常路径 + 幂等重复提交。
	ids := []int64{account.ID}
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &enabled, &ids, nil))
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &enabled, &ids, nil))
	assigned, err = repo.ListAssignedAccountSummaries(ctx, user.ID)
	require.NoError(t, err)
	require.Len(t, assigned, 1)
	require.Equal(t, account.Name, assigned[0].Name)
	require.Equal(t, account.Platform, assigned[0].Platform)
	require.Equal(t, account.Type, assigned[0].Type)
}

// TestUserVisibleAccountRepository_AssignmentFollowsAccountDelete 验证账号软删除
// 与物理删除两条路径：软删除按 deleted_at 过滤隐藏（分配行保留），物理删除由
// 迁移 250 的 ON DELETE CASCADE 清理分配行，不残留悬挂引用。
func TestUserVisibleAccountRepository_AssignmentFollowsAccountDelete(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUserVisibleAccountRepository(client)

	user := mustCreateUser(t, client, &service.User{})
	account := mustCreateAccount(t, client, &service.Account{Name: "cascade-account"})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM user_visible_accounts WHERE user_id = $1`, user.ID)
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})

	enabled := true
	ids := []int64{account.ID}
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &enabled, &ids, nil))

	countAssignment := func() int {
		t.Helper()
		var count int
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM user_visible_accounts WHERE user_id = $1 AND account_id = $2`,
			user.ID, account.ID).Scan(&count))
		return count
	}
	require.Equal(t, 1, countAssignment())

	// 软删除：分配行保留，但账号立刻不可见。
	require.NoError(t, client.Account.DeleteOneID(account.ID).Exec(ctx))
	require.Equal(t, 1, countAssignment(), "软删除不是物理删除，不应触发级联")
	accounts, total, err := repo.ListVisibleAccounts(ctx, user.ID, service.VisibleAccountFilter{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.EqualValues(t, 0, total)
	// 既有的分配允许原样保留：管理员界面会把 account_ids 原封回传，拒绝它等于把
	// 「打开即保存」变成 400，或者更糟——让原样保存静默撤销这条关系。
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, nil, &ids, nil))
	require.Equal(t, 1, countAssignment(), "保留既有分配不得删除分配行")
	// 但已软删除的账号不能被「新分配」给别的用户。
	otherUser := mustCreateUserWithUniqueEmail(t, client, "")
	t.Cleanup(func() { _, _ = integrationDB.Exec(`DELETE FROM users WHERE id = $1`, otherUser.ID) })
	err = repo.UpdateAccountView(ctx, otherUser.ID, nil, &ids, nil)
	require.ErrorIs(t, err, service.ErrUnknownVisibleAccount)

	// 物理删除：迁移 250 的 ON DELETE CASCADE 清理分配行。
	_, err = integrationDB.ExecContext(ctx, `DELETE FROM accounts WHERE id = $1`, account.ID)
	require.NoError(t, err)
	require.Zero(t, countAssignment(), "物理删除账号必须级联清理分配行")
}

// TestUserVisibleAccountRepository_DoesNotDisturbExistingUserCapabilities 验证
// 新增查看能力与分配都不影响该用户既有的 API Key 读取能力（权限不回退）。
func TestUserVisibleAccountRepository_DoesNotDisturbExistingUserCapabilities(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUserVisibleAccountRepository(client)
	apiKeyRepo := NewAPIKeyRepository(client, integrationDB)

	user := mustCreateUser(t, client, &service.User{})
	key := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID,
		Key:    fmt.Sprintf("sk-accountview-%d", user.ID),
	})
	account := mustCreateAccount(t, client, &service.Account{Name: "capability-account"})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM user_visible_accounts WHERE user_id = $1`, user.ID)
		_, _ = integrationDB.Exec(`DELETE FROM api_keys WHERE user_id = $1`, user.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = $1`, account.ID)
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})

	keysBefore, _, err := apiKeyRepo.ListByUserID(ctx, user.ID, pagination.PaginationParams{Page: 1, PageSize: 10}, service.APIKeyListFilters{})
	require.NoError(t, err)
	require.Len(t, keysBefore, 1)
	require.Equal(t, key.ID, keysBefore[0].ID)

	enabled := true
	ids := []int64{account.ID}
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &enabled, &ids, nil))

	// 开启查看能力并分配账号后：Key 仍可读、仍属同一用户、状态未被改动。
	keysAfter, _, err := apiKeyRepo.ListByUserID(ctx, user.ID, pagination.PaginationParams{Page: 1, PageSize: 10}, service.APIKeyListFilters{})
	require.NoError(t, err)
	require.Len(t, keysAfter, 1)
	require.Equal(t, key.ID, keysAfter[0].ID)
	require.Equal(t, service.StatusActive, keysAfter[0].Status)

	// 账号可见性不影响账号本身的调度状态。
	visible, err := repo.GetVisibleAccount(ctx, user.ID, account.ID)
	require.NoError(t, err)
	require.NotNil(t, visible)
	require.True(t, visible.Schedulable)
}

// TestUserVisibleAccountRepository_SoftDeletedUserIsInvisible 验证软删除用户即便
// 仍有分配关系也不会读到账号（与本仓库软删除语义一致）。
func TestUserVisibleAccountRepository_SoftDeletedUserIsInvisible(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUserVisibleAccountRepository(client)

	user := mustCreateUser(t, client, &service.User{})
	account := mustCreateAccount(t, client, &service.Account{Name: "soft-deleted-user-account"})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM user_visible_accounts WHERE user_id = $1`, user.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = $1`, account.ID)
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})

	enabled := true
	ids := []int64{account.ID}
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &enabled, &ids, nil))

	require.NoError(t, client.User.UpdateOneID(user.ID).SetDeletedAt(time.Now().UTC()).Exec(ctx))

	flag, err := repo.GetAccountViewEnabled(ctx, user.ID)
	require.NoError(t, err)
	require.False(t, flag, "soft-deleted user must fail closed")
	accounts, total, err := repo.ListVisibleAccounts(ctx, user.ID, service.VisibleAccountFilter{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.Empty(t, accounts)
	require.EqualValues(t, 0, total)

	var remaining int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE id = $1 AND deleted_at IS NULL`, user.ID).Scan(&remaining))
	require.Zero(t, remaining)
	// 分配关系保留（软删除不是物理删除，不触发 FK 级联），但已无法读到。
	var assignments int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_visible_accounts WHERE user_id = $1`, user.ID).Scan(&assignments))
	require.Equal(t, 1, assignments)

	// 被软删除的用户不会被误认为可写目标。
	err = repo.UpdateAccountView(ctx, user.ID, &enabled, &ids, nil)
	require.ErrorIs(t, err, service.ErrAccountViewUserNotFound)
}

// TestUserVisibleAccountRepository_AdminFlagReadIsSeparateFromUserFacingRead 验证
// 两个读取口径分工明确：
//   - 面向普通用户的 GetAccountViewEnabled 对已禁用用户 fail-closed，与列表／详情的
//     门禁同口径；
//   - 面向管理员的 GetStoredAccountViewEnabled 读「存储值」，使管理员界面不会把存储的
//     true 显示成 false，进而「打开即保存」一次就把能力真正关掉；
//   - 用户不存在或已软删除时，管理员读取必须与 PUT 同为 USER_NOT_FOUND。
func TestUserVisibleAccountRepository_AdminFlagReadIsSeparateFromUserFacingRead(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUserVisibleAccountRepository(client)

	disabledUser := mustCreateUserWithUniqueEmail(t, client, service.StatusDisabled)
	softDeletedUser := mustCreateUserWithUniqueEmail(t, client, "")
	account := mustCreateAccount(t, client, &service.Account{Name: "admin-flag-read"})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM user_visible_accounts WHERE user_id IN ($1, $2)`, disabledUser.ID, softDeletedUser.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = $1`, account.ID)
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id IN ($1, $2)`, disabledUser.ID, softDeletedUser.ID)
	})

	enabled := true
	ids := []int64{account.ID}
	// 已禁用（未软删除）用户：PUT 接受，用户侧读取 fail-closed，管理员侧读存储值。
	require.NoError(t, repo.UpdateAccountView(ctx, disabledUser.ID, &enabled, &ids, nil))
	userFacing, err := repo.GetAccountViewEnabled(ctx, disabledUser.ID)
	require.NoError(t, err)
	require.False(t, userFacing, "面向用户的读取必须对已禁用用户 fail-closed")
	stored, err := repo.GetStoredAccountViewEnabled(ctx, disabledUser.ID)
	require.NoError(t, err)
	require.True(t, stored, "面向管理员的读取必须返回存储的 true")

	accounts, total, err := repo.ListVisibleAccounts(ctx, disabledUser.ID, service.VisibleAccountFilter{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.Empty(t, accounts, "已禁用用户仍不得看到账号")
	require.EqualValues(t, 0, total)

	// 不存在的用户：GET 与 PUT 同为 USER_NOT_FOUND。
	missing := disabledUser.ID + 987654
	_, err = repo.GetStoredAccountViewEnabled(ctx, missing)
	require.ErrorIs(t, err, service.ErrAccountViewUserNotFound)
	require.ErrorIs(t, repo.UpdateAccountView(ctx, missing, &enabled, nil, nil), service.ErrAccountViewUserNotFound)

	// 软删除用户：即便存储的开关为 true，读取也必须是 USER_NOT_FOUND（与 PUT 同口径）。
	require.NoError(t, repo.UpdateAccountView(ctx, softDeletedUser.ID, &enabled, nil, nil))
	require.NoError(t, client.User.UpdateOneID(softDeletedUser.ID).SetDeletedAt(time.Now().UTC()).Exec(ctx))
	var rawFlag bool
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT can_view_assigned_accounts FROM users WHERE id = $1`, softDeletedUser.ID).Scan(&rawFlag))
	require.True(t, rawFlag, "fixture: 存储的开关为 true，读取仍须 404")
	_, err = repo.GetStoredAccountViewEnabled(ctx, softDeletedUser.ID)
	require.ErrorIs(t, err, service.ErrAccountViewUserNotFound, "软删除用户的管理员读取必须 404")
	require.ErrorIs(t, repo.UpdateAccountView(ctx, softDeletedUser.ID, &enabled, nil, nil),
		service.ErrAccountViewUserNotFound, "GET 与 PUT 必须同口径")
}

// TestUserVisibleAccountRepository_ConcurrentReplaceSetsCannotUnion 在真实 PostgreSQL
// （READ COMMITTED，与生产一致）上验证并发的「全量替换 account_ids」请求不会并集。
//
// 先到者在事务里删掉旧集合、写入 {first} 但尚未提交（本测试用独立连接持有它，
// 把「已写入未提交」这一瞬间固定下来），后到者通过仓储把集合替换为 {second}。
//   - 修复后：后到者必须在 users 行锁上等待先到者提交，随后删除 {first} 并写入
//     {second}，最终集合只反映最后一次请求的意图；
//   - 未修复：后到者的 DELETE 看不到未提交的 {first}，两个请求的分配会同时留下，
//     等于把两次不同的管理员意图合并授权给用户。
func TestUserVisibleAccountRepository_ConcurrentReplaceSetsCannotUnion(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUserVisibleAccountRepository(client)

	user := mustCreateUserWithUniqueEmail(t, client, "")
	kept := mustCreateAccount(t, client, &service.Account{Name: "union-kept"})
	first := mustCreateAccount(t, client, &service.Account{Name: "union-first"})
	second := mustCreateAccount(t, client, &service.Account{Name: "union-second"})
	allAccounts := []int64{kept.ID, first.ID, second.ID}
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM user_visible_accounts WHERE user_id = $1`, user.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = ANY($1)`, pq.Array(allAccounts))
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})

	enabled := true
	keptIDs := []int64{kept.ID}
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &enabled, &keptIDs, nil))

	// 先到者：独立连接 + 显式事务，已删除旧集合并写入 {first}，但不提交。
	conn, err := integrationDB.Conn(ctx)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	slow, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = slow.Rollback() }()
	_, err = slow.ExecContext(ctx,
		`SELECT id FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, user.ID)
	require.NoError(t, err, "先到者持有 users 行锁")
	_, err = slow.ExecContext(ctx, `DELETE FROM user_visible_accounts WHERE user_id = $1`, user.ID)
	require.NoError(t, err)
	_, err = slow.ExecContext(ctx,
		`INSERT INTO user_visible_accounts (user_id, account_id) VALUES ($1, $2)`, user.ID, first.ID)
	require.NoError(t, err)

	// 后到者：仓储的仅 account_ids 全量替换。
	secondIDs := []int64{second.ID}
	done := make(chan error, 1)
	go func() { done <- repo.UpdateAccountView(ctx, user.ID, nil, &secondIDs, nil) }()

	// 等待后到者进入「阻塞在锁上」或「已完成」这两种确定状态之一，
	// 靠观测 pg_stat_activity 而不是固定 sleep。
	finished := false
	state := lockWaiterState{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !finished && !state.anyWait {
		select {
		case err := <-done:
			finished = true
			require.NoError(t, err)
		default:
		}
		if finished {
			break
		}
		state = observeLockWaiters(ctx, t)
		if !state.anyWait {
			time.Sleep(5 * time.Millisecond)
		}
	}
	require.True(t, finished || state.anyWait, "后到者既没有等待锁，也没有完成")
	require.True(t, finished || state.onUserRowLock,
		"后到者必须阻塞在目标用户行的 FOR UPDATE 上：修复前它只会在 DELETE 上被挡住，"+
			"之后仍会把两次分配并存成并集")

	require.NoError(t, slow.Commit())
	if !finished {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("后到者在先到者提交后仍未返回")
		}
	}

	require.Equal(t, []int64{second.ID}, storedVisibleAssignmentIDs(t, ctx, user.ID),
		"并发全量替换必须串行化：最终只保留最后提交的集合，不得把两次分配并集成并集（等于越权授予）")
}

// TestUserVisibleAccountRepository_RetainedGrantsKeepProvenance 验证增量替换：
// 幂等重复提交、仅切换开关与追加新分配都不改写既有分配行的 granted_by/created_at，
// 只有真正被撤销的行才会被删除。
func TestUserVisibleAccountRepository_RetainedGrantsKeepProvenance(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUserVisibleAccountRepository(client)

	user := mustCreateUserWithUniqueEmail(t, client, "")
	firstAccount := mustCreateAccount(t, client, &service.Account{Name: "provenance-first"})
	secondAccount := mustCreateAccount(t, client, &service.Account{Name: "provenance-second"})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM user_visible_accounts WHERE user_id = $1`, user.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = ANY($1)`, pq.Array([]int64{firstAccount.ID, secondAccount.ID}))
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})

	adminA, adminB := int64(900001), int64(900002)
	enabled := true
	firstIDs := []int64{firstAccount.ID}
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &enabled, &firstIDs, &adminA))
	initial := readVisibleGrant(t, ctx, user.ID, firstAccount.ID)
	require.NotNil(t, initial.GrantedBy)
	require.Equal(t, adminA, *initial.GrantedBy)

	// 追加新分配：既有那行原样保留。
	bothIDs := []int64{firstAccount.ID, secondAccount.ID}
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, nil, &bothIDs, &adminB))
	kept := readVisibleGrant(t, ctx, user.ID, firstAccount.ID)
	require.Equal(t, adminA, *kept.GrantedBy, "保留的分配行不得改写 granted_by")
	require.True(t, initial.CreatedAt.Equal(kept.CreatedAt), "保留的分配行不得刷新 created_at")
	added := readVisibleGrant(t, ctx, user.ID, secondAccount.ID)
	require.Equal(t, adminB, *added.GrantedBy, "新增的分配行记录本次授予人")

	// 幂等重复提交同一集合（甚至是另一个管理员）：两行都不改写。
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, nil, &bothIDs, &adminA))
	require.True(t, initial.CreatedAt.Equal(readVisibleGrant(t, ctx, user.ID, firstAccount.ID).CreatedAt))
	require.True(t, added.CreatedAt.Equal(readVisibleGrant(t, ctx, user.ID, secondAccount.ID).CreatedAt))

	// 仅切换开关（enabled 走值、account_ids 省略）：分配行的来源完全不动。
	off, on := false, true
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &off, nil, &adminA))
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &on, nil, &adminA))
	require.Equal(t, bothIDs, storedVisibleAssignmentIDs(t, ctx, user.ID))
	require.True(t, initial.CreatedAt.Equal(readVisibleGrant(t, ctx, user.ID, firstAccount.ID).CreatedAt))

	// 撤销一个：被撤销的行删除，保留的行继续保有首次授予来源。
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, nil, &firstIDs, &adminB))
	require.Equal(t, firstIDs, storedVisibleAssignmentIDs(t, ctx, user.ID))
	final := readVisibleGrant(t, ctx, user.ID, firstAccount.ID)
	require.Equal(t, adminA, *final.GrantedBy)
	require.True(t, initial.CreatedAt.Equal(final.CreatedAt))
}

// TestUserVisibleAccountRepository_AdminAccountIDsKeepSoftDeletedAssignments 验证
// 管理员界面的 account_ids 是完整权威集合：账号被软删除后分配行仍在，若 id 从
// account_ids 消失，管理员「打开即保存」就会静默撤销这条关系。
//
// 同时确认两条边界：保留既有的软删除分配被接受（不得误删），把已软删除账号当成
// 新分配仍被拒绝（不得误授予）；用户侧可见范围绝不因此放宽。
func TestUserVisibleAccountRepository_AdminAccountIDsKeepSoftDeletedAssignments(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUserVisibleAccountRepository(client)

	user := mustCreateUserWithUniqueEmail(t, client, "")
	otherUser := mustCreateUserWithUniqueEmail(t, client, "")
	live := mustCreateAccount(t, client, &service.Account{Name: "keep-live"})
	removed := mustCreateAccount(t, client, &service.Account{Name: "keep-removed"})
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM user_visible_accounts WHERE user_id IN ($1, $2)`, user.ID, otherUser.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = ANY($1)`, pq.Array([]int64{live.ID, removed.ID}))
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id IN ($1, $2)`, user.ID, otherUser.ID)
	})

	admin := int64(900003)
	otherAdmin := int64(900004)
	enabled := true
	both := []int64{live.ID, removed.ID}
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &enabled, &both, &admin))
	before := readVisibleGrant(t, ctx, user.ID, removed.ID)

	// 账号被软删除：分配行保留，用户侧立即不可见。
	require.NoError(t, client.Account.DeleteOneID(removed.ID).Exec(ctx))
	accounts, total, err := repo.ListVisibleAccounts(ctx, user.ID, service.VisibleAccountFilter{Page: 1, PageSize: 10})
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, live.ID, accounts[0].ID, "软删除账号必须对普通用户不可见")
	require.EqualValues(t, 1, total)

	// 管理员侧：account_ids 必须仍然包含那条分配（详情可以缺失）。
	ids, err := repo.ListAssignedAccountIDs(ctx, user.ID)
	require.NoError(t, err)
	require.Equal(t, both, ids, "管理员权威集合必须包含已被软删除账号的分配 id")

	// 「打开即保存」：把 GET 的 account_ids 原样回传，关系与来源都不变。
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, nil, &ids, &otherAdmin))
	require.Equal(t, both, storedVisibleAssignmentIDs(t, ctx, user.ID), "原样保存不得撤销分配")
	after := readVisibleGrant(t, ctx, user.ID, removed.ID)
	require.Equal(t, admin, *after.GrantedBy, "保留的分配不得改写 granted_by")
	require.True(t, before.CreatedAt.Equal(after.CreatedAt), "保留的分配不得刷新 created_at")

	// 新分配一个已软删除账号（别的用户）：拒绝。
	err = repo.UpdateAccountView(ctx, otherUser.ID, nil, &[]int64{removed.ID}, nil)
	require.ErrorIs(t, err, service.ErrUnknownVisibleAccount, "不得把已软删除账号新分配给别的用户")
	otherIDs, err := repo.ListAssignedAccountIDs(ctx, otherUser.ID)
	require.NoError(t, err)
	require.Empty(t, otherIDs, "被拒绝的更新不得留下任何分配")

	// 撤销后再重新分配同一已软删除账号：仍属新分配，拒绝且整体回滚。
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, nil, &[]int64{live.ID}, nil))
	err = repo.UpdateAccountView(ctx, user.ID, nil, &[]int64{live.ID, removed.ID}, nil)
	require.ErrorIs(t, err, service.ErrUnknownVisibleAccount, "已撤销的软删除账号不能再被新分配")
	require.Equal(t, []int64{live.ID}, storedVisibleAssignmentIDs(t, ctx, user.ID), "失败的更新不得留下部分变更")

	// 未获分配的软删除账号不得被普通用户读到。
	notAssigned := mustCreateAccount(t, client, &service.Account{Name: "keep-not-assigned"})
	t.Cleanup(func() { _, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = $1`, notAssigned.ID) })
	require.NoError(t, client.Account.DeleteOneID(notAssigned.ID).Exec(ctx))
	account, err := repo.GetVisibleAccount(ctx, user.ID, notAssigned.ID)
	require.NoError(t, err)
	require.Nil(t, account)
}

// TestUserVisibleAccountRepository_StatusTrimMatchesServiceVisibility 验证 SQL 谓词与
// service.accountManuallyDisabled 同口径：制表符、换行、NBSP、全角空格等空白变体
// 必须两侧一致。否则 SQL 计数会把 DTO 过滤掉的账号算进分页 total（页面数量与内容
// 不一致），或反过来让 service 判定为可见而 SQL 已隐藏。
func TestUserVisibleAccountRepository_StatusTrimMatchesServiceVisibility(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	repo := NewUserVisibleAccountRepository(client)
	svc := service.NewVisibleAccountService(repo)

	user := mustCreateUserWithUniqueEmail(t, client, "")

	cases := []struct {
		status  string
		visible bool
	}{
		{status: service.StatusActive, visible: true},
		{status: service.StatusError, visible: true},
		{status: service.StatusExpired, visible: true},
		{status: " disabled ", visible: false},
		{status: "\tdisabled\t", visible: false},
		{status: "\nInactive\r", visible: false},
		{status: "\v\fDISABLED\v\f", visible: false},
		{status: "\u00a0disabled", visible: false},
		{status: "\u0085inactive", visible: false},
		{status: "\u3000Disabled", visible: false},
		{status: "\u1680disabled", visible: false},
		// 零宽空格不是空白：Go 与 SQL 都必须保持可见，不得把它当成禁用变体。
		{status: "\u200bdisabled", visible: true},
	}

	accountIDs := make([]int64, 0, len(cases))
	for i, tc := range cases {
		account := mustCreateAccount(t, client, &service.Account{Name: fmt.Sprintf("status-trim-%d", i)})
		_, err := integrationDB.ExecContext(ctx, `UPDATE accounts SET status = $1 WHERE id = $2`, tc.status, account.ID)
		require.NoError(t, err, "写入状态变体")
		accountIDs = append(accountIDs, account.ID)
	}
	t.Cleanup(func() {
		_, _ = integrationDB.Exec(`DELETE FROM user_visible_accounts WHERE user_id = $1`, user.ID)
		_, _ = integrationDB.Exec(`DELETE FROM accounts WHERE id = ANY($1)`, pq.Array(accountIDs))
		_, _ = integrationDB.Exec(`DELETE FROM users WHERE id = $1`, user.ID)
	})

	enabled := true
	require.NoError(t, repo.UpdateAccountView(ctx, user.ID, &enabled, &accountIDs, nil))

	wantVisible := make([]int64, 0, len(cases))
	for i, tc := range cases {
		if tc.visible {
			wantVisible = append(wantVisible, accountIDs[i])
		}
	}

	page, err := svc.List(ctx, user.ID, service.VisibleAccountFilter{Page: 1, PageSize: 100})
	require.NoError(t, err)
	gotVisible := make([]int64, 0, len(page.Items))
	for _, item := range page.Items {
		gotVisible = append(gotVisible, item.ID)
	}
	require.ElementsMatch(t, wantVisible, gotVisible, "空白变体的可见性必须与 service 判定一致")
	require.EqualValues(t, len(wantVisible), page.Total, "分页计数必须与返回内容同口径")
	require.EqualValues(t, len(page.Items), page.Total, "页面计数与 DTO 数量必须一致")

	// 详情与列表同口径：列表隐藏的账号按 ID 读取也必须 404。
	for i, tc := range cases {
		view, err := svc.Get(ctx, user.ID, accountIDs[i])
		if tc.visible {
			require.NoError(t, err, "可见账号 %d 必须能按 ID 读取", accountIDs[i])
			require.Equal(t, accountIDs[i], view.ID)
			continue
		}
		require.ErrorIs(t, err, service.ErrVisibleAccountNotFound, "账号 %d 不得按 ID 读取", accountIDs[i])
	}
}

// mustCreateUserWithUniqueEmail 创建带唯一 email 的测试用户。
// users_email_unique_active 是唯一索引，而共用夹具的 email 只带时间戳：同一个测试里
// 连续创建多个用户时 Windows 的时钟精度不足以区分，会撞唯一约束。
func mustCreateUserWithUniqueEmail(t *testing.T, client *dbent.Client, status string) *service.User {
	t.Helper()

	user := &service.User{Email: "uva-" + uuid.NewString() + "@example.com"}
	if status != "" {
		user.Status = status
	}
	return mustCreateUser(t, client, user)
}

// lockWaiterState 是一次锁等待观测：
//   - onUserRowLock 表示有本库会话正阻塞在含 FOR UPDATE 的语句上（即 users 行锁）；
//   - anyWait 表示有会话在等任意锁。
type lockWaiterState struct {
	onUserRowLock bool
	anyWait       bool
}

// observeLockWaiters 代替固定 sleep 观测「后到者已阻塞」这一确定状态。
// 两种等待都要区分：修复前后到者也会阻塞，但阻塞在 DELETE 上——它的判定快照
// 已经形成，所以仍会把两次分配并存下来。
func observeLockWaiters(ctx context.Context, t *testing.T) lockWaiterState {
	t.Helper()

	var onUserRowLock, anyWait int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE query ILIKE '%for update%'),
			COUNT(*)
		FROM pg_stat_activity
		WHERE datname = current_database()
		  AND state = 'active'
		  AND wait_event_type = 'Lock'
	`).Scan(&onUserRowLock, &anyWait), "读取 pg_stat_activity")
	return lockWaiterState{onUserRowLock: onUserRowLock > 0, anyWait: anyWait > 0}
}

// storedVisibleAssignmentIDs 直接读表返回该用户当前的分配 id（升序）。
func storedVisibleAssignmentIDs(t *testing.T, ctx context.Context, userID int64) []int64 {
	t.Helper()

	rows, err := integrationDB.QueryContext(ctx,
		`SELECT account_id FROM user_visible_accounts WHERE user_id = $1 ORDER BY account_id`, userID)
	require.NoError(t, err, "读取 user_visible_accounts")
	defer func() { _ = rows.Close() }()

	ids := []int64{}
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// visibleGrantRow 是一条分配行的授予来源。
type visibleGrantRow struct {
	GrantedBy *int64
	CreatedAt time.Time
}

func readVisibleGrant(t *testing.T, ctx context.Context, userID, accountID int64) visibleGrantRow {
	t.Helper()

	var row visibleGrantRow
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT granted_by, created_at FROM user_visible_accounts WHERE user_id = $1 AND account_id = $2`,
		userID, accountID).Scan(&row.GrantedBy, &row.CreatedAt), "读取分配行的授予来源")
	return row
}
