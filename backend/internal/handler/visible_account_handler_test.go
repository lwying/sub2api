//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// visibleAccountHTTPRepoFake 复刻真实仓储的可见性约束（能力开关 + 显式分配 +
// 账号未软删除 + 非手动禁用），用于 HTTP 级验收；真实 PostgreSQL 的迁移与
// 级联由 repository 的集成测试覆盖。
type visibleAccountHTTPRepoFake struct {
	enabled     map[int64]bool
	active      map[int64]bool
	assigned    map[int64][]int64
	accounts    map[int64]*service.Account
	lastGrantBy map[int64]*int64
	lastFilter  service.VisibleAccountFilter
}

func newVisibleAccountHTTPRepoFake() *visibleAccountHTTPRepoFake {
	return &visibleAccountHTTPRepoFake{
		enabled:     map[int64]bool{},
		active:      map[int64]bool{},
		assigned:    map[int64][]int64{},
		accounts:    map[int64]*service.Account{},
		lastGrantBy: map[int64]*int64{},
	}
}

func (f *visibleAccountHTTPRepoFake) addUser(id int64, enabled bool, active bool) {
	f.enabled[id] = enabled
	f.active[id] = active
}

func (f *visibleAccountHTTPRepoFake) addAccount(account *service.Account) {
	f.accounts[account.ID] = account
}

func (f *visibleAccountHTTPRepoFake) GetAccountViewEnabled(_ context.Context, userID int64) (bool, error) {
	if !f.active[userID] {
		return false, nil
	}
	return f.enabled[userID], nil
}

func (f *visibleAccountHTTPRepoFake) UpdateAccountView(
	_ context.Context,
	userID int64,
	enabled *bool,
	accountIDs *[]int64,
	grantedBy *int64,
) error {
	if _, ok := f.active[userID]; !ok {
		return service.ErrAccountViewUserNotFound
	}
	if accountIDs != nil {
		for _, id := range *accountIDs {
			if _, ok := f.accounts[id]; !ok {
				return service.ErrUnknownVisibleAccount
			}
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
	if !f.active[userID] || !f.enabled[userID] {
		return nil
	}
	out := []*service.Account{}
	for _, id := range f.assigned[userID] {
		account, ok := f.accounts[id]
		if !ok || testManuallyDisabled(account.Status) {
			continue
		}
		out = append(out, account)
	}
	return out
}

// testManuallyDisabled 与服务层同口径：大小写与首尾空白不敏感。
func testManuallyDisabled(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "disabled", "inactive":
		return true
	default:
		return false
	}
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
	engine.GET("/api/v1/accounts/:id", accountHandler.Get)
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

	// 2) 授权后：详情与列表可见（手动禁用的 11/12 不可见，error/expired 仍可见）。
	rec = doJSON(t, engine, http.MethodGet, "/api/v1/accounts/10", "", 1)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	body := rec.Body.String()
	require.Contains(t, body, "o***@e***")
	require.Contains(t, body, "ow***me")
	require.Contains(t, body, "ac***90")
	for _, sentinel := range []string{
		"sentinel-account-name", "sentinel-access-token", "sentinel-refresh-token",
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
		[]string{"id", "platform", "account_type", "email_masked", "username_masked", "upstream_account_id_masked"},
		keysOf(detail.Data))

	items := decodeAccountItems(t, doJSON(t, engine, http.MethodGet, "/api/v1/accounts", "", 1))
	ids := make([]float64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item["id"].(float64))
	}
	require.ElementsMatch(t, []float64{10, 13, 14, 15}, ids, "手动禁用账号必须消失，故障/过期账号仍可见")

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

	// error / expired 可见；disabled / inactive 不可见。
	require.Equal(t, http.StatusOK, doJSON(t, engine, http.MethodGet, "/api/v1/accounts/13", "", 1).Code)
	require.Equal(t, http.StatusOK, doJSON(t, engine, http.MethodGet, "/api/v1/accounts/14", "", 1).Code)
	require.Equal(t, http.StatusNotFound, doJSON(t, engine, http.MethodGet, "/api/v1/accounts/11", "", 1).Code)
	require.Equal(t, http.StatusNotFound, doJSON(t, engine, http.MethodGet, "/api/v1/accounts/12", "", 1).Code)

	// 手动禁用后立刻从列表与详情消失（含历史大小写/空白变体）。
	for _, status := range []string{"disabled", "inactive", " Inactive ", "DISABLED"} {
		repo.accounts[13].Status = status
		require.Equal(t, http.StatusNotFound, doJSON(t, engine, http.MethodGet, "/api/v1/accounts/13", "", 1).Code,
			"status %q must hide the account", status)
		items := decodeAccountItems(t, doJSON(t, engine, http.MethodGet, "/api/v1/accounts", "", 1))
		for _, item := range items {
			require.NotEqual(t, float64(13), item["id"])
		}
	}

	// 故障状态重新出现（不是手动禁用）。
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
