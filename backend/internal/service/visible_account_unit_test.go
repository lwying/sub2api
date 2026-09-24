//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// visibleAccountRepoFake 是 VisibleAccountRepository 的内存实现，
// 用于验证服务层的范围与脱敏规则（HTTP 级验收见 handler 包测试）。
type visibleAccountRepoFake struct {
	enabled    map[int64]bool
	assigned   map[int64][]int64
	accounts   map[int64]*Account
	userExists map[int64]bool
	userActive map[int64]bool
}

func newVisibleAccountRepoFake() *visibleAccountRepoFake {
	return &visibleAccountRepoFake{
		enabled:    map[int64]bool{},
		assigned:   map[int64][]int64{},
		accounts:   map[int64]*Account{},
		userExists: map[int64]bool{},
		userActive: map[int64]bool{},
	}
}

func (f *visibleAccountRepoFake) addUser(id int64, enabled bool, active bool) {
	f.userExists[id] = true
	f.userActive[id] = active
	f.enabled[id] = enabled
}

func (f *visibleAccountRepoFake) addAccount(account *Account) {
	f.accounts[account.ID] = account
}

func (f *visibleAccountRepoFake) assign(userID int64, accountIDs ...int64) {
	f.assigned[userID] = append([]int64{}, accountIDs...)
}

func (f *visibleAccountRepoFake) GetAccountViewEnabled(_ context.Context, userID int64) (bool, error) {
	if !f.userExists[userID] || !f.userActive[userID] {
		return false, nil
	}
	return f.enabled[userID], nil
}

func (f *visibleAccountRepoFake) UpdateAccountView(
	_ context.Context,
	userID int64,
	enabled *bool,
	accountIDs *[]int64,
	_ *int64,
) error {
	if !f.userExists[userID] {
		return ErrAccountViewUserNotFound
	}
	// 先校验再写入：任何失败都不得留下部分变更（与真实事务实现一致）。
	if accountIDs != nil {
		for _, id := range *accountIDs {
			if _, ok := f.accounts[id]; !ok {
				return ErrUnknownVisibleAccount
			}
		}
	}
	if enabled != nil {
		f.enabled[userID] = *enabled
	}
	if accountIDs != nil {
		f.assigned[userID] = append([]int64{}, (*accountIDs)...)
	}
	return nil
}

func (f *visibleAccountRepoFake) ListAssignedAccountSummaries(_ context.Context, userID int64) ([]AssignedVisibleAccount, error) {
	out := []AssignedVisibleAccount{}
	for _, id := range f.assigned[userID] {
		account, ok := f.accounts[id]
		if !ok {
			continue
		}
		out = append(out, AssignedVisibleAccount{
			ID: account.ID, Name: account.Name, Platform: account.Platform,
			Type: account.Type, Status: account.Status,
		})
	}
	return out, nil
}

func (f *visibleAccountRepoFake) ListVisibleAccounts(
	_ context.Context,
	userID int64,
	filter VisibleAccountFilter,
) ([]*Account, int64, error) {
	visible := f.visibleAccounts(userID)
	total := int64(len(visible))
	start := (filter.Page - 1) * filter.PageSize
	if start >= len(visible) {
		return []*Account{}, total, nil
	}
	end := start + filter.PageSize
	if end > len(visible) {
		end = len(visible)
	}
	return visible[start:end], total, nil
}

func (f *visibleAccountRepoFake) GetVisibleAccount(_ context.Context, userID, accountID int64) (*Account, error) {
	for _, account := range f.visibleAccounts(userID) {
		if account.ID == accountID {
			return account, nil
		}
	}
	return nil, nil
}

// visibleAccounts 复刻真实仓储的约束：能力开启 + 显式分配 + 账号可见状态。
func (f *visibleAccountRepoFake) visibleAccounts(userID int64) []*Account {
	if !f.userExists[userID] || !f.userActive[userID] || !f.enabled[userID] {
		return nil
	}
	out := []*Account{}
	for _, id := range f.assigned[userID] {
		account, ok := f.accounts[id]
		if !ok || accountManuallyDisabled(account.Status) {
			continue
		}
		out = append(out, account)
	}
	return out
}

func TestVisibleAccountMaskingNeverLeaksRawValue(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", maskVisibleAccountEmail(""))
	require.Equal(t, "", maskVisibleAccountIdentity("   "))

	// 短值整体替换，不原样输出（含 4~6 位的「保留首尾等于原样」情形）。
	require.Equal(t, "***@***", maskVisibleAccountEmail("a@b.co"))
	require.Equal(t, "***@***", maskVisibleAccountEmail("ab@c.io"))
	require.Equal(t, visibleAccountMaskPlaceholder, maskVisibleAccountIdentity("abc"))
	require.Equal(t, visibleAccountMaskPlaceholder, maskVisibleAccountIdentity("1234"))
	require.Equal(t, visibleAccountMaskPlaceholder, maskVisibleAccountIdentity("123456"))

	// 常规长度：邮箱只保留首字符，其余为占位符。
	require.Equal(t, "a***@e***", maskVisibleAccountEmail("alice@example.com"))
	require.Equal(t, "al***77", maskVisibleAccountIdentity("alice77"))
	require.Equal(t, "12***89", maskVisibleAccountIdentity("123456789"))

	// 无论何种输入，掩码结果都不能等于原文。
	raws := []string{
		"a@b.co", "ab@c.io", "alice@example.com", "x@y.z",
		"1234", "12345", "123456", "ABCDEF", "user@sub.domain.example.org",
		"upstream-account-id-0001", "1", "ab@c.d", "a@b",
	}
	for _, raw := range raws {
		for _, masked := range []string{maskVisibleAccountEmail(raw), maskVisibleAccountIdentity(raw)} {
			require.NotEqual(t, raw, masked, "masked output must differ from raw value")
		}
	}

	// 4~6 位的短标识一律完全不显示：不得出现原文的任何一个字符组合。
	for _, raw := range []string{"abcd", "1234", "12345", "123456"} {
		require.Equal(t, visibleAccountMaskPlaceholder, maskVisibleAccountIdentity(raw))
	}
}

func TestVisibleAccountManuallyDisabledStatuses(t *testing.T) {
	t.Parallel()

	// 手动禁用：历史 disabled 与编辑器 inactive（含大小写/空白差异）。
	for _, status := range []string{"disabled", "inactive", "DISABLED", "Inactive", " disabled "} {
		require.True(t, accountManuallyDisabled(status), "status %q must be hidden", status)
	}

	// 故障/过期/调度态不是手动禁用，必须保持可见。
	for _, status := range []string{"active", "error", "expired", "", "rate_limited", "overloaded"} {
		require.False(t, accountManuallyDisabled(status), "status %q must stay visible", status)
	}
}

func TestBuildVisibleAccountViewExcludesSensitiveFields(t *testing.T) {
	t.Parallel()

	account := &Account{
		ID:       42,
		Name:     "sentinel-account-name",
		Platform: "anthropic",
		Type:     "oauth",
		Status:   StatusError,
		Credentials: map[string]any{
			"email":                 "owner@example.com",
			"username":              "owner-username",
			"chatgpt_account_id":    "acct-1234567890",
			"access_token":          "sentinel-access-token",
			"refresh_token":         "sentinel-refresh-token",
			"api_key":               "sk-sentinel",
			"base_url":              "https://sentinel.example.com",
			"model_mapping":         map[string]any{"a": "b"},
			"fallback_credit_token": "sentinel-fallback-credit-token",
		},
		Extra: map[string]any{
			"notes":         "sentinel-extra-notes",
			"proxy":         "http://sentinel-proxy",
			"error_message": "sentinel-error",
		},
		ErrorMessage: "sentinel-error-message",
	}

	view := BuildVisibleAccountView(account)

	require.Equal(t, int64(42), view.ID)
	require.Equal(t, "anthropic", view.Platform)
	require.Equal(t, "oauth", view.AccountType)
	require.Equal(t, "o***@e***", view.EmailMasked)
	require.Equal(t, "ow***me", view.UsernameMasked)
	require.Equal(t, "ac***90", view.UpstreamAccountIDMasked)
	require.NotContains(t, view.EmailMasked+view.UsernameMasked+view.UpstreamAccountIDMasked, "sentinel")

	// 视图不含名称/凭据/备注等字段：结构体里根本没有这些字段，
	// 这里额外确认脱敏结果没有把原文带出来。
	require.NotContains(t, view.EmailMasked, "owner")
	require.NotContains(t, view.UsernameMasked, "owner-username")
	require.NotContains(t, view.UpstreamAccountIDMasked, "acct-1234567890")
}

func TestVisibleAccountServiceListAndGetScope(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newVisibleAccountRepoFake()
	repo.addUser(1, true, true)  // 已授权
	repo.addUser(2, true, true)  // 已授权但未分配账号
	repo.addUser(3, false, true) // 未授权
	repo.addUser(4, true, false) // 已禁用用户
	repo.addAccount(&Account{ID: 10, Name: "alpha", Platform: "anthropic", Type: "oauth", Status: StatusActive})
	repo.addAccount(&Account{ID: 11, Name: "beta", Platform: "openai", Type: "apikey", Status: StatusError})
	repo.addAccount(&Account{ID: 12, Name: "gamma", Platform: "openai", Type: "apikey", Status: StatusDisabled})
	repo.addAccount(&Account{ID: 13, Name: "delta", Platform: "openai", Type: "apikey", Status: StatusInactive})
	repo.assign(1, 10, 11, 12, 13)
	// 用户 2 已授权但未分配任何账号。

	svc := NewVisibleAccountService(repo)

	// 已授权用户只看到未被手动禁用的已分配账号（error 仍可见）。
	page, err := svc.List(ctx, 1, VisibleAccountFilter{})
	require.NoError(t, err)
	require.Equal(t, int64(2), page.Total)
	require.Len(t, page.Items, 2)
	require.Equal(t, int64(10), page.Items[0].ID)
	require.Equal(t, int64(11), page.Items[1].ID)

	// 未授权用户：能力未开启。
	_, err = svc.List(ctx, 3, VisibleAccountFilter{})
	require.ErrorIs(t, err, ErrAccountViewDisabled)

	// 已禁用的用户：能力判断 fail-closed。
	_, err = svc.List(ctx, 4, VisibleAccountFilter{})
	require.ErrorIs(t, err, ErrAccountViewDisabled)

	// 详情：未分配 / 手动禁用 / 不存在的账号返回同一 404。
	for _, id := range []int64{12, 13, 999} {
		_, err := svc.Get(ctx, 1, id)
		require.ErrorIs(t, err, ErrVisibleAccountNotFound, "account %d must be invisible", id)
	}

	// 用户 2 未获分配，即使能力开启也不可见。
	_, err = svc.Get(ctx, 2, 10)
	require.ErrorIs(t, err, ErrVisibleAccountNotFound)

	view, err := svc.Get(ctx, 1, 10)
	require.NoError(t, err)
	require.Equal(t, int64(10), view.ID)
}

func TestVisibleAccountServiceAdminUpdateIsAtomicAndDefaultOff(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newVisibleAccountRepoFake()
	repo.addUser(7, false, true)
	repo.addAccount(&Account{ID: 21, Name: "alpha", Platform: "anthropic", Type: "oauth", Status: StatusActive})
	svc := NewVisibleAccountService(repo)

	// 默认零个、默认关闭。
	view, err := svc.AdminView(ctx, 7)
	require.NoError(t, err)
	require.False(t, view.Enabled)
	require.Empty(t, view.AccountIDs)

	// 分配账号不会隐式打开开关。
	enabled := false
	ids := []int64{21}
	view, err = svc.AdminUpdate(ctx, 7, &enabled, &ids, nil)
	require.NoError(t, err)
	require.False(t, view.Enabled)
	require.Equal(t, []int64{21}, view.AccountIDs)

	// 用户此时仍看不到账号。
	_, err = svc.List(ctx, 7, VisibleAccountFilter{})
	require.ErrorIs(t, err, ErrAccountViewDisabled)

	// 打开开关后可见。
	enabled = true
	view, err = svc.AdminUpdate(ctx, 7, &enabled, nil, nil)
	require.NoError(t, err)
	require.True(t, view.Enabled)
	page, err := svc.List(ctx, 7, VisibleAccountFilter{})
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total)

	// 未知账号 id 被拒绝且不影响既有分配。
	bad := []int64{21, 999}
	_, err = svc.AdminUpdate(ctx, 7, nil, &bad, nil)
	require.ErrorIs(t, err, ErrUnknownVisibleAccount)
	page, err = svc.List(ctx, 7, VisibleAccountFilter{})
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total)

	// 原子性：同一次请求里「打开开关 + 含未知账号」必须整体失败，
	// 不能留下开关已打开但分配未落库的中间状态。
	repo.addUser(8, false, true)
	opened := true
	_, err = svc.AdminUpdate(ctx, 8, &opened, &bad, nil)
	require.ErrorIs(t, err, ErrUnknownVisibleAccount)
	view, err = svc.AdminView(ctx, 8)
	require.NoError(t, err)
	require.False(t, view.Enabled, "failed update must not flip the capability flag")
	require.Empty(t, view.AccountIDs)
	_, err = svc.List(ctx, 8, VisibleAccountFilter{})
	require.ErrorIs(t, err, ErrAccountViewDisabled)

	// 非法 id（0）必须报错，不能被静默丢弃成「清空分配」。
	invalid := []int64{0}
	_, err = svc.AdminUpdate(ctx, 7, nil, &invalid, nil)
	require.ErrorIs(t, err, ErrUnknownVisibleAccount)
	view, err = svc.AdminView(ctx, 7)
	require.NoError(t, err)
	require.Equal(t, []int64{21}, view.AccountIDs)

	// 撤销后立即不可见。
	empty := []int64{}
	view, err = svc.AdminUpdate(ctx, 7, nil, &empty, nil)
	require.NoError(t, err)
	require.Empty(t, view.AccountIDs)
	_, err = svc.Get(ctx, 7, 21)
	require.ErrorIs(t, err, ErrVisibleAccountNotFound)
}
