package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 本文件验证生产路由注册本身：普通用户的账号只读入口受 JWT 保护且与管理员
// 入口分离，管理员分配入口受管理员鉴权保护。行级可见性语义由 service /
// repository 的测试覆盖。

type routesVisibleAccountRepoStub struct {
	enabled  map[int64]bool
	assigned map[int64][]int64
}

func newRoutesVisibleAccountRepoStub() *routesVisibleAccountRepoStub {
	return &routesVisibleAccountRepoStub{
		enabled:  map[int64]bool{},
		assigned: map[int64][]int64{},
	}
}

func (s *routesVisibleAccountRepoStub) GetAccountViewEnabled(_ context.Context, userID int64) (bool, error) {
	return s.enabled[userID], nil
}

// GetStoredAccountViewEnabled 与用户口径在此 stub 里同值：本文件只验证路由与
// 鉴权注册，行级语义由 service / repository 的测试覆盖。
func (s *routesVisibleAccountRepoStub) GetStoredAccountViewEnabled(_ context.Context, userID int64) (bool, error) {
	return s.enabled[userID], nil
}

func (s *routesVisibleAccountRepoStub) ListAssignedAccountIDs(_ context.Context, userID int64) ([]int64, error) {
	return append([]int64{}, s.assigned[userID]...), nil
}

func (s *routesVisibleAccountRepoStub) UpdateAccountView(
	_ context.Context,
	userID int64,
	enabled *bool,
	accountIDs *[]int64,
	_ *int64,
) error {
	if enabled != nil {
		s.enabled[userID] = *enabled
	}
	if accountIDs != nil {
		s.assigned[userID] = append([]int64{}, (*accountIDs)...)
	}
	return nil
}

func (s *routesVisibleAccountRepoStub) ListAssignedAccountSummaries(_ context.Context, userID int64) ([]service.AssignedVisibleAccount, error) {
	return []service.AssignedVisibleAccount{}, nil
}

func (s *routesVisibleAccountRepoStub) ListVisibleAccounts(_ context.Context, userID int64, _ service.VisibleAccountFilter) ([]*service.Account, int64, error) {
	return []*service.Account{}, 0, nil
}

func (s *routesVisibleAccountRepoStub) GetVisibleAccount(_ context.Context, userID, accountID int64) (*service.Account, error) {
	return nil, nil
}

func newAccountViewTestRouter(t *testing.T) (*gin.Engine, *routesVisibleAccountRepoStub) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	repo := newRoutesVisibleAccountRepoStub()
	visibleAccountService := service.NewVisibleAccountService(repo)
	adminUserHandler := &adminhandler.UserHandler{}
	adminUserHandler.SetVisibleAccountService(visibleAccountService)

	handlers := &handler.Handlers{
		VisibleAccount: handler.NewVisibleAccountHandler(visibleAccountService),
		Admin:          &handler.AdminHandlers{User: adminUserHandler},
	}

	// 与生产一致：普通用户入口走 JWT 鉴权，管理员入口走管理员鉴权。
	jwtAuth := servermiddleware.JWTAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
			return
		}
		c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 1})
		c.Set(string(servermiddleware.ContextKeyUserRole), service.RoleUser)
		c.Next()
	})
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		switch c.GetHeader("Authorization") {
		case "":
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
		case "Bearer admin-token":
			c.Set(string(servermiddleware.ContextKeyUser), servermiddleware.AuthSubject{UserID: 99})
			c.Set(string(servermiddleware.ContextKeyUserRole), service.RoleAdmin)
			c.Next()
		default:
			servermiddleware.AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Admin access required")
		}
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })

	router := gin.New()
	// 本测试只装配与本功能相关的 handler，其余 handler 为 nil：用 Recovery 把
	// 未装配处理器产生的 panic 变成 500，从而能断言路由仍然注册且没有新增门槛。
	router.Use(gin.Recovery())
	v1 := router.Group("/api/v1")
	// 与生产注册保持一致：nil 的 settingService / 限流器在这些中间件里是 no-op。
	RegisterUserRoutes(v1, handlers, jwtAuth, auditLog, nil, nil)
	RegisterAdminRoutes(v1, handlers, adminAuth, auditLog, stepUp, nil, nil)
	return router, repo
}

func doRouteRequest(router http.Handler, method, path, body, auth string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		request.Header.Set("Authorization", auth)
	}
	router.ServeHTTP(recorder, request)
	return recorder
}

func TestUserAccountRoutesRequireJWTAndReflectAdminAssignment(t *testing.T) {
	router, repo := newAccountViewTestRouter(t)

	// 未认证：即使是只读账号视图也拒绝。
	recorder := doRouteRequest(router, http.MethodGet, "/api/v1/accounts", "", "")
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
	recorder = doRouteRequest(router, http.MethodGet, "/api/v1/accounts/10", "", "")
	require.Equal(t, http.StatusUnauthorized, recorder.Code)

	// 已认证但未获能力：拒绝，且不暴露任何账号。
	recorder = doRouteRequest(router, http.MethodGet, "/api/v1/accounts", "", "Bearer user-token")
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Contains(t, recorder.Body.String(), "ACCOUNT_VIEW_DISABLED")

	// 管理员通过分配接口开启能力。
	recorder = doRouteRequest(router, http.MethodPut, "/api/v1/admin/users/1/account-view",
		`{"enabled":true,"account_ids":[]}`, "Bearer admin-token")
	require.Equal(t, http.StatusOK, recorder.Code)

	// 同一用户下一次请求即可用：无需重新登录。
	recorder = doRouteRequest(router, http.MethodGet, "/api/v1/accounts", "", "Bearer user-token")
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.True(t, repo.enabled[1])
}

func TestAccountViewAdminRoutesRequireAdminAuthentication(t *testing.T) {
	router, repo := newAccountViewTestRouter(t)

	for _, tc := range []struct {
		name       string
		method     string
		body       string
		auth       string
		wantStatus int
	}{
		{name: "get/unauthenticated", method: http.MethodGet, wantStatus: http.StatusUnauthorized},
		{name: "get/non-admin", method: http.MethodGet, auth: "Bearer user-token", wantStatus: http.StatusForbidden},
		{name: "put/unauthenticated", method: http.MethodPut, body: `{"enabled":true}`, wantStatus: http.StatusUnauthorized},
		{name: "put/non-admin", method: http.MethodPut, body: `{"enabled":true}`, auth: "Bearer user-token", wantStatus: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := doRouteRequest(router, tc.method, "/api/v1/admin/users/1/account-view", tc.body, tc.auth)
			require.Equal(t, tc.wantStatus, recorder.Code)
		})
	}
	require.False(t, repo.enabled[1], "non-admin requests must not change the capability flag")

	// 管理员可用。
	recorder := doRouteRequest(router, http.MethodGet, "/api/v1/admin/users/1/account-view", "", "Bearer admin-token")
	require.Equal(t, http.StatusOK, recorder.Code)
}

// TestUserAccountViewDoESNotDisturbExistingUserRoutes 确认新增账号只读入口没有
// 改变既有普通用户能力：API Key 与个人用量路由仍然注册在同一个 JWT 保护之下
// （未认证得到 401 而不是 404，说明路由仍在；也不是 403，说明没有新增角色门槛）。
func TestUserAccountViewDoESNotDisturbExistingUserRoutes(t *testing.T) {
	router, repo := newAccountViewTestRouter(t)

	existing := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/keys"},
		{http.MethodGet, "/api/v1/keys/1"},
		{http.MethodPost, "/api/v1/keys"},
		{http.MethodGet, "/api/v1/usage"},
		{http.MethodGet, "/api/v1/usage/stats"},
		{http.MethodGet, "/api/v1/groups/available"},
	}

	// 能力开关关闭时：全部仍受 JWT 保护。
	for _, route := range existing {
		recorder := doRouteRequest(router, route.method, route.path, "", "")
		require.Equal(t, http.StatusUnauthorized, recorder.Code,
			"%s %s must stay JWT-guarded", route.method, route.path)
	}

	// 打开账号查看能力后，同样不受影响：不是 403，也不是 404。
	enabled := true
	require.NoError(t, repo.UpdateAccountView(context.Background(), 1, &enabled, &[]int64{}, nil))
	for _, route := range existing {
		recorder := doRouteRequest(router, route.method, route.path, "", "Bearer user-token")
		require.NotEqual(t, http.StatusUnauthorized, recorder.Code,
			"%s %s must accept an authenticated user", route.method, route.path)
		require.NotEqual(t, http.StatusForbidden, recorder.Code,
			"%s %s must not gain a new role gate", route.method, route.path)
		require.NotEqual(t, http.StatusNotFound, recorder.Code,
			"%s %s must stay registered", route.method, route.path)
	}
}

// TestUserTokenCannotReachAdminAccountEndpoints 确认新增的普通用户入口没有以任何
// 方式放宽既有管理员账号接口：用户 token 访问账号列表、导出、代理与诊断均被拒。
func TestUserTokenCannotReachAdminAccountEndpoints(t *testing.T) {
	router, _ := newAccountViewTestRouter(t)

	for _, path := range []string{
		"/api/v1/admin/accounts",
		"/api/v1/admin/accounts/data",
		"/api/v1/admin/accounts/batch-update-credentials",
		"/api/v1/admin/proxies",
		"/api/v1/admin/settings",
		"/api/v1/admin/users/1/account-view",
	} {
		recorder := doRouteRequest(router, http.MethodGet, path, "", "Bearer user-token")
		require.Equal(t, http.StatusForbidden, recorder.Code, "user token must not reach %s", path)

		recorder = doRouteRequest(router, http.MethodGet, path, "", "")
		require.Equal(t, http.StatusUnauthorized, recorder.Code, "%s must require authentication", path)
	}
}
