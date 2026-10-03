//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// visibleAccountHTTPRepoFake 复刻真实仓储的可见性约束（能力开关 + 显式分配 +
// 账号未软删除；手动停用保持可见），用于 HTTP 级验收；真实 PostgreSQL 的迁移与
// 级联由 repository 的集成测试覆盖。
type visibleAccountHTTPRepoFake struct {
	enabled     map[int64]bool
	active      map[int64]bool
	softDeleted map[int64]bool
	assigned    map[int64][]int64
	accounts    map[int64]*service.Account
	lastGrantBy map[int64]*int64
	lastFilter  service.VisibleAccountFilter
}

func newVisibleAccountHTTPRepoFake() *visibleAccountHTTPRepoFake {
	return &visibleAccountHTTPRepoFake{
		enabled:     map[int64]bool{},
		active:      map[int64]bool{},
		softDeleted: map[int64]bool{},
		assigned:    map[int64][]int64{},
		accounts:    map[int64]*service.Account{},
		lastGrantBy: map[int64]*int64{},
	}
}

func (f *visibleAccountHTTPRepoFake) addUser(id int64, enabled bool, active bool) {
	f.enabled[id] = enabled
	f.active[id] = active
}

// markSoftDeleted 把用户标记为已软删除：对普通用户与管理员读取都等同不存在。
func (f *visibleAccountHTTPRepoFake) markSoftDeleted(id int64) {
	f.softDeleted[id] = true
}

func (f *visibleAccountHTTPRepoFake) addAccount(account *service.Account) {
	f.accounts[account.ID] = account
}

func (f *visibleAccountHTTPRepoFake) assign(userID int64, accountIDs ...int64) {
	f.assigned[userID] = append([]int64{}, accountIDs...)
}

// exists 表示用户行仍在（未软删除）；active[id] 只是其状态值。
func (f *visibleAccountHTTPRepoFake) exists(userID int64) bool {
	if _, ok := f.active[userID]; !ok {
		return false
	}
	return !f.softDeleted[userID]
}

func (f *visibleAccountHTTPRepoFake) GetAccountViewEnabled(_ context.Context, userID int64) (bool, error) {
	if !f.exists(userID) || !f.active[userID] {
		return false, nil
	}
	return f.enabled[userID], nil
}

// GetStoredAccountViewEnabled 复刻管理员口径：只要求用户行存在且未软删除，
// 已禁用用户仍返回存储值。
func (f *visibleAccountHTTPRepoFake) GetStoredAccountViewEnabled(_ context.Context, userID int64) (bool, error) {
	if !f.exists(userID) {
		return false, service.ErrAccountViewUserNotFound
	}
	return f.enabled[userID], nil
}

func (f *visibleAccountHTTPRepoFake) ListAssignedAccountIDs(_ context.Context, userID int64) ([]int64, error) {
	if !f.exists(userID) {
		return []int64{}, nil
	}
	out := append([]int64{}, f.assigned[userID]...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (f *visibleAccountHTTPRepoFake) UpdateAccountView(
	_ context.Context,
	userID int64,
	enabled *bool,
	accountIDs *[]int64,
	grantedBy *int64,
) error {
	if !f.exists(userID) {
		return service.ErrAccountViewUserNotFound
	}
	if accountIDs != nil {
		retained := map[int64]struct{}{}
		for _, id := range f.assigned[userID] {
			retained[id] = struct{}{}
		}
		// 收集全部不可用的新增 id（真实仓储同样一次返回整批失败项），
		// 仍然在任何写入之前失败。
		var invalid []int64
		for _, id := range *accountIDs {
			if _, ok := f.accounts[id]; ok {
				continue
			}
			// 已分配但已无详情的账号（例如被软删除）允许保留，
			// 但不允许把它当成新分配。
			if _, ok := retained[id]; ok {
				continue
			}
			invalid = append(invalid, id)
		}
		if len(invalid) > 0 {
			return service.UnknownVisibleAccountError(invalid)
		}
	}
	if enabled != nil {
		f.enabled[userID] = *enabled
	}
	if accountIDs != nil {
		f.assigned[userID] = append([]int64{}, (*accountIDs)...)
	}
	f.lastGrantBy[userID] = grantedBy
	return nil
}

func (f *visibleAccountHTTPRepoFake) ListAssignedAccountSummaries(_ context.Context, userID int64) ([]service.AssignedVisibleAccount, error) {
	// 详情只覆盖仍可读的账号：已分配但已被软删除的账号仍留在
	// ListAssignedAccountIDs 的权威集合里，这里没有详情可给（与真实仓储一致）。
	out := []service.AssignedVisibleAccount{}
	for _, id := range f.assigned[userID] {
		account, ok := f.accounts[id]
		if !ok {
			continue
		}
		out = append(out, service.AssignedVisibleAccount{
			ID: account.ID, Name: account.Name, Platform: account.Platform,
			Type: account.Type, Status: account.Status,
		})
	}
	return out, nil
}

func (f *visibleAccountHTTPRepoFake) ListVisibleAccounts(
	_ context.Context,
	userID int64,
	filter service.VisibleAccountFilter,
) ([]*service.Account, int64, error) {
	f.lastFilter = filter
	visible := f.visible(userID)
	start := (filter.Page - 1) * filter.PageSize
	if start >= len(visible) {
		return []*service.Account{}, int64(len(visible)), nil
	}
	end := start + filter.PageSize
	if end > len(visible) {
		end = len(visible)
	}
	return visible[start:end], int64(len(visible)), nil
}

func (f *visibleAccountHTTPRepoFake) GetVisibleAccount(_ context.Context, userID, accountID int64) (*service.Account, error) {
	for _, account := range f.visible(userID) {
		if account.ID == accountID {
			return account, nil
		}
	}
	return nil, nil
}

func (f *visibleAccountHTTPRepoFake) visible(userID int64) []*service.Account {
	if !f.exists(userID) || !f.active[userID] || !f.enabled[userID] {
		return nil
	}
	out := []*service.Account{}
	for _, id := range f.assigned[userID] {
		account, ok := f.accounts[id]
		if !ok {
			continue
		}
		out = append(out, account)
	}
	return out
}

// ListVisibleAccountsByIDs 复刻批量范围查询：只在授权集合内命中。
func (f *visibleAccountHTTPRepoFake) ListVisibleAccountsByIDs(_ context.Context, userID int64, accountIDs []int64) ([]*service.Account, error) {
	want := make(map[int64]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		want[id] = struct{}{}
	}
	out := []*service.Account{}
	for _, account := range f.visible(userID) {
		if _, ok := want[account.ID]; ok {
			out = append(out, account)
		}
	}
	return out, nil
}

// ListVisibleAccountGroups 只从可见账号所属分组派生。
func (f *visibleAccountHTTPRepoFake) ListVisibleAccountGroups(_ context.Context, userID int64) ([]service.VisibleAccountGroup, error) {
	seen := map[int64]struct{}{}
	out := []service.VisibleAccountGroup{}
	for _, account := range f.visible(userID) {
		for _, group := range account.Groups {
			if _, ok := seen[group.ID]; ok {
				continue
			}
			seen[group.ID] = struct{}{}
			out = append(out, service.VisibleAccountGroup{
				ID: group.ID, Name: group.Name, Platform: group.Platform,
				SubscriptionType: group.SubscriptionType,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// visibleAccountTestRouter 用与生产一致的路径注册被测路由，并按请求头注入用户身份。
func visibleAccountTestRouter(repo *visibleAccountHTTPRepoFake, currentUser *int64) *gin.Engine {
	visibleAccountService := service.NewVisibleAccountService(repo)
	accountHandler := NewVisibleAccountHandler(visibleAccountService)
	adminUserHandler := &admin.UserHandler{}
	adminUserHandler.SetVisibleAccountService(visibleAccountService)

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		// 每个请求都以 X-Test-User 头声明身份；未声明时回退到默认用户。
		identity := int64(0)
		if raw := c.GetHeader("X-Test-User"); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				identity = parsed
			}
		} else if currentUser != nil {
			identity = *currentUser
		}
		if identity > 0 {
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: identity})
		}
		c.Next()
	})
	engine.GET("/api/v1/accounts", accountHandler.List)
	engine.GET("/api/v1/accounts/groups", accountHandler.Groups)
	engine.POST("/api/v1/accounts/runtime/batch", accountHandler.RuntimeBatch)
	engine.GET("/api/v1/accounts/:id", accountHandler.Get)
	engine.GET("/api/v1/accounts/:id/stats", accountHandler.Stats)
	engine.GET("/api/v1/accounts/:id/usage", accountHandler.Usage)
	engine.GET("/api/v1/admin/users/:id/account-view", adminUserHandler.GetAccountView)
	engine.PUT("/api/v1/admin/users/:id/account-view", adminUserHandler.UpdateAccountView)
	return engine
}

func doJSON(t *testing.T, engine *gin.Engine, method, path, body string, userID int64) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Test-User", strconv.FormatInt(userID, 10))
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	return recorder
}

func decodeAccountItems(t *testing.T, recorder *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Items []map[string]any `json:"items"`
			Total int64            `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	return payload.Data.Items
}

func sentinelAccounts() []*service.Account {
	return []*service.Account{
		{
			ID:       10,
			Name:     "sentinel-account-name",
			Platform: "anthropic",
			Type:     "oauth",
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"email":                 "owner@example.com",
				"username":              "owner-username",
				"chatgpt_account_id":    "acct-1234567890",
				"access_token":          "sentinel-access-token",
				"refresh_token":         "sentinel-refresh-token",
				"api_key":               "sk-sentinel",
				"base_url":              "https://sentinel-base-url.example",
				"fallback_credit_token": "sentinel-fallback-credit-token",
			},
			Extra: map[string]any{
				"proxy":         "http://sentinel-proxy",
				"error_message": "sentinel-error",
			},
			ErrorMessage: "sentinel-error-message",
			Notes:        stringPtr("sentinel-notes"),
		},
		{ID: 11, Name: "disabled-account", Platform: "openai", Type: "apikey", Status: service.StatusDisabled},
		{ID: 12, Name: "inactive-account", Platform: "openai", Type: "apikey", Status: service.StatusInactive},
		{ID: 13, Name: "failed-account", Platform: "openai", Type: "apikey", Status: service.StatusError},
		{ID: 14, Name: "expired-account", Platform: "openai", Type: "apikey", Status: service.StatusExpired},
		{ID: 15, Name: "short-identity", Platform: "openai", Type: "apikey", Status: service.StatusActive,
			Credentials: map[string]any{"email": "a@b.co", "username": "abcd", "account_id": "1234"}},
	}
}

func stringPtr(v string) *string { return &v }

func TestVisibleAccountHTTP_DefaultNoAccessThenAdminGrantThenRevoke(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newVisibleAccountHTTPRepoFake()
	for _, account := range sentinelAccounts() {
		repo.addAccount(account)
	}
	repo.addUser(1, false, true) // 新用户：无能力、无分配
	repo.addUser(2, true, true)  // 已授权但未分配
	repo.addUser(3, true, false) // 已禁用用户
	currentUser := int64(1)
	engine := visibleAccountTestRouter(repo, &currentUser)

	// 1) 未获能力：列表与详情都拒绝，且给出可区分的原因。
	rec := doJSON(t, engine, http.MethodGet, "/api/v1/accounts", "", 1)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "ACCOUNT_VIEW_DISABLED")
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/10", "", 1)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "ACCOUNT_VIEW_DISABLED")

	// 管理员（通过管理员接口）开启能力并分配账号 10、11、12、13、14、15。
	adminRec := doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":true,"account_ids":[10,11,12,13,14,15]}`, 99)
	require.Equal(t, http.StatusOK, adminRec.Code)
	require.Contains(t, adminRec.Body.String(), `"account_type"`)
	require.NotContains(t, adminRec.Body.String(), `"type":`)

	// 2) 授权后：详情与列表可见（含手动停用与故障/过期账号）。
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/10", "", 1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	body := rec.Body.String()
	require.Contains(t, body, `"name":"sentinel-account-name"`, "名称现在是可展示字段")
	require.Contains(t, body, "o***@e***")
	require.Contains(t, body, "ow***me")
	require.Contains(t, body, "ac***90")
	for _, sentinel := range []string{
		"sentinel-access-token", "sentinel-refresh-token",
		"sk-sentinel", "sentinel-base-url", "sentinel-fallback-credit-token",
		"sentinel-proxy", "sentinel-error", "sentinel-notes", "sentinel-error-message",
		"owner-username", "owner@example.com", "acct-1234567890",
	} {
		require.NotContains(t, body, sentinel, "sentinel %q must never reach a user response", sentinel)
	}
	var detail struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
	require.ElementsMatch(t,
		[]string{
			"id", "name", "platform", "account_type", "status", "schedulable", "concurrency",
			"groups", "rate_limit_reset_at", "overload_until", "temp_unschedulable_until",
			"email_masked", "username_masked", "upstream_account_id_masked",
		},
		keysOf(detail.Data))

	items := decodeAccountItems(t, doJSON(t, engine, http.MethodGet, "/api/v1/accounts", "", 1))
	ids := make([]float64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item["id"].(float64))
	}
	require.ElementsMatch(t, []float64{10, 11, 12, 13, 14, 15}, ids, "已授权账号全部可见（含手动停用与故障/过期）")

	// 短身份不得原样暴露。
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/15", "", 1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "abcd")
	require.NotContains(t, rec.Body.String(), "1234")
	require.NotContains(t, rec.Body.String(), "a@b.co")
	require.Contains(t, rec.Body.String(), `"email_masked":"***@***"`)

	// 3) 跨用户隔离：用户 2 未获分配，猜 ID 得到与不存在相同的 404。
	currentUser = 2
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/10", "", 2)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "ACCOUNT_NOT_FOUND")
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/99999", "", 2)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, 0, len(decodeAccountItems(t, doJSON(t, engine, http.MethodGet, "/api/v1/accounts", "", 2))))

	// 4) 已禁用用户：fail-closed。
	currentUser = 3
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts", "", 3)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// 5) 撤销后立即不可见（不需要重新登录或等缓存过期）。
	currentUser = 1
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"account_ids":[]}`, 99)
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/10", "", 1)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, 0, len(decodeAccountItems(t, doJSON(t, engine, http.MethodGet, "/api/v1/accounts", "", 1))))
}

func TestVisibleAccountHTTP_PassesAccountTypeFilterToScopedQuery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newVisibleAccountHTTPRepoFake()
	repo.addUser(1, true, true)
	currentUser := int64(1)
	engine := visibleAccountTestRouter(repo, &currentUser)

	rec := doJSON(t, engine, http.MethodGet, "/api/v1/accounts?platform=openai&account_type=%20UPSTREAM%20&search=oauth", "", 1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "openai", repo.lastFilter.Platform)
	require.Equal(t, "upstream", repo.lastFilter.AccountType)
	require.Equal(t, "oauth", repo.lastFilter.Search)
}

func TestVisibleAccountHTTP_AccountStatusChangesTakeEffectImmediately(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newVisibleAccountHTTPRepoFake()
	for _, account := range sentinelAccounts() {
		repo.addAccount(account)
	}
	repo.addUser(1, true, true)
	currentUser := int64(1)
	engine := visibleAccountTestRouter(repo, &currentUser)

	rec := doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":true,"account_ids":[11,12,13,14]}`, 99)
	require.Equal(t, http.StatusOK, rec.Code)

	// 停用、故障、过期全部保持可见（不再按手动禁用隐藏）。
	for _, id := range []int64{11, 12, 13, 14} {
		require.Equal(t, http.StatusOK, doJSON(t, engine, http.MethodGet, "/api/v1/accounts/"+strconv.FormatInt(id, 10), "", 1).Code,
			"account %d must stay visible", id)
	}

	// 切换到历史停用变体（含大小写/空白）后仍然可见。
	for _, status := range []string{"disabled", "inactive", " Inactive ", "DISABLED"} {
		repo.accounts[13].Status = status
		require.Equal(t, http.StatusOK, doJSON(t, engine, http.MethodGet, "/api/v1/accounts/13", "", 1).Code,
			"status %q must stay visible", status)
		items := decodeAccountItems(t, doJSON(t, engine, http.MethodGet, "/api/v1/accounts", "", 1))
		found := false
		for _, item := range items {
			if item["id"] == float64(13) {
				found = true
			}
		}
		require.True(t, found, "account 13 must stay in the list for status %q", status)
	}

	// 故障状态照旧可见。
	repo.accounts[13].Status = service.StatusError
	require.Equal(t, http.StatusOK, doJSON(t, engine, http.MethodGet, "/api/v1/accounts/13", "", 1).Code)
}

func TestVisibleAccountHTTP_AdminUpdateRejectsInvalidPayloads(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newVisibleAccountHTTPRepoFake()
	for _, account := range sentinelAccounts() {
		repo.addAccount(account)
	}
	repo.addUser(1, false, true)
	currentUser := int64(1)
	engine := visibleAccountTestRouter(repo, &currentUser)

	// 空载荷：不构成任何有效更新，且不得被当成清空。
	rec := doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view", `{}`, 99)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":null,"account_ids":null}`, 99)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.False(t, repo.enabled[1])
	require.Empty(t, repo.assigned[1])

	// 未知账号 id：整单失败，能力开关不得被打开。
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":true,"account_ids":[10,999]}`, 99)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "UNKNOWN_ACCOUNT")
	require.False(t, repo.enabled[1], "failed update must not flip the capability flag")
	require.Empty(t, repo.assigned[1])

	// 不存在的用户：404。
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/4242/account-view",
		`{"enabled":true}`, 99)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 合法的两次调用：分配后可用，且只改开关时分配保持不变。
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":true,"account_ids":[10]}`, 99)
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":false}`, 77)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []int64{10}, repo.assigned[1])
	require.NotNil(t, repo.lastGrantBy[1])
	require.Equal(t, int64(77), *repo.lastGrantBy[1])
	// 关闭开关后用户立刻不可见。
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts", "", 1)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestVisibleAccountHTTP_AdminViewIncludesDisabledAssignments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newVisibleAccountHTTPRepoFake()
	for _, account := range sentinelAccounts() {
		repo.addAccount(account)
	}
	repo.addUser(1, true, true)
	currentUser := int64(1)
	engine := visibleAccountTestRouter(repo, &currentUser)

	require.Equal(t, http.StatusOK, doJSON(t, engine, http.MethodPut,
		"/api/v1/admin/users/1/account-view", `{"enabled":true,"account_ids":[10,11]}`, 99).Code)

	var payload struct {
		Data struct {
			UserID     int64            `json:"user_id"`
			Enabled    bool             `json:"enabled"`
			AccountIDs []int64          `json:"account_ids"`
			Accounts   []map[string]any `json:"accounts"`
		} `json:"data"`
	}
	rec := doJSON(t, engine, http.MethodGet, "/api/v1/admin/users/1/account-view", "", 99)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, int64(1), payload.Data.UserID)
	require.True(t, payload.Data.Enabled)
	require.ElementsMatch(t, []int64{10, 11}, payload.Data.AccountIDs)
	require.Len(t, payload.Data.Accounts, 2)
	// 管理员视图保留真实名称与状态（含被禁用的账号），便于撤销。
	require.Contains(t, rec.Body.String(), "sentinel-account-name")
	require.Contains(t, rec.Body.String(), "disabled-account")
	require.Contains(t, rec.Body.String(), `"account_type"`)
	// 即便在管理员视图里也不返回凭据。
	require.NotContains(t, rec.Body.String(), "sentinel-access-token")
	require.NotContains(t, rec.Body.String(), "sk-sentinel")
}

// TestVisibleAccountHTTP_AdminViewKeepsStoredFlagAndRejectsDeletedUsers 验证管理员
// GET 的口径：读存储的开关值（已禁用用户也如实返回，避免界面原样保存把 true
// 改成 false），而用户不存在或已软删除时与 PUT 同为 404 USER_NOT_FOUND。
func TestVisibleAccountHTTP_AdminViewKeepsStoredFlagAndRejectsDeletedUsers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newVisibleAccountHTTPRepoFake()
	for _, account := range sentinelAccounts() {
		repo.addAccount(account)
	}
	repo.addUser(1, true, false) // 已禁用用户，存储的查看能力为 true
	engine := visibleAccountTestRouter(repo, nil)

	// 已禁用（未软删除）用户：管理员 GET 如实返回存储值。
	rec := doJSON(t, engine, http.MethodGet, "/api/v1/admin/users/1/account-view", "", 99)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"enabled":true`)

	// 同一个用户面向自己的能力读取仍然 fail-closed。
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts", "", 1)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "ACCOUNT_VIEW_DISABLED")

	// 不存在的用户：GET 与 PUT 同为 404 USER_NOT_FOUND。
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/admin/users/4242/account-view", "", 99)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "USER_NOT_FOUND")
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/4242/account-view", `{"enabled":true}`, 99)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "USER_NOT_FOUND")

	// 已软删除的用户：GET 与 PUT 同为 404，且不会写入任何数据。
	repo.addUser(2, true, true)
	repo.markSoftDeleted(2)
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/admin/users/2/account-view", "", 99)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "USER_NOT_FOUND")
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/2/account-view", `{"enabled":false}`, 99)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "USER_NOT_FOUND")
	require.True(t, repo.enabled[2], "被拒绝的 PUT 不得改动软删除用户的开关")
}

// TestVisibleAccountHTTP_AdminSaveRoundTripKeepsGrant 模拟管理员界面「打开即保存」：
// GET 返回的 enabled 与 account_ids 原封回传，其中包含一条已无详情的分配
// （对应账号已被软删除）。保存既不能关闭能力，也不能把这条分配关系撤销掉。
func TestVisibleAccountHTTP_AdminSaveRoundTripKeepsGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newVisibleAccountHTTPRepoFake()
	for _, account := range sentinelAccounts() {
		repo.addAccount(account)
	}
	repo.addUser(1, true, true)
	repo.assign(1, 10, 11)
	// 11 号账号随后被软删除：分配行仍在，但界面上已无详情（不在候选集合里）。
	delete(repo.accounts, 11)
	engine := visibleAccountTestRouter(repo, nil)

	rec := doJSON(t, engine, http.MethodGet, "/api/v1/admin/users/1/account-view", "", 99)
	require.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Data struct {
			Enabled    bool             `json:"enabled"`
			AccountIDs []int64          `json:"account_ids"`
			Accounts   []map[string]any `json:"accounts"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.True(t, payload.Data.Enabled)
	require.ElementsMatch(t, []int64{10, 11}, payload.Data.AccountIDs,
		"已软删除账号的 id 必须留在 account_ids 权威集合里")
	require.Len(t, payload.Data.Accounts, 1, "已软删除账号没有详情可展示")

	// 界面把 GET 的结果原样提交（account_ids 是权威集合，详情缺失的 id 也照传）。
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":true,"account_ids":[10,11]}`, 99)
	require.Equal(t, http.StatusOK, rec.Code)
	require.ElementsMatch(t, []int64{10, 11}, repo.assigned[1], "原样保存不得撤销已软删除账号的分配")
	require.True(t, repo.enabled[1], "原样保存不得关闭能力")

	// 新分配一个已无详情的账号仍然被拒（保留 ≠ 允许新增）。
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":true,"account_ids":[10,11,999]}`, 99)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "UNKNOWN_ACCOUNT")
	require.ElementsMatch(t, []int64{10, 11}, repo.assigned[1], "失败的更新不得留下部分变更")
}

// TestVisibleAccountHTTP_AdminUpdateReportsStaleAccountIDs 覆盖保存失败时的「明确失败项」：
// 待授权账号在管理员勾选后被删除（或整批校验失败）时，整批授权都不生效，但响应必须指出
// 具体不可用的 id，管理员才能在保留草稿的前提下就地修正，而不是只收到一句泛化的
// UNKNOWN_ACCOUNT 而无从判断该移除哪一项。响应只出现数字 id，不得携带账号名称或凭据。
func TestVisibleAccountHTTP_AdminUpdateReportsStaleAccountIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newVisibleAccountHTTPRepoFake()
	for _, account := range sentinelAccounts() {
		repo.addAccount(account)
	}
	repo.addUser(1, false, true)
	repo.assign(1, 10)
	// 12 号账号在勾选后被删除：仍可读的 10 保持不变，12 已无详情。
	delete(repo.accounts, 12)
	engine := visibleAccountTestRouter(repo, nil)

	rec := doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":true,"account_ids":[10,12,999]}`, 99)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var payload struct {
		Reason   string            `json:"reason"`
		Metadata map[string]string `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "UNKNOWN_ACCOUNT", payload.Reason, "既有错误码契约不变")
	require.Equal(t, "12,999", payload.Metadata["invalid_account_ids"],
		"必须指出保存前失效的具体账号 id（去重升序）")
	require.Equal(t, "2", payload.Metadata["invalid_account_count"])
	// 只返回 id：账号名称、状态与凭据都不得出现在失败响应里。
	require.NotContains(t, rec.Body.String(), "sentinel-account-name")
	require.NotContains(t, rec.Body.String(), "inactive-account")
	require.NotContains(t, rec.Body.String(), "sentinel-access-token")

	// 整批不落库：没有部分授权，开关也没有被打开。
	require.False(t, repo.enabled[1], "failed update must not flip the capability flag")
	require.Equal(t, []int64{10}, repo.assigned[1], "失败的更新不得留下部分变更")

	// 管理员移除失效项后，同一批内容可以原样保存成功。
	rec = doJSON(t, engine, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":true,"account_ids":[10]}`, 99)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, repo.enabled[1])
	require.Equal(t, []int64{10}, repo.assigned[1])
}

// TestTruncateSearchKeepsValidUTF8 覆盖多字节搜索词：按字节截断会切断 rune，
// 产生非法 UTF-8 传给 Postgres；必须按字符截断且保留合法编码。
func TestTruncateSearchKeepsValidUTF8(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		input string
	}{
		{name: "cjk", input: strings.Repeat("账号", 80)},   // 160 字符 / 480 字节
		{name: "emoji", input: strings.Repeat("🔒", 120)}, // 每字符 4 字节
		{name: "mixed", input: strings.Repeat("a账🔒", 60)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateSearch(tc.input)
			require.Equal(t, maxVisibleAccountSearchRunes, utf8.RuneCountInString(got))
			require.True(t, utf8.ValidString(got), "result must be valid UTF-8")
			require.NotContains(t, got, string(utf8.RuneError))
			require.Greater(t, len(got), maxVisibleAccountSearchRunes,
				"fixture must exercise the multi-byte path")
			require.True(t, strings.HasPrefix(tc.input, got))
		})
	}

	// 边界：恰好 100 字符不被截断，101 字符截到 100。
	exact := strings.Repeat("账", maxVisibleAccountSearchRunes)
	require.Equal(t, exact, truncateSearch(exact))
	require.Equal(t, exact, truncateSearch(exact+"号"))
	// 空白裁剪仍然生效。
	require.Equal(t, "账", truncateSearch("  账  "))
}

// TestVisibleAccountHTTP_SearchStaysUTF8Safe 在 HTTP 层确认长 CJK 搜索词既不被
// 截断成非法 UTF-8，也不会把服务端打挂。
func TestVisibleAccountHTTP_SearchStaysUTF8Safe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newVisibleAccountHTTPRepoFake()
	for _, account := range sentinelAccounts() {
		repo.addAccount(account)
	}
	repo.addUser(1, true, true)
	currentUser := int64(1)
	engine := visibleAccountTestRouter(repo, &currentUser)

	search := strings.Repeat("账", 150)
	rec := doJSON(t, engine, http.MethodGet, "/api/v1/accounts?search="+url.QueryEscape(search), "", 1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, maxVisibleAccountSearchRunes, utf8.RuneCountInString(repo.lastFilter.Search))
	require.True(t, utf8.ValidString(repo.lastFilter.Search))
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// visibleAccountHTTPUsageFake 是 VisibleAccountUsageReader 的 HTTP 级替身。
type visibleAccountHTTPUsageFake struct {
	today   map[int64]*service.WindowStats
	passive map[int64]*service.UsageInfo
	stats   *usagestats.AccountUsageStatsResponse
}

func (f *visibleAccountHTTPUsageFake) GetTodayStats(_ context.Context, accountID int64) (*service.WindowStats, error) {
	return f.today[accountID], nil
}

func (f *visibleAccountHTTPUsageFake) GetTodayStatsBatch(_ context.Context, accountIDs []int64) (map[int64]*service.WindowStats, error) {
	out := make(map[int64]*service.WindowStats, len(accountIDs))
	for _, id := range accountIDs {
		if stats, ok := f.today[id]; ok {
			out[id] = stats
		}
	}
	return out, nil
}

func (f *visibleAccountHTTPUsageFake) GetAccountUsageStats(_ context.Context, _ int64, _, _ time.Time) (*usagestats.AccountUsageStatsResponse, error) {
	return f.stats, nil
}

func (f *visibleAccountHTTPUsageFake) GetPassiveUsage(_ context.Context, accountID int64) (*service.UsageInfo, error) {
	return f.passive[accountID], nil
}

// visibleAccountHTTPConcurrencyFake 是 VisibleAccountConcurrencyReader 的 HTTP 级替身。
type visibleAccountHTTPConcurrencyFake struct {
	counts map[int64]int
}

func (f *visibleAccountHTTPConcurrencyFake) GetAccountConcurrencyBatch(_ context.Context, accountIDs []int64) (map[int64]int, error) {
	out := make(map[int64]int, len(accountIDs))
	for _, id := range accountIDs {
		out[id] = f.counts[id]
	}
	return out, nil
}

// newVisibleAccountRuntimeRouter 注册扩展后的只读路由并注入只读读取器。
func newVisibleAccountRuntimeRouter(
	repo *visibleAccountHTTPRepoFake,
	usage service.VisibleAccountUsageReader,
	concurrency service.VisibleAccountConcurrencyReader,
) *gin.Engine {
	visibleAccountService := service.NewVisibleAccountService(repo)
	visibleAccountService.SetUsageReader(usage)
	visibleAccountService.SetConcurrencyReader(concurrency)
	accountHandler := NewVisibleAccountHandler(visibleAccountService)

	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		identity := int64(0)
		if raw := c.GetHeader("X-Test-User"); raw != "" {
			if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil {
				identity = parsed
			}
		}
		if identity > 0 {
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: identity})
		}
		c.Next()
	})
	engine.GET("/api/v1/accounts", accountHandler.List)
	engine.GET("/api/v1/accounts/groups", accountHandler.Groups)
	engine.POST("/api/v1/accounts/runtime/batch", accountHandler.RuntimeBatch)
	engine.GET("/api/v1/accounts/:id", accountHandler.Get)
	engine.GET("/api/v1/accounts/:id/stats", accountHandler.Stats)
	engine.GET("/api/v1/accounts/:id/usage", accountHandler.Usage)
	return engine
}

// TestVisibleAccountHTTP_GroupsRuntimeBatchStatsAndUsage 覆盖新增只读端点：
// 分组候选来自已授权账号、运行时批量只回授权 id、统计不含原始上游端点明细、
// 被动用量只对 Anthropic 账号有数据，且全部 no-store。
func TestVisibleAccountHTTP_GroupsRuntimeBatchStatsAndUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newVisibleAccountHTTPRepoFake()
	repo.addUser(1, true, true)
	sessionStart := time.Now().Add(-time.Hour)
	sessionEnd := time.Now().Add(time.Hour)
	repo.addAccount(&service.Account{
		ID: 10, Name: "claude", Platform: "anthropic", Type: "oauth", Status: service.StatusActive,
		SessionWindowStart: &sessionStart, SessionWindowEnd: &sessionEnd,
		Groups: []*service.Group{{ID: 201, Name: "team-a", Platform: "anthropic", SubscriptionType: "pro"}},
	})
	repo.addAccount(&service.Account{
		ID: 11, Name: "openai", Platform: "openai", Type: "apikey", Status: service.StatusDisabled,
	})
	repo.assign(1, 10, 11)

	usage := &visibleAccountHTTPUsageFake{
		today: map[int64]*service.WindowStats{10: {Requests: 3, Tokens: 30}},
		passive: map[int64]*service.UsageInfo{
			10: {Source: "passive", FiveHour: &service.UsageProgress{Utilization: 20}},
		},
		stats: &usagestats.AccountUsageStatsResponse{
			Summary:   usagestats.AccountUsageSummary{TotalRequests: 7},
			Models:    []usagestats.ModelStat{{Model: "claude-3"}},
			Endpoints: []usagestats.EndpointStat{{Endpoint: "/v1/messages"}},
			UpstreamEndpoints: []usagestats.EndpointStat{
				{Endpoint: "https://sentinel-upstream.example/v1/messages"},
			},
		},
	}
	concurrency := &visibleAccountHTTPConcurrencyFake{counts: map[int64]int{10: 4}}
	engine := newVisibleAccountRuntimeRouter(repo, usage, concurrency)

	// 分组候选：数组，只含已授权账号所属分组。
	rec := doJSON(t, engine, http.MethodGet, "/api/v1/accounts/groups", "", 1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var groups struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	require.Len(t, groups.Data, 1)
	require.Equal(t, "team-a", groups.Data[0]["name"])
	require.Equal(t, "pro", groups.Data[0]["subscription_type"])
	require.ElementsMatch(t, []string{"id", "name", "platform", "subscription_type"}, keysOf(groups.Data[0]))

	// 运行时批量：只回授权 id，未授权 999 静默缺席。
	rec = doJSON(t, engine, http.MethodPost, "/api/v1/accounts/runtime/batch",
		`{"account_ids":[10,11,999]}`, 1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.NotContains(t, rec.Body.String(), "999")
	var batch struct {
		Data struct {
			Accounts map[string]map[string]any `json:"accounts"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &batch))
	require.ElementsMatch(t, []string{"10", "11"}, keysOfAny(batch.Data.Accounts))
	require.NotNil(t, batch.Data.Accounts["10"]["usage"], "Anthropic 账号有被动用量")
	require.Nil(t, batch.Data.Accounts["11"]["usage"], "非 Anthropic 账号用量为 null")
	require.NotNil(t, batch.Data.Accounts["10"]["today_stats"])
	require.Equal(t, float64(4), batch.Data.Accounts["10"]["current_concurrency"])

	// 超限：直接 400，不静默截断。
	oversized := make([]int64, 101)
	for i := range oversized {
		oversized[i] = int64(i + 1)
	}
	payload, err := json.Marshal(map[string]any{"account_ids": oversized})
	require.NoError(t, err)
	rec = doJSON(t, engine, http.MethodPost, "/api/v1/accounts/runtime/batch", string(payload), 1)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 统计：授权账号返回安全汇总，不含原始上游端点明细；未授权 404。
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/10/stats?days=30", "", 1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	require.Contains(t, rec.Body.String(), `"model":"claude-3"`)
	require.NotContains(t, rec.Body.String(), "upstream_endpoints")
	require.NotContains(t, rec.Body.String(), "sentinel-upstream.example")

	// days 显式提供但非法一律 400（不静默回退）；省略默认 30；边界 1/90 合法。
	for _, bad := range []string{"0", "91", "-1", "abc", "1.5"} {
		rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/10/stats?days="+bad, "", 1)
		require.Equal(t, http.StatusBadRequest, rec.Code, "days=%q must be rejected", bad)
	}
	for _, good := range []string{"1", "90"} {
		rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/10/stats?days="+good, "", 1)
		require.Equal(t, http.StatusOK, rec.Code, "days=%q must be accepted", good)
	}
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/10/stats", "", 1)
	require.Equal(t, http.StatusOK, rec.Code, "omitted days defaults to 30")

	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/999/stats", "", 1)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 被动用量详情：Anthropic 有数据，其它平台为 null。
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/10/usage", "", 1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"source":"passive"`)
	require.NotContains(t, rec.Body.String(), "error")

	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/11/usage", "", 1)
	require.Equal(t, http.StatusOK, rec.Code)
	var nullUsage struct {
		Data any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &nullUsage))
	require.Nil(t, nullUsage.Data)
}

func keysOfAny[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
