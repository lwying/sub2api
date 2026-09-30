package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 票据 10：旧值明细与错误诊断的采集开关、揭示入口不再是可访问的 REST 面。
//
// 判据是「路由根本没有注册」，而不是「注册了但返回不可用」：只藏按钮、却让 API 仍然
// 能把旧正文揭示出来，正是本票据要禁止的形态。命中这些路径必须落到 gin 的 NoRoute。
func TestLegacySensitiveCaptureRoutesAreNotRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	registered := map[string]bool{}
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}

	legacy := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/usage/:id/request-audit/value-detail"},
		{http.MethodPost, "/api/v1/admin/usage/:id/request-audit/value-detail"},
		{http.MethodGet, "/api/v1/admin/usage/request-audit-value-detail-settings"},
		{http.MethodPut, "/api/v1/admin/usage/request-audit-value-detail-settings"},
		{http.MethodGet, "/api/v1/admin/settings/error-diagnostic"},
		{http.MethodPut, "/api/v1/admin/settings/error-diagnostic"},
		{http.MethodGet, "/api/v1/admin/error-diagnostics"},
		{http.MethodGet, "/api/v1/admin/error-diagnostics/:id"},
		{http.MethodPost, "/api/v1/admin/error-diagnostics/:id/body"},
		{http.MethodPost, "/api/v1/admin/error-diagnostics/:id/headers"},
	}
	for _, route := range legacy {
		require.False(t, registered[route.method+" "+route.path],
			"旧入口不得再注册：%s %s", route.method, route.path)
	}

	// 直接访问同样不可揭示：路径不匹配任何已注册路由。
	for _, route := range legacy {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(route.method, route.path, nil)
		router.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusNotFound, recorder.Code,
			"旧入口的直接访问必须不可达：%s %s", route.method, route.path)
	}

	// 新版只保留会话限定的强制审计元数据入口和独立 Trace 总开关。
	require.False(t, registered[http.MethodGet+" /api/v1/admin/usage/:id/request-audit"])
	for _, kept := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/usage/:id/request-audit/forced"},
		{http.MethodGet, "/api/v1/admin/settings/request-trace"},
		{http.MethodPut, "/api/v1/admin/settings/request-trace"},
	} {
		require.True(t, registered[kept.method+" "+kept.path],
			"不得顺带删除：%s %s", kept.method, kept.path)
	}
}
