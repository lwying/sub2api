//go:build unit

package service

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
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

// GetStoredAccountViewEnabled 复刻管理员口径：只要求用户存在且未软删除，
// 已禁用用户仍返回其存储值（面向用户的读取则 fail-closed）。
func (f *visibleAccountRepoFake) GetStoredAccountViewEnabled(_ context.Context, userID int64) (bool, error) {
	if !f.userExists[userID] {
		return false, ErrAccountViewUserNotFound
	}
	return f.enabled[userID], nil
}

// ListAssignedAccountIDs 返回全部已存储的分配 id（升序，含对应账号已不可见的分配）。
func (f *visibleAccountRepoFake) ListAssignedAccountIDs(_ context.Context, userID int64) ([]int64, error) {
	if !f.userExists[userID] {
		return []int64{}, nil
	}
	out := append([]int64{}, f.assigned[userID]...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
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

// ListVisibleAccountsByIDs 复刻批量范围查询：只在授权集合内命中，未授权 id 静默缺席。
func (f *visibleAccountRepoFake) ListVisibleAccountsByIDs(_ context.Context, userID int64, accountIDs []int64) ([]*Account, error) {
	want := make(map[int64]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		want[id] = struct{}{}
	}
	out := []*Account{}
	for _, account := range f.visibleAccounts(userID) {
		if _, ok := want[account.ID]; ok {
			out = append(out, account)
		}
	}
	return out, nil
}

// ListVisibleAccountGroups 只从可见账号所属分组派生并去重（不是全局分组目录）。
func (f *visibleAccountRepoFake) ListVisibleAccountGroups(_ context.Context, userID int64) ([]VisibleAccountGroup, error) {
	seen := map[int64]struct{}{}
	out := []VisibleAccountGroup{}
	for _, account := range f.visibleAccounts(userID) {
		for _, group := range account.Groups {
			if _, ok := seen[group.ID]; ok {
				continue
			}
			seen[group.ID] = struct{}{}
			out = append(out, VisibleAccountGroup{
				ID: group.ID, Name: group.Name, Platform: group.Platform,
				SubscriptionType: group.SubscriptionType,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// visibleAccounts 复刻真实仓储的约束：能力开启 + 显式分配 + 账号可见状态。
func (f *visibleAccountRepoFake) visibleAccounts(userID int64) []*Account {
	if !f.userExists[userID] || !f.userActive[userID] || !f.enabled[userID] {
		return nil
	}
	out := []*Account{}
	for _, id := range f.assigned[userID] {
		account, ok := f.accounts[id]
		if !ok {
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

// TestVisibleAccountServiceDisabledAccountsStayVisible 验证本能力的新口径：
// 管理员已授权即保持可见，即使账号被手动停用（历史 disabled / 编辑器 inactive，
// 含大小写与首尾空白变体），状态按原样展示。
//
// 撤权、账号软删除、用户禁用或资格关闭仍然即时不可见（见其它用例）。
func TestVisibleAccountServiceDisabledAccountsStayVisible(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, status := range []string{
		"disabled", "inactive", "DISABLED", "Inactive", " disabled ",
		"\tdisabled\t", "\nInactive\r", "\v\fDISABLED\v\f",
		"\u00a0disabled", "\u0085inactive", "\u3000Disabled", "\u1680Disabled",
		"active", "error", "expired",
	} {
		repo := newVisibleAccountRepoFake()
		repo.addUser(1, true, true)
		repo.addAccount(&Account{ID: 51, Name: "paused", Platform: "anthropic", Type: "oauth", Status: status})
		repo.assign(1, 51)
		svc := NewVisibleAccountService(repo)

		page, err := svc.List(ctx, 1, VisibleAccountFilter{})
		require.NoError(t, err)
		require.Equal(t, int64(1), page.Total, "status %q must stay visible", status)
		require.Len(t, page.Items, 1)
		require.Equal(t, status, page.Items[0].Status)

		view, err := svc.Get(ctx, 1, 51)
		require.NoError(t, err)
		require.Equal(t, status, view.Status)
	}
}

// TestVisibleAccountServiceAdminViewKeepsStoredFlag 验证管理员视图与用户视图的
// 读取口径分工：面向用户的读取对已禁用用户 fail-closed，而管理员视图必须读
// 「存储值」，否则界面会把存储的 true 显示成 false，管理员原样保存一次就把
// 能力真正关掉（一次误操作即永久撤销）。不存在的用户 GET 与 PUT 同为 404。
func TestVisibleAccountServiceAdminViewKeepsStoredFlag(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newVisibleAccountRepoFake()
	repo.addUser(1, true, false) // 已禁用用户，存储的查看能力为 true
	repo.addAccount(&Account{ID: 31, Name: "alpha", Platform: "anthropic", Type: "oauth", Status: StatusActive})
	repo.assign(1, 31)
	svc := NewVisibleAccountService(repo)

	// 面向普通用户：已禁用的用户即使存储为 true 也必须 fail-closed。
	_, err := svc.List(ctx, 1, VisibleAccountFilter{})
	require.ErrorIs(t, err, ErrAccountViewDisabled)

	// 面向管理员：读存储值，不得把 true 显示成 false。
	view, err := svc.AdminView(ctx, 1)
	require.NoError(t, err)
	require.True(t, view.Enabled, "已禁用用户的管理员视图必须显示存储的开关值")
	require.Equal(t, []int64{31}, view.AccountIDs)

	// 管理员界面「原样保存」（GET 的 enabled 与 account_ids 原封回传）不得关闭能力。
	saved, err := svc.AdminUpdate(ctx, 1, &view.Enabled, &view.AccountIDs, nil)
	require.NoError(t, err)
	require.True(t, saved.Enabled, "原样保存不得把存储的 true 改写成 false")
	require.Equal(t, []int64{31}, saved.AccountIDs)

	// 用户仍然看不到账号：管理员口径没有放宽用户侧可见范围。
	require.Empty(t, repo.visibleAccounts(1))

	// 不存在的用户：GET 与 PUT 同为 USER_NOT_FOUND。
	_, err = svc.AdminView(ctx, 999)
	require.ErrorIs(t, err, ErrAccountViewUserNotFound)
	_, err = svc.AdminUpdate(ctx, 999, &view.Enabled, nil, nil)
	require.ErrorIs(t, err, ErrAccountViewUserNotFound)
}

// TestVisibleAccountServiceAdminAccountIDsKeepInvisibleAssignments 验证管理员视图的
// account_ids 是完整权威集合：只要分配行还在（例如对应账号已被手动禁用或已软删除），
// id 就不得从 account_ids 消失——否则管理员在界面上原样保存会静默撤销这条关系。
func TestVisibleAccountServiceAdminAccountIDsKeepInvisibleAssignments(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newVisibleAccountRepoFake()
	repo.addUser(1, true, true)
	// 41 正常可见；42 已手动禁用（用户看不到，但分配行仍在）。
	repo.addAccount(&Account{ID: 41, Name: "live", Platform: "openai", Type: "apikey", Status: StatusActive})
	repo.addAccount(&Account{ID: 42, Name: "hidden", Platform: "openai", Type: "apikey", Status: StatusDisabled})
	repo.assign(1, 42, 41)
	// 43 已被软删除：连详情都读不到（仓储的详情列表会跳过它），但分配行仍在。
	repo.addAccount(&Account{ID: 43, Name: "deleted", Platform: "openai", Type: "apikey", Status: StatusActive})
	repo.assign(1, 43, 42, 41)
	delete(repo.accounts, 43)
	svc := NewVisibleAccountService(repo)

	page, err := svc.List(ctx, 1, VisibleAccountFilter{})
	require.NoError(t, err)
	require.Len(t, page.Items, 2, "用户看到全部已分配账号（含手动停用）")

	view, err := svc.AdminView(ctx, 1)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{41, 42, 43}, view.AccountIDs,
		"管理员集合必须包含没有详情的分配 id，否则界面原样保存会静默撤销它")
	require.Len(t, view.Accounts, 2, "详情只覆盖仍可读的账号")
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
	require.Equal(t, "sentinel-account-name", view.Name, "名称现在是可展示字段")
	require.Equal(t, "anthropic", view.Platform)
	require.Equal(t, "oauth", view.AccountType)
	require.Equal(t, StatusError, view.Status)
	require.Empty(t, view.Groups)
	require.Nil(t, view.QuotaLimit)
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

	// 已授权用户看到全部已分配账号（含手动停用与故障/过期）。
	page, err := svc.List(ctx, 1, VisibleAccountFilter{})
	require.NoError(t, err)
	require.Equal(t, int64(4), page.Total)
	require.Len(t, page.Items, 4)
	require.Equal(t, int64(10), page.Items[0].ID)
	require.Equal(t, int64(11), page.Items[1].ID)
	require.Equal(t, int64(12), page.Items[2].ID)
	require.Equal(t, int64(13), page.Items[3].ID)

	// 未授权用户：能力未开启。
	_, err = svc.List(ctx, 3, VisibleAccountFilter{})
	require.ErrorIs(t, err, ErrAccountViewDisabled)

	// 已禁用的用户：能力判断 fail-closed。
	_, err = svc.List(ctx, 4, VisibleAccountFilter{})
	require.ErrorIs(t, err, ErrAccountViewDisabled)

	// 详情：已分配账号（含手动停用）全部可见；未分配/不存在的账号返回同一 404。
	for _, id := range []int64{10, 11, 12, 13} {
		view, getErr := svc.Get(ctx, 1, id)
		require.NoError(t, getErr, "account %d must be visible", id)
		require.Equal(t, id, view.ID)
	}
	_, err = svc.Get(ctx, 1, 999)
	require.ErrorIs(t, err, ErrVisibleAccountNotFound)

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

// TestUnknownVisibleAccountErrorCarriesBoundedMissingIDs 覆盖「整批拒绝时指出具体失败项」的
// 错误契约：失败仍是 400 UNKNOWN_ACCOUNT、错误链上仍匹配既有哨兵（既有调用方与客户端契约
// 不变），但 metadata 给出被拒账号的数字 id，使管理员界面能就地高亮修正，而不是把整批
// 失败笼统归因于「某个账号不存在」。id 去重升序、数量有界，且不改动包级哨兵。
func TestUnknownVisibleAccountErrorCarriesBoundedMissingIDs(t *testing.T) {
	t.Parallel()

	// 单个失效（已删除）id：必须能读到具体是哪一个。
	err := UnknownVisibleAccountError([]int64{4242})
	require.ErrorIs(t, err, ErrUnknownVisibleAccount)
	require.Equal(t, 400, int(err.Code))
	require.Equal(t, "UNKNOWN_ACCOUNT", err.Reason)
	require.Equal(t, "4242", err.Metadata[visibleAccountInvalidIDsMetadataKey])
	require.Equal(t, "1", err.Metadata[visibleAccountInvalidCountMetadataKey])

	// 重复与乱序：去重升序，同一批输入得到稳定输出。
	err = UnknownVisibleAccountError([]int64{12, 9, 12, 9, 11})
	require.Equal(t, "9,11,12", err.Metadata[visibleAccountInvalidIDsMetadataKey])
	require.Equal(t, "3", err.Metadata[visibleAccountInvalidCountMetadataKey])
	// 只承载数字 id，不携带账号名称、凭据或其它账号内部配置。
	require.Len(t, err.Metadata, 2)

	// 空输入：仍是一次明确的失败，只是没有可指的失败项。
	err = UnknownVisibleAccountError(nil)
	require.ErrorIs(t, err, ErrUnknownVisibleAccount)
	require.Equal(t, "", err.Metadata[visibleAccountInvalidIDsMetadataKey])
	require.Equal(t, "0", err.Metadata[visibleAccountInvalidCountMetadataKey])

	// 上限：一次批量添加可能提交上百个账号，错误载荷必须有界，总数另行说明。
	oversized := make([]int64, 0, maxReportedInvalidVisibleAccountIDs+7)
	for i := 0; i < maxReportedInvalidVisibleAccountIDs+7; i++ {
		oversized = append(oversized, int64(i+1))
	}
	err = UnknownVisibleAccountError(oversized)
	reported := strings.Split(err.Metadata[visibleAccountInvalidIDsMetadataKey], ",")
	require.Len(t, reported, maxReportedInvalidVisibleAccountIDs)
	require.Equal(t, "1", reported[0])
	require.Equal(t, strconv.Itoa(maxReportedInvalidVisibleAccountIDs),
		reported[maxReportedInvalidVisibleAccountIDs-1])
	require.Equal(t, strconv.Itoa(len(oversized)),
		err.Metadata[visibleAccountInvalidCountMetadataKey])

	// 包级哨兵不得被污染：构造出的错误必须与既有哨兵相互独立。
	require.Nil(t, ErrUnknownVisibleAccount.Metadata)
	require.False(t, err == ErrUnknownVisibleAccount)
}

// visibleAccountUsageFake 是 VisibleAccountUsageReader 的内存替身。
type visibleAccountUsageFake struct {
	todayBatch   map[int64]*WindowStats
	todayErr     error
	todayCalls   [][]int64
	passive      map[int64]*UsageInfo
	passiveErr   map[int64]error
	passiveCalls int
	stats        *usagestats.AccountUsageStatsResponse
	statsErr     error
	statsCalled  bool
}

func (f *visibleAccountUsageFake) GetTodayStats(_ context.Context, accountID int64) (*WindowStats, error) {
	return f.todayBatch[accountID], f.todayErr
}

func (f *visibleAccountUsageFake) GetTodayStatsBatch(_ context.Context, accountIDs []int64) (map[int64]*WindowStats, error) {
	f.todayCalls = append(f.todayCalls, append([]int64{}, accountIDs...))
	if f.todayErr != nil {
		return nil, f.todayErr
	}
	out := make(map[int64]*WindowStats, len(accountIDs))
	for _, id := range accountIDs {
		if stats, ok := f.todayBatch[id]; ok {
			out[id] = stats
		}
	}
	return out, nil
}

func (f *visibleAccountUsageFake) GetAccountUsageStats(_ context.Context, _ int64, _, _ time.Time) (*usagestats.AccountUsageStatsResponse, error) {
	f.statsCalled = true
	return f.stats, f.statsErr
}

func (f *visibleAccountUsageFake) GetPassiveUsage(_ context.Context, accountID int64) (*UsageInfo, error) {
	f.passiveCalls++
	if err, ok := f.passiveErr[accountID]; ok {
		return nil, err
	}
	return f.passive[accountID], nil
}

// visibleAccountBatchUsageFake 在基础替身之上实现批量被动读取，并记录批量调用次数，
// 用于验证 RuntimeBatch 走批量路径而不是逐账号 GetPassiveUsage。
type visibleAccountBatchUsageFake struct {
	*visibleAccountUsageFake
	batchCalls int
	batchErr   error
}

func (f *visibleAccountBatchUsageFake) GetPassiveUsageBatch(_ context.Context, accounts []*Account) (map[int64]*UsageInfo, error) {
	f.batchCalls++
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := make(map[int64]*UsageInfo, len(accounts))
	for _, account := range accounts {
		if usage, ok := f.passive[account.ID]; ok {
			out[account.ID] = usage
		}
	}
	return out, nil
}

// visibleAccountConcurrencyFake 是 VisibleAccountConcurrencyReader 的内存替身。
type visibleAccountConcurrencyFake struct {
	counts map[int64]int
	calls  int
}

func (f *visibleAccountConcurrencyFake) GetAccountConcurrencyBatch(_ context.Context, accountIDs []int64) (map[int64]int, error) {
	f.calls++
	out := make(map[int64]int, len(accountIDs))
	for _, id := range accountIDs {
		out[id] = f.counts[id]
	}
	return out, nil
}

// TestVisibleAccountServiceRuntimeBatchScopesCapsAndAvoidsActiveFetch 覆盖运行时批量的
// 关键约束：只在授权集合内返回、未授权 id 静默缺席（不暴露存在性）、超过一页上限直接 400、
// 今日统计与并发各一次批量读取、被动用量只对 Anthropic OAuth/Setup-Token 账号读取。
func TestVisibleAccountServiceRuntimeBatchScopesCapsAndAvoidsActiveFetch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sessionStart := time.Now().Add(-time.Hour)
	sessionEnd := time.Now().Add(time.Hour)
	repo := newVisibleAccountRepoFake()
	repo.addUser(1, true, true)
	repo.addAccount(&Account{
		ID: 10, Name: "claude", Platform: "anthropic", Type: "oauth", Status: StatusActive,
		SessionWindowStart: &sessionStart, SessionWindowEnd: &sessionEnd,
	})
	repo.addAccount(&Account{ID: 11, Name: "openai", Platform: "openai", Type: "apikey", Status: StatusActive})
	repo.addAccount(&Account{ID: 12, Name: "paused", Platform: "openai", Type: "apikey", Status: StatusDisabled})
	repo.assign(1, 10, 11, 12)
	delete(repo.accounts, 12) // 12 已被软删除：分配仍在，但不在可见集合里

	usageReader := &visibleAccountUsageFake{
		todayBatch: map[int64]*WindowStats{
			10: {Requests: 5, Tokens: 100},
			11: {Requests: 2, Tokens: 40},
		},
		passive: map[int64]*UsageInfo{
			10: {Source: "passive", FiveHour: &UsageProgress{Utilization: 15}, UpdatedAt: visibleAccountPtrTime(time.Now())},
		},
		passiveErr: map[int64]error{},
	}
	concurrencyReader := &visibleAccountConcurrencyFake{counts: map[int64]int{10: 3, 11: 1}}

	svc := NewVisibleAccountService(repo)
	svc.SetUsageReader(usageReader)
	svc.SetConcurrencyReader(concurrencyReader)

	result, err := svc.RuntimeBatch(ctx, 1, []int64{10, 11, 999, 0, 12})
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{10, 11}, keysOfVisibleRuntime(result),
		"只返回授权集合内命中的账号，未授权/已删除 id 静默缺席")

	require.NotNil(t, result[10].TodayStats)
	require.Equal(t, int64(5), result[10].TodayStats.Requests)
	require.NotNil(t, result[10].CurrentConcurrency)
	require.Equal(t, 3, *result[10].CurrentConcurrency)
	require.NotNil(t, result[10].Usage, "Anthropic oauth 账号读取被动用量")
	require.Equal(t, "passive", result[10].Usage.Source)

	require.NotNil(t, result[11].TodayStats)
	require.Nil(t, result[11].Usage, "非 Anthropic 账号不读取用量，保持未知")

	require.Len(t, usageReader.todayCalls, 1, "今日统计必须一次批量读取")
	require.ElementsMatch(t, []int64{10, 11}, usageReader.todayCalls[0])
	require.Equal(t, 1, concurrencyReader.calls, "并发必须一次批量读取")

	// 超过一页上限：直接拒绝，而不是静默截断。
	oversized := make([]int64, maxVisibleAccountBatchIDs+1)
	for i := range oversized {
		oversized[i] = int64(i + 1)
	}
	_, err = svc.RuntimeBatch(ctx, 1, oversized)
	require.ErrorIs(t, err, ErrVisibleAccountBatchTooLarge)

	// 未开启能力：拒绝。
	_, err = svc.RuntimeBatch(ctx, 3, []int64{10})
	require.ErrorIs(t, err, ErrAccountViewDisabled)
}

// TestVisibleAccountServiceRuntimeBatchUsesPassiveBatch 验证批量运行期读取
// 不再逐账号走 GetPassiveUsage（后者内部会 GetByID 并逐账号查窗口统计）。
// 读取器实现批量扩展时，一页多个 Anthropic 账号只触发一次批量调用，零次单账号读取。
func TestVisibleAccountServiceRuntimeBatchUsesPassiveBatch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newVisibleAccountRepoFake()
	repo.addUser(1, true, true)
	sessionStart := time.Now().Add(-time.Hour)
	sessionEnd := time.Now().Add(time.Hour)
	repo.addAccount(&Account{
		ID: 10, Name: "a", Platform: "anthropic", Type: "oauth", Status: StatusActive,
		SessionWindowStart: &sessionStart, SessionWindowEnd: &sessionEnd,
	})
	repo.addAccount(&Account{
		ID: 11, Name: "b", Platform: "anthropic", Type: "setup-token", Status: StatusActive,
		SessionWindowStart: &sessionStart, SessionWindowEnd: &sessionEnd,
	})
	repo.addAccount(&Account{ID: 12, Name: "c", Platform: "openai", Type: "apikey", Status: StatusActive})
	repo.addAccount(&Account{ID: 13, Name: "d", Platform: "anthropic", Type: "oauth", Status: StatusActive})
	repo.assign(1, 10, 11, 12, 13)

	base := &visibleAccountUsageFake{
		passive: map[int64]*UsageInfo{
			10: {Source: "passive", FiveHour: &UsageProgress{Utilization: 10}},
			11: {Source: "passive", FiveHour: &UsageProgress{Utilization: 20}},
			// 未采样：读取器给出 estimate 合成的空 5h，只读层必须判为未知。
			13: {Source: "passive", FiveHour: &UsageProgress{Utilization: 0}},
		},
		passiveErr: map[int64]error{},
	}
	reader := &visibleAccountBatchUsageFake{visibleAccountUsageFake: base}
	svc := NewVisibleAccountService(repo)
	svc.SetUsageReader(reader)

	result, err := svc.RuntimeBatch(ctx, 1, []int64{10, 11, 12, 13})
	require.NoError(t, err)
	require.NotNil(t, result[10].Usage)
	require.NotNil(t, result[11].Usage)
	require.Nil(t, result[12].Usage, "非 Anthropic 账号不进入批量被动读取")
	require.Nil(t, result[13].Usage, "未采样账号不得显示成 5h=0")
	require.Equal(t, 1, reader.batchCalls, "整页只触发一次批量被动读取")
	require.Zero(t, base.passiveCalls, "批量路径不得回退到逐账号 GetPassiveUsage")
}

// TestVisibleAccountServiceRuntimeBatchFallsBackWhenBatchUnsupported 验证读取器未实现
// 批量扩展时仍能工作（退回逐账号），保证接缝可选而非硬依赖。
func TestVisibleAccountServiceRuntimeBatchFallsBackWhenBatchUnsupported(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newVisibleAccountRepoFake()
	repo.addUser(1, true, true)
	repo.addAccount(&Account{ID: 10, Name: "a", Platform: "anthropic", Type: "oauth", Status: StatusActive})
	repo.assign(1, 10)
	base := &visibleAccountUsageFake{
		passive:    map[int64]*UsageInfo{},
		passiveErr: map[int64]error{},
	}
	svc := NewVisibleAccountService(repo)
	svc.SetUsageReader(base) // 只实现 VisibleAccountUsageReader

	result, err := svc.RuntimeBatch(ctx, 1, []int64{10})
	require.NoError(t, err)
	require.Equal(t, 1, base.passiveCalls, "无批量扩展时退回逐账号被动读取")
	require.Nil(t, result[10].Usage, "该账号无被动数据")
}

func keysOfVisibleRuntime(m map[int64]VisibleAccountRuntime) []int64 {
	out := make([]int64, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	return out
}

func visibleAccountPtrTime(t time.Time) *time.Time { return &t }

// TestVisibleAccountServiceStatsRequiresAuthorization 验证统计入口先做对象级授权：
// 未授权/不存在账号与「能力未开启」分别返回 404 与 403，且未授权时绝不调用统计读取器。
func TestVisibleAccountServiceStatsRequiresAuthorization(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newVisibleAccountRepoFake()
	repo.addUser(1, true, true)
	repo.addAccount(&Account{ID: 10, Name: "claude", Platform: "anthropic", Type: "oauth", Status: StatusActive})
	repo.assign(1, 10)
	reader := &visibleAccountUsageFake{stats: &usagestats.AccountUsageStatsResponse{}}
	svc := NewVisibleAccountService(repo)
	svc.SetUsageReader(reader)

	now := time.Now()
	stats, err := svc.Stats(ctx, 1, 10, now, now)
	require.NoError(t, err)
	require.NotNil(t, stats)
	require.True(t, reader.statsCalled)

	reader.statsCalled = false
	_, err = svc.Stats(ctx, 1, 999, now, now)
	require.ErrorIs(t, err, ErrVisibleAccountNotFound)
	require.False(t, reader.statsCalled, "未授权账号不得触发统计读取")

	_, err = svc.Stats(ctx, 3, 10, now, now)
	require.ErrorIs(t, err, ErrAccountViewDisabled)
	require.False(t, reader.statsCalled)
}

// TestVisibleAccountServiceUsagePassiveOnlyAndNeverFakeZero 验证被动用量读取：
// 只支持 Anthropic OAuth/Setup-Token；其它平台与读取失败都返回 nil（未知），
// 未采样（无真实 session 窗口）不得因 estimate 合成的空 5h 而显示成 5h=0；
// 仅有 7d/Sonnet 事实时保留事实、不带假 5h。绝不用空对象或零值冒充已知数据。
func TestVisibleAccountServiceUsagePassiveOnlyAndNeverFakeZero(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sessionStart := time.Now().Add(-time.Hour)
	sessionEnd := time.Now().Add(time.Hour)
	repo := newVisibleAccountRepoFake()
	repo.addUser(1, true, true)
	repo.addAccount(&Account{
		ID: 10, Name: "claude", Platform: "anthropic", Type: "oauth", Status: StatusActive,
		SessionWindowStart: &sessionStart, SessionWindowEnd: &sessionEnd,
	})
	repo.addAccount(&Account{ID: 11, Name: "openai", Platform: "openai", Type: "apikey", Status: StatusActive})
	repo.addAccount(&Account{ID: 12, Name: "broken", Platform: "anthropic", Type: "oauth", Status: StatusActive})
	// 13：未采样 Anthropic，但读取器仍会给出 estimate 合成的空 5h。
	repo.addAccount(&Account{ID: 13, Name: "unsampled", Platform: "anthropic", Type: "oauth", Status: StatusActive})
	// 14：仅有 7d Sonnet 事实。
	repo.addAccount(&Account{ID: 14, Name: "sonnet-only", Platform: "anthropic", Type: "oauth", Status: StatusActive})
	repo.assign(1, 10, 11, 12, 13, 14)
	reader := &visibleAccountUsageFake{
		passive: map[int64]*UsageInfo{
			10: {Source: "passive", FiveHour: &UsageProgress{Utilization: 12}},
			13: {Source: "passive", FiveHour: &UsageProgress{Utilization: 0}},
			14: {Source: "passive", SevenDaySonnet: &UsageProgress{Utilization: 7}},
		},
		passiveErr: map[int64]error{
			12: errVisiblePassiveUnavailable,
		},
	}
	svc := NewVisibleAccountService(repo)
	svc.SetUsageReader(reader)

	usage, err := svc.Usage(ctx, 1, 10)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, "passive", usage.Source)
	require.NotNil(t, usage.FiveHour)

	usage, err = svc.Usage(ctx, 1, 11)
	require.NoError(t, err)
	require.Nil(t, usage, "非 Anthropic 账号保持未知")

	usage, err = svc.Usage(ctx, 1, 12)
	require.NoError(t, err)
	require.Nil(t, usage, "读取失败不得把原始错误带给用户")

	usage, err = svc.Usage(ctx, 1, 13)
	require.NoError(t, err)
	require.Nil(t, usage, "未采样账号不得显示成 5h=0")

	usage, err = svc.Usage(ctx, 1, 14)
	require.NoError(t, err)
	require.NotNil(t, usage, "仅 7d/Sonnet 事实仍应可用")
	require.Nil(t, usage.FiveHour, "仅有 Sonnet 事实时不得带假 5h")
	require.NotNil(t, usage.SevenDaySonnet)

	_, err = svc.Usage(ctx, 1, 999)
	require.ErrorIs(t, err, ErrVisibleAccountNotFound)
}

var errVisiblePassiveUnavailable = errors.New("visible passive unavailable")

// TestVisibleAccountServiceGroupsComeFromAssignedAccountsOnly 验证分组目录只从
// 已授权账号的实际分组派生，且不因当前筛选而改变。
func TestVisibleAccountServiceGroupsComeFromAssignedAccountsOnly(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newVisibleAccountRepoFake()
	repo.addUser(1, true, true)
	repo.addAccount(&Account{
		ID: 10, Name: "claude", Platform: "anthropic", Type: "oauth", Status: StatusActive,
		Groups: []*Group{{ID: 201, Name: "team-a", Platform: "anthropic", SubscriptionType: "pro"}},
	})
	repo.addAccount(&Account{
		ID: 11, Name: "openai", Platform: "openai", Type: "apikey", Status: StatusActive,
		Groups: []*Group{
			{ID: 201, Name: "team-a", Platform: "anthropic", SubscriptionType: "pro"},
			{ID: 202, Name: "team-b", Platform: "openai", SubscriptionType: "plus"},
		},
	})
	repo.assign(1, 10, 11)
	svc := NewVisibleAccountService(repo)

	groups, err := svc.Groups(ctx, 1)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	require.Equal(t, int64(201), groups[0].ID)
	require.Equal(t, "team-a", groups[0].Name)
	require.Equal(t, "pro", groups[0].SubscriptionType)
	require.Equal(t, int64(202), groups[1].ID)

	// 能力未开启：拒绝，不泄露任何分组。
	_, err = svc.Groups(ctx, 3)
	require.ErrorIs(t, err, ErrAccountViewDisabled)
}
