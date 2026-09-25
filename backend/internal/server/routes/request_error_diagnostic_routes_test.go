package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestErrorDiagnosticHeaderRevealRequiresStepUpWhenEnforced(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{RequestErrorDiagnostic: adminhandler.NewRequestErrorDiagnosticHandler(nil)}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) {
		servermiddleware.AbortWithError(c, http.StatusForbidden, "STEP_UP_REQUIRED", "Recent two-factor verification required")
	})
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/v1/admin/error-diagnostics/0123456789abcdef0123456789abcdef", http.StatusServiceUnavailable},
		{http.MethodPost, "/api/v1/admin/error-diagnostics/0123456789abcdef0123456789abcdef/headers", http.StatusForbidden},
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		require.Equal(t, tc.want, recorder.Code)
		if tc.want == http.StatusForbidden {
			require.Contains(t, recorder.Body.String(), "STEP_UP_REQUIRED")
		}
	}
}

// 错误诊断入口必须留在 admin 组内：未认证 401，认证但非管理员 403,
// 三个路由（含显式揭示正文的 POST）都不得例外。
func TestErrorDiagnosticRoutesRequireAdminAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		RequestErrorDiagnostic: adminhandler.NewRequestErrorDiagnosticHandler(nil),
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
			return
		}
		servermiddleware.AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Admin access required")
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/v1/admin/error-diagnostics"},
		{method: http.MethodGet, path: "/api/v1/admin/error-diagnostics/0123456789abcdef0123456789abcdef"},
		{method: http.MethodPost, path: "/api/v1/admin/error-diagnostics/0123456789abcdef0123456789abcdef/body"},
		{method: http.MethodPost, path: "/api/v1/admin/error-diagnostics/0123456789abcdef0123456789abcdef/headers"},
	} {
		for _, authCase := range []struct {
			name       string
			auth       string
			wantStatus int
		}{
			{name: "unauthenticated", wantStatus: http.StatusUnauthorized},
			{name: "non-admin", auth: "Bearer user-token", wantStatus: http.StatusForbidden},
		} {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(tc.method, tc.path, nil)
			if authCase.auth != "" {
				request.Header.Set("Authorization", authCase.auth)
			}
			router.ServeHTTP(recorder, request)
			require.Equal(t, authCase.wantStatus, recorder.Code, tc.method+" "+tc.path+"/"+authCase.name)
		}
	}
}
