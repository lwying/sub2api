//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
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
	// 已被软删除的账号不能再被分配。
	err = repo.UpdateAccountView(ctx, user.ID, nil, &ids, nil)
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
