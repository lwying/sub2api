package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// errorDiagnosticSettingsRouteStubRepo 只提供服务构造所需的空仓储。
type errorDiagnosticSettingsRouteStubRepo struct{}

func (errorDiagnosticSettingsRouteStubRepo) Get(_ context.Context, _ string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}

func (errorDiagnosticSettingsRouteStubRepo) GetValue(_ context.Context, _ string) (string, error) {
	return "", service.ErrSettingNotFound
}

func (errorDiagnosticSettingsRouteStubRepo) Set(_ context.Context, _, _ string) error { return nil }

func (errorDiagnosticSettingsRouteStubRepo) GetMultiple(_ context.Context, _ []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (errorDiagnosticSettingsRouteStubRepo) SetMultiple(_ context.Context, _ map[string]string) error {
	return nil
}

func (errorDiagnosticSettingsRouteStubRepo) GetAll(_ context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (errorDiagnosticSettingsRouteStubRepo) Delete(_ context.Context, _ string) error { return nil }

// 错误诊断运维开关必须在 admin 组内：未认证 401，认证但非管理员 403，
// 管理员才拿得到「默认关闭」的状态——默认关闭这条事实在真实路由栈上验证。
func TestErrorDiagnosticOperatorSettingsRoutesRequireAdminAuthentication(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	settingService := service.NewSettingService(errorDiagnosticSettingsRouteStubRepo{}, &config.Config{})
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		Setting: adminhandler.NewSettingHandler(settingService, nil, nil, nil, nil, nil, nil),
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		switch c.GetHeader("Authorization") {
		case "":
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
		case "Bearer admin-token":
			c.Next()
		default:
			servermiddleware.AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Admin access required")
		}
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/v1/admin/settings/error-diagnostic"},
		{method: http.MethodPut, path: "/api/v1/admin/settings/error-diagnostic"},
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

	// 管理员在真实路由栈上读到的是「默认关闭」，且不写任何门控键。
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/error-diagnostic", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), `"enabled":false`)
	require.Contains(t, recorder.Body.String(), `"capture_allowed":false`)
	require.Contains(t, recorder.Body.String(), `"body_retention_allowed":false`)
}
