package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// valueDetailRouteStubRepo 只提供服务构造所需的空仓储。
type valueDetailRouteStubRepo struct{}

func (valueDetailRouteStubRepo) Get(_ context.Context, _ string) (*service.Setting, error) {
	return nil, service.ErrSettingNotFound
}

func (valueDetailRouteStubRepo) GetValue(_ context.Context, _ string) (string, error) {
	return "", service.ErrSettingNotFound
}

func (valueDetailRouteStubRepo) Set(_ context.Context, _, _ string) error { return nil }

func (valueDetailRouteStubRepo) GetMultiple(_ context.Context, _ []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (valueDetailRouteStubRepo) SetMultiple(_ context.Context, _ map[string]string) error { return nil }

func (valueDetailRouteStubRepo) GetAll(_ context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (valueDetailRouteStubRepo) Delete(_ context.Context, _ string) error { return nil }

// valueDetailRouteStubDetailRepo 提供一条可读的值明细记录。
type valueDetailRouteStubDetailRepo struct{}

func (valueDetailRouteStubDetailRepo) CreateRequestAuditValueDetail(context.Context, service.RequestAuditValueDetailWrite) (service.RequestAuditValueDetail, error) {
	return service.RequestAuditValueDetail{}, nil
}

func (valueDetailRouteStubDetailRepo) GetRequestAuditValueDetail(_ context.Context, usageLogID int64) (service.RequestAuditValueDetail, error) {
	if usageLogID != 4242 {
		return service.RequestAuditValueDetail{}, service.ErrRequestAuditValueDetailNotFound
	}
	now := time.Now().UTC()
	return service.RequestAuditValueDetail{
		UsageLogID: usageLogID,
		State:      service.RequestAuditValueDetailStateStored,
		Reason:     service.RequestAuditValueDetailRetained,
		Stored:     true,
		ExpiresAt:  now.Add(6 * 24 * time.Hour),
		CreatedAt:  now,
		Fields: service.RequestAuditValueDetailFields{
			Route:    service.RequestAuditValueDetailRouteMessages,
			Protocol: service.RequestAuditProtocolAnthropic,
		},
	}, nil
}

func (valueDetailRouteStubDetailRepo) ReadRequestAuditValueDetailValues(context.Context, int64, time.Time) (service.RequestAuditValueDetailValues, error) {
	return service.RequestAuditValueDetailValues{Model: "claude-sonnet-4-5"}, nil
}

func (valueDetailRouteStubDetailRepo) ClearExpiredRequestAuditValueDetails(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

func newValueDetailRoutesTestRouter(t *testing.T) *gin.Engine {
	return newValueDetailRoutesTestRouterWithStepUp(t, nil)
}

func newValueDetailRoutesTestRouterWithStepUp(t *testing.T, enforceStepUp servermiddleware.StepUpAuthMiddleware) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()

	settingService := service.NewSettingService(valueDetailRouteStubRepo{}, &config.Config{})
	usageHandler := adminhandler.NewUsageHandler(nil, nil, nil, nil, nil)
	usageHandler.SetRequestAuditValueDetailService(
		service.NewRequestAuditValueDetailService(valueDetailRouteStubDetailRepo{}, settingService))
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		Usage:   usageHandler,
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
	stepUp := enforceStepUp
	if stepUp == nil {
		stepUp = servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	}
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)
	return router
}

func TestRequestAuditValueDetailRevealRequiresStepUpWhenEnforced(t *testing.T) {
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) {
		servermiddleware.AbortWithError(c, http.StatusForbidden, "STEP_UP_REQUIRED", "Recent two-factor verification required")
	})
	router := newValueDetailRoutesTestRouterWithStepUp(t, stepUp)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/v1/admin/usage/4242/request-audit/value-detail", http.StatusOK},
		{http.MethodPost, "/api/v1/admin/usage/4242/request-audit/value-detail", http.StatusForbidden},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(tc.method, tc.path, nil)
		request.Header.Set("Authorization", "Bearer admin-token")
		router.ServeHTTP(recorder, request)
		require.Equal(t, tc.want, recorder.Code)
		if tc.want == http.StatusForbidden {
			require.Contains(t, recorder.Body.String(), "STEP_UP_REQUIRED")
		}
	}
}

// 值明细入口必须在 admin 组内：未认证 401，认证但非管理员 403。
// 这条测试同时在真实路由栈上验证注册不会 panic（静态段与 :id 参数段共存）。
func TestRequestAuditValueDetailRoutesRequireAdminAuthentication(t *testing.T) {
	router := newValueDetailRoutesTestRouter(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/usage/4242/request-audit/value-detail"},
		{http.MethodPost, "/api/v1/admin/usage/4242/request-audit/value-detail"},
		{http.MethodGet, "/api/v1/admin/usage/request-audit-value-detail-settings"},
		{http.MethodPut, "/api/v1/admin/usage/request-audit-value-detail-settings"},
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

// 管理员拿到的是「默认关闭」的运维状态，以及不含值的信封；两者都禁止中间缓存。
func TestRequestAuditValueDetailRoutesAdminViews(t *testing.T) {
	router := newValueDetailRoutesTestRouter(t)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage/request-audit-value-detail-settings", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store, private", recorder.Header().Get("Cache-Control"))
	require.Contains(t, recorder.Body.String(), `"capture_allowed":false`)

	// 默认 GET 只返回信封：没有值字段，也没有揭示出来的模型名。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage/4242/request-audit/value-detail", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "no-store, private", recorder.Header().Get("Cache-Control"))
	require.Contains(t, recorder.Body.String(), `"state":"stored"`)
	require.NotContains(t, recorder.Body.String(), "claude-sonnet")

	// 未知使用记录不是存在性探针：统一的 404。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage/1/request-audit/value-detail", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNotFound, recorder.Code)

	// 显式揭示才返回值。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/usage/4242/request-audit/value-detail", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, recorder.Body.String(), "claude-sonnet-4-5")

	// 既有请求审计入口一字未改。
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/usage/4242/request-audit", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNotFound, recorder.Code)
}
