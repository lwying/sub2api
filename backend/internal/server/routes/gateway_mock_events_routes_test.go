package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 最小 mock 事件的读取只用普通管理员凭据（与 request-trace 的运维状态端点同一先例），
// 改规则仍然要求管理员登录会话：前者只回元数据与总数，后者会把回复发给下游。
type gatewayMockEventRouteStub struct{}

func (gatewayMockEventRouteStub) ListGatewayMockEvents(context.Context, service.GatewayMockEventListFilter) ([]service.GatewayMockEventRecord, int64, error) {
	return []service.GatewayMockEventRecord{{
		OccurredAt: time.Date(2026, 9, 30, 3, 4, 5, 0, time.UTC), RuleID: "gmr_0123456789abcdef",
		Protocol: "messages", ClientIP: "203.0.113.7",
	}}, 1, nil
}

func TestGatewayMockEventRoutesSeparateReadsFromRuleChanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		GatewayMockEvent: adminhandler.NewGatewayMockEventHandler(gatewayMockEventRouteStub{}),
	}}
	// 普通管理员凭据：非登录会话。
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer admin-token" {
			servermiddleware.AbortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "Authorization required")
			return
		}
		c.Set("auth_method", service.AuditAuthMethodAdminAPIKey)
		c.Next()
	})
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })
	RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)

	registered := map[string]bool{}
	for _, route := range router.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	require.True(t, registered[http.MethodGet+" /api/v1/admin/settings/gateway-mock/events"])

	unauthed := httptest.NewRecorder()
	router.ServeHTTP(unauthed, httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/gateway-mock/events", nil))
	require.Equal(t, http.StatusUnauthorized, unauthed.Code)

	authed := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings/gateway-mock/events", nil)
	request.Header.Set("Authorization", "Bearer admin-token")
	router.ServeHTTP(authed, request)
	require.Equal(t, http.StatusOK, authed.Code, "a read must not require an admin login session")

	// 列表只回元数据：关键词与回复正文没有落库列，也不该在响应里出现。
	lowered := strings.ToLower(authed.Body.String())
	require.Contains(t, lowered, "occurred_at")
	for _, forbidden := range []string{"keyword", "reply", "prompt", "completion"} {
		require.NotContains(t, lowered, forbidden, "the event list must not carry %q", forbidden)
	}

	// 同一个凭据改不了规则：写路径仍要求登录会话，且该判断先于任何设置读写。
	write := httptest.NewRecorder()
	writeRequest := httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings/gateway-mock", strings.NewReader(`{"enabled":true,"rules":[]}`))
	writeRequest.Header.Set("Authorization", "Bearer admin-token")
	writeRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(write, writeRequest)
	require.Equal(t, http.StatusForbidden, write.Code)
	require.Contains(t, write.Body.String(), "GATEWAY_MOCK_ADMIN_SESSION_REQUIRED")
}
